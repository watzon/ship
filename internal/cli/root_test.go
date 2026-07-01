package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	"github.com/watzon/ship/internal/hetzner"
	providerpkg "github.com/watzon/ship/internal/provider"
	"github.com/watzon/ship/internal/scheduler"
	secretspkg "github.com/watzon/ship/internal/secrets"
	"github.com/watzon/ship/internal/state"
)

type recordingBootstrapSSH struct {
	host   scheduler.Host
	events *[]string
}

func (r recordingBootstrapSSH) Run(_ context.Context, command string) (string, error) {
	*r.events = append(*r.events, "bootstrap:"+r.host.Name+":run:"+firstCommandLine(command))
	return "ok", nil
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
	originalSSH := newBootstrapSSH
	originalAttempts := bootstrapMaxAttempts
	originalDelay := bootstrapRetryDelay
	readCurrentShipBinary = func() ([]byte, error) {
		return []byte("ship-test-binary"), nil
	}
	newBootstrapSSH = func(host scheduler.Host, dryRun bool) bootstrapSSH {
		return recordingBootstrapSSH{host: host, events: events}
	}
	bootstrapMaxAttempts = 2
	bootstrapRetryDelay = 0
	t.Cleanup(func() {
		readCurrentShipBinary = originalBinary
		newBootstrapSSH = originalSSH
		bootstrapMaxAttempts = originalAttempts
		bootstrapRetryDelay = originalDelay
	})
}

type recordingProvider struct {
	name      string
	plans     []providerpkg.HostPlan
	reconcile providerpkg.ReconcileResult
	listed    []providerpkg.Host
	deleted   []string
}

func (p *recordingProvider) Name() string {
	if p.name != "" {
		return p.name
	}
	return "recording"
}

func (p *recordingProvider) PlanHosts(string, string, config.Environment) ([]providerpkg.HostPlan, error) {
	return append([]providerpkg.HostPlan(nil), p.plans...), nil
}

func (p *recordingProvider) Reconcile(context.Context, string, string, config.Environment) (providerpkg.ReconcileResult, error) {
	return p.reconcile, nil
}

func (p *recordingProvider) List(context.Context, string, string) ([]providerpkg.Host, error) {
	return append([]providerpkg.Host(nil), p.listed...), nil
}

func (p *recordingProvider) Delete(_ context.Context, host providerpkg.Host) error {
	p.deleted = append(p.deleted, host.ID)
	return nil
}

func (p *recordingProvider) CredentialChecks(func(string) (string, bool)) []providerpkg.CredentialCheck {
	return nil
}

func installProviderHook(t *testing.T, provider providerpkg.Provider) {
	t.Helper()
	original := newEnvironmentProvider
	newEnvironmentProvider = func(config.Environment, bool) (providerpkg.Provider, error) {
		return provider, nil
	}
	t.Cleanup(func() {
		newEnvironmentProvider = original
	})
}

func TestProvisionApplyRequiresYes(t *testing.T) {
	path := writeConfig(t)
	cmd := provisionCmd(&options{configPath: path})
	cmd.SetArgs([]string{"apply", "production"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected confirmation error")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error = %v", err)
	}
}

func TestProvisionApplyDryRunDoesNotRequireYesOrToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(config.Sample()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("HCLOUD_TOKEN", "")

	var out bytes.Buffer
	cmd := provisionCmd(&options{configPath: path, dryRun: true})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"apply", "production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "would provision web-1 pool=web") {
		t.Fatalf("output = %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, config.LocalStateDir, "environments", "production", "hosts.json")); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote hosts.json: %v", err)
	}
}

func TestProvisionApplyDryRunUsesConfiguredProvider(t *testing.T) {
	path := writeConfig(t)
	prov := &recordingProvider{plans: []providerpkg.HostPlan{
		{Name: "fake-1", Pool: "fake"},
	}}
	installProviderHook(t, prov)

	var out bytes.Buffer
	cmd := provisionCmd(&options{configPath: path, dryRun: true})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"apply", "production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "would provision fake-1 pool=fake") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestProvisionApplyWithConfigWritesHostFactsNextToConfig(t *testing.T) {
	projectDir := t.TempDir()
	cwd := t.TempDir()
	path := filepath.Join(projectDir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	var bootstrapEvents []string
	installBootstrapHooks(t, &bootstrapEvents)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/servers":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"servers": []any{},
				"meta": map[string]any{
					"pagination": map[string]any{"next_page": nil},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/servers":
			var req struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"server": map[string]any{
					"id":     42,
					"name":   req.Name,
					"labels": req.Labels,
					"public_net": map[string]any{
						"ipv4": map[string]string{"ip": "203.0.113.10"},
					},
				},
				"action": map[string]any{"id": 99, "status": "running"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/actions/99":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"action": map[string]any{"id": 99, "status": "success"},
			})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)

	originalNewEnvironmentProvider := newEnvironmentProvider
	newEnvironmentProvider = func(_ config.Environment, dryRun bool) (providerpkg.Provider, error) {
		return hetzner.Client{
			Token:        "test-token",
			DryRun:       dryRun,
			HTTP:         api.Client(),
			BaseURL:      api.URL,
			PollInterval: time.Nanosecond,
		}, nil
	}
	t.Cleanup(func() {
		newEnvironmentProvider = originalNewEnvironmentProvider
	})

	var out bytes.Buffer
	cmd := provisionCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"apply", "production", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "bootstrapped web-1") {
		t.Fatalf("bootstrap output missing: %q", out.String())
	}
	wantEvents := []string{
		"bootstrap:web-1:run:true",
		"bootstrap:web-1:run:set -eu",
		"bootstrap:web-1:upload:set -eu:16",
		"bootstrap:web-1:run:set -eu",
	}
	if strings.Join(bootstrapEvents, "\n") != strings.Join(wantEvents, "\n") {
		t.Fatalf("bootstrap events = %#v, want %#v", bootstrapEvents, wantEvents)
	}

	wantPath := filepath.Join(projectDir, config.LocalStateDir, "environments", "production", "hosts.json")
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read host facts from config dir: %v", err)
	}
	var hosts []state.HostFact
	if err := json.Unmarshal(data, &hosts); err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 {
		t.Fatalf("hosts = %+v", hosts)
	}
	if hosts[0].Name != "web-1" || hosts[0].IPv4 != "203.0.113.10" || hosts[0].ServerID != 42 {
		t.Fatalf("host fact = %+v", hosts[0])
	}
	cwdPath := filepath.Join(cwd, config.LocalStateDir, "environments", "production", "hosts.json")
	if _, err := os.Stat(cwdPath); !os.IsNotExist(err) {
		t.Fatalf("host facts written in cwd: %v", err)
	}
}

func TestProvisionApplyWritesGenericProviderHostFacts(t *testing.T) {
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	var bootstrapEvents []string
	installBootstrapHooks(t, &bootstrapEvents)
	prov := &recordingProvider{
		name: "fake",
		reconcile: providerpkg.ReconcileResult{
			Desired: []providerpkg.HostPlan{{Name: "web-1", Pool: "web", User: "root"}},
			Created: []providerpkg.Host{{
				ID:            "instance-abc",
				Name:          "web-1",
				Pool:          "web",
				PublicAddress: "host.example.test",
			}},
		},
	}
	installProviderHook(t, prov)

	var out bytes.Buffer
	cmd := provisionCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"apply", "production", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "created web-1 pool=web provider_id=instance-abc public_address=host.example.test") {
		t.Fatalf("output = %q", out.String())
	}
	store := state.NewStore(filepath.Join(projectDir, config.LocalStateDir))
	facts, err := store.ReadHostFacts("production")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("facts = %+v", facts)
	}
	if facts[0].Provider != "fake" || facts[0].ProviderID != "instance-abc" || facts[0].ServerID != 0 || facts[0].IPv4 != "" || facts[0].PublicAddress != "host.example.test" {
		t.Fatalf("host fact = %+v", facts[0])
	}
}

func TestProvisionDecommissionRequiresYes(t *testing.T) {
	path := writeConfig(t)
	cmd := provisionCmd(&options{configPath: path})
	cmd.SetArgs([]string{"decommission", "production"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected confirmation error")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error = %v", err)
	}
}

