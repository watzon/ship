package planner

import (
	"strings"
	"testing"

	"github.com/watzon/ship/internal/config"
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
