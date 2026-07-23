package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accessorypkg "github.com/watzon/ship/internal/accessory"
	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/state"
)

func TestAccessoryDeployPersistsPlacementAndRunsOneContainer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	var runs []agent.RunContainerParams
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, runs: &runs}
	})

	var out bytes.Buffer
	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"deploy", "production", "postgres"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"agent:data-a:list_ship_containers",
		"agent:data-b:list_ship_containers",
		"agent:data-a:pull:postgres:17",
		"agent:data-a:ensure_volume:postgres-data:999:999",
		"agent:data-a:run:ship_demo_production_accessory_postgres:postgres:17",
		"agent:data-a:list_ship_containers",
		"agent:data-b:list_ship_containers",
	}
	if strings.Join(events, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %#v", runs)
	}
	run := runs[0]
	if run.Labels[docker.LabelAccessory] != "postgres" || run.Labels[docker.LabelProject] != "demo" {
		t.Fatalf("run labels = %+v", run.Labels)
	}
	if run.Labels["com.example.role"] != "database" {
		t.Fatalf("custom labels = %+v", run.Labels)
	}
	if run.Network != "ship-demo-production" {
		t.Fatalf("accessory network = %q", run.Network)
	}
	if strings.Join(run.NetworkAliases, ",") != "database,postgres" {
		t.Fatalf("accessory network aliases = %+v", run.NetworkAliases)
	}
	joinedArgs := strings.Join(run.Args, " ")
	for _, needle := range []string{"-p 5432:5432", "-v postgres-data:/var/lib/postgresql/data", "-e POSTGRES_PASSWORD_FILE=/run/secrets/postgres"} {
		if !strings.Contains(joinedArgs, needle) {
			t.Fatalf("run args %q missing %q", joinedArgs, needle)
		}
	}
	saved, err := state.NewStore(filepath.Join(dir, config.LocalStateDir)).ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Host.Name != "data-a" || saved.Host.User != "deploy" {
		t.Fatalf("saved placement = %+v", saved)
	}
	if !strings.Contains(out.String(), "deployed accessory postgres on data-a") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestAccessoryDeployRestartsCurrentServicesAfterRecreate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployWithAccessoryConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{
		ID:          "release-1",
		Environment: "production",
		Images:      map[string]string{"web": "registry.local/acme/web@sha256:" + strings.Repeat("1", 64)},
		CreatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events}
	})

	var out bytes.Buffer
	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"deploy", "production", "redis"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(events, "\n")
	for _, needle := range []string{
		"agent:web-1:run:ship_demo_production_accessory_redis:redis:7-alpine",
		"agent:web-1:run:ship_demo_production_web_1_release-1:registry.local/acme/web@sha256:",
	} {
		if !strings.Contains(joined, needle) {
			t.Fatalf("events missing %q:\n%s", needle, joined)
		}
	}
	if !strings.Contains(out.String(), "restarted ship_demo_production_web_1_release-1 on web-1 after accessory change") {
		t.Fatalf("output missing dependent restart:\n%s", out.String())
	}
}

func TestAccessoryDeployRestartsRemoteCurrentReleaseWhenLocalStateIsStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployWithAccessoryConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{
		ID:          "release-old",
		Environment: "production",
		Images:      map[string]string{"web": "registry.local/acme/web@sha256:" + strings.Repeat("0", 64)},
		CreatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}
	remote := state.Release{
		ID:          "release-new",
		Environment: "production",
		Images:      map[string]string{"web": "registry.local/acme/web@sha256:" + strings.Repeat("1", 64)},
		CreatedAt:   time.Unix(20, 0),
		Healthy:     true,
		Status:      state.ReleaseStatusHealthy,
	}
	observed := map[string][]docker.ContainerSummary{
		"web-1": {serviceContainer("web-1", "web", 1, "release-new", "Up 10 seconds")},
	}

	var events []string
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: observed, current: &remote}
	})

	var out bytes.Buffer
	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"deploy", "production", "redis"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "agent:web-1:read_release_state") {
		t.Fatalf("remote release state was not read:\n%s", joined)
	}
	if !strings.Contains(joined, "agent:web-1:run:ship_demo_production_web_1_release-new:registry.local/acme/web@sha256:") {
		t.Fatalf("dependent restart did not use remote current release:\n%s", joined)
	}
	if strings.Contains(joined, "ship_demo_production_web_1_release-old") || strings.Contains(out.String(), "release-old") {
		t.Fatalf("accessory deploy used stale local release:\nevents=%s\nout=%s", joined, out.String())
	}
}