func TestProvisionDecommissionUsesConfiguredProvider(t *testing.T) {
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(filepath.Join(projectDir, config.LocalStateDir))
	if err := store.SaveHostFacts("production", []state.HostFact{{Name: "web-1", Pool: "web", User: "root"}}); err != nil {
		t.Fatal(err)
	}
	prov := &recordingProvider{listed: []providerpkg.Host{{
		ID:   "instance-abc",
		Name: "web-1",
		Pool: "web",
	}}}
	installProviderHook(t, prov)

	var out bytes.Buffer
	cmd := provisionCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"decommission", "production", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(prov.deleted, ","); got != "instance-abc" {
		t.Fatalf("deleted = %q", got)
	}
	if !strings.Contains(out.String(), "decommissioned web-1 provider_id=instance-abc pool=web") {
		t.Fatalf("output = %q", out.String())
	}
	if _, err := store.ReadHostFacts("production"); !os.IsNotExist(err) {
		t.Fatalf("host facts should be removed, err=%v", err)
	}
}

func TestProvisionDecommissionDeletesServersAndClearsHostFacts(t *testing.T) {
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(filepath.Join(projectDir, config.LocalStateDir))
	if err := store.SaveHostFacts("production", []state.HostFact{{
		Name:          "web-1",
		Pool:          "web",
		User:          "root",
		IPv4:          "203.0.113.10",
		PublicAddress: "203.0.113.10",
		ServerID:      42,
	}}); err != nil {
		t.Fatal(err)
	}

	var deleted []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/servers":
			if got := r.URL.Query().Get("label_selector"); got != "managed-by=ship,project=demo,environment=production" {
				t.Fatalf("label selector = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"servers": []map[string]any{{
					"id":     42,
					"name":   "web-1",
					"labels": map[string]string{"managed-by": "ship", "project": "example", "environment": "production", "pool": "web"},
				}},
				"meta": map[string]any{"pagination": map[string]any{"next_page": nil}},
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/servers/42":
			deleted = append(deleted, "42")
			_ = json.NewEncoder(w).Encode(map[string]any{"action": map[string]any{"id": 99, "status": "running"}})
		case r.Method == http.MethodGet && r.URL.Path == "/actions/99":
			_ = json.NewEncoder(w).Encode(map[string]any{"action": map[string]any{"id": 99, "status": "success"}})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)

	originalNewEnvironmentProvider := newEnvironmentProvider
	newEnvironmentProvider = func(_ config.Environment, dryRun bool) (providerpkg.Provider, error) {
		return hetzner.Client{
			Token:        "test-token",
			DryRun:       dryRun,
			HTTP:         api.Client(),
			BaseURL:      api.URL,
			PollInterval: time.Nanosecond,
		}, nil
	}
	t.Cleanup(func() {
		newEnvironmentProvider = originalNewEnvironmentProvider
	})

	var out bytes.Buffer
	cmd := provisionCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"decommission", "production", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(deleted, ","); got != "42" {
		t.Fatalf("deleted = %q", got)
	}
	if !strings.Contains(out.String(), "decommissioned web-1 server_id=42 pool=web") {
		t.Fatalf("output = %q", out.String())
	}
	if _, err := store.ReadHostFacts("production"); !os.IsNotExist(err) {
		t.Fatalf("host facts should be removed, err=%v", err)
	}
}

func TestAgentInstallDryRunUsesSavedHostFactContact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveHostFacts("production", []state.HostFact{{
		Name:          "web-1",
		Pool:          "web",
		User:          "root",
		IPv4:          "203.0.113.10",
		PublicAddress: "198.51.100.10",
	}}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := agentCmd(&options{configPath: path, dryRun: true})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"install", "production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "web-1 ssh root@198.51.100.10 ") {
		t.Fatalf("dry-run output did not use saved public address:\n%s", got)
	}
	if strings.Contains(got, "root@web-1 ") {
		t.Fatalf("dry-run output used synthesized host name:\n%s", got)
	}
}

func TestDeployBuildsPushesResolvesBeforeAgentMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	workerDigestRef := "registry.local/acme/worker@sha256:" + strings.Repeat("3", 64)
	fakeDocker := &recordingDeployDocker{
		events: &events,
		resolved: map[string]string{
			tag:                                 digestRef,
			"registry.local/acme/worker:stable": workerDigestRef,
		},
	}
	installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})

	var out bytes.Buffer
	cmd := deployCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"build:" + tag,
		"push:" + tag,
		"resolve:" + tag,
		"resolve:registry.local/acme/worker:stable",
		"agent:web-1:write_release_state:" + releaseID + ":pending",
		"agent:web-2:write_release_state:" + releaseID + ":pending",
		"agent:web-1:list_ship_containers",
		"agent:web-2:list_ship_containers",
		"agent:web-1:pull:" + digestRef,
		"agent:web-1:run:" + digestRef,
		"agent:web-2:pull:" + digestRef,
		"agent:web-2:run:" + digestRef,
		"agent:web-1:write_release_state:" + releaseID + ":healthy",
		"agent:web-2:write_release_state:" + releaseID + ":healthy",
	}
	if strings.Join(events, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	if len(fakeDocker.builds) != 1 {
		t.Fatalf("builds = %#v", fakeDocker.builds)
	}
	build := fakeDocker.builds[0]
	if build.BuildArgs["RAILS_ENV"] != "production" || build.Target != "runtime" || build.Platform != "linux/amd64" {
		t.Fatalf("build options = %+v", build)
	}
}

func TestDeployWithSavedHostFactsPassesContactsAndPreservesLogicalEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveHostFacts("production", []state.HostFact{
		{Name: "web-1", Pool: "web", User: "root", IPv4: "203.0.113.10", PublicAddress: "198.51.100.10"},
		{Name: "web-2", Pool: "web", User: "root", IPv4: "203.0.113.20"},
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	seenContacts := map[string]string{}
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	workerDigestRef := "registry.local/acme/worker@sha256:" + strings.Repeat("3", 64)
	fakeDocker := &recordingDeployDocker{
		events: &events,
		resolved: map[string]string{
			tag:                                 digestRef,
			"registry.local/acme/worker:stable": workerDigestRef,
		},
	}
	installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent {
		seenContacts[host.Name] = host.Contact
		return recordingDeployAgent{host: host.Name, events: &events}
	})

	var out bytes.Buffer
	cmd := deployCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	if seenContacts["web-1"] != "198.51.100.10" || seenContacts["web-2"] != "203.0.113.20" {
		t.Fatalf("seen contacts = %+v", seenContacts)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "agent:web-1:run:"+digestRef) || !strings.Contains(joined, "agent:web-2:run:"+digestRef) {
		t.Fatalf("logical host events missing:\n%s", joined)
	}
	if strings.Contains(joined, "198.51.100.10") || strings.Contains(joined, "203.0.113.20") {
		t.Fatalf("contact addresses leaked into logical events:\n%s", joined)
	}
}

