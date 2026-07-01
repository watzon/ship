package accessory

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/state"
)

func TestValidateDeployRequiresBackupCommandForRequiredBackup(t *testing.T) {
	err := ValidateDeploy(config.Accessory{
		Image:  "postgres:17",
		Pool:   "data",
		Backup: config.BackupSpec{Required: true},
	})
	if err == nil || !strings.Contains(err.Error(), "backup.command") {
		t.Fatalf("expected backup command validation error, got %v", err)
	}
}

func TestValidateRestoreRequiresPrimaryBackupAndRestoreCommand(t *testing.T) {
	acc := config.Accessory{
		Image:   "postgres:17",
		Pool:    "data",
		Primary: true,
		Backup: config.BackupSpec{
			Command:  "pg_dumpall",
			Required: true,
		},
	}
	err := ValidateRestore(acc)
	if err == nil || !strings.Contains(err.Error(), "restore_command") {
		t.Fatalf("expected restore command validation error, got %v", err)
	}
	acc.Backup.RestoreCommand = `psql -f "$SHIP_BACKUP_ARTIFACT"`
	if err := ValidateRestore(acc); err != nil {
		t.Fatalf("restore validation failed: %v", err)
	}
}

func TestEnsurePlacementPersistsStableEligibleHost(t *testing.T) {
	cfg := accessoryConfig()
	env := cfg.Environments["production"]
	store := state.NewStore(t.TempDir())

	first, err := EnsurePlacement(cfg, env, "production", "postgres", store, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if first.Host.Name != "data-a" || !first.Persisted {
		t.Fatalf("first placement = %+v", first)
	}

	env.Hosts.Pools["data"] = config.Pool{Hosts: []string{"data-0", "data-a"}, User: "deploy"}
	second, err := PlacementFor(cfg, env, "production", "postgres", store)
	if err != nil {
		t.Fatal(err)
	}
	if second.Host.Name != "data-a" || !second.Persisted {
		t.Fatalf("placement did not keep persisted host: %+v", second)
	}

	saved, err := store.ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Host.Name != "data-a" || saved.Host.User != "deploy" {
		t.Fatalf("saved placement = %+v", saved)
	}
}

func TestPlacementRefusesPersistedHostThatIsNoLongerEligible(t *testing.T) {
	cfg := accessoryConfig()
	env := cfg.Environments["production"]
	store := state.NewStore(t.TempDir())
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-z", Pool: "data", User: "root"},
		LastBackup:  &state.AccessoryBackup{Artifact: "/backup/old", Host: "data-z", CreatedAt: time.Unix(15, 0)},
		UpdatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}

	_, err := EnsurePlacement(cfg, env, "production", "postgres", store, time.Unix(20, 0))
	if err == nil || !strings.Contains(err.Error(), "failover is not implemented") {
		t.Fatalf("expected stale placement error, got %v", err)
	}
	saved, err := store.ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Host.Name != "data-z" {
		t.Fatalf("saved = %+v", saved)
	}
	if saved.LastBackup == nil || saved.LastBackup.Artifact != "/backup/old" {
		t.Fatalf("last backup was not preserved: %+v", saved.LastBackup)
	}
}

func TestNamedVolumesAndCommands(t *testing.T) {
	acc := config.Accessory{
		Volumes: []string{
			"postgres-data:/var/lib/postgresql/data",
			"/srv/config:/config:ro",
			"postgres-data:/again",
		},
		Backup: config.BackupSpec{
			Command:        "pg_dumpall",
			RestoreCommand: `psql -f "$SHIP_BACKUP_ARTIFACT"`,
			Required:       true,
		},
		Primary: true,
	}
	volumes := NamedVolumes(acc)
	if len(volumes) != 1 || volumes[0] != "postgres-data" {
		t.Fatalf("volumes = %#v", volumes)
	}
	artifact := filepath.Join("/var/lib/ship/backups", "pg.backup")
	backup, err := BackupCommand(acc, artifact)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"mkdir -p", "pg_dumpall", "test -s", "pg.backup"} {
		if !strings.Contains(backup, needle) {
			t.Fatalf("backup command %q missing %q", backup, needle)
		}
	}
	restore, err := RestoreCommand(acc, artifact)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(restore, "SHIP_BACKUP_ARTIFACT='/var/lib/ship/backups/pg.backup'") {
		t.Fatalf("restore command = %q", restore)
	}
}

func TestValidateRestoreArtifactRestrictsToBackupDirAndBackupSuffix(t *testing.T) {
	acc := config.Accessory{}
	artifact := BackupArtifactPath(acc, "production", "postgres", time.Unix(10, 0))
	if _, err := ValidateRestoreArtifact(acc, "production", "postgres", artifact); err != nil {
		t.Fatalf("expected generated backup artifact to validate: %v", err)
	}
	for _, path := range []string{
		filepath.Join(config.RemoteStateDir, "backups", "postgres.backup"),
		filepath.Join(config.RemoteStateDir, "accessories", "production", "postgres", "backups", "postgres.sql"),
		filepath.Join(config.RemoteStateDir, "accessories", "production", "postgres", "backups", "..", "postgres.backup"),
	} {
		if _, err := ValidateRestoreArtifact(acc, "production", "postgres", path); err == nil {
			t.Fatalf("expected restore artifact %q to be rejected", path)
		}
	}
}

func accessoryConfig() *config.Config {
	return &config.Config{
		Project:  "demo",
		Registry: "registry.local/demo",
		Environments: map[string]config.Environment{
			"production": {
				Provider: config.ProviderConfig{Hetzner: &config.HetznerConfig{Location: "ash", ServerType: "cpx31", Image: "ubuntu-24.04"}},
				Hosts: config.HostsConfig{Pools: map[string]config.Pool{
					"data": {Hosts: []string{"data-b", "data-a"}, User: "deploy"},
				}},
			},
		},
		Services: map[string]config.Service{
			"web": {Pool: "data", Scale: 0, Image: config.ImageSpec{Ref: "example/web"}},
		},
		Accessories: map[string]config.Accessory{
			"postgres": {
				Image:   "postgres:17",
				Pool:    "data",
				Primary: true,
				Backup: config.BackupSpec{
					Command:        "pg_dumpall",
					RestoreCommand: `psql -f "$SHIP_BACKUP_ARTIFACT"`,
					Required:       true,
				},
			},
		},
	}
}
