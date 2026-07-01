package pulumi

import (
	"context"
	"strings"
	"testing"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

var _ provider.Provider = Provider{}

func TestHostsParsesPoolMapOutput(t *testing.T) {
	env := testEnvironment()
	prov := testProvider(env, `{
		"ship_hosts": {
			"web": ["198.51.100.10", "198.51.100.11"],
			"worker": [{"name": "worker-1", "address": "198.51.100.20", "user": "ops", "port": 2222}]
		}
	}`)

	hosts, err := prov.Hosts(context.Background(), "demo", "production", env)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 3 {
		t.Fatalf("hosts = %+v", hosts)
	}
	if hosts[0].Name != "198.51.100.10" || hosts[0].Pool != "web" || hosts[0].User != "deploy" {
		t.Fatalf("host[0] = %+v", hosts[0])
	}
	if hosts[2].Name != "worker-1" || hosts[2].Pool != "worker" || hosts[2].PublicAddress != "198.51.100.20" || hosts[2].User != "ops" || hosts[2].SSHPort != 2222 {
		t.Fatalf("host[2] = %+v", hosts[2])
	}
}

func TestHostsParsesObjectListOutputAndPassesStackFlags(t *testing.T) {
	env := testEnvironment()
	env.Provider.Pulumi.Stack = "production"
	env.Provider.Pulumi.WorkingDir = "infra"
	showSecrets := true
	env.Provider.Pulumi.ShowSecrets = &showSecrets
	var gotArgs []string
	var gotEnv []string
	prov := Provider{
		Env: env,
		Run: func(ctx context.Context, workDir, binary string, env []string, args ...string) ([]byte, error) {
			_ = ctx
			if workDir != "" {
				t.Fatalf("workDir = %q", workDir)
			}
			if binary != "pulumi" {
				t.Fatalf("binary = %q", binary)
			}
			gotEnv = append([]string(nil), env...)
			gotArgs = append([]string(nil), args...)
			return []byte(`{
				"ship_hosts": [
					{"id": "i-1", "name": "web-1", "address": "203.0.113.10", "pool": "web", "port": "2200"},
					{"id": "i-2", "name": "worker-1", "address": "203.0.113.20", "pool": "worker"}
				]
			}`), nil
		},
	}

	hosts, err := prov.Hosts(context.Background(), "demo", "production", env)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(gotArgs, " ") != "stack output --json --stack production --cwd infra --show-secrets" {
		t.Fatalf("args = %+v", gotArgs)
	}
	if len(gotEnv) != 0 {
		t.Fatalf("env = %+v", gotEnv)
	}
	if hosts[0].ID != "i-1" || hosts[0].Name != "web-1" || hosts[0].PublicAddress != "203.0.113.10" || hosts[0].SSHPort != 2200 {
		t.Fatalf("hosts = %+v", hosts)
	}
}

func TestReconcileTreatsPulumiHostsAsExisting(t *testing.T) {
	env := testEnvironment()
	prov := testProvider(env, `{
		"ship_hosts": [{"name": "web-1", "address": "203.0.113.10", "pool": "web"}]
	}`)

	result, err := prov.Reconcile(context.Background(), "demo", "production", env)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Desired) != 1 || len(result.Existing) != 1 || len(result.Created) != 0 || len(result.Extra) != 0 {
		t.Fatalf("result = %+v", result)
	}
	if result.Desired[0].Location != "pulumi" || result.Existing[0].PublicAddress != "203.0.113.10" {
		t.Fatalf("result = %+v", result)
	}
	if result.Existing[0].Labels[provider.LabelProject] != "demo" || result.Existing[0].Labels["tier"] != "edge" {
		t.Fatalf("labels = %+v", result.Existing[0].Labels)
	}
}

func TestHostsRejectsUnknownPool(t *testing.T) {
	env := testEnvironment()
	prov := testProvider(env, `{
		"ship_hosts": [{"name": "db-1", "address": "203.0.113.30", "pool": "db"}]
	}`)

	_, err := prov.Hosts(context.Background(), "demo", "production", env)
	if err == nil || !strings.Contains(err.Error(), `unknown pool "db"`) {
		t.Fatalf("expected unknown pool error, got %v", err)
	}
}

func TestCredentialChecksRequirePulumiBinary(t *testing.T) {
	env := testEnvironment()
	prov := Provider{
		Env: env,
		LookupPath: func(binary string) (string, error) {
			if binary != "pulumi" {
				t.Fatalf("binary = %q", binary)
			}
			return "/usr/bin/pulumi", nil
		},
	}
	checks := prov.CredentialChecks(func(string) (string, bool) {
		t.Fatal("pulumi provider should not need cloud credentials")
		return "", false
	})
	if len(checks) != 1 || !checks[0].Present || !checks[0].Required {
		t.Fatalf("checks = %+v", checks)
	}
}

func testProvider(env config.Environment, output string) Provider {
	return Provider{
		Env: env,
		Run: func(ctx context.Context, workDir, binary string, env []string, args ...string) ([]byte, error) {
			_ = ctx
			_ = workDir
			_ = env
			if binary != "pulumi" {
				return nil, nil
			}
			if strings.Join(args, " ") != "stack output --json" {
				return nil, nil
			}
			return []byte(output), nil
		},
	}
}

func testEnvironment() config.Environment {
	return config.Environment{
		Provider: config.ProviderConfig{Pulumi: &config.PulumiConfig{
			Output: "ship_hosts",
			User:   "deploy",
		}},
		Hosts: config.HostsConfig{
			Labels: map[string]string{"team": "platform"},
			Pools: map[string]config.Pool{
				"web":    {Labels: map[string]string{"tier": "edge"}},
				"worker": {User: "worker"},
			},
		},
	}
}
