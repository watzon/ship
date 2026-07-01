package ansible

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider"
)

var _ provider.Provider = Provider{}

func TestHostsParsesNestedYAMLInventory(t *testing.T) {
	env := testEnvironment()
	prov := testProvider(env, `
all:
  children:
    web:
      hosts:
        web-1:
          ansible_host: 203.0.113.10
          ansible_port: 2222
        web-2:
          ansible_host: 203.0.113.11
          ansible_user: ubuntu
    worker:
      hosts:
        worker-1:
          ansible_host: 203.0.113.20
`)

	hosts, err := prov.Hosts(context.Background(), "demo", "production", env)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 3 {
		t.Fatalf("hosts = %+v", hosts)
	}
	if hosts[0].Name != "web-1" || hosts[0].Pool != "web" || hosts[0].PublicAddress != "203.0.113.10" || hosts[0].User != "deploy" || hosts[0].SSHPort != 2222 {
		t.Fatalf("host[0] = %+v", hosts[0])
	}
	if hosts[1].Name != "web-2" || hosts[1].User != "ubuntu" {
		t.Fatalf("host[1] = %+v", hosts[1])
	}
	if hosts[2].Name != "worker-1" || hosts[2].Pool != "worker" || hosts[2].User != "worker" {
		t.Fatalf("host[2] = %+v", hosts[2])
	}
}

func TestHostsParsesDynamicInventoryHostvars(t *testing.T) {
	env := testEnvironment()
	env.Provider.Ansible = &config.AnsibleConfig{
		Command: []string{"ansible-inventory", "-i", "inventory.yml", "--list"},
		User:    "deploy",
	}
	var gotBinary string
	var gotArgs []string
	prov := Provider{
		Env: env,
		Run: func(ctx context.Context, binary string, args ...string) ([]byte, error) {
			_ = ctx
			gotBinary = binary
			gotArgs = append([]string(nil), args...)
			return []byte(`{
				"web": {"hosts": ["web-1"]},
				"worker": {"hosts": ["worker-1"]},
				"_meta": {
					"hostvars": {
						"web-1": {"ansible_host": "198.51.100.10", "ansible_user": "ubuntu", "ansible_ssh_port": "2200"},
						"worker-1": {"ansible_host": "198.51.100.20"}
					}
				}
			}`), nil
		},
	}

	hosts, err := prov.Hosts(context.Background(), "demo", "production", env)
	if err != nil {
		t.Fatal(err)
	}
	if gotBinary != "ansible-inventory" || strings.Join(gotArgs, " ") != "-i inventory.yml --list" {
		t.Fatalf("command = %s %+v", gotBinary, gotArgs)
	}
	if len(hosts) != 2 || hosts[0].PublicAddress != "198.51.100.10" || hosts[0].User != "ubuntu" || hosts[0].SSHPort != 2200 {
		t.Fatalf("hosts = %+v", hosts)
	}
}

func TestReconcileTreatsAnsibleHostsAsExisting(t *testing.T) {
	env := testEnvironment()
	prov := testProvider(env, `
web:
  hosts:
    web-1:
      ansible_host: 203.0.113.10
worker:
  hosts:
    worker-1:
      ansible_host: 203.0.113.20
`)

	result, err := prov.Reconcile(context.Background(), "demo", "production", env)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Desired) != 2 || len(result.Existing) != 2 || len(result.Created) != 0 || len(result.Extra) != 0 {
		t.Fatalf("result = %+v", result)
	}
	if result.Desired[0].Location != "ansible" || result.Existing[0].PublicAddress != "203.0.113.10" || result.Existing[0].SSHPort != 0 {
		t.Fatalf("result = %+v", result)
	}
	if result.Existing[0].Labels[provider.LabelProject] != "demo" || result.Existing[0].Labels["tier"] != "edge" {
		t.Fatalf("labels = %+v", result.Existing[0].Labels)
	}
}

func TestHostsRejectsMissingPoolGroup(t *testing.T) {
	env := testEnvironment()
	prov := testProvider(env, `
web:
  hosts:
    web-1:
`)

	_, err := prov.Hosts(context.Background(), "demo", "production", env)
	if err == nil || !strings.Contains(err.Error(), `missing group for pool "worker"`) {
		t.Fatalf("expected missing pool group error, got %v", err)
	}
}

func TestSinglePoolFallsBackToAllGroup(t *testing.T) {
	env := config.Environment{
		Provider: config.ProviderConfig{Ansible: &config.AnsibleConfig{
			InventoryFile: "inventory.yml",
			User:          "deploy",
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{"web": {}}},
	}
	prov := testProvider(env, `
all:
  hosts:
    web-1:
      ansible_host: 203.0.113.10
`)
	hosts, err := prov.Hosts(context.Background(), "demo", "production", env)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Pool != "web" || hosts[0].Name != "web-1" {
		t.Fatalf("hosts = %+v", hosts)
	}
}

func TestCredentialChecksRequireCommandBinary(t *testing.T) {
	env := testEnvironment()
	env.Provider.Ansible = &config.AnsibleConfig{Command: []string{"ansible-inventory", "--list"}}
	prov := Provider{
		Env: env,
		LookupPath: func(binary string) (string, error) {
			if binary != "ansible-inventory" {
				t.Fatalf("binary = %q", binary)
			}
			return "", errors.New("missing")
		},
	}
	checks := prov.CredentialChecks(func(string) (string, bool) {
		t.Fatal("ansible provider should not need cloud credentials")
		return "", false
	})
	if len(checks) != 1 || checks[0].Present || !checks[0].Required {
		t.Fatalf("checks = %+v", checks)
	}
}

func testProvider(env config.Environment, inventory string) Provider {
	return Provider{
		Env: env,
		ReadFile: func(path string) ([]byte, error) {
			if path != "inventory.yml" {
				return nil, errors.New("unexpected inventory path")
			}
			return []byte(inventory), nil
		},
	}
}

func testEnvironment() config.Environment {
	return config.Environment{
		Provider: config.ProviderConfig{Ansible: &config.AnsibleConfig{
			InventoryFile: "inventory.yml",
			User:          "deploy",
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