func TestAccessoryExecUsesPersistedPlacementAndJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy", PublicAddress: "data-a"},
		UpdatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}
	containerName := accessorypkg.ContainerName("demo", "production", "postgres")
	observed := map[string][]docker.ContainerSummary{
		"data-a": {
			{
				Names: containerName,
				Labels: map[string]string{
					docker.LabelManagedBy:   docker.LabelManagedByValue,
					docker.LabelProject:     "demo",
					docker.LabelEnvironment: "production",
					docker.LabelAccessory:   "postgres",
				},
			},
		},
	}

	var events []string
	var execCalls []agent.ExecContainerParams
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: observed, execCalls: &execCalls}
	})

	var out bytes.Buffer
	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"exec", "production", "postgres", "--timeout", "15", "--json", "--", "psql", "-c", "select 1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(execCalls) != 1 || execCalls[0].Name != containerName || execCalls[0].Command != "psql -c select 1" || execCalls[0].TimeoutSeconds != 15 {
		t.Fatalf("exec calls = %+v", execCalls)
	}
	want := []string{
		"agent:data-a:list_ship_containers",
		"agent:data-b:list_ship_containers",
		"agent:data-a:exec:" + containerName + ":psql -c select 1",
	}
	if strings.Join(events, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	var view execView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Accessory != "postgres" || view.Service != "" || len(view.Entries) != 1 || view.Entries[0].Host != "data-a" || view.Entries[0].Accessory != "postgres" || view.Entries[0].Output != "exec complete" {
		t.Fatalf("exec view = %+v", view)
	}
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "accessory_exec", "started", "") || !timelineContains(timeline, "accessory_exec", "succeeded", "") {
		t.Fatalf("timeline missing accessory exec events: %+v", timeline)
	}
}

func TestAccessoryLogsUsePersistedPlacementFollowAndJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy", PublicAddress: "data-a"},
		UpdatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}
	containerName := accessorypkg.ContainerName("demo", "production", "postgres")
	observed := map[string][]docker.ContainerSummary{
		"data-a": {
			{
				Names: containerName,
				Labels: map[string]string{
					docker.LabelManagedBy:   docker.LabelManagedByValue,
					docker.LabelProject:     "demo",
					docker.LabelEnvironment: "production",
					docker.LabelAccessory:   "postgres",
				},
			},
		},
	}
	originalInterval := logsFollowInterval
	logsFollowInterval = time.Nanosecond
	t.Cleanup(func() {
		logsFollowInterval = originalInterval
	})

	var events []string
	var logCalls []agent.LogsParams
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: observed, logCalls: &logCalls}
	})

	var out bytes.Buffer
	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"logs", "production", "postgres", "--lines", "25", "--follow", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(logCalls) != logsFollowPolls {
		t.Fatalf("log calls = %+v", logCalls)
	}
	for _, call := range logCalls {
		if call.Name != containerName || call.Lines != 25 {
			t.Fatalf("log call = %+v, want name=%s lines=25", call, containerName)
		}
	}
	wantPrefix := []string{
		"agent:data-a:list_ship_containers",
		"agent:data-b:list_ship_containers",
		"agent:data-a:logs:" + containerName + ":25",
	}
	joined := strings.Join(events, "\n")
	for _, needle := range wantPrefix {
		if !strings.Contains(joined, needle) {
			t.Fatalf("events missing %q: %#v", needle, events)
		}
	}
	var view logsView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Follow || view.Accessory != "postgres" || view.Service != "" || len(view.Entries) != logsFollowPolls {
		t.Fatalf("logs view = %+v", view)
	}
	if view.Entries[0].Host != "data-a" || view.Entries[0].Accessory != "postgres" || view.Entries[0].Logs != "logs from data-a" {
		t.Fatalf("logs entries = %+v", view.Entries)
	}
}

