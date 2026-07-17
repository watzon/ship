package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/scheduler"
	secretspkg "github.com/watzon/ship/internal/secrets"
	"github.com/watzon/ship/internal/state"
)

func TestRollbackAppliesTargetRelease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old-image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{ID: "current-release", Environment: "production", Images: map[string]string{"web": "current-image"}, CreatedAt: time.Unix(20, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var releaseWrites []releaseStateWrite
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &scriptedDeployAgent{host: host.Name, events: &events, releaseWrites: &releaseWrites}
	})

	cmd := rollbackCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "old-release" {
		t.Fatalf("current = %+v", current)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "agent:web-1:pull:old-image") || !strings.Contains(joined, "agent:web-1:health") {
		t.Fatalf("rollback events = %#v", events)
	}
	if len(releaseWrites) != 1 || releaseWrites[0].Host != "web-1" || releaseWrites[0].Release.ID != "old-release" || releaseWrites[0].Release.Status != state.ReleaseStatusHealthy {
		t.Fatalf("rollback release writes = %+v", releaseWrites)
	}
}

func TestRollbackRefusesAlreadyCurrentTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old-image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{ID: "current-release", Environment: "production", Images: map[string]string{"web": "current-image"}, CreatedAt: time.Unix(20, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &scriptedDeployAgent{host: host.Name, events: &events}
	})

	cmd := rollbackCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production", "--to", "current-release"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "already current") {
		t.Fatalf("expected already-current error, got %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("no-op rollback touched agents: %#v", events)
	}
}

func TestRollbackRefusesConcurrentOperationBeforeMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old-image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{ID: "current-release", Environment: "production", Images: map[string]string{"web": "current-image"}, CreatedAt: time.Unix(20, 0)}); err != nil {
		t.Fatal(err)
	}
	lock, err := store.AcquireOperationLock("production", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()

	var events []string
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &scriptedDeployAgent{host: host.Name, events: &events}
	})

	cmd := rollbackCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err = cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "already busy") {
		t.Fatalf("expected busy operation error, got %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("busy rollback touched agents: %#v", events)
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "current-release" {
		t.Fatalf("current = %+v", current)
	}
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "rollback", "blocked", "old-release") {
		t.Fatalf("timeline missing blocked rollback: %+v", timeline)
	}
}

func TestRollbackRemoteReleaseStateWriteReportsPartialFailures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old-image", "worker": "worker-image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{ID: "current-release", Environment: "production", Images: map[string]string{"web": "current-image", "worker": "worker-image"}, CreatedAt: time.Unix(20, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		failMethod := ""
		if host.Name == "web-2" {
			failMethod = "write_release_state"
		}
		return &scriptedDeployAgent{host: host.Name, events: &events, failMethod: failMethod}
	})

	cmd := rollbackCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "failed on 1/2 hosts") || !strings.Contains(err.Error(), "web-2") {
		t.Fatalf("expected partial release-state write error, got %v", err)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "agent:web-1:write_release_state:old-release:healthy") ||
		!strings.Contains(joined, "agent:web-2:write_release_state:old-release:healthy") {
		t.Fatalf("release state was not attempted on every host:\n%s", joined)
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "current-release" {
		t.Fatalf("current release changed after partial remote write failure: %+v", current)
	}
}

