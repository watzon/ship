package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSample(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultConfigFile)
	if err := os.WriteFile(path, []byte(Sample()), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Project != "example" {
		t.Fatalf("project = %q", cfg.Project)
	}
	if cfg.Services["web"].Scale != 6 {
		t.Fatalf("web scale = %d", cfg.Services["web"].Scale)
	}
}

func TestValidateReportsMissingPool(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04"}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"worker": {Pool: "worker", Scale: 1, Image: ImageSpec{Ref: "example"}},
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "missing pool") {
		t.Fatalf("expected missing pool error, got %v", err)
	}
}

func TestValidateAcceptsHetznerProvider(t *testing.T) {
	cfg := minimalValidConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if got := cfg.Environments["production"].Provider.Name(); got != ProviderHetzner {
		t.Fatalf("provider name = %q, want %q", got, ProviderHetzner)
	}
}

func TestValidateRequiresProvider(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Environments["production"] = Environment{
		Hosts: HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `environment "production" must define exactly one provider`) {
		t.Fatalf("expected missing provider error, got %v", err)
	}
}

func TestLoadReportsUnsupportedProvider(t *testing.T) {
	cfgText := strings.Replace(minimalConfigYAML(), "hetzner:", "digitalocean:", 1)
	_, err := loadConfigText(t, cfgText)
	if err == nil || !strings.Contains(err.Error(), `unsupported provider(s): digitalocean`) {
		t.Fatalf("expected unsupported provider error, got %v", err)
	}
}

func TestLoadReportsMultipleProviderBlocks(t *testing.T) {
	cfgText := strings.Replace(minimalConfigYAML(), "      hetzner:", "      digitalocean:\n        region: nyc1\n      hetzner:", 1)
	_, err := loadConfigText(t, cfgText)
	if err == nil || !strings.Contains(err.Error(), `must define exactly one provider`) {
		t.Fatalf("expected multiple provider error, got %v", err)
	}
}

func TestValidateBuildOptionsRequireBuild(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04"}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {
				Pool:  "web",
				Scale: 1,
				Image: ImageSpec{
					Ref:       "ghcr.io/acme/x:web",
					BuildArgs: map[string]string{"VERSION": "x"},
					Target:    "runtime",
					Platform:  "linux/amd64",
				},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, needle := range []string{"image.build_args requires image.build", "image.target requires image.build", "image.platform requires image.build"} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("validation error missing %q: %v", needle, err)
		}
	}
}

func TestValidateAccessoryBackupRequiredRequiresCommand(t *testing.T) {
	cfg := &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04"}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"data": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {Pool: "data", Scale: 0, Image: ImageSpec{Ref: "example/web"}},
		},
		Accessories: map[string]Accessory{
			"redis": {
				Image:  "redis:7",
				Pool:   "data",
				Backup: BackupSpec{Required: true},
			},
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `accessory "redis" requires backup.command`) {
		t.Fatalf("expected accessory backup command validation error, got %v", err)
	}
}

func minimalValidConfig() *Config {
	return &Config{
		Project:  "x",
		Registry: "ghcr.io/acme/x",
		Environments: map[string]Environment{
			"production": {
				Provider: ProviderConfig{Hetzner: &HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04"}},
				Hosts:    HostsConfig{Pools: map[string]Pool{"web": {Count: 1}}},
			},
		},
		Services: map[string]Service{
			"web": {Pool: "web", Scale: 1, Image: ImageSpec{Ref: "example/web"}},
		},
	}
}

func loadConfigText(t *testing.T, text string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultConfigFile)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func minimalConfigYAML() string {
	return `project: x
registry: ghcr.io/acme/x

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
      ref: example/web
    pool: web
    scale: 1
`
}