func TestDeployStopsFixedPortOldBeforeStartAndPromotesHealthyRelease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	oldRelease := state.Release{
		ID:          "old-release",
		Environment: "production",
		Images:      map[string]string{"web": "registry.local/acme/web@sha256:" + strings.Repeat("0", 64)},
		CreatedAt:   time.Unix(10, 0),
	}
	if err := store.SaveRelease(oldRelease); err != nil {
		t.Fatal(err)
	}
	var events []string
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	fakeDocker := &recordingDeployDocker{
		events:   &events,
		resolved: map[string]string{tag: digestRef},
	}
	agents := map[string]*scriptedDeployAgent{}
	installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent {
		a := agents[host.Name]
		if a == nil {
			a = &scriptedDeployAgent{
				host:   host.Name,
				events: &events,
				observed: []docker.ContainerSummary{{
					Names: "ship_demo_production_web_1_old-release",
					Labels: map[string]string{
						docker.LabelManagedBy:   docker.LabelManagedByValue,
						docker.LabelProject:     "demo",
						docker.LabelEnvironment: "production",
						docker.LabelService:     "web",
						docker.LabelReplica:     "1",
						docker.LabelRelease:     "old-release",
					},
				}},
			}
			agents[host.Name] = a
		}
		return a
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	wantOrder := []string{
		"agent:web-1:list_ship_containers",
		"agent:web-1:pull:" + digestRef,
		"agent:web-1:stop:ship_demo_production_web_1_old-release",
		"agent:web-1:run:ship_demo_production_web_1_" + releaseID,
		"agent:web-1:health",
	}
	joined := strings.Join(events, "\n")
	for _, want := range wantOrder {
		if !strings.Contains(joined, want) {
			t.Fatalf("events missing %q:\n%s", want, joined)
		}
	}
	pullAt := strings.Index(joined, "agent:web-1:pull:"+digestRef)
	stopAt := strings.Index(joined, "agent:web-1:stop:ship_demo_production_web_1_old-release")
	runAt := strings.Index(joined, "agent:web-1:run:ship_demo_production_web_1_"+releaseID)
	healthAt := strings.Index(joined, "agent:web-1:health")
	if !(pullAt < stopAt && stopAt < runAt && runAt < healthAt) {
		t.Fatalf("fixed-port deploy order is wrong:\n%s", joined)
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != releaseID || !current.Healthy || current.Status != state.ReleaseStatusHealthy {
		t.Fatalf("current release = %+v", current)
	}
	if _, err := os.Stat(filepath.Join(dir, config.LocalStateDir, "ingress", "production.Caddyfile")); err != nil {
		t.Fatalf("ingress file missing: %v", err)
	}
}

func TestDeployFailedPullMarksReleaseFailedAndKeepsCurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	fakeDocker := &recordingDeployDocker{
		events:   &events,
		resolved: map[string]string{tag: digestRef},
	}
	installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent {
		return &scriptedDeployAgent{host: host.Name, events: &events, failMethod: "pull"}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected deploy failure")
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "old-release" {
		t.Fatalf("current = %+v", current)
	}
	failed, err := store.ReadRelease(releaseID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != state.ReleaseStatusFailed || failed.Healthy {
		t.Fatalf("failed release = %+v", failed)
	}
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "deploy_rollout", "failed", releaseID) || !timelineContains(timeline, "deploy_mark_failed", "succeeded", releaseID) {
		t.Fatalf("timeline = %+v", timeline)
	}
}

func TestDeployFailedHealthDoesNotShiftTrafficOrCurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	fakeDocker := &recordingDeployDocker{
		events:   &events,
		resolved: map[string]string{tag: digestRef},
	}
	installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent {
		return &scriptedDeployAgent{
			host:   host.Name,
			events: &events,
			observed: []docker.ContainerSummary{{
				Names: "ship_demo_production_web_1_old-release",
				Labels: map[string]string{
					docker.LabelManagedBy:   docker.LabelManagedByValue,
					docker.LabelProject:     "demo",
					docker.LabelEnvironment: "production",
					docker.LabelService:     "web",
					docker.LabelReplica:     "1",
					docker.LabelRelease:     "old-release",
				},
			}},
			failMethod: "health_check",
		}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected deploy failure")
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "old-release" {
		t.Fatalf("current = %+v", current)
	}
	if _, err := os.Stat(filepath.Join(dir, config.LocalStateDir, "ingress", "production.Caddyfile")); !os.IsNotExist(err) {
		t.Fatalf("health failure wrote ingress: %v", err)
	}
	joined := strings.Join(events, "\n")
	stopAt := strings.Index(joined, "agent:web-1:stop:ship_demo_production_web_1_old-release")
	runAt := strings.Index(joined, "agent:web-1:run:ship_demo_production_web_1_"+releaseID)
	if stopAt < 0 || runAt < 0 || stopAt > runAt {
		t.Fatalf("fixed-port health failure did not stop old before start:\n%s", joined)
	}
}

func TestDeployPartialHostFailureLeavesPreviousCurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	workerDigestRef := "registry.local/acme/worker@sha256:" + strings.Repeat("3", 64)
	fakeDocker := &recordingDeployDocker{
		events: &events,
		resolved: map[string]string{
			tag:                                 digestRef,
			"registry.local/acme/worker:stable": workerDigestRef,
		},
	}
	installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent {
		failMethod := ""
		if host.Name == "web-2" {
			failMethod = "run_container"
		}
		return &scriptedDeployAgent{host: host.Name, events: &events, failMethod: failMethod}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected deploy failure")
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "agent:web-1:run:ship_demo_production_web_1_"+releaseID) || !strings.Contains(joined, "agent:web-2:run:ship_demo_production_web_2_"+releaseID) {
		t.Fatalf("partial host events = %#v", events)
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "old-release" {
		t.Fatalf("current = %+v", current)
	}
	failed, err := store.ReadRelease(releaseID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != state.ReleaseStatusFailed {
		t.Fatalf("failed release = %+v", failed)
	}
}

func TestRollbackAppliesTargetRelease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old-image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{ID: "current-release", Environment: "production", Images: map[string]string{"web": "current-image"}, CreatedAt: time.Unix(20, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var releaseWrites []releaseStateWrite
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &scriptedDeployAgent{host: host.Name, events: &events, releaseWrites: &releaseWrites}
	})

	cmd := rollbackCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "old-release" {
		t.Fatalf("current = %+v", current)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "agent:web-1:pull:old-image") || !strings.Contains(joined, "agent:web-1:health") {
		t.Fatalf("rollback events = %#v", events)
	}
	if len(releaseWrites) != 1 || releaseWrites[0].Host != "web-1" || releaseWrites[0].Release.ID != "old-release" || releaseWrites[0].Release.Status != state.ReleaseStatusHealthy {
		t.Fatalf("rollback release writes = %+v", releaseWrites)
	}
}

func TestRollbackRefusesAlreadyCurrentTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old-image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{ID: "current-release", Environment: "production", Images: map[string]string{"web": "current-image"}, CreatedAt: time.Unix(20, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &scriptedDeployAgent{host: host.Name, events: &events}
	})

	cmd := rollbackCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production", "--to", "current-release"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "already current") {
		t.Fatalf("expected already-current error, got %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("no-op rollback touched agents: %#v", events)
	}
}

func TestRollbackRemoteReleaseStateWriteReportsPartialFailures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old-image", "worker": "worker-image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{ID: "current-release", Environment: "production", Images: map[string]string{"web": "current-image", "worker": "worker-image"}, CreatedAt: time.Unix(20, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		failMethod := ""
		if host.Name == "web-2" {
			failMethod = "write_release_state"
		}
		return &scriptedDeployAgent{host: host.Name, events: &events, failMethod: failMethod}
	})

	cmd := rollbackCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "failed on 1/2 hosts") || !strings.Contains(err.Error(), "web-2") {
		t.Fatalf("expected partial release-state write error, got %v", err)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "agent:web-1:write_release_state:old-release:healthy") ||
		!strings.Contains(joined, "agent:web-2:write_release_state:old-release:healthy") {
		t.Fatalf("release state was not attempted on every host:\n%s", joined)
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "current-release" {
		t.Fatalf("current release changed after partial remote write failure: %+v", current)
	}
}

