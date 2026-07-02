package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/docker"
	providerpkg "github.com/watzon/ship/internal/provider"
	"github.com/watzon/ship/internal/provider/hetzner"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/state"
)

func TestAcceptanceDryRunFlowFromBlankProject(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, config.DefaultConfigFile)

	out := runAcceptanceCommand(t, initCmd(&options{configPath: path}))
	if !strings.Contains(out, "created") {
		t.Fatalf("init output = %q", out)
	}
	if err := os.WriteFile(path, []byte(acceptanceDryRunConfigYAML(sampleAppPath(t))), 0o644); err != nil {
		t.Fatal(err)
	}

	out = runAcceptanceCommand(t, provisionCmd(&options{configPath: path}), "plan", "production")
	assertAcceptanceOutput(t, out, "provision web-1", "provision ingress-1")

	out = runAcceptanceCommand(t, provisionCmd(&options{configPath: path, dryRun: true}), "apply", "production")
	assertAcceptanceOutput(t, out, "would provision web-1 pool=web", "would provision worker-1 pool=worker")
	if _, err := os.Stat(filepath.Join(dir, config.LocalStateDir, "environments", "production", "hosts.json")); !os.IsNotExist(err) {
		t.Fatalf("dry-run provision wrote host facts: %v", err)
	}

	out = runAcceptanceCommand(t, planCmd(&options{configPath: path}), "production")
	assertAcceptanceOutput(t, out, "build web", "start web.2", "backup-check postgres")

	out = runAcceptanceCommand(t, deployCmd(&options{configPath: path, dryRun: true}), "production")
	assertAcceptanceOutput(t, out, "resolve web: pushed image -> immutable digest", "ingress web: sample.example.com", "reload caddy on ingress-1 after validation")

	out = runAcceptanceCommand(t, scaleCmd(&options{configPath: path, dryRun: true}), "production", "web=3")
	assertAcceptanceOutput(t, out, "start web.3: on web-1", "accessory postgres")
}

