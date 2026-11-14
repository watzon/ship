package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/planner"
	"github.com/watzon/ship/internal/scheduler"
)

// progressHarness records what the fake docker client and agents really did,
// in plan vocabulary ("start web.1"), and owns a fake clock that the fakes
// advance, so tests can compare execution against the printed plan and check
// per-step durations without sleeping.
type progressHarness struct {
	now      time.Time
	executed []string
	events   []string
	oneOff   []agent.RunOneOffContainerParams

	startCost      map[string]time.Duration // "web.1" -> time the container start takes
	healthCost     time.Duration
	releaseCost    time.Duration
	failStart      string // "web.2": run_container fails
	healthFailures map[string]int
	healthCalls    map[string]int
	releaseFail    *agent.CommandResult
}

func newProgressHarness(t *testing.T) *progressHarness {
	t.Helper()
	h := &progressHarness{
		now:            time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC),
		startCost:      map[string]time.Duration{},
		healthFailures: map[string]int{},
		healthCalls:    map[string]int{},
	}
	original := deployClock
	deployClock = func() time.Time { return h.now }
	t.Cleanup(func() { deployClock = original })
	return h
}

func (h *progressHarness) record(step string) { h.executed = append(h.executed, step) }

type progressDocker struct {
	*recordingDeployDocker
	h *progressHarness
}

func (d progressDocker) BuildImage(ctx context.Context, opts docker.BuildOptions) error {
	return d.recordingDeployDocker.BuildImage(ctx, opts)
}

func (d progressDocker) ResolveDigest(ctx context.Context, image string) (string, error) {
	// Image refs look like registry.local/acme/web:stable.
	name := image[strings.LastIndex(image, "/")+1:]
	if i := strings.IndexAny(name, ":@"); i >= 0 {
		name = name[:i]
	}
	d.h.record("resolve " + name)
	return d.recordingDeployDocker.ResolveDigest(ctx, image)
}

type progressAgent struct {
	recordingDeployAgent
	h *progressHarness
}

func (a progressAgent) Call(ctx context.Context, method string, params any, out any) error {
	h := a.h
	switch method {
	case "run_container":
		p := params.(agent.RunContainerParams)
		if name := p.Labels[docker.LabelAccessory]; name != "" {
			h.record("accessory " + name)
		} else {
			target := fmt.Sprintf("%s.%s", p.Labels[docker.LabelService], p.Labels[docker.LabelReplica])
			h.now = h.now.Add(h.startCost[target])
			if target == h.failStart {
				return agent.RemoteError{Code: agent.ErrorDocker, Message: "port is already allocated"}
			}
			h.record("start " + target)
		}
	case "health_check":
		p := params.(agent.HealthCheckParams)
		h.now = h.now.Add(h.healthCost)
		key := p.Command
		h.healthCalls[key]++
		if h.healthCalls[key] <= h.healthFailures[key] {
			if result, ok := out.(*agent.HealthCheckResult); ok {
				*result = agent.HealthCheckResult{OK: false}
			}
			return nil
		}
	case "run_oneoff_container":
		p := params.(agent.RunOneOffContainerParams)
		h.oneOff = append(h.oneOff, p)
		h.now = h.now.Add(h.releaseCost)
		if h.releaseFail != nil {
			if result, ok := out.(*agent.CommandResult); ok {
				*result = *h.releaseFail
			}
			return agent.RemoteError{Code: agent.ErrorDocker, Message: "one-off container failed: docker failed: exit status 1"}
		}
		h.record("release " + p.Labels[docker.LabelService])
	}
	return a.recordingDeployAgent.Call(ctx, method, params, out)
}

func installProgressHooks(t *testing.T, h *progressHarness) {
	t.Helper()
	installDeployHooks(t, progressDocker{&recordingDeployDocker{events: &h.events}, h}, func(host scheduler.Host) deployAgent {
		return progressAgent{recordingDeployAgent{host: host.Name, events: &h.events}, h}
	})
}

func progressConfig() string {
	cfg := strings.Replace(deployWithAccessoryConfig(), "    scale: 1\n", "    scale: 2\n    health:\n      command: ./healthcheck\n    rolling:\n      health_retries: 2\n      health_interval_seconds: 1\n    release:\n      command: php artisan migrate --force\n      timeout_seconds: 600\n", 1)
	return strings.Replace(cfg, "count: 1", "count: 2", 1)
}

func runProgressDeploy(t *testing.T, h *progressHarness) (stdout, stderr string, err error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if werr := os.WriteFile(path, []byte(progressConfig()), 0o644); werr != nil {
		t.Fatal(werr)
	}
	t.Chdir(dir)
	installProgressHooks(t, h)
	var out, errOut bytes.Buffer
	cmd := deployCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"production"})
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

// plannedSteps returns the "kind target" of every `- kind target: ...` plan line.
func plannedSteps(output string) []string {
	var steps []string
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "- "))
		if len(fields) < 2 || fields[0] == "backup-check" {
			continue // backup-check is plan-only: deploy does not run it
		}
		steps = append(steps, fields[0]+" "+strings.TrimSuffix(fields[1], ":"))
	}
	return steps
}

var progressLine = regexp.MustCompile(`(?m)^([a-z-]+) (\S+)(?: on \S+)?: ok \(`)

func progressSteps(output string) []string {
	var steps []string
	for _, m := range progressLine.FindAllStringSubmatch(output, -1) {
		steps = append(steps, m[1]+" "+m[2])
	}
	return steps
}

