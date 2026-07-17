package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	accessorypkg "github.com/watzon/ship/internal/accessory"
	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/deployment"
	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/state"
)

type recordingBootstrapSSH struct {
	host   scheduler.Host
	events *[]string
}

func (r recordingBootstrapSSH) Run(_ context.Context, command string) (string, error) {
	*r.events = append(*r.events, "bootstrap:"+r.host.Name+":run:"+firstCommandLine(command))
	switch strings.TrimSpace(command) {
	case "uname -s":
		return "Linux", nil
	case "uname -m":
		return "x86_64", nil
	default:
		return "ok", nil
	}
}

func (r recordingBootstrapSSH) RunWithStdin(_ context.Context, command, stdin string) (string, error) {
	*r.events = append(*r.events, fmt.Sprintf("bootstrap:%s:upload:%s:%d", r.host.Name, firstCommandLine(command), len(stdin)))
	return "ok", nil
}

func firstCommandLine(command string) string {
	if line, _, ok := strings.Cut(command, "\n"); ok {
		return line
	}
	return command
}

func installBootstrapHooks(t *testing.T, events *[]string) {
	t.Helper()
	originalBinary := readCurrentShipBinary
	originalResolve := resolveShipBinaryForHost
	originalSSH := newBootstrapSSH
	originalAttempts := bootstrapMaxAttempts
	originalDelay := bootstrapRetryDelay
	readCurrentShipBinary = func() ([]byte, error) {
		return []byte("ship-test-binary"), nil
	}
	resolveShipBinaryForHost = func(ctx context.Context, host scheduler.Host, opts *options) ([]byte, error) {
		return readCurrentShipBinary()
	}
	newBootstrapSSH = func(host scheduler.Host, dryRun bool) bootstrapSSH {
		return recordingBootstrapSSH{host: host, events: events}
	}
	bootstrapMaxAttempts = 2
	bootstrapRetryDelay = 0
	t.Cleanup(func() {
		readCurrentShipBinary = originalBinary
		resolveShipBinaryForHost = originalResolve
		newBootstrapSSH = originalSSH
		bootstrapMaxAttempts = originalAttempts
		bootstrapRetryDelay = originalDelay
	})
}

func installUpgradeSSHHook(t *testing.T, events *[]string) {
	t.Helper()
	originalSSH := newBootstrapSSH
	newBootstrapSSH = func(host scheduler.Host, dryRun bool) bootstrapSSH {
		return recordingBootstrapSSH{host: host, events: events}
	}
	t.Cleanup(func() { newBootstrapSSH = originalSSH })
}

func installShipBinaryReader(t *testing.T, content []byte) {
	t.Helper()
	originalBinary := readCurrentShipBinary
	originalResolve := resolveShipBinaryForHost
	readCurrentShipBinary = func() ([]byte, error) {
		return append([]byte(nil), content...), nil
	}
	resolveShipBinaryForHost = func(ctx context.Context, host scheduler.Host, opts *options) ([]byte, error) {
		return readCurrentShipBinary()
	}
	t.Cleanup(func() {
		readCurrentShipBinary = originalBinary
		resolveShipBinaryForHost = originalResolve
	})
}