func TestAcceptanceFakeInfrastructureWorkflow(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(acceptanceFakeInfraConfigYAML(sampleAppPath(t))), 0o644); err != nil {
		t.Fatal(err)
	}

	var events []string
	fakeDocker := &acceptanceDocker{events: &events}
	fakeInfra := newAcceptanceFakeInfra(t, &events)
	installAcceptanceDeployHooks(t, fakeDocker, fakeInfra.agent)
	installBootstrapHooks(t, &events)

	fakeHetzner := newAcceptanceHetznerAPI(t)
	originalNewEnvironmentProvider := newEnvironmentProvider
	newEnvironmentProvider = func(_ config.Environment, dryRun bool) (providerpkg.Provider, error) {
		return hetzner.Client{
			Token:        "acceptance-token",
			DryRun:       dryRun,
			HTTP:         fakeHetzner.server.Client(),
			BaseURL:      fakeHetzner.server.URL,
			PollInterval: time.Nanosecond,
		}, nil
	}
	t.Cleanup(func() {
		newEnvironmentProvider = originalNewEnvironmentProvider
	})

	out := runAcceptanceCommand(t, provisionCmd(&options{configPath: path}), "apply", "production", "--yes")
	assertAcceptanceOutput(t, out, "created web-1 pool=web", "created worker-1 pool=worker", "created data-1 pool=data")
	if got, want := len(fakeHetzner.createdNames()), 4; got != want {
		t.Fatalf("created servers = %d, want %d (%v)", got, want, fakeHetzner.createdNames())
	}
	if _, err := os.Stat(filepath.Join(dir, config.LocalStateDir, "environments", "production", "hosts.json")); err != nil {
		t.Fatalf("host facts were not written: %v", err)
	}

	out = runAcceptanceCommand(t, provisionCmd(&options{configPath: path}), "apply", "production", "--yes")
	assertAcceptanceOutput(t, out, "exists web-1 pool=web", "exists worker-1 pool=worker")
	if got, want := len(fakeHetzner.createdNames()), 4; got != want {
		t.Fatalf("second provision was not idempotent: created servers = %d, want %d", got, want)
	}

	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	facts, err := store.ReadHostFacts("production")
	if err != nil {
		t.Fatal(err)
	}
	web1Contact := ""
	for _, fact := range facts {
		if fact.Name == "web-1" && fact.Pool == "web" {
			web1Contact = hostFactContact(fact)
		}
	}
	if web1Contact == "" {
		t.Fatalf("web-1 contact missing from facts: %+v", facts)
	}
	runAcceptanceCommand(t, deployCmd(&options{configPath: path}), "production")
	first := currentAcceptanceRelease(t, store)
	assertAcceptanceEvent(t, events, "build:registry.local/ship-sample:web-"+first.ID)
	assertAcceptanceEvent(t, events, "agent:web-1:contact:"+web1Contact)
	assertAcceptanceEvent(t, events, "agent:web-1:run:ship_sample_production_web_1_"+first.ID)

	var status statusView
	out = runAcceptanceCommand(t, statusCmd(&options{configPath: path}), "production", "--json")
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatal(err)
	}
	if status.CurrentRelease == nil || status.CurrentRelease.ID != first.ID || status.Summary.Drift {
		t.Fatalf("status after first deploy = %+v", status)
	}

	out = runAcceptanceCommand(t, logsCmd(&options{configPath: path}), "production", "web", "--replica", "1", "--lines", "5")
	assertAcceptanceOutput(t, out, "==> web-1/ship_sample_production_web_1_"+first.ID+" <==", "logs from web-1")

	out = runAcceptanceCommand(t, scaleCmd(&options{configPath: path, dryRun: true}), "production", "web=3")
	assertAcceptanceOutput(t, out, "start web.3: on web-1")

	runAcceptanceCommand(t, deployCmd(&options{configPath: path}), "production")
	second := currentAcceptanceRelease(t, store)
	if second.ID == first.ID {
		t.Fatalf("second deploy reused release id %s", second.ID)
	}

	fakeInfra.failHealth = true
	failedOut, err := runAcceptanceCommandError(t, deployCmd(&options{configPath: path}), "production")
	fakeInfra.failHealth = false
	if err == nil || !strings.Contains(err.Error(), "health") {
		t.Fatalf("expected failed health deploy, err=%v out=%s", err, failedOut)
	}
	current := currentAcceptanceRelease(t, store)
	if current.ID != second.ID {
		t.Fatalf("failed deploy changed current release: got %s want %s", current.ID, second.ID)
	}

	out = runAcceptanceCommand(t, recoverCmd(&options{configPath: path}), "production")
	assertAcceptanceOutput(t, out, "Failed releases", "rollback target: "+first.ID, "suggested rollback: ship rollback production --to "+first.ID+" --allow-data-rollback")

	out = runAcceptanceCommand(t, rollbackCmd(&options{configPath: path}), "production", "--to", first.ID, "--allow-data-rollback")
	assertAcceptanceOutput(t, out, "rollback production to release "+first.ID)
	current = currentAcceptanceRelease(t, store)
	if current.ID != first.ID {
		t.Fatalf("rollback current release = %s, want %s", current.ID, first.ID)
	}

	out = runAcceptanceCommand(t, statusCmd(&options{configPath: path}), "production", "--json")
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatal(err)
	}
	if status.CurrentRelease == nil || status.CurrentRelease.ID != first.ID || status.Summary.Drift {
		t.Fatalf("status after rollback = %+v", status)
	}
	assertAcceptanceEvent(t, events, "agent:web-1:stop:ship_sample_production_web_1_"+second.ID)
}

func acceptanceDryRunConfigYAML(sampleApp string) string {
	return fmt.Sprintf(`project: sample
registry: registry.local/ship-sample

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
        worker:
          count: 1
        ingress:
          count: 1
        data:
          count: 1

services:
  web:
    image:
      build: %q
      dockerfile: Dockerfile
    command: /app/sample-app server
    pool: web
    scale: 2
    ports: [3000]
    health:
      http: /up
    ingress:
      domains:
        - sample.example.com

  worker:
    image:
      build: %q
      dockerfile: Dockerfile
    command: /app/sample-app worker
    pool: worker
    scale: 1
    health:
      command: /app/sample-app healthcheck

accessories:
  postgres:
    image: postgres:17
    pool: data
    primary: true
    volumes:
      - postgres-data:/var/lib/postgresql/data
    backup:
      command: pg_dumpall
      restore_command: psql -f "$SHIP_BACKUP_ARTIFACT"
      artifact_dir: /var/lib/ship/backups
      required: true
      restore_check: true
`, sampleApp, sampleApp)
}