func TestDeployExecutionOrderComesFromRecordedEventsAndMatchesPlan(t *testing.T) {
	h := newProgressHarness(t)
	out, errOut, err := runProgressDeploy(t, h)
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	if !strings.Contains(out, "- release web: php artisan migrate --force (replica 1, timeout 600s)") {
		t.Fatalf("plan missing release step:\n%s", out)
	}
	want := []string{"resolve web", "accessory redis", "release web", "start web.1", "start web.2"}
	// What the fakes really did, not what the output says.
	if !reflect.DeepEqual(h.executed, want) {
		t.Fatalf("recorded execution = %v, want %v", h.executed, want)
	}
	if planned := plannedSteps(out); !reflect.DeepEqual(planned, h.executed) {
		t.Fatalf("plan order %v != recorded execution order %v\n%s", planned, h.executed, out)
	}
	if printed := progressSteps(out); !reflect.DeepEqual(printed, h.executed) {
		t.Fatalf("progress lines %v != recorded execution order %v\n%s", printed, h.executed, out)
	}
	if errOut != "" {
		t.Fatalf("unexpected stderr (progress reported an unplanned or repeated step): %s", errOut)
	}
	if !strings.Contains(out, "is current") {
		t.Fatalf("missing final current-release line:\n%s", out)
	}
}

func TestDeployMidRolloutFailureShowsEarlierCompletionsThenTheFailure(t *testing.T) {
	h := newProgressHarness(t)
	h.failStart = "web.2"
	out, _, err := runProgressDeploy(t, h)
	if err == nil || !strings.Contains(err.Error(), "port is already allocated") {
		t.Fatalf("err = %v", err)
	}
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "- ") && strings.Contains(line, ": ") {
			lines = append(lines, line)
		}
	}
	got := strings.Join(lines, "\n")
	order := []string{"resolve web: ok", "accessory redis: ok", "release web: ok", "start web.1 on", "health web.1: ok (1 check", "start web.2: FAILED"}
	pos := 0
	for _, want := range order {
		i := strings.Index(got[pos:], want)
		if i < 0 {
			t.Fatalf("missing %q (in order) in:\n%s", want, out)
		}
		pos += i + len(want)
	}
	if strings.Contains(out, "is current") {
		t.Fatalf("failed deploy printed success:\n%s", out)
	}
}

func TestDeployFailedReleaseCommandShowsItsOutput(t *testing.T) {
	h := newProgressHarness(t)
	h.releaseFail = &agent.CommandResult{Output: "Migrating: 2026_create_users\nMigrated: 2026_create_users", Stderr: "SQLSTATE[42S01]: Base table already exists", ExitCode: 1}
	out, _, err := runProgressDeploy(t, h)
	if err == nil {
		t.Fatal("deploy succeeded, want release failure")
	}
	for _, want := range []string{"release web: FAILED", "exit code: 1", "Migrating: 2026_create_users", "SQLSTATE[42S01]: Base table already exists"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\nstart web") || len(h.executed) != 2 {
		t.Fatalf("rollout started after a failed release command:\n%s", out)
	}
}

func TestDeployProgressTimesEachOperation(t *testing.T) {
	h := newProgressHarness(t)
	h.startCost = map[string]time.Duration{"web.1": 4 * time.Second, "web.2": 1 * time.Second}
	h.healthCost = 1 * time.Second
	h.releaseCost = 3 * time.Second
	h.healthFailures["./healthcheck"] = 0
	out, _, err := runProgressDeploy(t, h)
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	for _, want := range []string{
		"release web: ok (3s)",
		"start web.1 on ",
		"start web.2 on ",
		"health web.1: ok (1 check, 1s)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "start web.1") && !strings.HasSuffix(line, ": ok (4s)") {
			t.Fatalf("web.1 start line = %q, want 4s", line)
		}
		if strings.HasPrefix(line, "start web.2") && !strings.HasSuffix(line, ": ok (1s)") {
			t.Fatalf("web.2 start line = %q, want 1s (not time since the previous line)", line)
		}
	}
}

func TestDeployProgressRejectsUnplannedAndRepeatedSteps(t *testing.T) {
	plan := planner.Plan{Actions: []planner.Action{{Kind: "release", Target: "web"}, {Kind: "start", Target: "web.1", Details: "on web-1"}}}
	var out bytes.Buffer
	p := newDeployProgress(&out, plan)
	p.done("release", "web", 2*time.Second)
	p.done("start", "web.1", time.Second)
	if err := p.Err(); err != nil {
		t.Fatalf("planned steps flagged: %v", err)
	}
	p.done("start", "web.1", time.Second)
	p.done("start", "web.9", time.Second)
	err := p.Err()
	if err == nil || !strings.Contains(err.Error(), "start web.1 completed twice") || !strings.Contains(err.Error(), "start web.9 completed but is not in the plan") {
		t.Fatalf("Err = %v", err)
	}
}

func TestDeployProgressFailureLinesIncludeOutput(t *testing.T) {
	var out bytes.Buffer
	p := newDeployProgress(&out, planner.Plan{})
	p.failedWithOutput("release", "web", 1500*time.Millisecond, errors.New("boom"), agent.CommandResult{Output: "a\nb", Stderr: "e", ExitCode: 3})
	want := "release web: FAILED (1.5s): boom\n  exit code: 3\n  stdout:\n    a\n    b\n  stderr:\n    e\n"
	if out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}
