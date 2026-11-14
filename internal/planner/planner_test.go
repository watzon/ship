package planner

import (
	"reflect"
	"strings"
	"testing"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

func TestDeploymentPlanRendersBuildPlacementAndIngress(t *testing.T) {
	cfg := sampleConfig()
	plan, err := DeploymentPlan(cfg, "production")
	if err != nil {
		t.Fatal(err)
	}
	text := plan.String()
	for _, needle := range []string{
		"build web",
		"resolve web: pushed image -> immutable digest",
		"start web.1: on web-1",
		"start web.2: on web-2",
		"ingress web: example.com",
		"backup-check postgres: pg_dumpall",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("plan missing %q:\n%s", needle, text)
		}
	}
}

func sampleConfig() *config.Config {
	return &config.Config{
		Project:  "example",
		Registry: "ghcr.io/acme/example",
		Environments: map[string]config.Environment{
			"production": {
				Provider: config.ProviderConfig{Hetzner: &config.HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts: config.HostsConfig{Pools: map[string]config.Pool{
					"web":    {Count: 2},
					"worker": {Count: 1},
				}},
			},
		},
		Services: map[string]config.Service{
			"web": {
				Image:   config.ImageSpec{Build: ".", Dockerfile: "Dockerfile"},
				Pool:    "web",
				Scale:   2,
				Ports:   []int{3000},
				Ingress: &config.Ingress{Domains: []string{"example.com"}},
			},
		},
		Accessories: map[string]config.Accessory{
			"postgres": {
				Image:   "postgres:17",
				Pool:    "worker",
				Primary: boolPtr(true),
				Backup:  config.BackupSpec{Command: "pg_dumpall", Required: boolPtr(true), RestoreCheck: boolPtr(true)},
			},
		},
	}
}

func boolPtr(value bool) *bool {
	return &value
}

func TestProvisionPlanDistinguishesExistingAndNewHosts(t *testing.T) {
	plan := ProvisionPlan("production", ProvisionInput{
		Desired: []provider.HostPlan{
			{Name: "web-1", Pool: "web"},
			{Name: "web-2", Pool: "web"},
			{Name: "worker-1", Pool: "worker"},
		},
		Known: []KnownHost{
			{Name: "web-1", Pool: "web", Address: "203.0.113.10", ProviderID: "abc"},
			{Name: "web-3", Pool: "web", Address: "203.0.113.12", ProviderID: "def"},
		},
		Source:         "local host facts",
		CreatesServers: true,
		Provider:       "vultr",
	})
	want := []Action{
		{Kind: ProvisionExists, Target: "web-1", Details: "pool=web provider_id=abc address=203.0.113.10"},
		{Kind: ProvisionCreate, Target: "web-2", Details: "pool=web new server"},
		{Kind: ProvisionCreate, Target: "worker-1", Details: "pool=worker new server"},
		{Kind: ProvisionExtra, Target: "web-3", Details: "pool=web provider_id=def address=203.0.113.12 not in config; apply leaves it running"},
	}
	if !reflect.DeepEqual(plan.Actions, want) {
		t.Fatalf("actions = %+v\nwant %+v", plan.Actions, want)
	}
	if plan.Summary[ProvisionCreate] != 2 || plan.Summary[ProvisionExists] != 1 || plan.Summary[ProvisionExtra] != 1 {
		t.Fatalf("summary = %+v", plan.Summary)
	}
	text := plan.String()
	for _, needle := range []string{
		"- exists web-1: pool=web",
		"- create web-2: pool=web new server",
		"apply will create 2 servers: web-2, worker-1",
		"existing servers from local host facts",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("plan missing %q:\n%s", needle, text)
		}
	}
	if strings.Contains(text, "provision ") {
		t.Fatalf("plan still uses the ambiguous provision action:\n%s", text)
	}
}

func TestProvisionPlanSecondApplyCreatesNothing(t *testing.T) {
	plan := ProvisionPlan("production", ProvisionInput{
		Desired:        []provider.HostPlan{{Name: "web-1", Pool: "web"}},
		Known:          []KnownHost{{Name: "web-1", Pool: "web", Address: "203.0.113.10"}},
		CreatesServers: true,
	})
	if plan.Summary[ProvisionCreate] != 0 || !strings.Contains(plan.String(), "apply will not create any servers") {
		t.Fatalf("plan = %s", plan.String())
	}
}

func TestProvisionPlanPinnedHosts(t *testing.T) {
	tests := []struct {
		name          string
		known         []KnownHost
		authoritative bool
		wantKind      string
		wantDetails   string
	}{
		{
			name:        "pinned host matched",
			known:       []KnownHost{{Name: "web-1", Pool: "web", Address: "203.0.113.10"}},
			wantKind:    ProvisionExists,
			wantDetails: "pool=web address=203.0.113.10",
		},
		{
			name:        "pinned host without local facts",
			wantKind:    ProvisionPinned,
			wantDetails: "pool=web address=203.0.113.10 (not in local host facts; apply never creates a pinned host)",
		},
		{
			name:          "pinned host missing at provider",
			authoritative: true,
			wantKind:      ProvisionConflict,
			wantDetails:   "pool=web pinned to 203.0.113.10 but local host facts has no server for it; apply refuses and never creates a pinned host",
		},
		{
			name:        "pinned address differs",
			known:       []KnownHost{{Name: "web-1", Pool: "web", Address: "198.51.100.7"}},
			wantKind:    ProvisionConflict,
			wantDetails: "pool=web pinned to 203.0.113.10 but local host facts reports 198.51.100.7; apply refuses until the pin matches",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := ProvisionPlan("production", ProvisionInput{
				Desired:        []provider.HostPlan{{Name: "web-1", Pool: "web", PinnedAddress: "203.0.113.10"}},
				Known:          tt.known,
				Source:         "local host facts",
				Authoritative:  tt.authoritative,
				CreatesServers: true,
			})
			if len(plan.Actions) != 1 || plan.Actions[0].Kind != tt.wantKind || plan.Actions[0].Details != tt.wantDetails {
				t.Fatalf("actions = %+v", plan.Actions)
			}
			if plan.Summary[ProvisionCreate] != 0 {
				t.Fatalf("pinned host planned as create: %+v", plan.Actions)
			}
		})
	}
}

func TestProvisionPlanInventoryProviderNeverCreates(t *testing.T) {
	plan := ProvisionPlan("production", ProvisionInput{
		Desired:  []provider.HostPlan{{Name: "10.0.0.5", Pool: "web"}},
		Provider: "manual",
	})
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != ProvisionExists || plan.Actions[0].Details != "pool=web inventory=manual" {
		t.Fatalf("actions = %+v", plan.Actions)
	}
	if !strings.Contains(plan.String(), "apply never creates servers") {
		t.Fatalf("plan = %s", plan.String())
	}
}