func TestRollbackWritesRemoteSecretFileAndUsesEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(secretDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	secretValue := "postgres://user:pass@example/db"
	t.Setenv("SHIP_TEST_DATABASE_URL", secretValue)

	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{
		ID:            "old-release",
		Environment:   "production",
		Images:        map[string]string{"web": "old-image"},
		SecretDigests: map[string]string{"SHIP_TEST_DATABASE_URL": secretspkg.Digest(secretValue)},
		CreatedAt:     time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{
		ID:            "current-release",
		Environment:   "production",
		Images:        map[string]string{"web": "current-image"},
		SecretDigests: map[string]string{"SHIP_TEST_DATABASE_URL": secretspkg.Digest(secretValue)},
		CreatedAt:     time.Unix(20, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var writes []agent.WriteFileParams
	var runs []agent.RunContainerParams
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &secretDeployAgent{host: host.Name, events: &events, writes: &writes, runs: &runs}
	})

	cmd := rollbackCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	secretPath := "/var/lib/ship/secrets/production/service-web.env"
	wantEvents := []string{
		"agent:web-1:write_file:" + secretPath + ":0600",
		"agent:web-1:list_ship_containers",
		"agent:web-1:pull:old-image",
		"agent:web-1:run:ship_demo_production_web_1_old-release",
		"agent:web-1:write_release_state:old-release:healthy",
	}
	if strings.Join(events, "\n") != strings.Join(wantEvents, "\n") {
		t.Fatalf("events = %#v, want %#v", events, wantEvents)
	}
	if len(writes) != 1 || writes[0].Path != secretPath || writes[0].Mode != 0o600 {
		t.Fatalf("writes = %#v", writes)
	}
	if writes[0].Content != "SHIP_TEST_DATABASE_URL="+secretValue+"\n" {
		t.Fatalf("secret file content = %q", writes[0].Content)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %#v", runs)
	}
	joinedArgs := strings.Join(runs[0].Args, " ")
	if !strings.Contains(joinedArgs, "--env-file "+secretPath) {
		t.Fatalf("run args %q missing env file %q", joinedArgs, secretPath)
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "old-release" {
		t.Fatalf("current = %+v", current)
	}
}

func TestRollbackBlocksSecretDriftUnlessAllowed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(secretDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	currentSecret := "postgres://new"
	t.Setenv("SHIP_TEST_DATABASE_URL", currentSecret)

	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{
		ID:          "old-release",
		Environment: "production",
		Images:      map[string]string{"web": "old-image"},
		SecretDigests: map[string]string{
			"service-web:SHIP_TEST_DATABASE_URL": secretspkg.Digest("postgres://old"),
		},
		CreatedAt: time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{
		ID:          "current-release",
		Environment: "production",
		Images:      map[string]string{"web": "current-image"},
		SecretDigests: map[string]string{
			"service-web:SHIP_TEST_DATABASE_URL": secretspkg.Digest(currentSecret),
		},
		CreatedAt: time.Unix(20, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var writes []agent.WriteFileParams
	var runs []agent.RunContainerParams
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &secretDeployAgent{host: host.Name, events: &events, writes: &writes, runs: &runs}
	})

	cmd := rollbackCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "rollback secret drift detected") || !strings.Contains(err.Error(), "changed service-web:SHIP_TEST_DATABASE_URL") {
		t.Fatalf("expected secret drift error, got %v", err)
	}
	if len(events) != 0 || len(writes) != 0 || len(runs) != 0 {
		t.Fatalf("blocked rollback touched agents: events=%#v writes=%#v runs=%#v", events, writes, runs)
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "current-release" {
		t.Fatalf("current changed after blocked rollback: %+v", current)
	}
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "rollback", "blocked", "old-release") {
		t.Fatalf("timeline missing blocked rollback: %+v", timeline)
	}

	cmd = rollbackCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production", "--allow-secret-drift"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 || !strings.Contains(writes[0].Content, currentSecret) {
		t.Fatalf("allowed rollback writes = %#v", writes)
	}
	current, err = store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "old-release" {
		t.Fatalf("current after allowed rollback = %+v", current)
	}
}

func TestRollbackMissingSecretsStopBeforeAgentMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(secretDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	unsetEnv(t, "SHIP_TEST_DATABASE_URL")

	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{
		ID:          "old-release",
		Environment: "production",
		Images:      map[string]string{"web": "old-image"},
		CreatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{
		ID:          "current-release",
		Environment: "production",
		Images:      map[string]string{"web": "current-image"},
		CreatedAt:   time.Unix(20, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &secretDeployAgent{host: host.Name, events: &events}
	})

	cmd := rollbackCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "missing secrets: SHIP_TEST_DATABASE_URL") {
		t.Fatalf("expected missing secret error, got %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("missing secret rollback touched agent: %#v", events)
	}
}

func TestRollbackFailurePreservesCurrentAndRecordsAttempt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old-image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{ID: "current-release", Environment: "production", Images: map[string]string{"web": "current-image"}, CreatedAt: time.Unix(20, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var releaseWrites []releaseStateWrite
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &scriptedDeployAgent{host: host.Name, events: &events, failMethod: "health_check", releaseWrites: &releaseWrites}
	})

	cmd := rollbackCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected rollback failure")
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "current-release" {
		t.Fatalf("current = %+v", current)
	}
	target, err := store.ReadRelease("old-release")
	if err != nil {
		t.Fatal(err)
	}
	if target.Status != state.ReleaseStatusHealthy || !target.Healthy {
		t.Fatalf("target should remain healthy, got %+v", target)
	}
	if len(releaseWrites) != 0 {
		t.Fatalf("failed rollback wrote remote release state: %+v", releaseWrites)
	}
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "rollback_rollout", "failed", "old-release") || !timelineContains(timeline, "rollback_attempt", "failed", "old-release") || !timelineContains(timeline, "rollback", "failed", "old-release") {
		t.Fatalf("timeline = %+v", timeline)
	}
}

