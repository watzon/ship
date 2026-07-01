package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/provider/hetzner"
	"github.com/watzon/ship/internal/state"
)

func TestLiveHetznerAcceptanceGate(t *testing.T) {
	if os.Getenv("SHIP_LIVE_HETZNER") != "1" {
		t.Skip("set SHIP_LIVE_HETZNER=1 and HCLOUD_TOKEN to run the read-only live Hetzner acceptance gate")
	}
	if strings.TrimSpace(os.Getenv("HCLOUD_TOKEN")) == "" {
		t.Fatal("HCLOUD_TOKEN is required when SHIP_LIVE_HETZNER=1")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	cfgText := acceptanceDryRunConfigYAML(sampleAppPath(t))
	if project := strings.TrimSpace(os.Getenv("SHIP_LIVE_HETZNER_PROJECT")); project != "" {
		cfgText = strings.Replace(cfgText, "project: sample", "project: "+project, 1)
	}
	if err := os.WriteFile(path, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}

	out := runAcceptanceCommand(t, provisionCmd(&options{configPath: path}), "plan", "production")
	assertAcceptanceOutput(t, out, "provision web-1", "provision worker-1")
	out = runAcceptanceCommand(t, provisionCmd(&options{configPath: path, dryRun: true}), "apply", "production")
	assertAcceptanceOutput(t, out, "would provision web-1 pool=web")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	servers, err := hetzner.NewFromEnv(false).ListServers(ctx, cfg.Project, "production")
	if err != nil {
		t.Fatalf("read-only Hetzner list failed: %v", err)
	}
	t.Logf("read-only Hetzner gate passed for project=%s environment=production matching_servers=%d", cfg.Project, len(servers))
}

func TestDestructiveLiveHetznerFullCycle(t *testing.T) {
	if os.Getenv("SHIP_LIVE_HETZNER_DESTRUCTIVE") != "1" {
		t.Skip("set SHIP_LIVE_HETZNER_DESTRUCTIVE=1 to run the destructive live Hetzner full-cycle acceptance test")
	}
	for _, name := range []string{"HCLOUD_TOKEN", "SHIP_LIVE_HETZNER_REGISTRY", "SHIP_LIVE_HETZNER_SSH_KEY"} {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			t.Fatalf("%s is required for destructive live Hetzner acceptance", name)
		}
	}

	dir := t.TempDir()
	t.Setenv("SHIP_SSH_KNOWN_HOSTS_FILE", filepath.Join(dir, "known_hosts"))
	path := filepath.Join(dir, config.DefaultConfigFile)
	project := strings.TrimSpace(os.Getenv("SHIP_LIVE_HETZNER_PROJECT"))
	if project == "" {
		project = fmt.Sprintf("ship-live-%d", time.Now().Unix())
	}
	cfgText := destructiveLiveHetznerConfigYAML(project, sampleAppPath(t))
	if err := os.WriteFile(path, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	shipBinary := filepath.Join(dir, "ship")
	build := exec.Command("go", "build", "-o", shipBinary, "./cmd/ship")
	build.Dir = filepath.Clean(filepath.Join(sampleAppPath(t), "..", ".."))
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build ship binary for live bootstrap: %v\n%s", err, out)
	}
	originalReadCurrentShipBinary := readCurrentShipBinary
	readCurrentShipBinary = func() ([]byte, error) {
		return os.ReadFile(shipBinary)
	}
	t.Cleanup(func() {
		readCurrentShipBinary = originalReadCurrentShipBinary
	})
	t.Cleanup(func() {
		out, err := runAcceptanceCommandError(t, provisionCmd(&options{configPath: path}), "decommission", "production", "--yes")
		if err != nil {
			t.Logf("cleanup decommission failed: %v\n%s", err, out)
		}
	})

	out := runAcceptanceCommand(t, provisionCmd(&options{configPath: path}), "plan", "production")
	assertAcceptanceOutput(t, out, "provision web-1", "provision web-2")
	runAcceptanceCommand(t, provisionCmd(&options{configPath: path}), "apply", "production", "--yes")
	runAcceptanceCommand(t, agentCmd(&options{configPath: path}), "status", "production")

	runAcceptanceCommand(t, deployCmd(&options{configPath: path}), "production")
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	first := currentAcceptanceRelease(t, store)

	out = runAcceptanceCommand(t, scaleCmd(&options{configPath: path, dryRun: true}), "production", "web=2")
	assertAcceptanceOutput(t, out, "start web.2")
	if err := os.WriteFile(path, []byte(strings.Replace(cfgText, "scale: 1", "scale: 2", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	runAcceptanceCommand(t, deployCmd(&options{configPath: path}), "production")
	second := currentAcceptanceRelease(t, store)
	if second.ID == first.ID {
		t.Fatalf("second deploy reused release id %s", second.ID)
	}

	runAcceptanceCommand(t, rollbackCmd(&options{configPath: path}), "production", "--to", first.ID)
	current := currentAcceptanceRelease(t, store)
	if current.ID != first.ID {
		t.Fatalf("rollback current release = %s, want %s", current.ID, first.ID)
	}

	runAcceptanceCommand(t, provisionCmd(&options{configPath: path}), "decommission", "production", "--yes")
	if _, err := store.ReadHostFacts("production"); !os.IsNotExist(err) {
		t.Fatalf("host facts should be removed after decommission, err=%v", err)
	}
}

func destructiveLiveHetznerConfigYAML(project, sampleApp string) string {
	location := envOrDefault("SHIP_LIVE_HETZNER_LOCATION", "ash")
	serverType := envOrDefault("SHIP_LIVE_HETZNER_SERVER_TYPE", "cpx11")
	image := envOrDefault("SHIP_LIVE_HETZNER_IMAGE", "ubuntu-24.04")
	return fmt.Sprintf(`project: %s
registry: %s

environments:
  production:
    provider:
      hetzner:
        location: %s
        server_type: %s
        image: %s
        ssh_keys:
          - %s
    hosts:
      pools:
        web:
          count: 2

services:
  web:
    image:
      build: %q
      dockerfile: Dockerfile
      platform: linux/amd64
    command: /app/sample-app server
    pool: web
    scale: 1
    health:
      command: /app/sample-app healthcheck
`, project, strings.TrimSpace(os.Getenv("SHIP_LIVE_HETZNER_REGISTRY")), location, serverType, image, strings.TrimSpace(os.Getenv("SHIP_LIVE_HETZNER_SSH_KEY")), sampleApp)
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