func TestAccessoryDeployWritesRemoteSecretFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessorySecretConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	secretValue := "postgres-secret"
	t.Setenv("SHIP_TEST_POSTGRES_PASSWORD", secretValue)

	var events []string
	var writes []agent.WriteFileParams
	var runs []agent.RunContainerParams
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, writes: &writes, runs: &runs}
	})

	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetArgs([]string{"deploy", "production", "postgres"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	secretPath := "/var/lib/ship/secrets/production/accessory-postgres.env"
	want := []string{
		"agent:data-a:list_ship_containers",
		"agent:data-b:list_ship_containers",
		"agent:data-a:write_file:" + secretPath + ":0600",
		"agent:data-a:pull:postgres:17",
		"agent:data-a:ensure_volume:postgres-data:999:999",
		"agent:data-a:run:ship_demo_production_accessory_postgres:postgres:17",
		"agent:data-a:list_ship_containers",
		"agent:data-b:list_ship_containers",
	}
	if strings.Join(events, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	if len(writes) != 1 {
		t.Fatalf("writes = %#v", writes)
	}
	if writes[0].Path != secretPath || writes[0].Mode != 0o600 || writes[0].Content != "SHIP_TEST_POSTGRES_PASSWORD="+secretValue+"\n" {
		t.Fatalf("write params = %+v", writes[0])
	}
	if len(runs) != 1 || !strings.Contains(strings.Join(runs[0].Args, " "), "--env-file "+secretPath) {
		t.Fatalf("runs = %#v", runs)
	}
}

func TestAccessoryDeployRefusesExistingReplicaOnDifferentHost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
		UpdatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"data-b": {accessoryContainer("postgres", "data-b", "Up 1 second")},
	}
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: observed}
	})

	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetArgs([]string{"deploy", "production", "postgres"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "already has a managed container on data-b") {
		t.Fatalf("expected replica guard error, got %v", err)
	}
	for _, event := range events {
		if strings.Contains(event, ":pull:") || strings.Contains(event, ":run:") {
			t.Fatalf("mutated after replica guard: %#v", events)
		}
	}
}

func TestAccessoryStatusReportsPlacementAndObservedContainer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
		UpdatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"data-a": {accessoryContainer("postgres", "data-a", "Up 5 seconds")},
	}
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: observed}
	})

	var out bytes.Buffer
	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"status", "production", "postgres"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"postgres", "data-a", "Up 5 seconds"} {
		if !strings.Contains(out.String(), needle) {
			t.Fatalf("status output missing %q:\n%s", needle, out.String())
		}
	}
}

func TestAccessoryBackupRunsOnPersistedPlacementAndRecordsArtifact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
		UpdatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"data-a": {accessoryContainer("postgres", "data-a", "Up 5 seconds")},
	}
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: observed}
	})

	var out bytes.Buffer
	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"backup", "production", "postgres"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 ||
		events[0] != "agent:data-a:list_ship_containers" ||
		events[1] != "agent:data-b:list_ship_containers" ||
		!strings.HasPrefix(events[2], "agent:data-a:accessory_backup:postgres:") {
		t.Fatalf("events = %#v", events)
	}
	saved, err := store.ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if saved.LastBackup == nil || !strings.Contains(saved.LastBackup.Artifact, "/var/lib/ship/backups/postgres-") {
		t.Fatalf("saved backup = %+v", saved.LastBackup)
	}
	if !strings.Contains(out.String(), "backed up accessory postgres on data-a") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestAccessoryBackupExportsArtifactWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryExportConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
		UpdatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"data-a": {accessoryContainer("postgres", "data-a", "Up 5 seconds")},
	}
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: observed}
	})

	var out bytes.Buffer
	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"backup", "production", "postgres"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || !strings.Contains(events[3], `export SHIP_BACKUP_ARTIFACT`) || !strings.Contains(events[3], `printf "s3://ship/%s`) {
		t.Fatalf("events = %#v", events)
	}
	saved, err := store.ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if saved.LastBackup == nil || saved.LastBackup.ExportedArtifact != "s3://ship/postgres.backup" || !strings.Contains(saved.LastBackup.ExportOutput, "s3://ship/postgres.backup") {
		t.Fatalf("saved backup = %+v", saved.LastBackup)
	}
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "accessory_backup_export", "succeeded", "") {
		t.Fatalf("timeline missing backup export success: %+v", timeline)
	}
	if !strings.Contains(out.String(), "exported=s3://ship/postgres.backup") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestAccessoryBackupRefusesStaleObservedTopology(t *testing.T) {
	tests := []struct {
		name     string
		observed map[string][]docker.ContainerSummary
		want     string
	}{
		{
			name: "different host",
			observed: map[string][]docker.ContainerSummary{
				"data-b": {accessoryContainer("postgres", "data-b", "Up 1 second")},
			},
			want: "already has a managed container on data-b",
		},
		{
			name: "multiple replicas",
			observed: map[string][]docker.ContainerSummary{
				"data-a": {
					accessoryContainer("postgres", "data-a", "Up 1 second"),
					accessoryContainer("postgres", "data-a", "Up 2 seconds"),
				},
			},
			want: "multiple managed containers",
		},
		{
			name: "container name mismatch",
			observed: map[string][]docker.ContainerSummary{
				"data-a": {func() docker.ContainerSummary {
					container := accessoryContainer("postgres", "data-a", "Up 1 second")
					container.Names = "ship_demo_production_accessory_postgres_old"
					return container
				}()},
			},
			want: "expected ship_demo_production_accessory_postgres",
		},
		{
			name:     "missing saved host container",
			observed: map[string][]docker.ContainerSummary{},
			want:     "no managed container on saved placement host data-a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, config.DefaultConfigFile)
			if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)
			store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
			if err := store.SaveAccessoryState(state.AccessoryState{
				Environment: "production",
				Name:        "postgres",
				Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
				UpdatedAt:   time.Unix(10, 0),
			}); err != nil {
				t.Fatal(err)
			}

			var events []string
			installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
				return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: tt.observed}
			})

			cmd := accessoryCmd(&options{configPath: path})
			cmd.SetArgs([]string{"backup", "production", "postgres"})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %q error, got %v", tt.want, err)
			}
			for _, event := range events {
				if strings.Contains(event, ":accessory_backup:") {
					t.Fatalf("mutated after topology guard: %#v", events)
				}
			}
			saved, err := store.ReadAccessoryState("production", "postgres")
			if err != nil {
				t.Fatal(err)
			}
			if saved.LastBackup != nil {
				t.Fatalf("recorded backup after topology guard: %+v", saved.LastBackup)
			}
		})
	}
}