func TestRollbackWritesRemoteSecretFileAndUsesEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(secretDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	secretValue := "postgres://user:pass@example/db"
	t.Setenv("SHIP_TEST_DATABASE_URL", secretValue)

	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{
		ID:            "old-release",
		Environment:   "production",
		Images:        map[string]string{"web": "old-image"},
		SecretDigests: map[string]string{"SHIP_TEST_DATABASE_URL": secretspkg.Digest(secretValue)},
		CreatedAt:     time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{
		ID:            "current-release",
		Environment:   "production",
		Images:        map[string]string{"web": "current-image"},
		SecretDigests: map[string]string{"SHIP_TEST_DATABASE_URL": secretspkg.Digest(secretValue)},
		CreatedAt:     time.Unix(20, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var writes []agent.WriteFileParams
	var runs []agent.RunContainerParams
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &secretDeployAgent{host: host.Name, events: &events, writes: &writes, runs: &runs}
	})

	cmd := rollbackCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	secretPath := "/var/lib/ship/secrets/production/service-web.env"
	wantEvents := []string{
		"agent:web-1:write_file:" + secretPath + ":0600",
		"agent:web-1:list_ship_containers",
		"agent:web-1:pull:old-image",
		"agent:web-1:run:ship_demo_production_web_1_old-release",
		"agent:web-1:write_release_state:old-release:healthy",
	}
	if strings.Join(events, "\n") != strings.Join(wantEvents, "\n") {
		t.Fatalf("events = %#v, want %#v", events, wantEvents)
	}
	if len(writes) != 1 || writes[0].Path != secretPath || writes[0].Mode != 0o600 {
		t.Fatalf("writes = %#v", writes)
	}
	if writes[0].Content != "SHIP_TEST_DATABASE_URL="+secretValue+"\n" {
		t.Fatalf("secret file content = %q", writes[0].Content)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %#v", runs)
	}
	joinedArgs := strings.Join(runs[0].Args, " ")
	if !strings.Contains(joinedArgs, "--env-file "+secretPath) {
		t.Fatalf("run args %q missing env file %q", joinedArgs, secretPath)
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "old-release" {
		t.Fatalf("current = %+v", current)
	}
}

func TestRollbackMissingSecretsStopBeforeAgentMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(secretDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	unsetEnv(t, "SHIP_TEST_DATABASE_URL")

	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{
		ID:          "old-release",
		Environment: "production",
		Images:      map[string]string{"web": "old-image"},
		CreatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{
		ID:          "current-release",
		Environment: "production",
		Images:      map[string]string{"web": "current-image"},
		CreatedAt:   time.Unix(20, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &secretDeployAgent{host: host.Name, events: &events}
	})

	cmd := rollbackCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "missing secrets: SHIP_TEST_DATABASE_URL") {
		t.Fatalf("expected missing secret error, got %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("missing secret rollback touched agent: %#v", events)
	}
}

func TestRollbackFailurePreservesCurrentAndRecordsAttempt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old-image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{ID: "current-release", Environment: "production", Images: map[string]string{"web": "current-image"}, CreatedAt: time.Unix(20, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var releaseWrites []releaseStateWrite
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &scriptedDeployAgent{host: host.Name, events: &events, failMethod: "health_check", releaseWrites: &releaseWrites}
	})

	cmd := rollbackCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected rollback failure")
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "current-release" {
		t.Fatalf("current = %+v", current)
	}
	target, err := store.ReadRelease("old-release")
	if err != nil {
		t.Fatal(err)
	}
	if target.Status != state.ReleaseStatusHealthy || !target.Healthy {
		t.Fatalf("target should remain healthy, got %+v", target)
	}
	if len(releaseWrites) != 0 {
		t.Fatalf("failed rollback wrote remote release state: %+v", releaseWrites)
	}
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "rollback_rollout", "failed", "old-release") || !timelineContains(timeline, "rollback_attempt", "failed", "old-release") || !timelineContains(timeline, "rollback", "failed", "old-release") {
		t.Fatalf("timeline = %+v", timeline)
	}
}

func TestRollbackBlocksUnsafeFixedPortReplacementBeforeMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old-image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{ID: "current-release", Environment: "production", Images: map[string]string{"web": "current-image"}, CreatedAt: time.Unix(20, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var releaseWrites []releaseStateWrite
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &scriptedDeployAgent{
			host:          host.Name,
			events:        &events,
			observed:      []docker.ContainerSummary{serviceContainer(host.Name, "web", 1, "current-release", "Up 2 minutes")},
			failMethod:    "health_check",
			releaseWrites: &releaseWrites,
		}
	})

	cmd := rollbackCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected unsafe fixed-port rollback failure")
	}
	for _, needle := range []string{"unsafe fixed-port rollback", "ship_demo_production_web_1_current-release", "before ship_demo_production_web_1_old-release is healthy"} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("error missing %q: %v", needle, err)
		}
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "current-release" {
		t.Fatalf("current = %+v", current)
	}
	if len(releaseWrites) != 0 {
		t.Fatalf("blocked rollback wrote remote release state: %+v", releaseWrites)
	}
	joined := strings.Join(events, "\n")
	if joined != "agent:web-1:list_ship_containers" {
		t.Fatalf("blocked rollback touched agent:\n%s", joined)
	}
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "rollback", "blocked", "old-release") {
		t.Fatalf("timeline = %+v", timeline)
	}
}

func TestRollbackBlockersDetectStatefulAccessories(t *testing.T) {
	cfg := &config.Config{
		Accessories: map[string]config.Accessory{
			"cache":    {Image: "redis:7", Pool: "data"},
			"postgres": {Image: "postgres:17", Pool: "data", Primary: true, Backup: config.BackupSpec{Required: true}},
			"search":   {Image: "opensearch:2", Pool: "data", Backup: config.BackupSpec{Required: true}},
		},
	}
	blockers := rollbackBlockers(cfg)
	if len(blockers) != 2 {
		t.Fatalf("blockers = %+v", blockers)
	}
	message := rollbackBlockerError(blockers)
	for _, needle := range []string{"accessory postgres", "primary backup-required", "accessory search", "--allow-data-rollback"} {
		if !strings.Contains(message, needle) {
			t.Fatalf("message missing %q: %s", needle, message)
		}
	}
	stateless := &config.Config{Accessories: map[string]config.Accessory{
		"cache": {Image: "redis:7", Pool: "data"},
	}}
	if got := rollbackBlockers(stateless); len(got) != 0 {
		t.Fatalf("stateless accessory blockers = %+v", got)
	}
}

func TestRollbackRefusesAccessoryRiskWithoutConfirmation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old-image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{ID: "current-release", Environment: "production", Images: map[string]string{"web": "current-image"}, CreatedAt: time.Unix(20, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &scriptedDeployAgent{host: host.Name, events: &events}
	})

	cmd := rollbackCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--allow-data-rollback") {
		t.Fatalf("expected accessory risk error, got %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("blocked rollback touched agent: %#v", events)
	}
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "rollback", "blocked", "old-release") {
		t.Fatalf("timeline = %+v", timeline)
	}
}

func TestDeployImageFailuresStopBeforeAgentMutation(t *testing.T) {
	for _, stage := range []string{"build", "push", "resolve"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, config.DefaultConfigFile)
			if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)

			var events []string
			fakeDocker := &recordingDeployDocker{events: &events, failStage: stage}
			installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent {
				return recordingDeployAgent{host: host.Name, events: &events}
			})

			cmd := deployCmd(&options{configPath: path})
			cmd.SetArgs([]string{"production"})
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected deploy failure")
			}
			for _, event := range events {
				if strings.HasPrefix(event, "agent:") {
					t.Fatalf("agent mutated after %s failure: %#v", stage, events)
				}
			}
		})
	}
}

func TestDeployDryRunDoesNotBuildResolveOrCallAgents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	installDeployHooks(t, panicDeployDocker{t: t}, func(host scheduler.Host) deployAgent {
		t.Fatalf("unexpected agent client for %s", host.Name)
		return nil
	})

	var out bytes.Buffer
	cmd := deployCmd(&options{configPath: path, dryRun: true})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "resolve web: pushed image -> immutable digest") {
		t.Fatalf("dry-run output missing immutable resolve intent:\n%s", out.String())
	}
}

func TestDeployDryRunShowsIngressReloadTargetsWithoutAgents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	installDeployHooks(t, panicDeployDocker{t: t}, func(host scheduler.Host) deployAgent {
		t.Fatalf("unexpected agent client for %s", host.Name)
		return nil
	})

	var out bytes.Buffer
	cmd := deployCmd(&options{configPath: path, dryRun: true})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, needle := range []string{
		"ingress web: example.com",
		".ship/ingress/production.Caddyfile",
		"reload caddy on web-1 after validation",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("dry-run output missing %q:\n%s", needle, text)
		}
	}
}

