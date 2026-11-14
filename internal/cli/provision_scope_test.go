package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/watzon/ship/internal/config"
	providerpkg "github.com/watzon/ship/internal/provider"
	"github.com/watzon/ship/internal/provider/latitude"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/state"
)

// fakeLatitudeAPI serves just enough of the Latitude API for plan and apply.
// Project A ("proj-a") holds a server with this config's Ship identity
// (ship-demo-production-web-1); project B ("proj-b"), which the config uses,
// has none. Writes are recorded and answered without touching a real API.
type fakeLatitudeAPI struct {
	mu             sync.Mutex
	serverProjects []string
	creates        int
	nextTag        int
}

func (api *fakeLatitudeAPI) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()
		write := func(v any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/tags":
			write(map[string]any{"data": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/tags":
			api.nextTag++
			write(map[string]any{"data": map[string]any{"id": "tag-" + strconv.Itoa(api.nextTag), "attributes": map[string]any{"name": "t"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/servers":
			filter := r.URL.Query().Get("filter[project]")
			api.serverProjects = append(api.serverProjects, filter)
			var servers []latitude.Server
			if filter == "" || filter == "proj-a" {
				servers = append(servers, latitude.Server{ID: "sv_a", Attributes: latitude.ServerAttributes{Hostname: "ship-demo-production-web-1", PrimaryIPv4: "198.51.100.1"}})
			}
			write(map[string]any{"data": servers, "meta": map[string]any{"current_page": 1, "total_pages": 1}})
		case r.Method == http.MethodPost && r.URL.Path == "/servers":
			api.creates++
			write(map[string]any{"data": latitude.Server{ID: "sv_b", Attributes: latitude.ServerAttributes{Hostname: "ship-demo-production-web-1", PrimaryIPv4: "198.51.100.2"}}})
		case r.Method == http.MethodPatch && r.URL.Path == "/servers/sv_b":
			write(map[string]any{"data": latitude.Server{ID: "sv_b", Attributes: latitude.ServerAttributes{Hostname: "ship-demo-production-web-1", PrimaryIPv4: "198.51.100.2"}}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	})
}

func writeLatitudeConfig(t *testing.T) string {
	t.Helper()
	text := strings.Replace(singleHostConfig(), `      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
`, `      latitude:
        project: proj-b
        site: ASH
        plan: c2-small-x86
        operating_system: ubuntu_24_04_x64_lts
        ssh_keys: [ssh-key-1]
        firewall:
          managed: false
`, 1)
	path := filepath.Join(t.TempDir(), config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProvisionRefreshAndApplyUseTheSameLatitudeProjectScope(t *testing.T) {
	// Thorn QA P1: refresh listed servers in every Latitude project and showed
	// "exists web-1 / apply will not create any servers", while apply listed
	// only project B and created web-1.
	api := &fakeLatitudeAPI{}
	server := httptest.NewServer(api.handler(t))
	defer server.Close()
	original := newEnvironmentProvider
	newEnvironmentProvider = func(env config.Environment, dryRun bool) (providerpkg.Provider, error) {
		// Build the client the way production does, then point it at the fake.
		client := latitude.NewFromEnv(dryRun, *env.Provider.Latitude)
		client.Token, client.BaseURL, client.HTTP = "token", server.URL, server.Client()
		return client, nil
	}
	t.Cleanup(func() { newEnvironmentProvider = original })
	path := writeLatitudeConfig(t)

	plan, err := runProvision(t, &options{configPath: path}, "plan", "production", "--refresh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "- create web-1: pool=web new server") || !strings.Contains(plan, "apply will create 1 server: web-1") {
		t.Fatalf("refresh plan must match what apply does:\n%s", plan)
	}

	var events []string
	installBootstrapHooks(t, &events)
	if _, err := runProvision(t, &options{configPath: path}, "apply", "production", "--yes"); err != nil {
		t.Fatal(err)
	}
	if api.creates != 1 {
		t.Fatalf("apply created %d servers, plan said 1", api.creates)
	}
	for _, filter := range api.serverProjects {
		if filter != "proj-b" {
			t.Fatalf("a server listing was not scoped to proj-b: %q (all: %v)", filter, api.serverProjects)
		}
	}
}

func TestProvisionApplyAcceptsPinInEquivalentSpelling(t *testing.T) {
	// Thorn QA P2: preflight accepted these pins, apply created web-2 and saved
	// facts, then host-fact matching compared strings literally and failed
	// before bootstrap. Thorn QA round 3: the saved fact and the SSH target
	// must keep the provider's spelling (known_hosts is keyed by it); only
	// comparisons canonicalize.
	tests := []struct {
		name     string
		pin      string
		reported string
	}{
		{name: "ipv6 case and zero compression", pin: "2001:DB8::1", reported: "2001:db8::0:1"},
		{name: "provider reports ipv4-mapped", pin: "203.0.113.10", reported: "::ffff:203.0.113.10"},
		{name: "pin written ipv4-mapped", pin: "\"::ffff:203.0.113.10\"", reported: "203.0.113.10"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writePinConfig(t, "          count: 2\n          pin:\n            web-1: "+tt.pin+"\n")
			var events []string
			installBootstrapHooks(t, &events)
			prov := &reconcilingProvider{existing: []providerpkg.Host{{
				ID: "abc", Name: "web-1", Pool: "web", PublicAddress: tt.reported,
				Labels: map[string]string{providerpkg.LabelHost: "web-1", providerpkg.LabelPool: "web"},
			}}}
			installProviderHook(t, prov)

			out, err := runProvision(t, &options{configPath: path}, "apply", "production", "--yes")
			if err != nil {
				t.Fatalf("apply failed: %v\n%s", err, out)
			}
			if strings.Join(prov.created, ",") != "web-2" {
				t.Fatalf("created = %v", prov.created)
			}
			if !strings.Contains(out, "bootstrapped web-1") || !strings.Contains(out, "bootstrapped web-2") {
				t.Fatalf("bootstrap did not run for every host:\n%s", out)
			}

			store := state.NewStore(filepath.Join(filepath.Dir(path), config.LocalStateDir))
			facts, err := store.ReadHostFacts("production")
			if err != nil {
				t.Fatal(err)
			}
			if facts[0].Name != "web-1" || facts[0].PublicAddress != tt.reported {
				t.Fatalf("saved web-1 fact = %+v, want provider spelling %s", facts[0], tt.reported)
			}

			// Later remote commands resolve the host at the same address.
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			hosts, err := resolvedHostsForEnvironment(store, "production", cfg.Environments["production"])
			if err != nil {
				t.Fatalf("hosts after apply: %v", err)
			}
			if hosts[0].ContactTarget() != tt.reported {
				t.Fatalf("web-1 contact = %q, want %q", hosts[0].ContactTarget(), tt.reported)
			}
		})
	}
}

func TestProvisionKeepsStoredAddressSpellingAsSSHTarget(t *testing.T) {
	// Thorn QA round 3: apply rewrote a stored ::ffff:127.0.0.1 fact to
	// 127.0.0.1, which changed the SSH target and broke strict host-key
	// checking against the known_hosts entry for the original spelling.
	const stored = "::ffff:127.0.0.1"
	tests := []struct {
		name string
		pool string
	}{
		{name: "unpinned", pool: "          count: 1\n"},
		{name: "pinned in another spelling", pool: "          count: 1\n          pin:\n            web-1: 127.0.0.1\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writePinConfig(t, tt.pool)
			saveFactsNextToConfig(t, path, []state.HostFact{{Name: "web-1", Pool: "web", User: "root", ProviderID: "abc", IPv4: stored, PublicAddress: stored}})
			prov := &reconcilingProvider{existing: []providerpkg.Host{{
				ID: "abc", Name: "web-1", Pool: "web", PublicAddress: stored,
				Labels: map[string]string{providerpkg.LabelHost: "web-1", providerpkg.LabelPool: "web"},
			}}}
			installProviderHook(t, prov)
			var events []string
			installBootstrapHooks(t, &events)
			var targets []string
			recording := newBootstrapSSH
			newBootstrapSSH = func(host scheduler.Host, dryRun bool) bootstrapSSH {
				targets = append(targets, host.ContactTarget())
				return recording(host, dryRun)
			}

			plan, err := runProvision(t, &options{configPath: path}, "plan", "production", "--refresh")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(plan, "apply will not create any servers") {
				t.Fatalf("refresh plan:\n%s", plan)
			}
			// Two applies: the stored spelling must survive repeated refreshes.
			for i := 0; i < 2; i++ {
				if out, err := runProvision(t, &options{configPath: path}, "apply", "production", "--yes"); err != nil {
					t.Fatalf("apply %d: %v\n%s", i+1, err, out)
				}
			}
			if len(prov.created) != 0 {
				t.Fatalf("created = %v", prov.created)
			}

			store := state.NewStore(filepath.Join(filepath.Dir(path), config.LocalStateDir))
			facts, err := store.ReadHostFacts("production")
			if err != nil {
				t.Fatal(err)
			}
			if facts[0].PublicAddress != stored || facts[0].IPv4 != stored {
				t.Fatalf("saved fact = %+v, want address kept as %s", facts[0], stored)
			}
			if len(targets) == 0 {
				t.Fatal("bootstrap SSH was never opened")
			}
			for _, target := range targets {
				if target != stored {
					t.Fatalf("bootstrap SSH target = %q, want %q (all: %v)", target, stored, targets)
				}
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			hosts, err := resolvedHostsForEnvironment(store, "production", cfg.Environments["production"])
			if err != nil {
				t.Fatal(err)
			}
			if hosts[0].ContactTarget() != stored {
				t.Fatalf("remote command target = %q, want %q", hosts[0].ContactTarget(), stored)
			}
		})
	}
}