func TestAccessoryRestoreRequiresConfirmationAndDryRunDoesNotMutate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
		UpdatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"data-a": {accessoryContainer("postgres", "data-a", "Up 5 seconds")},
	}
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: observed}
	})

	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetArgs([]string{"restore", "production", "postgres", "--artifact", "/var/lib/ship/backups/postgres.backup"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("expected confirmation error, got %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("restore without confirmation touched agent: %#v", events)
	}

	var out bytes.Buffer
	cmd = accessoryCmd(&options{configPath: path, dryRun: true})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"restore", "production", "postgres", "--artifact", "/var/lib/ship/backups/postgres.backup"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("dry-run touched agent: %#v", events)
	}
	saved, err := store.ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if saved.LastRestore != nil {
		t.Fatalf("dry-run recorded restore: %+v", saved.LastRestore)
	}
	if !strings.Contains(out.String(), "would restore accessory postgres on data-a") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestAccessoryRestoreChecksArtifactAndRecordsRestore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
		UpdatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"data-a": {accessoryContainer("postgres", "data-a", "Up 5 seconds")},
	}
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: observed}
	})

	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetArgs([]string{"restore", "production", "postgres", "--artifact", "/var/lib/ship/backups/postgres.backup", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"agent:data-a:list_ship_containers",
		"agent:data-b:list_ship_containers",
		"agent:data-a:health_check:test -s '/var/lib/ship/backups/postgres.backup'",
		"agent:data-a:accessory_restore:postgres:SHIP_BACKUP_ARTIFACT='/var/lib/ship/backups/postgres.backup' psql -f \"$SHIP_BACKUP_ARTIFACT\"",
	}
	if strings.Join(events, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	saved, err := store.ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if saved.LastRestore == nil || saved.LastRestore.Artifact != "/var/lib/ship/backups/postgres.backup" {
		t.Fatalf("saved restore = %+v", saved.LastRestore)
	}
}

func TestAccessoryRestoreRefusesBadTopologyBeforeRemoteCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
		UpdatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"data-b": {accessoryContainer("postgres", "data-b", "Up 1 second")},
	}
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: observed}
	})

	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetArgs([]string{"restore", "production", "postgres", "--artifact", "/var/lib/ship/backups/postgres.backup", "--yes"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "already has a managed container on data-b") {
		t.Fatalf("expected topology error, got %v", err)
	}
	for _, event := range events {
		if strings.Contains(event, ":health_check:") || strings.Contains(event, ":accessory_restore:") {
			t.Fatalf("remote restore check/mutation happened after topology guard: %#v", events)
		}
	}
	saved, err := store.ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if saved.LastRestore != nil {
		t.Fatalf("recorded restore after topology guard: %+v", saved.LastRestore)
	}
}