func TestDeployDryRunValidatesMissingSecretsWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(secretDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	unsetEnv(t, "SHIP_TEST_DATABASE_URL")

	installDeployHooks(t, panicDeployDocker{t: t}, func(host scheduler.Host) deployAgent {
		t.Fatalf("unexpected agent client for %s", host.Name)
		return nil
	})

	var out bytes.Buffer
	cmd := deployCmd(&options{configPath: path, dryRun: true})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "missing secrets: SHIP_TEST_DATABASE_URL") {
		t.Fatalf("expected missing secret error, got %v", err)
	}
	if strings.Contains(out.String(), "postgres://") {
		t.Fatalf("dry-run leaked secret value:\n%s", out.String())
	}
}

func TestDeployMissingSecretsStopBeforeBuildResolveOrAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(secretDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	unsetEnv(t, "SHIP_TEST_DATABASE_URL")

	var events []string
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "missing secrets: SHIP_TEST_DATABASE_URL") {
		t.Fatalf("expected missing secret error, got %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("missing secret deploy touched docker or agent: %#v", events)
	}
}

func TestDeployWritesRemoteSecretFileAndStoresOnlyDigests(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(secretDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	secretValue := "postgres://user:pass@example/db"
	t.Setenv("SHIP_TEST_DATABASE_URL", secretValue)

	var events []string
	var writes []agent.WriteFileParams
	var runs []agent.RunContainerParams
	imageRef := "registry.local/acme/web:stable"
	digestRef := "registry.local/acme/web@sha256:" + strings.Repeat("4", 64)
	installDeployHooks(t, &recordingDeployDocker{events: &events, resolved: map[string]string{imageRef: digestRef}}, func(host scheduler.Host) deployAgent {
		return &secretDeployAgent{host: host.Name, events: &events, writes: &writes, runs: &runs}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	secretPath := "/var/lib/ship/secrets/production/service-web.env"
	wantEvents := []string{
		"resolve:" + imageRef,
		"agent:web-1:write_release_state:abc123def456-20260630T183456.123456789Z:pending",
		"agent:web-1:write_file:" + secretPath + ":0600",
		"agent:web-1:list_ship_containers",
		"agent:web-1:pull:" + digestRef,
		"agent:web-1:run:ship_demo_production_web_1_abc123def456-20260630T183456.123456789Z",
		"agent:web-1:write_release_state:abc123def456-20260630T183456.123456789Z:healthy",
	}
	if strings.Join(events, "\n") != strings.Join(wantEvents, "\n") {
		t.Fatalf("events = %#v, want %#v", events, wantEvents)
	}
	if len(writes) != 1 {
		t.Fatalf("writes = %#v", writes)
	}
	if writes[0].Path != secretPath || writes[0].Mode != 0o600 {
		t.Fatalf("write params = %+v", writes[0])
	}
	if writes[0].Content != "SHIP_TEST_DATABASE_URL="+secretValue+"\n" {
		t.Fatalf("secret file content = %q", writes[0].Content)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %#v", runs)
	}
	joinedArgs := strings.Join(runs[0].Args, " ")
	for _, needle := range []string{"-e RACK_ENV=production", "--env-file " + secretPath} {
		if !strings.Contains(joinedArgs, needle) {
			t.Fatalf("run args %q missing %q", joinedArgs, needle)
		}
	}
	releaseID := "abc123def456-20260630T183456.123456789Z"
	data, err := os.ReadFile(filepath.Join(dir, config.LocalStateDir, "releases", releaseID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secretValue) {
		t.Fatalf("release metadata leaked secret value: %s", data)
	}
	var release state.Release
	if err := json.Unmarshal(data, &release); err != nil {
		t.Fatal(err)
	}
	if release.SecretDigests["SHIP_TEST_DATABASE_URL"] != secretspkg.Digest(secretValue) {
		t.Fatalf("secret digests = %+v", release.SecretDigests)
	}
}

func TestDeployWritesRemoteReleaseStateToEveryHostWithoutSecretValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(secretMultiHostDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	secretValue := "postgres://user:pass@example/db"
	t.Setenv("SHIP_TEST_DATABASE_URL", secretValue)

	var events []string
	var releaseWrites []releaseStateWrite
	imageRef := "registry.local/acme/web:stable"
	digestRef := "registry.local/acme/web@sha256:" + strings.Repeat("5", 64)
	installDeployHooks(t, &recordingDeployDocker{events: &events, resolved: map[string]string{imageRef: digestRef}}, func(host scheduler.Host) deployAgent {
		return &secretDeployAgent{host: host.Name, events: &events, releaseWrites: &releaseWrites}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	seen := map[string]map[string]bool{}
	for _, write := range releaseWrites {
		if seen[write.Host] == nil {
			seen[write.Host] = map[string]bool{}
		}
		seen[write.Host][write.Release.Status] = true
		data, err := json.Marshal(write.Release)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), secretValue) {
			t.Fatalf("remote release metadata leaked secret value: %s", data)
		}
		if write.Release.SecretDigests["SHIP_TEST_DATABASE_URL"] != secretspkg.Digest(secretValue) {
			t.Fatalf("secret digests = %+v", write.Release.SecretDigests)
		}
		if write.Release.Images["web"] != digestRef {
			t.Fatalf("images = %+v", write.Release.Images)
		}
	}
	for _, host := range []string{"web-1", "web-2"} {
		if !seen[host][state.ReleaseStatusPending] || !seen[host][state.ReleaseStatusHealthy] {
			t.Fatalf("release writes by host = %+v", seen)
		}
	}
}

func TestStatusReportsObservedDriftAndJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{
		ID:          "release-new",
		Environment: "production",
		Images:      map[string]string{"web": "new"},
		CreatedAt:   time.Unix(20, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"web-1": {
			serviceContainer("web-1", "web", 1, "release-new", "Up 10 seconds"),
			serviceContainer("web-1", "worker", 1, "old-worker", "Exited"),
		},
		"web-2": {
			serviceContainer("web-2", "web", 2, "release-old", "Up 2 minutes"),
		},
	}
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &observabilityAgent{host: host.Name, events: &events, observed: observed}
	})

	var out bytes.Buffer
	cmd := statusCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, needle := range []string{"web.2 desired host=web-2", "state=wrong_release", "release=release-old", "extra managed containers", "drift detected"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("status output missing %q:\n%s", needle, text)
		}
	}

	out.Reset()
	cmd = statusCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view statusView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.CurrentRelease == nil || view.CurrentRelease.ID != "release-new" {
		t.Fatalf("current release = %+v", view.CurrentRelease)
	}
	if !view.Summary.Drift || view.Summary.WrongRelease != 1 || view.Summary.Extra != 2 {
		t.Fatalf("summary = %+v", view.Summary)
	}
}

func TestStatusKeepsConfiguredAccessoryObservedWithoutExtraDrift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(serviceAccessoryStatusConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{
		ID:          "release-new",
		Environment: "production",
		Images:      map[string]string{"web": "new"},
		CreatedAt:   time.Unix(20, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"web-1": {
			serviceContainer("web-1", "web", 1, "release-new", "Up 10 seconds"),
			accessoryContainer("postgres", "web-1", "Up 5 seconds"),
		},
	}
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &observabilityAgent{host: host.Name, events: &events, observed: observed}
	})

	var out bytes.Buffer
	cmd := statusCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view statusView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Summary.Extra != 0 || view.Summary.Drift {
		t.Fatalf("summary = %+v", view.Summary)
	}
	if len(view.ExtraObserved) != 0 {
		t.Fatalf("extra observed = %+v", view.ExtraObserved)
	}
	foundAccessory := false
	for _, observed := range view.Observed {
		if observed.Accessory == "postgres" && observed.Kind == "accessory" {
			foundAccessory = true
		}
	}
	if !foundAccessory {
		t.Fatalf("observed did not include configured accessory: %+v", view.Observed)
	}
}

