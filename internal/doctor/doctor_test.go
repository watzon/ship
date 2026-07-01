package doctor

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/watzon/ship/internal/config"
	providerpkg "github.com/watzon/ship/internal/provider"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/state"
)

type fakeDocker struct {
	available error
	buildkit  error
	registry  error
}

func (f fakeDocker) Available(context.Context) error {
	return f.available
}

func (f fakeDocker) BuildKitSupported(context.Context) error {
	return f.buildkit
}

func (f fakeDocker) RegistryLoggedIn(context.Context, string) error {
	return f.registry
}

type fakeRemote struct {
	outputs map[string]string
	errors  map[string]error
	hosts   []scheduler.Host
}

func (f *fakeRemote) Run(_ context.Context, host scheduler.Host, command string) (string, error) {
	f.hosts = append(f.hosts, host)
	if err := f.errors[command]; err != nil {
		return "", err
	}
	return f.outputs[command], nil
}

type fakeProvider struct {
	name   string
	checks []providerpkg.CredentialCheck
}

func (f fakeProvider) Name() string {
	return f.name
}

func (f fakeProvider) PlanHosts(string, string, config.Environment) ([]providerpkg.HostPlan, error) {
	return nil, nil
}

func (f fakeProvider) Reconcile(context.Context, string, string, config.Environment) (providerpkg.ReconcileResult, error) {
	return providerpkg.ReconcileResult{}, nil
}

func (f fakeProvider) List(context.Context, string, string) ([]providerpkg.Host, error) {
	return nil, nil
}

func (f fakeProvider) Delete(context.Context, providerpkg.Host) error {
	return nil
}

func (f fakeProvider) CredentialChecks(func(string) (string, bool)) []providerpkg.CredentialCheck {
	return f.checks
}

func TestRunAggregatesIndependentLocalFailures(t *testing.T) {
	cfg, configPath := testConfig(t, nil)
	t.Setenv("HCLOUD_TOKEN", "token")

	report := Run(context.Background(), cfg, Options{
		ConfigPath:   configPath,
		Docker:       fakeDocker{available: errors.New("docker missing")},
		SSHAvailable: func(context.Context) error { return errors.New("ssh missing") },
	})

	if !report.Failed() {
		t.Fatal("expected report to fail")
	}
	assertCheck(t, report, "docker", StatusFail, "docker missing")
	assertCheck(t, report, "ssh", StatusFail, "ssh missing")
	assertCheck(t, report, "docker buildkit", StatusPass, "")
	assertCheck(t, report, "registry auth", StatusPass, "")
}

func TestRunUsesProviderCredentialChecks(t *testing.T) {
	cfg, configPath := testConfig(t, nil)

	report := Run(context.Background(), cfg, Options{
		ConfigPath:   configPath,
		Docker:       fakeDocker{},
		SSHAvailable: func(context.Context) error { return nil },
		ProviderFor: func(config.Environment, bool) (providerpkg.Provider, error) {
			return fakeProvider{
				name: "fake",
				checks: []providerpkg.CredentialCheck{{
					Name:           "fake token",
					Required:       true,
					Present:        false,
					MissingMessage: "missing FAKE_TOKEN",
				}},
			}, nil
		},
	})

	assertCheck(t, report, "fake token", StatusFail, "missing FAKE_TOKEN")
}

func TestRunChecksVultrCredentialsWithDefaultProvider(t *testing.T) {
	cfg, configPath := testConfig(t, nil)
	cfg.Environments["staging"] = config.Environment{
		Provider: config.ProviderConfig{Vultr: &config.VultrConfig{Region: "ewr", Plan: "vc2-2c-4gb", OSID: 2284, SSHAllowedCIDRs: []string{"203.0.113.0/24"}}},
		Hosts:    config.HostsConfig{Pools: map[string]config.Pool{"web": {Count: 1}}},
	}
	t.Setenv("HCLOUD_TOKEN", "token")
	restoreEnv(t, "VULTR_API_KEY")
	if err := os.Unsetenv("VULTR_API_KEY"); err != nil {
		t.Fatal(err)
	}

	report := Run(context.Background(), cfg, Options{
		ConfigPath:   configPath,
		Docker:       fakeDocker{},
		SSHAvailable: func(context.Context) error { return nil },
	})

	assertCheck(t, report, "hetzner token", StatusPass, "HCLOUD_TOKEN is set")
	assertCheck(t, report, "vultr token", StatusFail, "missing VULTR_API_KEY")
}