func TestAccessoryRestoreRejectsArtifactOutsideBackupDirBeforeRemoteCheck(t *testing.T) {
	tests := []struct {
		name     string
		artifact string
		want     string
	}{
		{
			name:     "outside configured directory",
			artifact: "/tmp/postgres.backup",
			want:     "must be within backup artifact directory",
		},
		{
			name:     "wrong suffix",
			artifact: "/var/lib/ship/backups/postgres.sql",
			want:     "must be a .backup file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, config.DefaultConfigFile)
			if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)
			store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
			if err := store.SaveAccessoryState(state.AccessoryState{
				Environment: "production",
				Name:        "postgres",
				Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
				UpdatedAt:   time.Unix(10, 0),
			}); err != nil {
				t.Fatal(err)
			}

			var events []string
			installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
				return &scriptedAccessoryAgent{host: host.Name, events: &events}
			})

			cmd := accessoryCmd(&options{configPath: path})
			cmd.SetArgs([]string{"restore", "production", "postgres", "--artifact", tt.artifact, "--yes"})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %q error, got %v", tt.want, err)
			}
			if len(events) != 0 {
				t.Fatalf("artifact validation touched agent: %#v", events)
			}
		})
	}
}

func TestAccessoryFailoverRestoresOnTargetStopsOldAndPersistsPlacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	backup := state.AccessoryBackup{
		Artifact:  "/var/lib/ship/backups/postgres.backup",
		Host:      "data-a",
		CreatedAt: time.Unix(20, 0),
	}
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
		UpdatedAt:   time.Unix(20, 0),
		LastBackup:  &backup,
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var runs []agent.RunContainerParams
	observed := map[string][]docker.ContainerSummary{
		"data-a": {accessoryContainer("postgres", "data-a", "Up 5 seconds")},
	}
	installAccessoryHooks(t, func(host scheduler.Host) deployAgent {
		return &scriptedAccessoryAgent{host: host.Name, events: &events, observed: observed, runs: &runs}
	})

	var out bytes.Buffer
	cmd := accessoryCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"failover", "production", "postgres", "--to", "data-b", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"agent:data-a:list_ship_containers",
		"agent:data-b:list_ship_containers",
		"agent:data-b:pull:postgres:17",
		"agent:data-b:ensure_volume:postgres-data:999:999",
		"agent:data-b:run:ship_demo_production_accessory_postgres:postgres:17",
		"agent:data-b:health_check:test -s '/var/lib/ship/backups/postgres.backup'",
		"agent:data-b:accessory_restore:postgres:SHIP_BACKUP_ARTIFACT='/var/lib/ship/backups/postgres.backup' psql -f \"$SHIP_BACKUP_ARTIFACT\"",
		"agent:data-a:stop_container",
	}
	if strings.Join(events, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	if len(runs) != 1 || runs[0].Labels[docker.LabelAccessory] != "postgres" {
		t.Fatalf("run params = %+v", runs)
	}
	saved, err := store.ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Host.Name != "data-b" {
		t.Fatalf("saved host = %+v", saved.Host)
	}
	if saved.LastBackup == nil || saved.LastBackup.Artifact != backup.Artifact {
		t.Fatalf("last backup not preserved: %+v", saved.LastBackup)
	}
	if saved.LastRestore == nil || saved.LastRestore.Host != "data-b" || saved.LastRestore.Artifact != backup.Artifact {
		t.Fatalf("last restore = %+v", saved.LastRestore)
	}
	if !strings.Contains(out.String(), "failed over accessory postgres from data-a to data-b") {
		t.Fatalf("output = %q", out.String())
	}
}

