package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveHostFactsWritesEnvironmentHosts(t *testing.T) {
	store := NewStore(t.TempDir())
	hosts := []HostFact{{
		Name:          "web-1",
		Pool:          "web",
		User:          "root",
		IPv4:          "192.0.2.10",
		PublicAddress: "192.0.2.10",
		ServerID:      42,
	}}
	if err := store.SaveHostFacts("production", hosts); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(store.Dir, "environments", "production", "hosts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got []HostFact
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "web-1" || got[0].ServerID != 42 || got[0].IPv4 != "192.0.2.10" {
		t.Fatalf("hosts = %+v", got)
	}
}

func TestSaveHostFactsRejectsInvalidEnvironmentName(t *testing.T) {
	store := NewStore(t.TempDir())
	hosts := []HostFact{{Name: "web-1", Pool: "web"}}

	for _, environment := range []string{"../evil", "a/b"} {
		t.Run(environment, func(t *testing.T) {
			if err := store.SaveHostFacts(environment, hosts); err == nil {
				t.Fatal("expected error")
			}
			if _, err := os.Stat(filepath.Join(store.Dir, "environments", environment, "hosts.json")); !os.IsNotExist(err) {
				t.Fatalf("hosts.json should not exist: %v", err)
			}
		})
	}
}

func TestReadHostFactsReadsEnvironmentHosts(t *testing.T) {
	store := NewStore(t.TempDir())
	hosts := []HostFact{{
		Name:          "web-1",
		Pool:          "web",
		User:          "root",
		IPv4:          "192.0.2.10",
		PublicAddress: "198.51.100.10",
		ServerID:      42,
	}}
	if err := store.SaveHostFacts("production", hosts); err != nil {
		t.Fatal(err)
	}

	got, err := store.ReadHostFacts("production")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "web-1" || got[0].PublicAddress != "198.51.100.10" {
		t.Fatalf("hosts = %+v", got)
	}
}

func TestDeployLockRoundTripAndDelete(t *testing.T) {
	store := NewStore(t.TempDir())
	lock := DeployLock{
		Environment: "production",
		Message:     "incident response",
		CreatedAt:   time.Unix(10, 0),
	}
	if err := store.SaveDeployLock(lock); err != nil {
		t.Fatal(err)
	}
	read, err := store.ReadDeployLock("production")
	if err != nil {
		t.Fatal(err)
	}
	if read.Environment != "production" || read.Message != "incident response" || !read.CreatedAt.Equal(time.Unix(10, 0).UTC()) {
		t.Fatalf("lock = %+v", read)
	}
	if err := store.DeleteDeployLock("production"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadDeployLock("production"); !os.IsNotExist(err) {
		t.Fatalf("expected missing lock after delete, got %v", err)
	}
}

func TestOperationLockRefusesConcurrentOperation(t *testing.T) {
	store := NewStore(t.TempDir())
	lock, err := store.AcquireOperationLock("production", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()

	if _, err := store.AcquireOperationLock("production", "rollback"); err == nil || !strings.Contains(err.Error(), "already busy") {
		t.Fatalf("expected busy operation lock, got %v", err)
	}
}

func TestOperationLockReleasesAndScopesByEnvironment(t *testing.T) {
	store := NewStore(t.TempDir())
	production, err := store.AcquireOperationLock("production", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	staging, err := store.AcquireOperationLock("staging", "deploy")
	if err != nil {
		t.Fatalf("different environment lock failed: %v", err)
	}
	if err := staging.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := production.Unlock(); err != nil {
		t.Fatal(err)
	}
	next, err := store.AcquireOperationLock("production", "rollback")
	if err != nil {
		t.Fatalf("lock after unlock failed: %v", err)
	}
	if err := next.Unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestSaveAndListAccessoryState(t *testing.T) {
	store := NewStore(t.TempDir())
	accessory := AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        HostFact{Name: "data-1", Pool: "data", User: "deploy"},
		UpdatedAt:   time.Unix(10, 0),
	}
	if err := store.SaveAccessoryState(accessory); err != nil {
		t.Fatal(err)
	}
	read, err := store.ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if read.Host.Name != "data-1" || read.Host.User != "deploy" {
		t.Fatalf("accessory state = %+v", read)
	}
	list, err := store.AccessoryStates("production")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "postgres" {
		t.Fatalf("accessory states = %+v", list)
	}
}

func TestRecordAccessoryBackupAndRestore(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.SaveAccessoryState(AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        HostFact{Name: "data-1", Pool: "data", User: "root"},
		UpdatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordAccessoryBackup("production", "postgres", AccessoryBackup{
		Artifact:         "/var/lib/ship/backups/pg.backup",
		ExportedArtifact: "s3://ship/pg.backup",
		Host:             "data-1",
		Output:           "ok",
		ExportOutput:     "s3://ship/pg.backup",
		CreatedAt:        time.Unix(20, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordAccessoryRestore("production", "postgres", AccessoryRestore{
		Artifact:  "/var/lib/ship/backups/pg.backup",
		Host:      "data-1",
		CreatedAt: time.Unix(30, 0),
	}); err != nil {
		t.Fatal(err)
	}
	read, err := store.ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if read.LastBackup == nil || read.LastBackup.Output != "ok" || read.LastBackup.ExportedArtifact != "s3://ship/pg.backup" || read.LastBackup.ExportOutput != "s3://ship/pg.backup" {
		t.Fatalf("last backup = %+v", read.LastBackup)
	}
	if read.LastRestore == nil || read.LastRestore.Artifact != "/var/lib/ship/backups/pg.backup" {
		t.Fatalf("last restore = %+v", read.LastRestore)
	}
}

func TestRecordAndReadEvents(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.RecordEvent(Event{
		Time:        time.Unix(20, 0),
		Environment: "production",
		Kind:        "deploy",
		Status:      "succeeded",
		Release:     "release-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordEvent(Event{
		Time:        time.Unix(10, 0),
		Environment: "production",
		Kind:        "deploy",
		Status:      "started",
		Release:     "release-1",
	}); err != nil {
		t.Fatal(err)
	}
	events, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Status != "started" || events[1].Status != "succeeded" {
		t.Fatalf("events not sorted by time: %+v", events)
	}
	data, err := os.ReadFile(filepath.Join(store.Dir, "events", "production.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Fatalf("events file is not json: %s", data)
	}
}

func TestRollbackTargetReturnsPreviousRelease(t *testing.T) {
	store := NewStore(t.TempDir())
	old := Release{ID: "old", Environment: "production", CreatedAt: time.Unix(10, 0), Images: map[string]string{"web": "old"}}
	current := Release{ID: "current", Environment: "production", CreatedAt: time.Unix(20, 0), Images: map[string]string{"web": "current"}}
	if err := store.SaveRelease(old); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(current); err != nil {
		t.Fatal(err)
	}
	target, err := store.RollbackTarget("production")
	if err != nil {
		t.Fatal(err)
	}
	if target.ID != "old" {
		t.Fatalf("target = %q", target.ID)
	}
}

func TestReadAndCurrentRelease(t *testing.T) {
	store := NewStore(t.TempDir())
	old := Release{ID: "old", Environment: "staging", CreatedAt: time.Unix(10, 0), Images: map[string]string{"web": "old"}}
	current := Release{ID: "current", Environment: "production", CreatedAt: time.Unix(20, 0), Images: map[string]string{"web": "current"}}
	if err := store.SaveRelease(old); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(current); err != nil {
		t.Fatal(err)
	}
	read, err := store.ReadRelease("current")
	if err != nil {
		t.Fatal(err)
	}
	if read.ID != "current" {
		t.Fatalf("read release = %+v", read)
	}
	got, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "current" {
		t.Fatalf("current release = %+v", got)
	}
	fallback, err := store.CurrentRelease("staging")
	if err != nil {
		t.Fatal(err)
	}
	if fallback.ID != "old" {
		t.Fatalf("fallback release = %+v", fallback)
	}
}

func TestSaveReleaseWritesReleaseAndCurrentFiles(t *testing.T) {
	store := NewStore(t.TempDir())
	release := Release{
		ID:          "release-1",
		Environment: "production",
		Images:      map[string]string{"web": "example/web:1"},
		CreatedAt:   time.Unix(30, 0),
		Healthy:     true,
	}
	if err := store.SaveRelease(release); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(store.Dir, "releases", "release-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got Release
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != release.ID || got.Images["web"] != release.Images["web"] {
		t.Fatalf("release file = %+v", got)
	}
	current, err := os.ReadFile(filepath.Join(store.Dir, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "release-1\n" {
		t.Fatalf("current = %q", current)
	}
	envCurrent, err := os.ReadFile(filepath.Join(store.Dir, "environments", "production", "current"))
	if err != nil {
		t.Fatal(err)
	}
	if string(envCurrent) != "release-1\n" {
		t.Fatalf("environment current = %q", envCurrent)
	}
	if !got.Healthy || got.Status != ReleaseStatusHealthy || got.CompletedAt == nil {
		t.Fatalf("release status = %+v", got)
	}
}

func TestCurrentReleaseUsesEnvironmentPointerAfterOtherEnvironmentPromote(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.SaveRelease(Release{ID: "prod-old", Environment: "production", CreatedAt: time.Unix(10, 0), Images: map[string]string{"web": "prod-old"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(Release{ID: "prod-new", Environment: "production", CreatedAt: time.Unix(20, 0), Images: map[string]string{"web": "prod-new"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkReleaseHealthy("prod-old", time.Unix(30, 0)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(Release{ID: "staging-new", Environment: "staging", CreatedAt: time.Unix(40, 0), Images: map[string]string{"web": "staging-new"}}); err != nil {
		t.Fatal(err)
	}

	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "prod-old" {
		t.Fatalf("production current = %+v", current)
	}
	staging, err := store.CurrentRelease("staging")
	if err != nil {
		t.Fatal(err)
	}
	if staging.ID != "staging-new" {
		t.Fatalf("staging current = %+v", staging)
	}
}

func TestPendingAndFailedReleaseDoesNotReplaceCurrent(t *testing.T) {
	store := NewStore(t.TempDir())
	old := Release{ID: "old", Environment: "production", CreatedAt: time.Unix(10, 0), Images: map[string]string{"web": "old"}}
	if err := store.SaveRelease(old); err != nil {
		t.Fatal(err)
	}
	pending := Release{
		ID:          "new",
		Environment: "production",
		CreatedAt:   time.Unix(20, 0),
		Images:      map[string]string{"web": "new"},
		Status:      ReleaseStatusPending,
	}
	if err := store.SaveReleaseRecord(pending); err != nil {
		t.Fatal(err)
	}
	failedAt := time.Unix(30, 0)
	failed, err := store.MarkReleaseFailed("new", "health failed", failedAt)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Healthy || failed.Status != ReleaseStatusFailed || failed.Error != "health failed" || failed.FailedAt == nil {
		t.Fatalf("failed release = %+v", failed)
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "old" {
		t.Fatalf("current = %+v", current)
	}
	target, err := store.RollbackTarget("production")
	if err == nil {
		t.Fatalf("unexpected rollback target after one healthy release: %+v", target)
	}
}

func TestMarkReleaseHealthyPromotesCurrent(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.SaveRelease(Release{ID: "old", Environment: "production", CreatedAt: time.Unix(10, 0), Images: map[string]string{"web": "old"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveReleaseRecord(Release{
		ID:          "new",
		Environment: "production",
		CreatedAt:   time.Unix(20, 0),
		Images:      map[string]string{"web": "new"},
		Status:      ReleaseStatusPending,
	}); err != nil {
		t.Fatal(err)
	}
	healthy, err := store.MarkReleaseHealthy("new", time.Unix(30, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !healthy.Healthy || healthy.Status != ReleaseStatusHealthy || healthy.CompletedAt == nil {
		t.Fatalf("healthy release = %+v", healthy)
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "new" {
		t.Fatalf("current = %+v", current)
	}
}

func TestReleaseIDCannotTraverseStateDir(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.SaveRelease(Release{ID: "../escape", Environment: "production"}); err == nil {
		t.Fatal("expected invalid release id on save")
	}
	if _, err := store.ReadRelease("../escape"); err == nil {
		t.Fatal("expected invalid release id on read")
	}
}