func TestRollbackBlocksUnsafeFixedPortReplacementBeforeMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old-image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{ID: "current-release", Environment: "production", Images: map[string]string{"web": "current-image"}, CreatedAt: time.Unix(20, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var releaseWrites []releaseStateWrite
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &scriptedDeployAgent{
			host:          host.Name,
			events:        &events,
			observed:      []docker.ContainerSummary{serviceContainer(host.Name, "web", 1, "current-release", "Up 2 minutes")},
			failMethod:    "health_check",
			releaseWrites: &releaseWrites,
		}
	})

	cmd := rollbackCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected unsafe fixed-port rollback failure")
	}
	for _, needle := range []string{"unsafe fixed-port rollback", "ship_demo_production_web_1_current-release", "before ship_demo_production_web_1_old-release is healthy"} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("error missing %q: %v", needle, err)
		}
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "current-release" {
		t.Fatalf("current = %+v", current)
	}
	if len(releaseWrites) != 0 {
		t.Fatalf("blocked rollback wrote remote release state: %+v", releaseWrites)
	}
	joined := strings.Join(events, "\n")
	if joined != "agent:web-1:list_ship_containers" {
		t.Fatalf("blocked rollback touched agent:\n%s", joined)
	}
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "rollback", "blocked", "old-release") {
		t.Fatalf("timeline = %+v", timeline)
	}
}

func TestRollbackBlockersDetectStatefulAccessories(t *testing.T) {
	cfg := &config.Config{
		Accessories: map[string]config.Accessory{
			"cache":    {Image: "redis:7", Pool: "data"},
			"postgres": {Image: "postgres:17", Pool: "data", Primary: boolPtr(true), Backup: config.BackupSpec{Required: boolPtr(true)}},
			"search":   {Image: "opensearch:2", Pool: "data", Backup: config.BackupSpec{Required: boolPtr(true)}},
		},
	}
	blockers := rollbackBlockers(cfg)
	if len(blockers) != 2 {
		t.Fatalf("blockers = %+v", blockers)
	}
	message := rollbackBlockerError(blockers)
	for _, needle := range []string{"accessory postgres", "primary backup-required", "accessory search", "--allow-data-rollback"} {
		if !strings.Contains(message, needle) {
			t.Fatalf("message missing %q: %s", needle, message)
		}
	}
	stateless := &config.Config{Accessories: map[string]config.Accessory{
		"cache": {Image: "redis:7", Pool: "data"},
	}}
	if got := rollbackBlockers(stateless); len(got) != 0 {
		t.Fatalf("stateless accessory blockers = %+v", got)
	}
}

func TestRollbackRefusesAccessoryRiskWithoutConfirmation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(serviceAccessoryStatusConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old-image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{ID: "current-release", Environment: "production", Images: map[string]string{"web": "current-image"}, CreatedAt: time.Unix(20, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &scriptedDeployAgent{host: host.Name, events: &events}
	})

	cmd := rollbackCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--allow-data-rollback") {
		t.Fatalf("expected accessory risk error, got %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("blocked rollback touched agent: %#v", events)
	}
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "rollback", "blocked", "old-release") {
		t.Fatalf("timeline = %+v", timeline)
	}
}

func TestRecoverShowsFailedReleaseAndSuggestedRollback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{ID: "current-release", Environment: "production", Images: map[string]string{"web": "current"}, CreatedAt: time.Unix(20, 0)}); err != nil {
		t.Fatal(err)
	}
	failed := state.Release{ID: "failed-release", Environment: "production", Images: map[string]string{"web": "failed"}, CreatedAt: time.Unix(30, 0), Status: state.ReleaseStatusPending}
	if err := store.SaveReleaseRecord(failed); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkReleaseFailed("failed-release", "health failed", time.Unix(40, 0)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordEvent(state.Event{
		Time:        time.Unix(40, 0),
		Environment: "production",
		Kind:        "deploy_rollout",
		Status:      "failed",
		Release:     "failed-release",
		Message:     "health failed",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := recoverCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, needle := range []string{
		"current-release",
		"failed-release",
		"rollback target: old-release",
		"suggested rollback: ship rollback production --to old-release",
		"deploy_rollout",
		"failed-release",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("recover output missing %q:\n%s", needle, text)
		}
	}

	out.Reset()
	cmd = recoverCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view recoveryView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.CurrentRelease == nil || view.CurrentRelease.ID != "current-release" || view.RollbackTarget == nil || view.RollbackTarget.ID != "old-release" || len(view.FailedReleases) != 1 {
		t.Fatalf("recovery view = %+v", view)
	}
}