func acceptanceFakeInfraConfigYAML(sampleApp string) string {
	return fmt.Sprintf(`project: sample
registry: registry.local/ship-sample

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
        worker:
          count: 1
        data:
          count: 1

services:
  web:
    image:
      build: %q
      dockerfile: Dockerfile
    command: /app/sample-app server
    pool: web
    scale: 2
    health:
      command: /app/sample-app healthcheck

  worker:
    image:
      build: %q
      dockerfile: Dockerfile
    command: /app/sample-app worker
    pool: worker
    scale: 1
    health:
      command: /app/sample-app healthcheck

accessories:
  postgres:
    image: postgres:17
    pool: data
    primary: true
    volumes:
      - postgres-data:/var/lib/postgresql/data
    backup:
      command: pg_dumpall
      restore_command: psql -f "$SHIP_BACKUP_ARTIFACT"
      artifact_dir: /var/lib/ship/backups
      required: true
      restore_check: true
`, sampleApp, sampleApp)
}

func runAcceptanceCommand(t *testing.T, cmd *cobra.Command, args ...string) string {
	t.Helper()
	out, err := runAcceptanceCommandError(t, cmd, args...)
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func runAcceptanceCommandError(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func assertAcceptanceOutput(t *testing.T, output string, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if !strings.Contains(output, needle) {
			t.Fatalf("output missing %q:\n%s", needle, output)
		}
	}
}

func assertAcceptanceEvent(t *testing.T, events []string, needle string) {
	t.Helper()
	for _, event := range events {
		if strings.Contains(event, needle) {
			return
		}
	}
	t.Fatalf("events missing %q:\n%s", needle, strings.Join(events, "\n"))
}

func currentAcceptanceRelease(t *testing.T, store state.Store) state.Release {
	t.Helper()
	release, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	return release
}

func sampleAppPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate acceptance test file")
	}
	path := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "testdata", "sample-app"))
	if _, err := os.Stat(filepath.Join(path, "Dockerfile")); err != nil {
		t.Fatalf("sample app fixture missing: %v", err)
	}
	return path
}

func installAcceptanceDeployHooks(t *testing.T, dockerClient deployDocker, agentFactory func(scheduler.Host) deployAgent) {
	t.Helper()
	originalDocker := newDeployDocker
	originalAgent := newDeployAgent
	originalNow := deployNow
	originalGitRevision := deployGitRevision
	clock := &acceptanceClock{next: time.Date(2026, 6, 30, 18, 0, 0, 0, time.UTC)}
	newDeployDocker = func() deployDocker { return dockerClient }
	newDeployAgent = agentFactory
	deployNow = clock.Now
	deployGitRevision = func(context.Context) (string, error) {
		return "accept123456", nil
	}
	t.Cleanup(func() {
		newDeployDocker = originalDocker
		newDeployAgent = originalAgent
		deployNow = originalNow
		deployGitRevision = originalGitRevision
	})
}

type acceptanceClock struct {
	next time.Time
}

func (c *acceptanceClock) Now() time.Time {
	now := c.next
	c.next = c.next.Add(time.Second)
	return now
}

type acceptanceDocker struct {
	events *[]string
	builds []docker.BuildOptions
}

func (d *acceptanceDocker) BuildImage(ctx context.Context, opts docker.BuildOptions) error {
	d.builds = append(d.builds, opts)
	*d.events = append(*d.events, "build:"+opts.Tag)
	return nil
}

func (d *acceptanceDocker) Push(ctx context.Context, image string) error {
	*d.events = append(*d.events, "push:"+image)
	return nil
}

func (d *acceptanceDocker) ResolveDigest(ctx context.Context, image string) (string, error) {
	*d.events = append(*d.events, "resolve:"+image)
	sum := sha256.Sum256([]byte(image))
	return imageRepository(image) + "@sha256:" + hex.EncodeToString(sum[:]), nil
}

func (d *acceptanceDocker) RegistryAuth(context.Context, string) (docker.RegistryAuth, bool, error) {
	return docker.RegistryAuth{}, false, nil
}

