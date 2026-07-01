package providers

import (
	"testing"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/vultr"
)

func TestForEnvironmentReturnsVultrProvider(t *testing.T) {
	prov, err := ForEnvironment(config.Environment{
		Provider: config.ProviderConfig{Vultr: &config.VultrConfig{Region: "ewr", Plan: "vc2-2c-4gb", OSID: 2284}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := prov.(vultr.Client)
	if !ok {
		t.Fatalf("provider = %T, want vultr.Client", prov)
	}
	if !client.DryRun {
		t.Fatal("expected dry-run client")
	}
}
