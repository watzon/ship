package manual

import (
	"context"
	"testing"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

var _ provider.Provider = Provider{}

func TestPlanHostsUsesExplicitConfiguredHosts(t *testing.T) {
	plans, err := New(true, testEnvironment()).PlanHosts("demo", "production", testEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %+v", plans)
	}
	if plans[0].Name != "web-1.example.com" || plans[0].Pool != "web" || plans[0].User != "deploy" {
		t.Fatalf("plan[0] = %+v", plans[0])
	}
	if plans[0].Location != "manual" || plans[0].Size != "existing" || plans[0].Image != "existing" {
		t.Fatalf("provider options = %+v", plans[0])
	}
}

func TestReconcileTreatsConfiguredHostsAsExisting(t *testing.T) {
	result, err := New(false, testEnvironment()).Reconcile(context.Background(), "demo", "production", testEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Desired) != 2 || len(result.Existing) != 2 || len(result.Created) != 0 || len(result.Extra) != 0 {
		t.Fatalf("result = %+v", result)
	}
	if result.Existing[0].ID != "web-1.example.com" || result.Existing[0].PublicAddress != "web-1.example.com" {
		t.Fatalf("existing[0] = %+v", result.Existing[0])
	}
}

func TestListReturnsConfiguredHosts(t *testing.T) {
	hosts, err := New(false, testEnvironment()).List(context.Background(), "demo", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts = %+v", hosts)
	}
	if hosts[1].Name != "web-2.example.com" || hosts[1].Labels[provider.LabelEnvironment] != "production" {
		t.Fatalf("host[1] = %+v", hosts[1])
	}
}

func TestCredentialChecksPassWithoutCloudToken(t *testing.T) {
	checks := New(false, testEnvironment()).CredentialChecks(func(string) (string, bool) {
		t.Fatal("manual provider should not read environment credentials")
		return "", false
	})
	if len(checks) != 1 || !checks[0].Present || checks[0].Required {
		t.Fatalf("checks = %+v", checks)
	}
}

func testEnvironment() config.Environment {
	return config.Environment{
		Provider: config.ProviderConfig{Manual: &config.ManualConfig{}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {
				User:  "deploy",
				Hosts: []string{"web-2.example.com", "web-1.example.com"},
			},
		}},
	}
}