func TestInspectIncludesAccessoriesEventsAndJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "release-1", Environment: "production", Images: map[string]string{"web": "image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
		UpdatedAt:   time.Unix(20, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordEvent(state.Event{
		Time:        time.Unix(30, 0),
		Environment: "production",
		Kind:        "deploy",
		Status:      "succeeded",
		Release:     "release-1",
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"data-a": {accessoryContainer("postgres", "data-a", "Up 5 seconds")},
	}
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &observabilityAgent{host: host.Name, events: &events, observed: observed}
	})

	var out bytes.Buffer
	cmd := inspectCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view inspectView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.CurrentRelease == nil || view.CurrentRelease.ID != "release-1" {
		t.Fatalf("inspect current = %+v", view.CurrentRelease)
	}
	if len(view.Accessories) != 1 || view.Accessories[0].Name != "postgres" {
		t.Fatalf("inspect accessories = %+v", view.Accessories)
	}
	if len(view.Events) != 1 || view.Events[0].Kind != "deploy" {
		t.Fatalf("inspect events = %+v", view.Events)
	}
	if len(view.Observed) != 1 || view.Observed[0].Accessory != "postgres" {
		t.Fatalf("inspect observed = %+v", view.Observed)
	}
}

func TestEventsCommandJSONAndText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.RecordEvent(state.Event{
		Time:        time.Unix(10, 0),
		Environment: "production",
		Kind:        "scale",
		Status:      "planned",
		Service:     "web",
		Message:     "web=2",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := eventsCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "scale planned service=web web=2") {
		t.Fatalf("events output = %q", out.String())
	}

	out.Reset()
	cmd = eventsCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var events []state.Event
	if err := json.Unmarshal(out.Bytes(), &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "scale" {
		t.Fatalf("events json = %+v", events)
	}
}

func TestRecoverShowsFailedReleaseAndSuggestedRollback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{ID: "current-release", Environment: "production", Images: map[string]string{"web": "current"}, CreatedAt: time.Unix(20, 0)}); err != nil {
		t.Fatal(err)
	}
	failed := state.Release{ID: "failed-release", Environment: "production", Images: map[string]string{"web": "failed"}, CreatedAt: time.Unix(30, 0), Status: state.ReleaseStatusPending}
	if err := store.SaveReleaseRecord(failed); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkReleaseFailed("failed-release", "health failed", time.Unix(40, 0)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordEvent(state.Event{
		Time:        time.Unix(40, 0),
		Environment: "production",
		Kind:        "deploy_rollout",
		Status:      "failed",
		Release:     "failed-release",
		Message:     "health failed",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := recoverCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, needle := range []string{
		"current release current-release",
		"failed-release",
		"rollback target old-release",
		"suggested rollback: ship rollback production --to old-release",
		"deploy_rollout failed release=failed-release",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("recover output missing %q:\n%s", needle, text)
		}
	}

	out.Reset()
	cmd = recoverCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view recoveryView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.CurrentRelease == nil || view.CurrentRelease.ID != "current-release" || view.RollbackTarget == nil || view.RollbackTarget.ID != "old-release" || len(view.FailedReleases) != 1 {
		t.Fatalf("recovery view = %+v", view)
	}
}

func TestLogsTargetsReplicaLinesFollowAndJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "release-1", Environment: "production", Images: map[string]string{"web": "image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	originalInterval := logsFollowInterval
	logsFollowInterval = time.Nanosecond
	t.Cleanup(func() {
		logsFollowInterval = originalInterval
	})

	var events []string
	var logCalls []agent.LogsParams
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &observabilityAgent{host: host.Name, events: &events, logCalls: &logCalls}
	})

	var out bytes.Buffer
	cmd := logsCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "web", "--replica", "2", "--lines", "42", "--follow", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(logCalls) != logsFollowPolls {
		t.Fatalf("log calls = %+v", logCalls)
	}
	wantName := deployment.ContainerName("demo", "production", "web", 2, "release-1")
	for _, call := range logCalls {
		if call.Name != wantName || call.Lines != 42 {
			t.Fatalf("log call = %+v, want name=%s lines=42", call, wantName)
		}
	}
	var view logsView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Follow || view.Replica != 2 || len(view.Entries) != logsFollowPolls || view.Entries[0].Host != "web-2" {
		t.Fatalf("logs view = %+v", view)
	}
}

func TestAccessoryDeployPersistsPlacementAndRunsOneContainer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	var runs []agent.RunContainerParams
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, runs: &runs}
	})

	var out bytes.Buffer
	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"deploy", "production", "postgres"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"agent:data-a:list_ship_containers",
		"agent:data-b:list_ship_containers",
		"agent:data-a:pull:postgres:17",
		"agent:data-a:ensure_volume:postgres-data:999:999",
		"agent:data-a:run:ship_demo_production_accessory_postgres:postgres:17",
	}
	if strings.Join(events, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %#v", runs)
	}
	run := runs[0]
	if run.Labels[docker.LabelAccessory] != "postgres" || run.Labels[docker.LabelProject] != "demo" {
		t.Fatalf("run labels = %+v", run.Labels)
	}
	joinedArgs := strings.Join(run.Args, " ")
	for _, needle := range []string{"-p 5432:5432", "-v postgres-data:/var/lib/postgresql/data", "-e POSTGRES_PASSWORD_FILE=/run/secrets/postgres"} {
		if !strings.Contains(joinedArgs, needle) {
			t.Fatalf("run args %q missing %q", joinedArgs, needle)
		}
	}
	saved, err := state.NewStore(filepath.Join(dir, config.LocalStateDir)).ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Host.Name != "data-a" || saved.Host.User != "deploy" {
		t.Fatalf("saved placement = %+v", saved)
	}
	if !strings.Contains(out.String(), "deployed accessory postgres on data-a") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestAccessoryDeployWritesRemoteSecretFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessorySecretConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	secretValue := "postgres-secret"
	t.Setenv("SHIP_TEST_POSTGRES_PASSWORD", secretValue)

	var events []string
	var writes []agent.WriteFileParams
	var runs []agent.RunContainerParams
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, writes: &writes, runs: &runs}
	})

	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetArgs([]string{"deploy", "production", "postgres"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	secretPath := "/var/lib/ship/secrets/production/accessory-postgres.env"
	want := []string{
		"agent:data-a:list_ship_containers",
		"agent:data-b:list_ship_containers",
		"agent:data-a:write_file:" + secretPath + ":0600",
		"agent:data-a:pull:postgres:17",
		"agent:data-a:ensure_volume:postgres-data:999:999",
		"agent:data-a:run:ship_demo_production_accessory_postgres:postgres:17",
	}
	if strings.Join(events, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	if len(writes) != 1 {
		t.Fatalf("writes = %#v", writes)
	}
	if writes[0].Path != secretPath || writes[0].Mode != 0o600 || writes[0].Content != "SHIP_TEST_POSTGRES_PASSWORD="+secretValue+"\n" {
		t.Fatalf("write params = %+v", writes[0])
	}
	if len(runs) != 1 || !strings.Contains(strings.Join(runs[0].Args, " "), "--env-file "+secretPath) {
		t.Fatalf("runs = %#v", runs)
	}
}

func TestAccessoryDeployRefusesExistingReplicaOnDifferentHost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
		UpdatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"data-b": {accessoryContainer("postgres", "data-b", "Up 1 second")},
	}
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: observed}
	})

	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetArgs([]string{"deploy", "production", "postgres"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "already has a managed container on data-b") {
		t.Fatalf("expected replica guard error, got %v", err)
	}
	for _, event := range events {
		if strings.Contains(event, ":pull:") || strings.Contains(event, ":run:") {
			t.Fatalf("mutated after replica guard: %#v", events)
		}
	}
}

func TestAccessoryStatusReportsPlacementAndObservedContainer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
		UpdatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"data-a": {accessoryContainer("postgres", "data-a", "Up 5 seconds")},
	}
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: observed}
	})

	var out bytes.Buffer
	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"status", "production", "postgres"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"accessory postgres", "placement=data-a", "host=data-a", "status=Up 5 seconds"} {
		if !strings.Contains(out.String(), needle) {
			t.Fatalf("status output missing %q:\n%s", needle, out.String())
		}
	}
}