func imageRepository(image string) string {
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon > slash {
		return image[:colon]
	}
	return image
}

type acceptanceFakeInfra struct {
	t          *testing.T
	events     *[]string
	containers map[string][]docker.ContainerSummary
	failHealth bool
}

func newAcceptanceFakeInfra(t *testing.T, events *[]string) *acceptanceFakeInfra {
	t.Helper()
	return &acceptanceFakeInfra{
		t:          t,
		events:     events,
		containers: map[string][]docker.ContainerSummary{},
	}
}

func (f *acceptanceFakeInfra) agent(host scheduler.Host) deployAgent {
	if host.Contact != "" {
		f.record("agent:%s:contact:%s", host.Name, host.Contact)
	}
	return acceptanceFakeAgent{infra: f, host: host}
}

func (f *acceptanceFakeInfra) record(format string, args ...any) {
	*f.events = append(*f.events, fmt.Sprintf(format, args...))
}

func (f *acceptanceFakeInfra) containersFor(host string) []docker.ContainerSummary {
	containers := f.containers[host]
	out := make([]docker.ContainerSummary, 0, len(containers))
	for _, container := range containers {
		labels := map[string]string{}
		for key, value := range container.Labels {
			labels[key] = value
		}
		container.Labels = labels
		out = append(out, container)
	}
	return out
}

func (f *acceptanceFakeInfra) upsertContainer(host string, container docker.ContainerSummary) {
	containers := f.containers[host]
	filtered := containers[:0]
	for _, existing := range containers {
		if existing.Names != container.Names {
			filtered = append(filtered, existing)
		}
	}
	f.containers[host] = append(filtered, container)
}

func (f *acceptanceFakeInfra) removeContainer(host, name string) {
	containers := f.containers[host]
	filtered := containers[:0]
	for _, existing := range containers {
		if existing.Names != name {
			filtered = append(filtered, existing)
		}
	}
	f.containers[host] = filtered
}

type acceptanceFakeAgent struct {
	infra *acceptanceFakeInfra
	host  scheduler.Host
}

func (a acceptanceFakeAgent) Call(ctx context.Context, method string, params any, out any) error {
	switch method {
	case "write_release_state":
		p := params.(agent.WriteReleaseStateParams)
		a.infra.record("agent:%s:write_release_state:%s:%s", a.host.Name, p.Release.ID, p.Release.Status)
	case "list_ship_containers":
		a.infra.record("agent:%s:list_ship_containers", a.host.Name)
		if containers, ok := out.(*[]docker.ContainerSummary); ok {
			*containers = a.infra.containersFor(a.host.Name)
		}
	case "pull":
		image := params.(map[string]string)["image"]
		a.infra.record("agent:%s:pull:%s", a.host.Name, image)
	case "run_container":
		p := params.(agent.RunContainerParams)
		a.infra.record("agent:%s:run:%s", a.host.Name, p.Name)
		labels := map[string]string{docker.LabelManagedBy: docker.LabelManagedByValue}
		for key, value := range p.Labels {
			labels[key] = value
		}
		a.infra.upsertContainer(a.host.Name, docker.ContainerSummary{
			ID:     a.host.Name + "-" + p.Name,
			Image:  p.Image,
			Names:  p.Name,
			Status: "Up 1 second",
			Labels: labels,
		})
	case "health_check":
		p := params.(agent.HealthCheckParams)
		a.infra.record("agent:%s:health_check:%s%s", a.host.Name, p.Command, p.URL)
		if result, ok := out.(*agent.HealthCheckResult); ok {
			*result = agent.HealthCheckResult{OK: !a.infra.failHealth}
		}
	case "stop_container":
		name := params.(map[string]string)["name"]
		a.infra.record("agent:%s:stop:%s", a.host.Name, name)
		a.infra.removeContainer(a.host.Name, name)
	case "logs":
		p := params.(agent.LogsParams)
		a.infra.record("agent:%s:logs:%s:%d", a.host.Name, p.Name, p.Lines)
		if result, ok := out.(*map[string]string); ok {
			*result = map[string]string{"logs": "logs from " + a.host.Name}
		}
	case "caddy_reload":
		a.infra.record("agent:%s:caddy_reload", a.host.Name)
	case "write_file", "run_oneoff_container", "ensure_network":
		a.infra.record("agent:%s:%s", a.host.Name, method)
	case "docker_inspect":
		a.infra.record("agent:%s:docker_inspect", a.host.Name)
		if result, ok := out.(*agent.DockerInspectResult); ok {
			result.Inspect = json.RawMessage(`[{"State":{"Running":true}}]`)
		}
	default:
		a.infra.record("agent:%s:%s", a.host.Name, method)
	}
	return nil
}