func accessoryConfigYAML() string {
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
          user: deploy
          hosts:
            - data-b
            - data-a

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
    labels:
      com.example.role: database
    network_aliases:
      - database
    volume_owner: "999:999"
    volumes:
      - postgres-data:/var/lib/postgresql/data
    ports: [5432]
    env:
      - POSTGRES_PASSWORD_FILE=/run/secrets/postgres
    backup:
      command: pg_dumpall
      restore_command: 'psql -f "$SHIP_BACKUP_ARTIFACT"'
      artifact_dir: /var/lib/ship/backups
      required: true
      restore_check: true
`
}

func accessoryExportConfigYAML() string {
	return strings.Replace(accessoryConfigYAML(), "restore_check: true\n", "restore_check: true\n      export_command: 'printf \"s3://ship/%s\\\\n\" \"$(basename \"$SHIP_BACKUP_ARTIFACT\")\"'\n      export_timeout_seconds: 45\n", 1)
}

func accessorySecretConfigYAML() string {
	return strings.Replace(accessoryConfigYAML(), "primary: true\n", "primary: true\n    secrets:\n      - SHIP_TEST_POSTGRES_PASSWORD\n", 1) + `
secrets:
  - SHIP_TEST_POSTGRES_PASSWORD
`
}

func installAccessoryHooks(t *testing.T, agentFactory func(scheduler.Host) deployAgent) {
	t.Helper()
	originalAgent := newDeployAgent
	originalNow := deployNow
	newDeployAgent = agentFactory
	deployNow = func() time.Time {
		return time.Date(2026, 6, 30, 12, 34, 56, 123456789, time.FixedZone("MDT", -6*60*60))
	}
	t.Cleanup(func() {
		newDeployAgent = originalAgent
		deployNow = originalNow
	})
}

type scriptedAccessoryAgent struct {
	host       string
	events     *[]string
	observed   map[string][]docker.ContainerSummary
	current    *state.Release
	runs       *[]agent.RunContainerParams
	writes     *[]agent.WriteFileParams
	logCalls   *[]agent.LogsParams
	execCalls  *[]agent.ExecContainerParams
	failMethod string
}

func (s *scriptedAccessoryAgent) Call(ctx context.Context, method string, params any, out any) error {
	switch method {
	case "list_ship_containers":
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:list_ship_containers", s.host))
		if containers, ok := out.(*[]docker.ContainerSummary); ok {
			*containers = append([]docker.ContainerSummary(nil), s.observed[s.host]...)
		}
	case "read_release_state":
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:read_release_state", s.host))
		if s.current != nil {
			if release, ok := out.(*state.Release); ok {
				*release = *s.current
			}
		}
	case "pull":
		image := params.(map[string]string)["image"]
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:pull:%s", s.host, image))
	case "ensure_volume":
		p := params.(agent.EnsureVolumeParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:ensure_volume:%s:%s", s.host, p.Name, p.Owner))
	case "ensure_network":
		return nil
	case "write_file":
		p := params.(agent.WriteFileParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:write_file:%s:%04o", s.host, p.Path, p.Mode))
		if s.writes != nil {
			*s.writes = append(*s.writes, p)
		}
	case "logs":
		p := params.(agent.LogsParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:logs:%s:%d", s.host, p.Name, p.Lines))
		if s.logCalls != nil {
			*s.logCalls = append(*s.logCalls, p)
		}
		if result, ok := out.(*map[string]string); ok {
			*result = map[string]string{"logs": "logs from " + s.host}
		}
	case "run_container":
		p := params.(agent.RunContainerParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:run:%s:%s", s.host, p.Name, p.Image))
		if s.runs != nil {
			*s.runs = append(*s.runs, p)
		}
	case "accessory_backup":
		p := params.(agent.AccessoryCommandParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:accessory_backup:%s:%s", s.host, p.Name, p.Command))
		if result, ok := out.(*agent.CommandResult); ok {
			output := "backup complete"
			if strings.Contains(p.Command, "SHIP_BACKUP_ARTIFACT=") && strings.Contains(p.Command, "s3://ship") {
				output = "s3://ship/postgres.backup\nexport complete"
			}
			*result = agent.CommandResult{Output: output}
		}
	case "health_check":
		p := params.(agent.HealthCheckParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:health_check:%s", s.host, p.Command))
		if result, ok := out.(*agent.HealthCheckResult); ok {
			*result = agent.HealthCheckResult{OK: true}
		}
	case "accessory_restore":
		p := params.(agent.AccessoryCommandParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:accessory_restore:%s:%s", s.host, p.Name, p.Command))
		if result, ok := out.(*agent.CommandResult); ok {
			*result = agent.CommandResult{Output: "restore complete"}
		}
	case "exec_container":
		p := params.(agent.ExecContainerParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:exec:%s:%s", s.host, p.Name, p.Command))
		if s.execCalls != nil {
			*s.execCalls = append(*s.execCalls, p)
		}
		if result, ok := out.(*agent.CommandResult); ok {
			*result = agent.CommandResult{Output: "exec complete"}
		}
	default:
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:%s", s.host, method))
	}
	if method == s.failMethod {
		return fmt.Errorf("%s failed", method)
	}
	return nil
}
