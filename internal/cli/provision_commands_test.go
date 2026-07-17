package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/watzon/ship/internal/config"
	providerpkg "github.com/watzon/ship/internal/provider"
	"github.com/watzon/ship/internal/provider/hetzner"
	"github.com/watzon/ship/internal/state"
)

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

func TestProvisionPlanJSON(t *testing.T) {
	path := writeConfig(t)
	var out bytes.Buffer
	cmd := provisionCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"plan", "production", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view struct {
		Environment string `json:"environment"`
		Actions     []struct {
			Kind    string `json:"kind"`
			Target  string `json:"target"`
			Details string `json:"details,omitempty"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("invalid json %q: %v", out.String(), err)
	}
	if view.Environment != "production" || len(view.Actions) == 0 {
		t.Fatalf("view = %+v", view)
	}
	var sawWeb bool
	for _, action := range view.Actions {
		if action.Kind == "provision" && action.Target == "web-1" && action.Details == "pool=web" {
			sawWeb = true
		}
	}
	if !sawWeb {
		t.Fatalf("missing web provision action: %+v", view.Actions)
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
		case r.Method == http.MethodGet && r.URL.Path == "/networks":
			_ = json.NewEncoder(w).Encode(map[string]any{"networks": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/networks":
			_ = json.NewEncoder(w).Encode(map[string]any{"network": map[string]any{"id": 300, "name": "ship-demo-production-network"}})
		case r.Method == http.MethodGet && r.URL.Path == "/firewalls":
			_ = json.NewEncoder(w).Encode(map[string]any{"firewalls": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/firewalls":
			_ = json.NewEncoder(w).Encode(map[string]any{"firewall": map[string]any{"id": 400, "name": "ship-demo-production-firewall"}})
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

func TestProvisionApplyManualProviderBootstrapsExistingHosts(t *testing.T) {
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(manualHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	var bootstrapEvents []string
	installBootstrapHooks(t, &bootstrapEvents)

	var out bytes.Buffer
	cmd := provisionCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"apply", "production", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "exists web-1.example.com pool=web provider_id=web-1.example.com public_address=web-1.example.com") {
		t.Fatalf("output = %q", out.String())
	}
	if !strings.Contains(out.String(), "bootstrapped web-1.example.com") {
		t.Fatalf("bootstrap output missing: %q", out.String())
	}
	wantEvents := []string{
		"bootstrap:web-1.example.com:run:true",
		"bootstrap:web-1.example.com:run:set -eu",
		"bootstrap:web-1.example.com:upload:set -eu:16",
		"bootstrap:web-1.example.com:run:set -eu",
	}
	if strings.Join(bootstrapEvents, "\n") != strings.Join(wantEvents, "\n") {
		t.Fatalf("bootstrap events = %#v, want %#v", bootstrapEvents, wantEvents)
	}
	store := state.NewStore(filepath.Join(projectDir, config.LocalStateDir))
	facts, err := store.ReadHostFacts("production")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("facts = %+v", facts)
	}
	if facts[0].Provider != config.ProviderManual || facts[0].Name != "web-1.example.com" || facts[0].User != "deploy" || facts[0].PublicAddress != "web-1.example.com" {
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

func manualHostConfig() string {
	return `project: demo
registry: ghcr.io/acme/demo

environments:
  production:
    provider:
      manual: {}
    hosts:
      pools:
        web:
          user: deploy
          hosts:
            - web-1.example.com

services:
  web:
    image:
      ref: ghcr.io/acme/demo:web
    pool: web
    scale: 1
`
}