type acceptanceHetznerAPI struct {
	t          *testing.T
	server     *httptest.Server
	servers    map[string]hetzner.Server
	created    []string
	nextID     int64
	nextAction int64
}

func newAcceptanceHetznerAPI(t *testing.T) *acceptanceHetznerAPI {
	t.Helper()
	api := &acceptanceHetznerAPI{
		t:          t,
		servers:    map[string]hetzner.Server{},
		nextID:     100,
		nextAction: 500,
	}
	api.server = httptest.NewServer(http.HandlerFunc(api.handle))
	t.Cleanup(api.server.Close)
	return api
}

func (api *acceptanceHetznerAPI) createdNames() []string {
	names := append([]string(nil), api.created...)
	sort.Strings(names)
	return names
}

func (api *acceptanceHetznerAPI) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer acceptance-token" {
		api.t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/networks":
		_ = json.NewEncoder(w).Encode(map[string]any{"networks": []any{}})
	case r.Method == http.MethodPost && r.URL.Path == "/networks":
		_ = json.NewEncoder(w).Encode(map[string]any{"network": map[string]any{"id": 300, "name": "ship-sample-production-network"}})
	case r.Method == http.MethodGet && r.URL.Path == "/firewalls":
		_ = json.NewEncoder(w).Encode(map[string]any{"firewalls": []any{}})
	case r.Method == http.MethodPost && r.URL.Path == "/firewalls":
		_ = json.NewEncoder(w).Encode(map[string]any{"firewall": map[string]any{"id": 400, "name": "ship-sample-production-firewall"}})
	case r.Method == http.MethodGet && r.URL.Path == "/servers":
		servers := make([]hetzner.Server, 0, len(api.servers))
		for _, server := range api.servers {
			servers = append(servers, server)
		}
		sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
		_ = json.NewEncoder(w).Encode(map[string]any{
			"servers": servers,
			"meta": map[string]any{
				"pagination": map[string]any{"next_page": nil},
			},
		})
	case r.Method == http.MethodPost && r.URL.Path == "/servers":
		var req struct {
			Name      string             `json:"name"`
			Labels    map[string]string  `json:"labels"`
			Networks  []int64            `json:"networks"`
			Firewalls []map[string]int64 `json:"firewalls"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			api.t.Fatal(err)
		}
		privateNet := make([]hetzner.PrivateNet, 0, len(req.Networks))
		for _, networkID := range req.Networks {
			privateNet = append(privateNet, hetzner.PrivateNet{Network: networkID})
		}
		firewalls := make([]hetzner.ServerFirewall, 0, len(req.Firewalls))
		for _, firewall := range req.Firewalls {
			if id := firewall["firewall"]; id != 0 {
				firewalls = append(firewalls, hetzner.ServerFirewall{ID: id, Status: "applied"})
			}
		}
		api.nextID++
		api.nextAction++
		server := hetzner.Server{
			ID:         api.nextID,
			Name:       req.Name,
			Labels:     req.Labels,
			PrivateNet: privateNet,
			PublicNet: hetzner.PublicNet{
				IPv4:      hetzner.PublicIPv4{IP: "192.0.2." + strconv.FormatInt(api.nextID-100, 10)},
				Firewalls: firewalls,
			},
		}
		api.servers[req.Name] = server
		api.created = append(api.created, req.Name)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"server": server,
			"action": map[string]any{"id": api.nextAction, "status": "running"},
		})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/actions/"):
		_ = json.NewEncoder(w).Encode(map[string]any{
			"action": map[string]any{"id": api.nextAction, "status": "success"},
		})
	default:
		api.t.Fatalf("%s %s", r.Method, r.URL.Path)
	}
}