func TestRecordEventWarnsOnceWithoutMessage(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(statePath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(statePath)
	event := state.Event{
		Environment: "production",
		Kind:        "deploy",
		Status:      "failed",
		Message:     "sensitive command output",
	}
	wantErr := store.RecordEvent(event)
	if wantErr == nil {
		t.Fatal("expected RecordEvent error")
	}

	var warning bytes.Buffer
	originalWarningWriter := eventWarningWriter
	eventWarningWriter = &warning
	t.Cleanup(func() { eventWarningWriter = originalWarningWriter })

	recordEvent(store, event)
	got := warning.String()
	if strings.Count(got, "warning:") != 1 {
		t.Fatalf("warning count = %d, output = %q", strings.Count(got, "warning:"), got)
	}
	for _, value := range []string{"production", "deploy", "failed", wantErr.Error()} {
		if !strings.Contains(got, value) {
			t.Fatalf("warning %q does not contain %q", got, value)
		}
	}
	if strings.Contains(got, event.Message) {
		t.Fatalf("warning leaked event message: %q", got)
	}
}

func writeConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(config.Sample()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func singleHostConfig() string {
	return `project: demo
registry: ghcr.io/acme/demo

environments:
  production:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 1

services:
  web:
    image:
      ref: ghcr.io/acme/demo:web
    pool: web
    scale: 1
`
}

func deployBuildConfig() string {
	return `project: demo
registry: registry.local/acme/demo

environments:
  production:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 2

services:
  web:
    image:
      build: .
      dockerfile: Dockerfile
      tags:
        - latest
        - production
      build_args:
        RAILS_ENV: production
      target: runtime
      builder: ship-cloud
      platform: linux/amd64
      pull: true
      no_cache_filter:
        - install
        - assets
      cache_from:
        - type=registry,ref=registry.local/acme/demo:build-cache
      cache_to:
        - type=registry,ref=registry.local/acme/demo:build-cache,mode=max
      secrets:
        - id=npm_token,env=NPM_TOKEN
      ssh:
        - default
    command: ./bin/server
    pool: web
    scale: 2
    ports: [3000]

  worker:
    image:
      ref: registry.local/acme/worker:stable
    pool: web
    scale: 0
`
}

func deployWithAccessoryConfig() string {
	return `project: demo
registry: registry.local/acme/demo

environments:
  production:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 1

services:
  web:
    image:
      ref: registry.local/acme/web:stable
    command: ./bin/server
    pool: web
    scale: 1

accessories:
  redis:
    image: redis:7-alpine
    pool: web
`
}

func restartConfig() string {
	return `project: demo
registry: registry.local/acme/demo
secrets:
  - DATABASE_URL

environments:
  production:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 2

services:
  web:
    image:
      ref: registry.local/acme/web:stable
    labels:
      com.example.team: platform
    network_aliases:
      - app
    command: ./bin/server
    pool: web
    scale: 2
    ports: [3000]
    health:
      http: /up
    secrets:
      - DATABASE_URL
    rolling:
      health_retries: 1
      health_interval_seconds: 1
`
}

func serviceAccessoryStatusConfig() string {
	return `project: demo
registry: registry.local/acme/demo

environments:
  production:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 1

services:
  web:
    image:
      ref: registry.local/acme/web:stable
    command: ./bin/server
    pool: web
    scale: 1

accessories:
  postgres:
    image: postgres:17
    pool: web
    primary: true
`
}

func secretDeployConfig() string {
	return `project: demo
registry: registry.local/acme/demo

environments:
  production:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 1

services:
  web:
    image:
      ref: registry.local/acme/web:stable
    command: ./bin/server
    pool: web
    scale: 1
    env:
      - RACK_ENV=production
    secrets:
      - SHIP_TEST_DATABASE_URL

secrets:
  - SHIP_TEST_DATABASE_URL
`
}

func rollingDeployConfig() string {
	return `project: demo
registry: registry.local/acme/demo

environments:
  production:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 1

services:
  web:
    image:
      build: .
      dockerfile: Dockerfile
    command: ./bin/server
    pool: web
    scale: 1
    ports: [3000]
    health:
      http: /up
    ingress:
      domains:
        - example.com
`
}

func accessoryContainer(name, host, status string) docker.ContainerSummary {
	return docker.ContainerSummary{
		ID:     host + "-" + name,
		Image:  "postgres:17",
		Names:  accessorypkg.ContainerName("demo", "production", name),
		Status: status,
		Labels: map[string]string{
			docker.LabelManagedBy:   docker.LabelManagedByValue,
			docker.LabelProject:     "demo",
			docker.LabelEnvironment: "production",
			docker.LabelAccessory:   name,
		},
	}
}

func serviceContainer(host, service string, replica int, release, status string) docker.ContainerSummary {
	return docker.ContainerSummary{
		ID:     fmt.Sprintf("%s-%s-%d", host, service, replica),
		Image:  "registry.local/acme/" + service + ":" + release,
		Names:  deployment.ContainerName("demo", "production", service, replica, release),
		Status: status,
		Labels: map[string]string{
			docker.LabelManagedBy:   docker.LabelManagedByValue,
			docker.LabelProject:     "demo",
			docker.LabelEnvironment: "production",
			docker.LabelService:     service,
			docker.LabelReplica:     strconv.Itoa(replica),
			docker.LabelRelease:     release,
		},
	}
}

func caddyContainer(host, status string) docker.ContainerSummary {
	return docker.ContainerSummary{
		ID:     host + "-caddy",
		Image:  "caddy:2",
		Names:  deployment.CaddyContainerName("demo", "production"),
		Status: status,
		Labels: map[string]string{
			docker.LabelManagedBy:   docker.LabelManagedByValue,
			docker.LabelProject:     "demo",
			docker.LabelEnvironment: "production",
			docker.LabelService:     "caddy",
		},
	}
}

func timelineContains(events []state.Event, kind, status, release string) bool {
	for _, event := range events {
		if event.Kind == kind && event.Status == status && event.Release == release {
			return true
		}
	}
	return false
}

type releaseStateWrite struct {
	Host    string
	Release state.Release
}

type deployAgentFunc func(context.Context, string, any, any) error

func (f deployAgentFunc) Call(ctx context.Context, method string, params any, out any) error {
	return f(ctx, method, params, out)
}

func installAgentHook(t *testing.T, agentFactory func(scheduler.Host) deployAgent) {
	t.Helper()
	originalAgent := newDeployAgent
	newDeployAgent = agentFactory
	t.Cleanup(func() {
		newDeployAgent = originalAgent
	})
}

type observabilityAgent struct {
	host      string
	events    *[]string
	observed  map[string][]docker.ContainerSummary
	logCalls  *[]agent.LogsParams
	execCalls *[]agent.ExecContainerParams
}

func (o *observabilityAgent) Call(ctx context.Context, method string, params any, out any) error {
	switch method {
	case "list_ship_containers":
		*o.events = append(*o.events, fmt.Sprintf("agent:%s:list_ship_containers", o.host))
		if containers, ok := out.(*[]docker.ContainerSummary); ok {
			*containers = append([]docker.ContainerSummary(nil), o.observed[o.host]...)
		}
	case "logs":
		p := params.(agent.LogsParams)
		*o.events = append(*o.events, fmt.Sprintf("agent:%s:logs:%s:%d", o.host, p.Name, p.Lines))
		if o.logCalls != nil {
			*o.logCalls = append(*o.logCalls, p)
		}
		if result, ok := out.(*map[string]string); ok {
			*result = map[string]string{"logs": "logs from " + o.host}
		}
	case "exec_container":
		p := params.(agent.ExecContainerParams)
		*o.events = append(*o.events, fmt.Sprintf("agent:%s:exec:%s:%s", o.host, p.Name, p.Command))
		if o.execCalls != nil {
			*o.execCalls = append(*o.execCalls, p)
		}
		if result, ok := out.(*agent.CommandResult); ok {
			*result = agent.CommandResult{Output: "exec from " + o.host}
		}
	default:
		*o.events = append(*o.events, fmt.Sprintf("agent:%s:%s", o.host, method))
	}
	return nil
}

func installDeployHooks(t *testing.T, dockerClient deployDocker, agentFactory func(scheduler.Host) deployAgent) {
	t.Helper()
	originalDocker := newDeployDocker
	originalAgent := newDeployAgent
	originalNow := deployNow
	originalGitRevision := deployGitRevision
	originalReleaseID := newReleaseID
	newDeployDocker = func() deployDocker { return dockerClient }
	newDeployAgent = func(host scheduler.Host) deployAgent {
		return defaultNegotiationAgent{delegate: agentFactory(host)}
	}
	deployNow = func() time.Time {
		return time.Date(2026, 6, 30, 12, 34, 56, 123456789, time.FixedZone("MDT", -6*60*60))
	}
	deployGitRevision = func(context.Context) (string, error) {
		return "abc123def456", nil
	}
	newReleaseID = func() (string, error) {
		return "abc123def456-20260630T183456.123456789Z", nil
	}
	t.Cleanup(func() {
		newDeployDocker = originalDocker
		newDeployAgent = originalAgent
		deployNow = originalNow
		deployGitRevision = originalGitRevision
		newReleaseID = originalReleaseID
	})
}

type defaultNegotiationAgent struct {
	delegate deployAgent
}

func (a defaultNegotiationAgent) Call(ctx context.Context, method string, params any, out any) error {
	if err := a.delegate.Call(ctx, method, params, out); err != nil {
		return err
	}
	if method != "negotiate" {
		return nil
	}
	if result, ok := out.(*agent.NegotiateResult); ok && result.SupportedMethods == nil {
		result.AgentVersion = agent.Version()
		result.ProtocolVersion = agent.AgentProtocol
		result.SupportedMethods = append([]string(nil), requiredRolloutAgentMethods...)
	}
	return nil
}

func unsetEnv(t *testing.T, name string) {
	t.Helper()
	value, ok := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(name, value)
			return
		}
		_ = os.Unsetenv(name)
	})
}