func TestAccessoryBackupRunsOnPersistedPlacementAndRecordsArtifact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
		UpdatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"data-a": {accessoryContainer("postgres", "data-a", "Up 5 seconds")},
	}
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: observed}
	})

	var out bytes.Buffer
	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"backup", "production", "postgres"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 ||
		events[0] != "agent:data-a:list_ship_containers" ||
		events[1] != "agent:data-b:list_ship_containers" ||
		!strings.HasPrefix(events[2], "agent:data-a:accessory_backup:postgres:") {
		t.Fatalf("events = %#v", events)
	}
	saved, err := store.ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if saved.LastBackup == nil || !strings.Contains(saved.LastBackup.Artifact, "/var/lib/ship/backups/postgres-") {
		t.Fatalf("saved backup = %+v", saved.LastBackup)
	}
	if !strings.Contains(out.String(), "backed up accessory postgres on data-a") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestAccessoryBackupRefusesStaleObservedTopology(t *testing.T) {
	tests := []struct {
		name     string
		observed map[string][]docker.ContainerSummary
		want     string
	}{
		{
			name: "different host",
			observed: map[string][]docker.ContainerSummary{
				"data-b": {accessoryContainer("postgres", "data-b", "Up 1 second")},
			},
			want: "already has a managed container on data-b",
		},
		{
			name: "multiple replicas",
			observed: map[string][]docker.ContainerSummary{
				"data-a": {
					accessoryContainer("postgres", "data-a", "Up 1 second"),
					accessoryContainer("postgres", "data-a", "Up 2 seconds"),
				},
			},
			want: "multiple managed containers",
		},
		{
			name: "container name mismatch",
			observed: map[string][]docker.ContainerSummary{
				"data-a": {func() docker.ContainerSummary {
					container := accessoryContainer("postgres", "data-a", "Up 1 second")
					container.Names = "ship_demo_production_accessory_postgres_old"
					return container
				}()},
			},
			want: "expected ship_demo_production_accessory_postgres",
		},
		{
			name:     "missing saved host container",
			observed: map[string][]docker.ContainerSummary{},
			want:     "no managed container on saved placement host data-a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, config.DefaultConfigFile)
			if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)
			store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
			if err := store.SaveAccessoryState(state.AccessoryState{
				Environment: "production",
				Name:        "postgres",
				Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
				UpdatedAt:   time.Unix(10, 0),
			}); err != nil {
				t.Fatal(err)
			}

			var events []string
			installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
				return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: tt.observed}
			})

			cmd := accessoryCmd(&options{configPath: path})
			cmd.SetArgs([]string{"backup", "production", "postgres"})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %q error, got %v", tt.want, err)
			}
			for _, event := range events {
				if strings.Contains(event, ":accessory_backup:") {
					t.Fatalf("mutated after topology guard: %#v", events)
				}
			}
			saved, err := store.ReadAccessoryState("production", "postgres")
			if err != nil {
				t.Fatal(err)
			}
			if saved.LastBackup != nil {
				t.Fatalf("recorded backup after topology guard: %+v", saved.LastBackup)
			}
		})
	}
}

func TestAccessoryRestoreRequiresConfirmationAndDryRunDoesNotMutate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
		UpdatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"data-a": {accessoryContainer("postgres", "data-a", "Up 5 seconds")},
	}
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: observed}
	})

	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetArgs([]string{"restore", "production", "postgres", "--artifact", "/var/lib/ship/backups/postgres.backup"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("expected confirmation error, got %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("restore without confirmation touched agent: %#v", events)
	}

	var out bytes.Buffer
	cmd = accessoryCmd(&options{configPath: path, dryRun: true})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"restore", "production", "postgres", "--artifact", "/var/lib/ship/backups/postgres.backup"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("dry-run touched agent: %#v", events)
	}
	saved, err := store.ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if saved.LastRestore != nil {
		t.Fatalf("dry-run recorded restore: %+v", saved.LastRestore)
	}
	if !strings.Contains(out.String(), "would restore accessory postgres on data-a") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestAccessoryRestoreChecksArtifactAndRecordsRestore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
		UpdatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"data-a": {accessoryContainer("postgres", "data-a", "Up 5 seconds")},
	}
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: observed}
	})

	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetArgs([]string{"restore", "production", "postgres", "--artifact", "/var/lib/ship/backups/postgres.backup", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"agent:data-a:list_ship_containers",
		"agent:data-b:list_ship_containers",
		"agent:data-a:health_check:test -s '/var/lib/ship/backups/postgres.backup'",
		"agent:data-a:accessory_restore:postgres:SHIP_BACKUP_ARTIFACT='/var/lib/ship/backups/postgres.backup' psql -f \"$SHIP_BACKUP_ARTIFACT\"",
	}
	if strings.Join(events, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	saved, err := store.ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if saved.LastRestore == nil || saved.LastRestore.Artifact != "/var/lib/ship/backups/postgres.backup" {
		t.Fatalf("saved restore = %+v", saved.LastRestore)
	}
}

func TestAccessoryRestoreRefusesBadTopologyBeforeRemoteCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
		UpdatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"data-b": {accessoryContainer("postgres", "data-b", "Up 1 second")},
	}
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: observed}
	})

	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetArgs([]string{"restore", "production", "postgres", "--artifact", "/var/lib/ship/backups/postgres.backup", "--yes"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "already has a managed container on data-b") {
		t.Fatalf("expected topology error, got %v", err)
	}
	for _, event := range events {
		if strings.Contains(event, ":health_check:") || strings.Contains(event, ":accessory_restore:") {
			t.Fatalf("remote restore check/mutation happened after topology guard: %#v", events)
		}
	}
	saved, err := store.ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if saved.LastRestore != nil {
		t.Fatalf("recorded restore after topology guard: %+v", saved.LastRestore)
	}
}

func TestAccessoryRestoreRejectsArtifactOutsideBackupDirBeforeRemoteCheck(t *testing.T) {
	tests := []struct {
		name     string
		artifact string
		want     string
	}{
		{
			name:     "outside configured directory",
			artifact: "/tmp/postgres.backup",
			want:     "must be within backup artifact directory",
		},
		{
			name:     "wrong suffix",
			artifact: "/var/lib/ship/backups/postgres.sql",
			want:     "must be a .backup file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, config.DefaultConfigFile)
			if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)
			store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
			if err := store.SaveAccessoryState(state.AccessoryState{
				Environment: "production",
				Name:        "postgres",
				Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
				UpdatedAt:   time.Unix(10, 0),
			}); err != nil {
				t.Fatal(err)
			}

			var events []string
			installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
				return &scriptedAccessoryAgent{host: host.Name, events: &events}
			})

			cmd := accessoryCmd(&options{configPath: path})
			cmd.SetArgs([]string{"restore", "production", "postgres", "--artifact", tt.artifact, "--yes"})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %q error, got %v", tt.want, err)
			}
			if len(events) != 0 {
				t.Fatalf("artifact validation touched agent: %#v", events)
			}
		})
	}
}

func TestAccessoryFailoverRestoresOnTargetStopsOldAndPersistsPlacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	backup := state.AccessoryBackup{
		Artifact:  "/var/lib/ship/backups/postgres.backup",
		Host:      "data-a",
		CreatedAt: time.Unix(20, 0),
	}
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
		UpdatedAt:   time.Unix(20, 0),
		LastBackup:  &backup,
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var runs []agent.RunContainerParams
	observed := map[string][]docker.ContainerSummary{
		"data-a": {accessoryContainer("postgres", "data-a", "Up 5 seconds")},
	}
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: observed, runs: &runs}
	})

	var out bytes.Buffer
	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"failover", "production", "postgres", "--to", "data-b", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"agent:data-a:list_ship_containers",
		"agent:data-b:list_ship_containers",
		"agent:data-b:pull:postgres:17",
		"agent:data-b:ensure_volume:postgres-data:999:999",
		"agent:data-b:run:ship_demo_production_accessory_postgres:postgres:17",
		"agent:data-b:health_check:test -s '/var/lib/ship/backups/postgres.backup'",
		"agent:data-b:accessory_restore:postgres:SHIP_BACKUP_ARTIFACT='/var/lib/ship/backups/postgres.backup' psql -f \"$SHIP_BACKUP_ARTIFACT\"",
		"agent:data-a:stop_container",
	}
	if strings.Join(events, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	if len(runs) != 1 || runs[0].Labels[docker.LabelAccessory] != "postgres" {
		t.Fatalf("run params = %+v", runs)
	}
	saved, err := store.ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Host.Name != "data-b" {
		t.Fatalf("saved host = %+v", saved.Host)
	}
	if saved.LastBackup == nil || saved.LastBackup.Artifact != backup.Artifact {
		t.Fatalf("last backup not preserved: %+v", saved.LastBackup)
	}
	if saved.LastRestore == nil || saved.LastRestore.Host != "data-b" || saved.LastRestore.Artifact != backup.Artifact {
		t.Fatalf("last restore = %+v", saved.LastRestore)
	}
	if !strings.Contains(out.String(), "failed over accessory postgres from data-a to data-b") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestSecretsRenderDryRunRedactsValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(secretDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	secretValue := "postgres://user:pass@example/db"
	t.Setenv("SHIP_TEST_DATABASE_URL", secretValue)

	var out bytes.Buffer
	cmd := secretsCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"render", "production", "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "/var/lib/ship/secrets/production/service-web.env") {
		t.Fatalf("render output missing remote path:\n%s", text)
	}
	if !strings.Contains(text, "SHIP_TEST_DATABASE_URL=<redacted:") {
		t.Fatalf("render output missing redacted secret:\n%s", text)
	}
	if strings.Contains(text, secretValue) {
		t.Fatalf("render output leaked secret value:\n%s", text)
	}
}

func TestSecretsDiffReportsDriftWithoutValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(secretDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	secretValue := "new-secret"
	t.Setenv("SHIP_TEST_DATABASE_URL", secretValue)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{
		ID:          "release-1",
		Environment: "production",
		Images:      map[string]string{"web": "registry.local/acme/web@sha256:" + strings.Repeat("1", 64)},
		SecretDigests: map[string]string{
			"SHIP_TEST_DATABASE_URL": secretspkg.Digest("old-secret"),
			"OLD_SECRET":             secretspkg.Digest("old"),
		},
		CreatedAt: time.Unix(30, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := secretsCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"diff", "production"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "secret drift detected") {
		t.Fatalf("expected drift error, got %v", err)
	}
	text := out.String()
	for _, needle := range []string{"changed SHIP_TEST_DATABASE_URL", "extra OLD_SECRET"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("diff output missing %q:\n%s", needle, text)
		}
	}
	if strings.Contains(text, secretValue) || strings.Contains(text, "old-secret") {
		t.Fatalf("diff output leaked secret value:\n%s", text)
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
    hosts:
      pools:
        web:
          count: 2

services:
  web:
    image:
      build: .
      dockerfile: Dockerfile
      build_args:
        RAILS_ENV: production
      target: runtime
      platform: linux/amd64
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
`
}

func secretMultiHostDeployConfig() string {
	return `project: demo
registry: registry.local/acme/demo

environments:
  production:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
    hosts:
      pools:
        web:
          count: 2

services:
  web:
    image:
      ref: registry.local/acme/web:stable
    command: ./bin/server
    pool: web
    scale: 2

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

func accessoryConfigYAML() string {
	return `project: demo
registry: registry.local/acme/demo

environments:
  production:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
    hosts:
      pools:
        data:
          user: deploy
          hosts:
            - data-b
            - data-a

services:
  web:
    image:
      ref: registry.local/acme/web:stable
    pool: data
    scale: 0

accessories:
  postgres:
    image: postgres:17
    pool: data
    primary: true
    volume_owner: "999:999"
    volumes:
      - postgres-data:/var/lib/postgresql/data
    ports: [5432]
    env:
      - POSTGRES_PASSWORD_FILE=/run/secrets/postgres
    backup:
      command: pg_dumpall
      restore_command: 'psql -f "$SHIP_BACKUP_ARTIFACT"'
      artifact_dir: /var/lib/ship/backups
      required: true
      restore_check: true
`
}

func accessorySecretConfigYAML() string {
	return accessoryConfigYAML() + `
secrets:
  - SHIP_TEST_POSTGRES_PASSWORD
`
}

func installAccessoryHooks(t *testing.T, agentFactory func(scheduler.Host) deployAgent) {
	t.Helper()
	originalAgent := newDeployAgent
	originalNow := deployNow
	newDeployAgent = agentFactory
	deployNow = func() time.Time {
		return time.Date(2026, 6, 30, 12, 34, 56, 123456789, time.FixedZone("MDT", -6*60*60))
	}
	t.Cleanup(func() {
		newDeployAgent = originalAgent
		deployNow = originalNow
	})
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

type observabilityAgent struct {
	host     string
	events   *[]string
	observed map[string][]docker.ContainerSummary
	logCalls *[]agent.LogsParams
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
	default:
		*o.events = append(*o.events, fmt.Sprintf("agent:%s:%s", o.host, method))
	}
	return nil
}

type scriptedAccessoryAgent struct {
	host       string
	events     *[]string
	observed   map[string][]docker.ContainerSummary
	runs       *[]agent.RunContainerParams
	writes     *[]agent.WriteFileParams
	failMethod string
}

func (s *scriptedAccessoryAgent) Call(ctx context.Context, method string, params any, out any) error {
	switch method {
	case "list_ship_containers":
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:list_ship_containers", s.host))
		if containers, ok := out.(*[]docker.ContainerSummary); ok {
			*containers = append([]docker.ContainerSummary(nil), s.observed[s.host]...)
		}
	case "pull":
		image := params.(map[string]string)["image"]
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:pull:%s", s.host, image))
	case "ensure_volume":
		p := params.(agent.EnsureVolumeParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:ensure_volume:%s:%s", s.host, p.Name, p.Owner))
	case "write_file":
		p := params.(agent.WriteFileParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:write_file:%s:%04o", s.host, p.Path, p.Mode))
		if s.writes != nil {
			*s.writes = append(*s.writes, p)
		}
	case "run_container":
		p := params.(agent.RunContainerParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:run:%s:%s", s.host, p.Name, p.Image))
		if s.runs != nil {
			*s.runs = append(*s.runs, p)
		}
	case "accessory_backup":
		p := params.(agent.AccessoryCommandParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:accessory_backup:%s:%s", s.host, p.Name, p.Command))
		if result, ok := out.(*agent.CommandResult); ok {
			*result = agent.CommandResult{Output: "backup complete"}
		}
	case "health_check":
		p := params.(agent.HealthCheckParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:health_check:%s", s.host, p.Command))
		if result, ok := out.(*agent.HealthCheckResult); ok {
			*result = agent.HealthCheckResult{OK: true}
		}
	case "accessory_restore":
		p := params.(agent.AccessoryCommandParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:accessory_restore:%s:%s", s.host, p.Name, p.Command))
		if result, ok := out.(*agent.CommandResult); ok {
			*result = agent.CommandResult{Output: "restore complete"}
		}
	default:
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:%s", s.host, method))
	}
	if method == s.failMethod {
		return fmt.Errorf("%s failed", method)
	}
	return nil
}

func installDeployHooks(t *testing.T, dockerClient deployDocker, agentFactory func(scheduler.Host) deployAgent) {
	t.Helper()
	originalDocker := newDeployDocker
	originalAgent := newDeployAgent
	originalNow := deployNow
	originalGitRevision := deployGitRevision
	newDeployDocker = func() deployDocker { return dockerClient }
	newDeployAgent = agentFactory
	deployNow = func() time.Time {
		return time.Date(2026, 6, 30, 12, 34, 56, 123456789, time.FixedZone("MDT", -6*60*60))
	}
	deployGitRevision = func(context.Context) (string, error) {
		return "abc123def456", nil
	}
	t.Cleanup(func() {
		newDeployDocker = originalDocker
		newDeployAgent = originalAgent
		deployNow = originalNow
		deployGitRevision = originalGitRevision
	})
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

type recordingDeployAgent struct {
	host          string
	events        *[]string
	releaseWrites *[]releaseStateWrite
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
			*containers = nil
		}
	case "pull":
		image := params.(map[string]string)["image"]
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:pull:%s", r.host, image))
	case "run_container":
		p := params.(agent.RunContainerParams)
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:run:%s", r.host, p.Image))
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
	default:
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:%s", s.host, method))
	}
	if method == s.failMethod {
		return fmt.Errorf("%s failed", method)
	}
	return nil
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
