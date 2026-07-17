package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/deployment"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/state"
)

func TestDeploySyncsServiceSchedulesToCurrentReleaseContainers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(scheduledDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	var syncs []agent.SyncCronFilesParams
	imageRef := "registry.local/acme/web:stable"
	digestRef := "registry.local/acme/web@sha256:" + strings.Repeat("4", 64)
	installDeployHooks(t, &recordingDeployDocker{events: &events, resolved: map[string]string{imageRef: digestRef}}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events, cronSyncs: &syncs}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	if len(syncs) != 2 {
		t.Fatalf("cron syncs = %#v", syncs)
	}
	var scheduled agent.CronFile
	for _, sync := range syncs {
		if sync.Prefix != "ship-demo-production-" {
			t.Fatalf("prefix = %q", sync.Prefix)
		}
		if len(sync.Files) == 1 {
			scheduled = sync.Files[0]
		}
	}
	if scheduled.Name != "ship-demo-production-web-cleanup" {
		t.Fatalf("scheduled file = %+v", scheduled)
	}
	releaseID := "abc123def456-20260630T183456.123456789Z"
	container := deployment.ContainerName("demo", "production", "web", 2, releaseID)
	for _, needle := range []string{
		"17 * * * * root timeout 300s docker exec '" + container + "' sh -lc 'bin/rails cleanup'",
		">> '/var/log/ship-demo-production-web-cleanup.log' 2>&1",
	} {
		if !strings.Contains(scheduled.Content, needle) {
			t.Fatalf("cron content missing %q:\n%s", needle, scheduled.Content)
		}
	}
	joined := strings.Join(events, "\n")
	scheduleAt := strings.Index(joined, "agent:web-2:sync_cron_files:ship-demo-production-:1")
	healthyAt := strings.Index(joined, "agent:web-2:write_release_state:"+releaseID+":healthy")
	if scheduleAt < 0 || healthyAt < 0 || scheduleAt > healthyAt {
		t.Fatalf("schedule sync did not precede healthy release state:\n%s", joined)
	}
}

func TestDeploySyncsAccessoryBackupSchedules(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(scheduledAccessoryBackupConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-1", Pool: "data", User: "root"},
		UpdatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var syncs []agent.SyncCronFilesParams
	imageRef := "registry.local/acme/web:stable"
	digestRef := "registry.local/acme/web@sha256:" + strings.Repeat("5", 64)
	installDeployHooks(t, &recordingDeployDocker{events: &events, resolved: map[string]string{imageRef: digestRef}}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events, cronSyncs: &syncs}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	if len(syncs) != 1 {
		t.Fatalf("cron syncs = %#v", syncs)
	}
	if syncs[0].Prefix != "ship-demo-production-" || len(syncs[0].Files) != 1 {
		t.Fatalf("cron sync = %+v", syncs[0])
	}
	scheduled := syncs[0].Files[0]
	if scheduled.Name != "ship-demo-production-accessory-postgres-backup" {
		t.Fatalf("scheduled file = %+v", scheduled)
	}
	for _, needle := range []string{
		"13 3 * * * root",
		"timeout 600s",
		"artifact=\"$artifact_dir/postgres-$(date -u +\\%Y\\%m\\%dT\\%H\\%M\\%S.000000000Z).backup\"",
		"pg_dumpall",
		`SHIP_BACKUP_ARTIFACT="$artifact"; export SHIP_BACKUP_ARTIFACT; printf "s3://ship/\%s\n" "$(basename "$SHIP_BACKUP_ARTIFACT")"`,
		"/var/log/ship-demo-production-accessory-postgres-backup.log",
	} {
		if !strings.Contains(scheduled.Content, needle) {
			t.Fatalf("accessory backup schedule missing %q:\n%s", needle, scheduled.Content)
		}
	}
}

func scheduledDeployConfig() string {
	return `project: demo
registry: registry.local/acme/demo

environments:
  production:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        web:
          count: 2

services:
  web:
    image:
      ref: registry.local/acme/web:stable
    command: ./bin/server
    pool: web
    scale: 2
    schedules:
      cleanup:
        cron: "17 * * * *"
        command: bin/rails cleanup
        replica: 2
        timeout_seconds: 300
`
}

func scheduledAccessoryBackupConfig() string {
	return `project: demo
registry: registry.local/acme/demo

environments:
  production:
    provider:
      hetzner:
        location: ash
        server_type: cpx31
        image: ubuntu-24.04
        ssh_allowed_cidrs: [0.0.0.0/0]
    hosts:
      pools:
        data:
          count: 1

services:
  web:
    image:
      ref: registry.local/acme/web:stable
    pool: data
    scale: 0

accessories:
  postgres:
    image: postgres:17
    pool: data
    primary: true
    backup:
      command: pg_dumpall
      export_command: 'printf "s3://ship/%s\n" "$(basename "$SHIP_BACKUP_ARTIFACT")"'
      required: true
      schedule:
        cron: "13 3 * * *"
        timeout_seconds: 600
`
}