type recordingDeployDocker struct {
	events    *[]string
	builds    []docker.BuildOptions
	resolved  map[string]string
	auths     map[string]docker.RegistryAuth
	failStage string
}

func (r *recordingDeployDocker) BuildImage(ctx context.Context, opts docker.BuildOptions) error {
	r.builds = append(r.builds, opts)
	*r.events = append(*r.events, "build:"+opts.Tag)
	if r.failStage == "build" {
		return errors.New("build failed")
	}
	return nil
}

func (r *recordingDeployDocker) Push(ctx context.Context, image string) error {
	*r.events = append(*r.events, "push:"+image)
	if r.failStage == "push" {
		return errors.New("push failed")
	}
	return nil
}

func (r *recordingDeployDocker) ResolveDigest(ctx context.Context, image string) (string, error) {
	*r.events = append(*r.events, "resolve:"+image)
	if r.failStage == "resolve" {
		return "", errors.New("resolve failed")
	}
	if resolved := r.resolved[image]; resolved != "" {
		return resolved, nil
	}
	return image + "@sha256:" + strings.Repeat("2", 64), nil
}

func (r *recordingDeployDocker) RegistryAuth(ctx context.Context, image string) (docker.RegistryAuth, bool, error) {
	if r.failStage == "registry_auth" {
		*r.events = append(*r.events, "registry_auth:"+image)
		return docker.RegistryAuth{}, false, errors.New("registry auth failed")
	}
	auth, ok := r.auths[image]
	if ok {
		*r.events = append(*r.events, "registry_auth:"+image)
	}
	return auth, ok, nil
}