func TestRunChecksDigitalOceanCredentialsWithDefaultProvider(t *testing.T) {
	cfg, configPath := testConfig(t, nil)
	cfg.Environments["staging"] = config.Environment{
		Provider: config.ProviderConfig{DigitalOcean: &config.DigitalOceanConfig{
			Region:          "nyc3",
			Size:            "s-2vcpu-4gb",
			Image:           "ubuntu-24-04-x64",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{"web": {Count: 1}}},
	}
	t.Setenv("HCLOUD_TOKEN", "token")
	restoreEnv(t, "DIGITALOCEAN_TOKEN")
	if err := os.Unsetenv("DIGITALOCEAN_TOKEN"); err != nil {
		t.Fatal(err)
	}

	report := Run(context.Background(), cfg, Options{
		ConfigPath:   configPath,
		Docker:       fakeDocker{},
		SSHAvailable: func(context.Context) error { return nil },
	})

	assertCheck(t, report, "hetzner token", StatusPass, "HCLOUD_TOKEN is set")
	assertCheck(t, report, "digitalocean token", StatusFail, "missing DIGITALOCEAN_TOKEN")
}

func TestRunChecksLinodeCredentialsWithDefaultProvider(t *testing.T) {
	cfg, configPath := testConfig(t, nil)
	cfg.Environments["staging"] = config.Environment{
		Provider: config.ProviderConfig{Linode: &config.LinodeConfig{
			Region:          "us-east",
			Type:            "g6-standard-2",
			Image:           "linode/ubuntu24.04",
			AuthorizedKeys:  []string{"ssh-ed25519 AAAA..."},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{"web": {Count: 1}}},
	}
	t.Setenv("HCLOUD_TOKEN", "token")
	restoreEnv(t, "LINODE_TOKEN")
	if err := os.Unsetenv("LINODE_TOKEN"); err != nil {
		t.Fatal(err)
	}

	report := Run(context.Background(), cfg, Options{
		ConfigPath:   configPath,
		Docker:       fakeDocker{},
		SSHAvailable: func(context.Context) error { return nil },
	})

	assertCheck(t, report, "hetzner token", StatusPass, "HCLOUD_TOKEN is set")
	assertCheck(t, report, "linode token", StatusFail, "missing LINODE_TOKEN")
}

func TestRunChecksAWSCredentialsWithDefaultProvider(t *testing.T) {
	cfg, configPath := testConfig(t, nil)
	cfg.Environments["staging"] = config.Environment{
		Provider: config.ProviderConfig{AWS: &config.AWSConfig{
			Region:          "us-east-1",
			InstanceType:    "t3.medium",
			AMI:             "ami-0123456789abcdef0",
			SubnetID:        "subnet-0123456789abcdef0",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{"web": {Count: 1}}},
	}
	t.Setenv("HCLOUD_TOKEN", "token")
	restoreEnv(t, "AWS_ACCESS_KEY_ID")
	restoreEnv(t, "AWS_SECRET_ACCESS_KEY")
	if err := os.Unsetenv("AWS_ACCESS_KEY_ID"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("AWS_SECRET_ACCESS_KEY"); err != nil {
		t.Fatal(err)
	}

	report := Run(context.Background(), cfg, Options{
		ConfigPath:   configPath,
		Docker:       fakeDocker{},
		SSHAvailable: func(context.Context) error { return nil },
	})

	assertCheck(t, report, "hetzner token", StatusPass, "HCLOUD_TOKEN is set")
	assertCheck(t, report, "aws access key", StatusFail, "missing AWS_ACCESS_KEY_ID")
	assertCheck(t, report, "aws secret key", StatusFail, "missing AWS_SECRET_ACCESS_KEY")
}

func TestRunChecksGCPCredentialsWithDefaultProvider(t *testing.T) {
	cfg, configPath := testConfig(t, nil)
	cfg.Environments["staging"] = config.Environment{
		Provider: config.ProviderConfig{GCP: &config.GCPConfig{
			ProjectID:       "demo-project",
			Zone:            "us-central1-a",
			MachineType:     "e2-medium",
			Image:           "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{"web": {Count: 1}}},
	}
	t.Setenv("HCLOUD_TOKEN", "token")
	restoreEnv(t, "GCP_ACCESS_TOKEN")
	restoreEnv(t, "GOOGLE_APPLICATION_CREDENTIALS")
	if err := os.Unsetenv("GCP_ACCESS_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("GOOGLE_APPLICATION_CREDENTIALS"); err != nil {
		t.Fatal(err)
	}

	report := Run(context.Background(), cfg, Options{
		ConfigPath:   configPath,
		Docker:       fakeDocker{},
		SSHAvailable: func(context.Context) error { return nil },
	})

	assertCheck(t, report, "hetzner token", StatusPass, "HCLOUD_TOKEN is set")
	assertCheck(t, report, "gcp credentials", StatusFail, "missing GCP_ACCESS_TOKEN or GOOGLE_APPLICATION_CREDENTIALS")
}

func TestRunChecksAzureCredentialsWithDefaultProvider(t *testing.T) {
	cfg, configPath := testConfig(t, nil)
	cfg.Environments["staging"] = config.Environment{
		Provider: config.ProviderConfig{Azure: &config.AzureConfig{
			SubscriptionID:  "sub-123",
			ResourceGroup:   "rg-ship",
			Location:        "eastus",
			VMSize:          "Standard_B2s",
			Image:           "Canonical:ubuntu-24_04-lts:server:latest",
			AdminUsername:   "deploy",
			SSHPublicKey:    "ssh-ed25519 AAAA...",
			VirtualNetwork:  "ship-vnet",
			Subnet:          "default",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{"web": {Count: 1}}},
	}
	t.Setenv("HCLOUD_TOKEN", "token")
	for _, key := range []string{"AZURE_ACCESS_TOKEN", "AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_CLIENT_SECRET"} {
		restoreEnv(t, key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}

	report := Run(context.Background(), cfg, Options{
		ConfigPath:   configPath,
		Docker:       fakeDocker{},
		SSHAvailable: func(context.Context) error { return nil },
	})

	assertCheck(t, report, "hetzner token", StatusPass, "HCLOUD_TOKEN is set")
	assertCheck(t, report, "azure credentials", StatusFail, "missing AZURE_ACCESS_TOKEN or AZURE_TENANT_ID/AZURE_CLIENT_ID/AZURE_CLIENT_SECRET")
}

func TestRunChecksScalewayCredentialsWithDefaultProvider(t *testing.T) {
	cfg, configPath := testConfig(t, nil)
	cfg.Environments["staging"] = config.Environment{
		Provider: config.ProviderConfig{Scaleway: &config.ScalewayConfig{
			ProjectID:       "project-id",
			Zone:            "fr-par-1",
			CommercialType:  "DEV1-S",
			Image:           "ubuntu_noble",
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{"web": {Count: 1}}},
	}
	t.Setenv("HCLOUD_TOKEN", "token")
	for _, key := range []string{"SCW_SECRET_KEY", "SCALEWAY_SECRET_KEY"} {
		restoreEnv(t, key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}

	report := Run(context.Background(), cfg, Options{
		ConfigPath:   configPath,
		Docker:       fakeDocker{},
		SSHAvailable: func(context.Context) error { return nil },
	})

	assertCheck(t, report, "hetzner token", StatusPass, "HCLOUD_TOKEN is set")
	assertCheck(t, report, "scaleway token", StatusFail, "missing SCW_SECRET_KEY")
}

func TestRunChecksOpenStackCredentialsWithDefaultProvider(t *testing.T) {
	cfg, configPath := testConfig(t, nil)
	cfg.Environments["staging"] = config.Environment{
		Provider: config.ProviderConfig{OpenStack: &config.OpenStackConfig{
			Region: "GRA11",
			Flavor: "b2-7",
			Image:  "ubuntu-24.04",
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{"web": {Count: 1}}},
	}
	t.Setenv("HCLOUD_TOKEN", "token")
	for _, key := range []string{
		"OS_AUTH_TOKEN",
		"OS_COMPUTE_API_URL",
		"OS_AUTH_URL",
		"OS_APPLICATION_CREDENTIAL_ID",
		"OS_APPLICATION_CREDENTIAL_NAME",
		"OS_APPLICATION_CREDENTIAL_SECRET",
		"OS_USERNAME",
		"OS_USER_ID",
		"OS_PASSWORD",
		"OS_PROJECT_ID",
		"OS_PROJECT_NAME",
	} {
		restoreEnv(t, key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}

	report := Run(context.Background(), cfg, Options{
		ConfigPath:   configPath,
		Docker:       fakeDocker{},
		SSHAvailable: func(context.Context) error { return nil },
	})

	assertCheck(t, report, "hetzner token", StatusPass, "HCLOUD_TOKEN is set")
	assertCheck(t, report, "openstack credentials", StatusFail, "missing OS_AUTH_TOKEN/OS_COMPUTE_API_URL")
}

func TestRunChecksExoscaleCredentialsWithDefaultProvider(t *testing.T) {
	cfg, configPath := testConfig(t, nil)
	cfg.Environments["staging"] = config.Environment{
		Provider: config.ProviderConfig{Exoscale: &config.ExoscaleConfig{
			Zone:            "ch-gva-2",
			InstanceType:    "standard.medium",
			Template:        "template-id",
			SSHKeys:         []string{"deploy"},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{"web": {Count: 1}}},
	}
	t.Setenv("HCLOUD_TOKEN", "token")
	for _, key := range []string{"EXOSCALE_API_KEY", "EXOSCALE_API_SECRET"} {
		restoreEnv(t, key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}

	report := Run(context.Background(), cfg, Options{
		ConfigPath:   configPath,
		Docker:       fakeDocker{},
		SSHAvailable: func(context.Context) error { return nil },
	})

	assertCheck(t, report, "hetzner token", StatusPass, "HCLOUD_TOKEN is set")
	assertCheck(t, report, "exoscale credentials", StatusFail, "missing EXOSCALE_API_KEY/EXOSCALE_API_SECRET")
}

func TestRunChecksCloudscaleCredentialsWithDefaultProvider(t *testing.T) {
	cfg, configPath := testConfig(t, nil)
	cfg.Environments["staging"] = config.Environment{
		Provider: config.ProviderConfig{Cloudscale: &config.CloudscaleConfig{
			Zone:    "rma1",
			Flavor:  "flex-4-2",
			Image:   "debian-13",
			SSHKeys: []string{"ssh-ed25519 AAAA..."},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{"web": {Count: 1}}},
	}
	t.Setenv("HCLOUD_TOKEN", "token")
	restoreEnv(t, "CLOUDSCALE_API_TOKEN")
	if err := os.Unsetenv("CLOUDSCALE_API_TOKEN"); err != nil {
		t.Fatal(err)
	}

	report := Run(context.Background(), cfg, Options{
		ConfigPath:   configPath,
		Docker:       fakeDocker{},
		SSHAvailable: func(context.Context) error { return nil },
	})

	assertCheck(t, report, "hetzner token", StatusPass, "HCLOUD_TOKEN is set")
	assertCheck(t, report, "cloudscale token", StatusFail, "missing CLOUDSCALE_API_TOKEN")
}

func TestRunChecksLatitudeCredentialsWithDefaultProvider(t *testing.T) {
	cfg, configPath := testConfig(t, nil)
	cfg.Environments["staging"] = config.Environment{
		Provider: config.ProviderConfig{Latitude: &config.LatitudeConfig{
			Project:         "proj-demo",
			Site:            "ASH",
			Plan:            "c2-small-x86",
			OperatingSystem: "ubuntu_24_04_x64_lts",
			SSHKeys:         []string{"ssh-key-1"},
			SSHAllowedCIDRs: []string{"203.0.113.0/24"},
		}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{"web": {Count: 1}}},
	}
	t.Setenv("HCLOUD_TOKEN", "token")
	for _, key := range []string{"LATITUDE_API_TOKEN", "LATITUDESH_BEARER"} {
		restoreEnv(t, key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}

	report := Run(context.Background(), cfg, Options{
		ConfigPath:   configPath,
		Docker:       fakeDocker{},
		SSHAvailable: func(context.Context) error { return nil },
	})

	assertCheck(t, report, "hetzner token", StatusPass, "HCLOUD_TOKEN is set")
	assertCheck(t, report, "latitude token", StatusFail, "missing LATITUDE_API_TOKEN or LATITUDESH_BEARER")
}

func TestRunAcceptsManualProviderWithoutCloudCredentials(t *testing.T) {
	cfg, configPath := testConfig(t, []string{"web-1.example.com"})
	cfg.Environments["production"] = config.Environment{
		Provider: config.ProviderConfig{Manual: &config.ManualConfig{}},
		Hosts: config.HostsConfig{Pools: map[string]config.Pool{
			"web": {User: "deploy", Hosts: []string{"web-1.example.com"}},
		}},
	}

	report := Run(context.Background(), cfg, Options{
		ConfigPath: configPath,
		Docker:     fakeDocker{},
		SSHAvailable: func(context.Context) error {
			return nil
		},
		Remote: &fakeRemote{
			outputs: map[string]string{"uname -s": "Linux\n"},
			errors:  map[string]error{},
		},
	})

	assertCheck(t, report, "manual provider", StatusPass, "using existing SSH hosts")
}

func TestRunReportsMissingSecretsByName(t *testing.T) {
	secretName := "SHIP_DOCTOR_TEST_MISSING_SECRET"
	restoreEnv(t, secretName)
	if err := os.Unsetenv(secretName); err != nil {
		t.Fatal(err)
	}
	cfg, configPath := testConfig(t, nil)
	cfg.Secrets = []string{secretName}
	t.Setenv("HCLOUD_TOKEN", "token")

	report := Run(context.Background(), cfg, Options{
		ConfigPath:   configPath,
		Docker:       fakeDocker{},
		SSHAvailable: func(context.Context) error { return nil },
	})

	check := assertCheck(t, report, "secret:"+secretName, StatusFail, "missing")
	if strings.Contains(check.Message, "value") {
		t.Fatalf("secret check leaked value language: %+v", check)
	}
}

func TestRunChecksExplicitHostsWithFakeRemote(t *testing.T) {
	cfg, configPath := testConfig(t, []string{"web.example.com"})
	t.Setenv("HCLOUD_TOKEN", "token")
	remote := &fakeRemote{
		outputs: map[string]string{"uname -s": "Linux\n"},
		errors: map[string]error{
			"command -v systemctl >/dev/null && test -d /run/systemd/system":                  errors.New("systemd missing"),
			"systemctl is-enabled docker >/dev/null && systemctl is-active docker >/dev/null": errors.New("docker disabled"),
		},
	}

	report := Run(context.Background(), cfg, Options{
		ConfigPath:   configPath,
		Docker:       fakeDocker{},
		SSHAvailable: func(context.Context) error { return nil },
		Remote:       remote,
	})

	assertCheck(t, report, "remote:production/web.example.com ssh", StatusPass, "reachable")
	assertCheck(t, report, "remote:production/web.example.com linux", StatusPass, "Linux")
	assertCheck(t, report, "remote:production/web.example.com systemd", StatusFail, "systemd missing")
	assertCheck(t, report, "remote:production/web.example.com docker boot", StatusFail, "docker disabled")
	if len(remote.hosts) == 0 || remote.hosts[0].User != "deployer" {
		t.Fatalf("expected explicit host user to be preserved, got %+v", remote.hosts)
	}
}

func TestRunUsesSavedHostFactsForRemoteChecks(t *testing.T) {
	cfg, configPath := testConfig(t, nil)
	t.Setenv("HCLOUD_TOKEN", "token")
	store := state.NewStore(filepath.Join(filepath.Dir(configPath), config.LocalStateDir))
	if err := store.SaveHostFacts("production", []state.HostFact{{
		Name:          "web-1",
		Pool:          "web",
		User:          "deployer",
		IPv4:          "203.0.113.10",
		PublicAddress: "198.51.100.10",
		ServerID:      42,
	}}); err != nil {
		t.Fatal(err)
	}
	remote := &fakeRemote{
		outputs: map[string]string{"uname -s": "Linux\n"},
		errors:  map[string]error{},
	}

	report := Run(context.Background(), cfg, Options{
		ConfigPath:   configPath,
		Docker:       fakeDocker{},
		SSHAvailable: func(context.Context) error { return nil },
		Remote:       remote,
	})

	assertCheck(t, report, "remote:production/web-1 ssh", StatusPass, "reachable")
	if len(remote.hosts) == 0 {
		t.Fatal("expected remote checks to run")
	}
	for _, host := range remote.hosts {
		if host.Name != "web-1" || host.Contact != "198.51.100.10" || host.ContactTarget() != "198.51.100.10" {
			t.Fatalf("remote check used wrong host contact: %+v", host)
		}
	}
}

func TestRunReportsMismatchedSavedHostFacts(t *testing.T) {
	cfg, configPath := testConfig(t, nil)
	t.Setenv("HCLOUD_TOKEN", "token")
	store := state.NewStore(filepath.Join(filepath.Dir(configPath), config.LocalStateDir))
	if err := store.SaveHostFacts("production", []state.HostFact{{
		Name: "old-web-1",
		Pool: "web",
		User: "deployer",
		IPv4: "203.0.113.10",
	}}); err != nil {
		t.Fatal(err)
	}
	remote := &fakeRemote{outputs: map[string]string{"uname -s": "Linux\n"}, errors: map[string]error{}}

	report := Run(context.Background(), cfg, Options{
		ConfigPath:   configPath,
		Docker:       fakeDocker{},
		SSHAvailable: func(context.Context) error { return nil },
		Remote:       remote,
	})

	assertCheck(t, report, "remote hosts:production", StatusFail, "do not match configured hosts")
	if len(remote.hosts) != 0 {
		t.Fatalf("remote checks should not run with mismatched host facts: %+v", remote.hosts)
	}
}

func TestRunUsesFakeExecutablesOnPATH(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "docker"), `#!/bin/sh
case "$1" in
  version) exit 0 ;;
  buildx) [ "$2" = "version" ] && exit 0 ;;
esac
echo "unexpected docker command: $*" >&2
exit 2
`)
	writeExecutable(t, filepath.Join(dir, "ssh"), `#!/bin/sh
if [ "$1" = "-V" ]; then
  echo "OpenSSH_fake" >&2
  exit 0
fi
echo "unexpected ssh command: $*" >&2
exit 2
`)
	t.Setenv("PATH", dir)
	registryHost := newBearerRegistry(t, "u", "s")
	auth := base64.StdEncoding.EncodeToString([]byte("u:s"))
	t.Setenv("DOCKER_AUTH_CONFIG", fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, registryHost, auth))
	t.Setenv("HCLOUD_TOKEN", "token")
	cfg, configPath := testConfig(t, nil)
	cfg.Registry = registryHost + "/acme/example"

	report := Run(context.Background(), cfg, Options{ConfigPath: configPath})

	if report.Failed() {
		t.Fatalf("expected fake PATH tools to pass, got %+v", report)
	}
	assertCheck(t, report, "docker", StatusPass, "")
	assertCheck(t, report, "ssh", StatusPass, "")
	assertCheck(t, report, "registry auth", StatusPass, "")
}

func testConfig(t *testing.T, explicitHosts []string) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pool := config.Pool{Count: 1, Hosts: explicitHosts, User: "deployer"}
	return &config.Config{
		Project:  "example",
		Registry: "ghcr.io/acme/example",
		Environments: map[string]config.Environment{
			"production": {
				Provider: config.ProviderConfig{Hetzner: &config.HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04", SSHAllowedCIDRs: []string{"0.0.0.0/0"}}},
				Hosts:    config.HostsConfig{Pools: map[string]config.Pool{"web": pool}},
			},
		},
		Services: map[string]config.Service{
			"web": {Image: config.ImageSpec{Build: "."}, Pool: "web", Scale: 1},
		},
	}, filepath.Join(dir, config.DefaultConfigFile)
}

func assertCheck(t *testing.T, report Report, name string, status Status, messageContains string) Check {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name != name {
			continue
		}
		if check.Status != status {
			t.Fatalf("%s status = %s, want %s; check=%+v", name, check.Status, status, check)
		}
		if messageContains != "" && !strings.Contains(check.Message, messageContains) {
			t.Fatalf("%s message = %q, want containing %q", name, check.Message, messageContains)
		}
		return check
	}
	t.Fatalf("missing check %q in %+v", name, report.Checks)
	return Check{}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func newBearerRegistry(t *testing.T, username, password string) string {
	t.Helper()

	const token = "issued-token"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			if r.Header.Get("Authorization") == "Bearer "+token {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="ship-test"`, server.URL))
			w.WriteHeader(http.StatusUnauthorized)
		case "/token":
			gotUsername, gotPassword, ok := r.BasicAuth()
			if !ok || gotUsername != username || gotPassword != password {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"token":"`+token+`"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

func restoreEnv(t *testing.T, name string) {
	t.Helper()
	value, ok := os.LookupEnv(name)
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(name, value)
			return
		}
		_ = os.Unsetenv(name)
	})
}