type recordingDeployAgent struct {
	host          string
	events        *[]string
	observed      map[string][]docker.ContainerSummary
	releaseWrites *[]releaseStateWrite
	current       *state.Release
	cronSyncs     *[]agent.SyncCronFilesParams
	oneOffRuns    *[]agent.RunOneOffContainerParams
	writes        *[]agent.WriteFileParams
}

func (r recordingDeployAgent) Call(ctx context.Context, method string, params any, out any) error {
	switch method {
	case "write_release_state":
		p := params.(agent.WriteReleaseStateParams)
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:write_release_state:%s:%s", r.host, p.Release.ID, p.Release.Status))
		if r.releaseWrites != nil {
			*r.releaseWrites = append(*r.releaseWrites, releaseStateWrite{Host: r.host, Release: p.Release})
		}
	case "list_ship_containers":
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:list_ship_containers", r.host))
		if containers, ok := out.(*[]docker.ContainerSummary); ok {
			*containers = append([]docker.ContainerSummary(nil), r.observed[r.host]...)
		}
	case "read_release_state":
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:read_release_state", r.host))
		if r.current != nil {
			if release, ok := out.(*state.Release); ok {
				*release = *r.current
			}
		}
	case "pull":
		image := params.(map[string]string)["image"]
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:pull:%s", r.host, image))
	case "write_registry_auth":
		p := params.(agent.WriteRegistryAuthParams)
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:write_registry_auth:%s", r.host, p.Server))
	case "write_file":
		p := params.(agent.WriteFileParams)
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:write_file:%s", r.host, p.Path))
		if r.writes != nil {
			*r.writes = append(*r.writes, p)
		}
	case "sync_cron_files":
		p := params.(agent.SyncCronFilesParams)
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:sync_cron_files:%s:%d", r.host, p.Prefix, len(p.Files)))
		if r.cronSyncs != nil {
			*r.cronSyncs = append(*r.cronSyncs, p)
		}
	case "ensure_network":
		return nil
	case "run_container":
		p := params.(agent.RunContainerParams)
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:run:%s", r.host, p.Image))
	case "health_check":
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:health_check", r.host))
		if result, ok := out.(*agent.HealthCheckResult); ok {
			*result = agent.HealthCheckResult{OK: true}
		}
	case "run_oneoff_container":
		p := params.(agent.RunOneOffContainerParams)
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:run_oneoff:%s:%s", r.host, p.Name, p.Command))
		if r.oneOffRuns != nil {
			*r.oneOffRuns = append(*r.oneOffRuns, p)
		}
		if result, ok := out.(*agent.CommandResult); ok {
			*result = agent.CommandResult{Output: "one-off complete"}
		}
	case "docker_inspect":
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:docker_inspect", r.host))
		populateRunningDockerInspect(out)
	default:
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:%s", r.host, method))
	}
	return nil
}

type secretDeployAgent struct {
	host          string
	events        *[]string
	writes        *[]agent.WriteFileParams
	runs          *[]agent.RunContainerParams
	releaseWrites *[]releaseStateWrite
}

func (s *secretDeployAgent) Call(ctx context.Context, method string, params any, out any) error {
	switch method {
	case "write_release_state":
		p := params.(agent.WriteReleaseStateParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:write_release_state:%s:%s", s.host, p.Release.ID, p.Release.Status))
		if s.releaseWrites != nil {
			*s.releaseWrites = append(*s.releaseWrites, releaseStateWrite{Host: s.host, Release: p.Release})
		}
	case "write_file":
		p := params.(agent.WriteFileParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:write_file:%s:%04o", s.host, p.Path, p.Mode))
		if s.writes != nil {
			*s.writes = append(*s.writes, p)
		}
	case "list_ship_containers":
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:list_ship_containers", s.host))
		if containers, ok := out.(*[]docker.ContainerSummary); ok {
			*containers = nil
		}
	case "pull":
		image := params.(map[string]string)["image"]
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:pull:%s", s.host, image))
	case "ensure_network":
		return nil
	case "run_container":
		p := params.(agent.RunContainerParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:run:%s", s.host, p.Name))
		if s.runs != nil {
			*s.runs = append(*s.runs, p)
		}
	default:
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:%s", s.host, method))
	}
	return nil
}

type scriptedDeployAgent struct {
	host          string
	events        *[]string
	observed      []docker.ContainerSummary
	failMethod    string
	releaseWrites *[]releaseStateWrite
}

func (s *scriptedDeployAgent) Call(ctx context.Context, method string, params any, out any) error {
	switch method {
	case "write_release_state":
		p := params.(agent.WriteReleaseStateParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:write_release_state:%s:%s", s.host, p.Release.ID, p.Release.Status))
		if s.releaseWrites != nil {
			*s.releaseWrites = append(*s.releaseWrites, releaseStateWrite{Host: s.host, Release: p.Release})
		}
	case "list_ship_containers":
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:list_ship_containers", s.host))
		if containers, ok := out.(*[]docker.ContainerSummary); ok {
			*containers = append([]docker.ContainerSummary(nil), s.observed...)
		}
	case "pull":
		image := params.(map[string]string)["image"]
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:pull:%s", s.host, image))
	case "ensure_network", "write_file", "run_oneoff_container":
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:%s", s.host, method))
	case "docker_inspect":
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:docker_inspect", s.host))
		populateRunningDockerInspect(out)
	case "run_container":
		p := params.(agent.RunContainerParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:run:%s", s.host, p.Name))
	case "health_check":
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:health", s.host))
		if result, ok := out.(*agent.HealthCheckResult); ok {
			*result = agent.HealthCheckResult{OK: true}
		}
	case "stop_container":
		name := params.(map[string]string)["name"]
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:stop:%s", s.host, name))
	case "stop_container_keep":
		name := params.(map[string]string)["name"]
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:stop_keep:%s", s.host, name))
	case "start_container":
		name := params.(map[string]string)["name"]
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:start_existing:%s", s.host, name))
	case "remove_container":
		name := params.(map[string]string)["name"]
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:remove:%s", s.host, name))
	default:
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:%s", s.host, method))
	}
	if method == s.failMethod {
		return fmt.Errorf("%s failed", method)
	}
	return nil
}

func populateRunningDockerInspect(out any) {
	if result, ok := out.(*agent.DockerInspectResult); ok {
		result.Inspect = json.RawMessage(`[{"State":{"Running":true}}]`)
	}
}

type panicDeployDocker struct {
	t *testing.T
}

func (p panicDeployDocker) BuildImage(context.Context, docker.BuildOptions) error {
	p.t.Fatal("unexpected build")
	return nil
}

func (p panicDeployDocker) Push(context.Context, string) error {
	p.t.Fatal("unexpected push")
	return nil
}

func (p panicDeployDocker) ResolveDigest(context.Context, string) (string, error) {
	p.t.Fatal("unexpected digest resolve")
	return "", nil
}

func (p panicDeployDocker) RegistryAuth(context.Context, string) (docker.RegistryAuth, bool, error) {
	p.t.Fatal("unexpected registry auth")
	return docker.RegistryAuth{}, false, nil
}

func boolPtr(value bool) *bool {
	return &value
}
