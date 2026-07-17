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

	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/cli/ui"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/deployment"
	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/state"
)

func TestStatusReportsObservedDriftAndJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{
		ID:          "release-new",
		Environment: "production",
		Images:      map[string]string{"web": "new"},
		CreatedAt:   time.Unix(20, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"web-1": {
			serviceContainer("web-1", "web", 1, "release-new", "Up 10 seconds"),
			serviceContainer("web-1", "worker", 1, "old-worker", "Exited"),
		},
		"web-2": {
			serviceContainer("web-2", "web", 2, "release-old", "Up 2 minutes"),
		},
	}
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &observabilityAgent{host: host.Name, events: &events, observed: observed}
	})

	var out bytes.Buffer
	cmd := statusCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, needle := range []string{"web.2", "web-2", "wrong_release", "release-old", "Extra containers", "drift detected"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("status output missing %q:\n%s", needle, text)
		}
	}

	out.Reset()
	cmd = statusCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view statusView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.CurrentRelease == nil || view.CurrentRelease.ID != "release-new" {
		t.Fatalf("current release = %+v", view.CurrentRelease)
	}
	if view.CurrentConfig == "" {
		t.Fatalf("current config hash missing: %+v", view)
	}
	if !view.Summary.Drift || view.Summary.WrongRelease != 1 || view.Summary.Extra != 2 {
		t.Fatalf("summary = %+v", view.Summary)
	}
}

func TestStatusReportsConfigDrift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, _, err := cfg.ResolveEnvironment("production")
	if err != nil {
		t.Fatal(err)
	}
	deployedHash := configHash(resolved)
	if err := os.WriteFile(path, []byte(strings.Replace(singleHostConfig(), "scale: 1", "scale: 2", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{
		ID:          "release-1",
		Environment: "production",
		Images:      map[string]string{"web": "image"},
		ConfigHash:  deployedHash,
		CreatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &observabilityAgent{host: host.Name, events: &events}
	})

	var out bytes.Buffer
	cmd := statusCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view statusView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.ConfigDrift || view.DeployedConfig != deployedHash || view.CurrentConfig == "" || view.CurrentConfig == deployedHash {
		t.Fatalf("config drift view = %+v", view)
	}
}

func TestStatusUsesRemoteCurrentReleaseWhenLocalStateIsStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{
		ID:          "release-old",
		Environment: "production",
		Images:      map[string]string{"web": "old"},
		CreatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}
	remote := state.Release{
		ID:          "release-new",
		Environment: "production",
		Images:      map[string]string{"web": "new"},
		ConfigHash:  "remote-config",
		CreatedAt:   time.Unix(20, 0),
		Healthy:     true,
		Status:      state.ReleaseStatusHealthy,
	}
	observed := map[string][]docker.ContainerSummary{
		"web-1": {serviceContainer("web-1", "web", 1, "release-new", "Up 10 seconds")},
	}

	var events []string
	installAgentHook(t, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events, observed: observed, current: &remote}
	})

	var out bytes.Buffer
	cmd := statusCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view statusView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.CurrentRelease == nil || view.CurrentRelease.ID != "release-new" || view.Summary.WrongRelease != 0 || view.Summary.Drift {
		t.Fatalf("status view = %+v", view)
	}
	if !strings.Contains(strings.Join(events, "\n"), "agent:web-1:read_release_state") {
		t.Fatalf("remote release state was not read: %#v", events)
	}
}

func TestPSListsObservedContainersTextAndJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(serviceAccessoryStatusConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{
		ID:          "release-new",
		Environment: "production",
		Images:      map[string]string{"web": "new"},
		CreatedAt:   time.Unix(20, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "web-1", Pool: "web", User: "root"},
		UpdatedAt:   time.Unix(20, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"web-1": {
			serviceContainer("web-1", "web", 1, "release-new", "Up 10 seconds"),
			serviceContainer("web-1", "web", 1, "old-release", "Exited"),
			caddyContainer("web-1", "Up 10 seconds"),
			accessoryContainer("postgres", "web-1", "Up 5 minutes"),
		},
	}
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &observabilityAgent{host: host.Name, events: &events, observed: observed}
	})

	var out bytes.Buffer
	cmd := psCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, needle := range []string{
		"production",
		"release-new",
		"web-1",
		"service",
		"web.1",
		"ingress",
		"postgres",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("ps output missing %q:\n%s", needle, text)
		}
	}
	if strings.Contains(text, "old-release") {
		t.Fatalf("ps default output included extra old release:\n%s", text)
	}

	out.Reset()
	cmd = psCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--all", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view psView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Environment != "production" || view.Current != "release-new" || len(view.Containers) != 4 {
		t.Fatalf("ps view = %+v", view)
	}
	foundOld := false
	for _, container := range view.Containers {
		if container.Release == "old-release" {
			foundOld = true
		}
	}
	if !foundOld {
		t.Fatalf("ps --all did not include old release: %+v", view.Containers)
	}
}

func TestStatusKeepsConfiguredAccessoryObservedWithoutExtraDrift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(serviceAccessoryStatusConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{
		ID:          "release-new",
		Environment: "production",
		Images:      map[string]string{"web": "new"},
		CreatedAt:   time.Unix(20, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"web-1": {
			serviceContainer("web-1", "web", 1, "release-new", "Up 10 seconds"),
			accessoryContainer("postgres", "web-1", "Up 5 seconds"),
			caddyContainer("web-1", "Up 5 seconds"),
		},
	}
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &observabilityAgent{host: host.Name, events: &events, observed: observed}
	})

	var out bytes.Buffer
	cmd := statusCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view statusView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Summary.Extra != 0 || view.Summary.Drift {
		t.Fatalf("summary = %+v", view.Summary)
	}
	if len(view.ExtraObserved) != 0 {
		t.Fatalf("extra observed = %+v", view.ExtraObserved)
	}
	foundAccessory := false
	foundIngress := false
	for _, observed := range view.Observed {
		if observed.Accessory == "postgres" && observed.Kind == "accessory" {
			foundAccessory = true
		}
		if observed.Kind == "ingress" && observed.Name == deployment.CaddyContainerName("demo", "production") {
			foundIngress = true
			if observed.Service != "" || observed.Replica != 0 || observed.Release != "" {
				t.Fatalf("ingress status retained service drift fields: %+v", observed)
			}
		}
	}
	if !foundAccessory {
		t.Fatalf("observed did not include configured accessory: %+v", view.Observed)
	}
	if !foundIngress {
		t.Fatalf("observed did not include managed ingress: %+v", view.Observed)
	}
}

func TestInspectIncludesAccessoriesEventsAndJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(accessoryConfigYAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "release-1", Environment: "production", Images: map[string]string{"web": "image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-a", Pool: "data", User: "deploy"},
		UpdatedAt:   time.Unix(20, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordEvent(state.Event{
		Time:        time.Unix(30, 0),
		Environment: "production",
		Kind:        "deploy",
		Status:      "succeeded",
		Release:     "release-1",
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"data-a": {accessoryContainer("postgres", "data-a", "Up 5 seconds")},
	}
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &observabilityAgent{host: host.Name, events: &events, observed: observed}
	})

	var out bytes.Buffer
	cmd := inspectCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view inspectView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.CurrentRelease == nil || view.CurrentRelease.ID != "release-1" {
		t.Fatalf("inspect current = %+v", view.CurrentRelease)
	}
	if len(view.Accessories) != 1 || view.Accessories[0].Name != "postgres" {
		t.Fatalf("inspect accessories = %+v", view.Accessories)
	}
	if len(view.Events) != 1 || view.Events[0].Kind != "deploy" {
		t.Fatalf("inspect events = %+v", view.Events)
	}
	if len(view.Observed) != 1 || view.Observed[0].Accessory != "postgres" {
		t.Fatalf("inspect observed = %+v", view.Observed)
	}
}

func TestSupportBundleCollectsRedactedIncidentSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveReleaseRecord(state.Release{
		ID:          "old-release",
		Environment: "production",
		Images:      map[string]string{"web": "old-image"},
		CreatedAt:   time.Unix(10, 0).UTC(),
		Status:      state.ReleaseStatusHealthy,
		Healthy:     true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{
		ID:          "current-release",
		Environment: "production",
		Images:      map[string]string{"web": "current-image"},
		CreatedAt:   time.Unix(20, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	for i, kind := range []string{"deploy", "scale"} {
		if err := store.RecordEvent(state.Event{
			Time:        time.Unix(int64(30+i), 0).UTC(),
			Environment: "production",
			Kind:        kind,
			Status:      "succeeded",
		}); err != nil {
			t.Fatal(err)
		}
	}

	var events []string
	observed := map[string][]docker.ContainerSummary{
		"web-1": {serviceContainer("web-1", "web", 1, "current-release", "Up 10 seconds")},
		"web-2": {serviceContainer("web-2", "web", 2, "current-release", "Up 10 seconds")},
	}
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &observabilityAgent{host: host.Name, events: &events, observed: observed}
	})

	var out bytes.Buffer
	cmd := supportCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json", "--events-limit", "1", "--releases-limit", "1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var bundle supportBundle
	if err := json.Unmarshal(out.Bytes(), &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.Environment != "production" || bundle.Config["project"] != "demo" {
		t.Fatalf("support config = %+v", bundle.Config)
	}
	if bundle.Hosts == nil || len(bundle.Hosts.Hosts) != 2 {
		t.Fatalf("support hosts = %+v", bundle.Hosts)
	}
	if bundle.Status == nil || bundle.Status.CurrentRelease == nil || bundle.Status.CurrentRelease.ID != "current-release" || len(bundle.Status.Observed) != 2 {
		t.Fatalf("support status = %+v", bundle.Status)
	}
	if bundle.Releases == nil || len(bundle.Releases.Releases) != 1 || bundle.Releases.Releases[0].Release.ID != "current-release" {
		t.Fatalf("support releases = %+v", bundle.Releases)
	}
	if len(bundle.Events) != 1 || bundle.Events[0].Kind != "scale" {
		t.Fatalf("support events = %+v", bundle.Events)
	}
	if !strings.Contains(strings.Join(events, "\n"), "agent:web-1:list_ship_containers") {
		t.Fatalf("support did not inspect observed containers: %#v", events)
	}

	out.Reset()
	cmd = supportCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--events-limit", "1", "--releases-limit", "1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	expectedDoctor := fmt.Sprintf("passed=%d warnings=%d failed=%d", bundle.Doctor.Summary.Passed, bundle.Doctor.Summary.Warnings, bundle.Doctor.Summary.Failed)
	for _, needle := range []string{"environment production", "doctor", expectedDoctor, "hosts", "count=2", "status", "desired=2 observed=2", "events", "count=1"} {
		if !strings.Contains(out.String(), needle) {
			t.Fatalf("support text missing %q:\n%s", needle, out.String())
		}
	}
}

func TestEventsCommandJSONAndText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.RecordEvent(state.Event{
		Time:        time.Unix(10, 0),
		Environment: "production",
		Kind:        "scale",
		Status:      "planned",
		Service:     "web",
		Message:     "web=2",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := eventsCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "scale") || !strings.Contains(out.String(), "planned") || !strings.Contains(out.String(), "service=web") || !strings.Contains(out.String(), "web=2") {
		t.Fatalf("events output = %q", out.String())
	}

	out.Reset()
	cmd = eventsCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var events []state.Event
	if err := json.Unmarshal(out.Bytes(), &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "scale" {
		t.Fatalf("events json = %+v", events)
	}
}

func TestReleasesCommandShowsHistoryTextAndJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	oldCompleted := time.Unix(11, 0).UTC()
	currentCompleted := time.Unix(21, 0).UTC()
	if err := store.SaveReleaseRecord(state.Release{
		ID:          "old-release",
		Environment: "production",
		Images:      map[string]string{"web": "old-image"},
		ConfigHash:  "sha256:old",
		CreatedAt:   time.Unix(10, 0).UTC(),
		CompletedAt: &oldCompleted,
		Status:      state.ReleaseStatusHealthy,
		Healthy:     true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{
		ID:          "current-release",
		Environment: "production",
		Images:      map[string]string{"web": "current-image"},
		ConfigHash:  "sha256:current",
		CreatedAt:   time.Unix(20, 0).UTC(),
		CompletedAt: &currentCompleted,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveReleaseRecord(state.Release{
		ID:          "failed-release",
		Environment: "production",
		Images:      map[string]string{"web": "failed-image"},
		CreatedAt:   time.Unix(30, 0).UTC(),
		Status:      state.ReleaseStatusFailed,
		Error:       "health failed",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := releasesCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, needle := range []string{
		"environment production",
		"failed-release",
		"failed",
		"error=\"health failed\"",
		"current-release",
		"healthy",
		"[current]",
		"config=sha256:current",
		"image web=current-image",
		"old-release",
		"[rollback-target]",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("releases output missing %q:\n%s", needle, text)
		}
	}
	if strings.Index(text, "failed-release") > strings.Index(text, "current-release") {
		t.Fatalf("releases not newest-first:\n%s", text)
	}

	out.Reset()
	cmd = releasesCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json", "--limit", "2"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view releaseHistoryView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Environment != "production" || len(view.Releases) != 2 {
		t.Fatalf("release history json = %+v", view)
	}
	if view.Releases[0].Release.ID != "failed-release" || view.Releases[1].Release.ID != "current-release" || !view.Releases[1].Current {
		t.Fatalf("release history entries = %+v", view.Releases)
	}
}

func TestReleasesCommandRejectsInvalidLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := releasesCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production", "--limit", "0"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--limit must be greater than zero") {
		t.Fatalf("expected invalid limit error, got %v", err)
	}
}

func TestReleasesDiffShowsImagesConfigAndSecretChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveReleaseRecord(state.Release{
		ID:          "old-release",
		Environment: "production",
		Images: map[string]string{
			"web":    "old-web",
			"worker": "old-worker",
		},
		SecretDigests: map[string]string{
			"service-web:DATABASE_URL": "old-digest",
			"service-web:OLD_SECRET":   "removed-digest",
		},
		ConfigHash: "sha256:old",
		CreatedAt:  time.Unix(10, 0),
		Status:     state.ReleaseStatusHealthy,
		Healthy:    true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveReleaseRecord(state.Release{
		ID:          "new-release",
		Environment: "production",
		Images: map[string]string{
			"web": "new-web",
			"api": "new-api",
		},
		SecretDigests: map[string]string{
			"service-web:DATABASE_URL": "new-digest",
			"service-web:API_TOKEN":    "added-digest",
		},
		ConfigHash: "sha256:new",
		CreatedAt:  time.Unix(20, 0),
		Status:     state.ReleaseStatusHealthy,
		Healthy:    true,
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := releasesCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"diff", "production", "--from", "old-release", "--to", "new-release"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "release diff detected") {
		t.Fatalf("expected release diff error, got %v", err)
	}
	text := out.String()
	for _, needle := range []string{
		"environment production",
		"old-release",
		"new-release",
		"config",
		"sha256:old",
		"sha256:new",
		"api",
		"new-api",
		"web",
		"old-web",
		"new-web",
		"worker",
		"service-web:API_TOKEN",
		"service-web:DATABASE_URL",
		"service-web:OLD_SECRET",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("release diff output missing %q:\n%s", needle, text)
		}
	}

	out.Reset()
	cmd = releasesCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"diff", "production", "--from", "old-release", "--to", "new-release", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view releaseDiffView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Changed || !view.Config.Changed || len(view.Images.Added) != 1 || len(view.Images.Changed) != 1 || len(view.Images.Removed) != 1 {
		t.Fatalf("release diff json = %+v", view)
	}
	if len(view.Secrets.Missing) != 1 || len(view.Secrets.Changed) != 1 || len(view.Secrets.Extra) != 1 {
		t.Fatalf("release diff secrets = %+v", view.Secrets)
	}

	out.Reset()
	cmd = releasesCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"diff", "production", "--from", "old-release", "--to", "old-release"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no changes") {
		t.Fatalf("same-release diff output = %q", out.String())
	}
}

func TestLogsTargetsReplicaLinesFollowAndJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "release-1", Environment: "production", Images: map[string]string{"web": "image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	originalInterval := logsFollowInterval
	logsFollowInterval = time.Nanosecond
	t.Cleanup(func() {
		logsFollowInterval = originalInterval
	})

	var events []string
	var logCalls []agent.LogsParams
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &observabilityAgent{host: host.Name, events: &events, logCalls: &logCalls}
	})

	var out bytes.Buffer
	cmd := logsCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "web", "--replica", "2", "--lines", "42", "--follow", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(logCalls) != logsFollowPolls {
		t.Fatalf("log calls = %+v", logCalls)
	}
	wantName := deployment.ContainerName("demo", "production", "web", 2, "release-1")
	for _, call := range logCalls {
		if call.Name != wantName || call.Lines != 42 {
			t.Fatalf("log call = %+v, want name=%s lines=42", call, wantName)
		}
	}
	var view logsView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Follow || view.Replica != 2 || len(view.Entries) != logsFollowPolls || view.Entries[0].Host != "web-2" {
		t.Fatalf("logs view = %+v", view)
	}
	if view.Release != "release-1" || view.Entries[0].Release != "release-1" {
		t.Fatalf("logs release = view %q entry %q", view.Release, view.Entries[0].Release)
	}
}

func TestLogsAcceptsShorthandAndFullContainerName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "release-1", Environment: "production", Images: map[string]string{"web": "image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	wantName := deployment.ContainerName("demo", "production", "web", 2, "release-1")

	cases := []struct {
		name    string
		service string
	}{
		{"shorthand", "web.2"},
		{"full container name", wantName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			var logCalls []agent.LogsParams
			installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
				return &observabilityAgent{host: host.Name, events: &events, logCalls: &logCalls}
			})

			cmd := logsCmd(&options{configPath: path})
			cmd.SetArgs([]string{"production", tc.service})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if len(logCalls) != 1 || logCalls[0].Name != wantName {
				t.Fatalf("log calls = %+v, want single call against %q", logCalls, wantName)
			}
		})
	}
}

func TestLogsCanTargetExplicitAndFailedReleases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "current-release", Environment: "production", Images: map[string]string{"web": "current"}, CreatedAt: time.Unix(30, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveReleaseRecord(state.Release{ID: "requested-release", Environment: "production", Images: map[string]string{"web": "requested"}, CreatedAt: time.Unix(20, 0), Status: state.ReleaseStatusHealthy}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveReleaseRecord(state.Release{ID: "failed-worker", Environment: "production", Images: map[string]string{"worker": "failed"}, CreatedAt: time.Unix(40, 0), Status: state.ReleaseStatusFailed}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveReleaseRecord(state.Release{ID: "failed-web", Environment: "production", Images: map[string]string{"web": "failed"}, CreatedAt: time.Unix(50, 0), Status: state.ReleaseStatusFailed}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var logCalls []agent.LogsParams
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &observabilityAgent{host: host.Name, events: &events, logCalls: &logCalls}
	})

	var out bytes.Buffer
	cmd := logsCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "web", "--replica", "1", "--release", "requested-release", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(logCalls) != 1 || logCalls[0].Name != deployment.ContainerName("demo", "production", "web", 1, "requested-release") {
		t.Fatalf("explicit release log calls = %+v", logCalls)
	}
	var explicit logsView
	if err := json.Unmarshal(out.Bytes(), &explicit); err != nil {
		t.Fatal(err)
	}
	if explicit.Release != "requested-release" || explicit.Entries[0].Release != "requested-release" {
		t.Fatalf("explicit release view = %+v", explicit)
	}

	out.Reset()
	logCalls = nil
	cmd = logsCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "web", "--replica", "1", "--failed", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(logCalls) != 1 || logCalls[0].Name != deployment.ContainerName("demo", "production", "web", 1, "failed-web") {
		t.Fatalf("failed release log calls = %+v", logCalls)
	}
	var failed logsView
	if err := json.Unmarshal(out.Bytes(), &failed); err != nil {
		t.Fatal(err)
	}
	if failed.Release != "failed-web" || failed.Entries[0].Release != "failed-web" {
		t.Fatalf("failed release view = %+v", failed)
	}
}

func TestHealthChecksCurrentReleaseTextAndJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(restartConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "release-1", Environment: "production", Images: map[string]string{"web": "image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &healthDeployAgent{host: host.Name, events: &events}
	})

	var out bytes.Buffer
	cmd := healthCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "web", "--replica", "2"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	wantName := deployment.ContainerName("demo", "production", "web", 2, "release-1")
	for _, needle := range []string{
		"environment production",
		"release-1",
		"web-2",
		"web.2",
		wantName,
		"ok",
		"200",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("health output missing %q:\n%s", needle, text)
		}
	}
	if joined := strings.Join(events, "\n"); !strings.Contains(joined, "agent:web-2:health_check:http://127.0.0.1:3000/up") {
		t.Fatalf("health events = %#v", events)
	}

	out.Reset()
	cmd = healthCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view healthView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Environment != "production" || view.Current != "release-1" || !view.OK || len(view.Checks) != 2 {
		t.Fatalf("health view = %+v", view)
	}
	if view.Checks[0].URL != "http://127.0.0.1:3000/up" || !view.Checks[0].Checked || view.Checks[0].Status != "ok" {
		t.Fatalf("first health check = %+v", view.Checks[0])
	}
}

func TestHealthAcceptsShorthandAndFullContainerName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(restartConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "release-1", Environment: "production", Images: map[string]string{"web": "image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	wantName := deployment.ContainerName("demo", "production", "web", 2, "release-1")

	cases := []struct {
		name    string
		service string
	}{
		{"shorthand", "web.2"},
		{"full container name", wantName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
				return &healthDeployAgent{host: host.Name, events: &events}
			})

			cmd := healthCmd(&options{configPath: path})
			cmd.SetArgs([]string{"production", tc.service})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if joined := strings.Join(events, "\n"); !strings.Contains(joined, "agent:web-2:health_check:http://127.0.0.1:3000/up") {
				t.Fatalf("health events = %#v", events)
			}
		})
	}
}

func TestHealthReportsFailuresBeforeReturningError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(restartConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "release-1", Environment: "production", Images: map[string]string{"web": "image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &healthDeployAgent{host: host.Name, events: &events, failHost: "web-2"}
	})

	var out bytes.Buffer
	cmd := healthCmd(&options{configPath: path})
	cmd.SilenceUsage = true
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "health checks failed") {
		t.Fatalf("expected health failure, got %v", err)
	}
	var view healthView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.OK || len(view.Checks) != 2 {
		t.Fatalf("health view = %+v", view)
	}
	foundFailed := false
	for _, check := range view.Checks {
		if check.Host == "web-2" && check.Status == "failed" && !check.OK {
			foundFailed = true
		}
	}
	if !foundFailed {
		t.Fatalf("missing failed check: %+v", view.Checks)
	}
	if len(events) != 2 {
		t.Fatalf("health should check every target before returning: %#v", events)
	}
}

func TestMaintenanceEnableStatusDisable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	var writes []agent.WriteFileParams
	installDeployHooks(t, panicDeployDocker{t: t}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events, writes: &writes}
	})

	var out bytes.Buffer
	cmd := maintenanceCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"enable", "production", "--message", "Back soon"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "enabled maintenance for production on web-1") {
		t.Fatalf("enable output = %q", out.String())
	}
	if len(writes) != 1 || !strings.Contains(writes[0].Content, `respond "Back soon" 503`) {
		t.Fatalf("maintenance write = %+v", writes)
	}

	out.Reset()
	cmd = maintenanceCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"status", "production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "maintenance production enabled=true") || !strings.Contains(out.String(), `message="Back soon"`) {
		t.Fatalf("status output = %q", out.String())
	}

	out.Reset()
	cmd = maintenanceCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"disable", "production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "disabled maintenance for production") {
		t.Fatalf("disable output = %q", out.String())
	}
	if len(writes) != 2 || !strings.Contains(writes[1].Content, "reverse_proxy") || strings.Contains(writes[1].Content, "Back soon") {
		t.Fatalf("normal ingress restore write = %+v", writes)
	}
	statePath := maintenanceStatePath(filepath.Join(dir, config.LocalStateDir), "production")
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("maintenance state still exists: %v", err)
	}
}

func TestExecTargetsReplicaCommandTimeoutAndJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "release-1", Environment: "production", Images: map[string]string{"web": "image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var execCalls []agent.ExecContainerParams
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &observabilityAgent{host: host.Name, events: &events, execCalls: &execCalls}
	})

	var out bytes.Buffer
	cmd := execServiceCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "web", "--replica", "2", "--timeout", "12", "--json", "--", "bin/rails", "db:migrate"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	wantName := deployment.ContainerName("demo", "production", "web", 2, "release-1")
	if len(execCalls) != 1 || execCalls[0].Name != wantName || execCalls[0].Command != "bin/rails db:migrate" || execCalls[0].TimeoutSeconds != 12 {
		t.Fatalf("exec calls = %+v", execCalls)
	}
	var view execView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Service != "web" || view.Replica != 2 || len(view.Entries) != 1 || view.Entries[0].Host != "web-2" || view.Entries[0].Output != "exec from web-2" {
		t.Fatalf("exec view = %+v", view)
	}
}

func TestExecAcceptsShorthandAndFullContainerName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "release-1", Environment: "production", Images: map[string]string{"web": "image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	wantName := deployment.ContainerName("demo", "production", "web", 2, "release-1")

	cases := []struct {
		name    string
		service string
	}{
		{"shorthand", "web.2"},
		{"full container name", wantName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			var execCalls []agent.ExecContainerParams
			installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
				return &observabilityAgent{host: host.Name, events: &events, execCalls: &execCalls}
			})

			cmd := execServiceCmd(&options{configPath: path})
			cmd.SetArgs([]string{"production", tc.service, "--", "echo", "hi"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if len(execCalls) != 1 || execCalls[0].Name != wantName {
				t.Fatalf("exec calls = %+v, want single call against %q", execCalls, wantName)
			}
		})
	}
}

func TestExecRejectsConflictingReplicaFlagAndShorthand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "release-1", Environment: "production", Images: map[string]string{"web": "image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &observabilityAgent{host: host.Name, events: &events}
	})

	cmd := execServiceCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production", "web.2", "--replica", "1", "--", "echo", "hi"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "conflicts with replica") {
		t.Fatalf("expected replica conflict error, got %v", err)
	}
}

func TestPruneRunsOnEveryHostAndRecordsEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})

	var out bytes.Buffer
	cmd := pruneCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(events, "\n")
	for _, host := range []string{"web-1", "web-2"} {
		if !strings.Contains(joined, "agent:"+host+":prune_images") {
			t.Fatalf("prune events missing host %s:\n%s", host, joined)
		}
		if !strings.Contains(out.String(), "pruned unused Ship images on "+host) {
			t.Fatalf("output missing host %s:\n%s", host, out.String())
		}
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "prune_images", "started", "") || !timelineContains(timeline, "prune_images", "succeeded", "") {
		t.Fatalf("prune timeline missing start/success: %+v", timeline)
	}
}

func TestPruneRefusesConcurrentOperationBeforeMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	lock, err := store.AcquireOperationLock("production", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()

	var events []string
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})

	cmd := pruneCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err = cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "already busy") {
		t.Fatalf("expected busy operation error, got %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("busy prune touched agents: %#v", events)
	}
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "prune_images", "blocked", "") {
		t.Fatalf("timeline missing blocked prune: %+v", timeline)
	}
}

type healthDeployAgent struct {
	host     string
	events   *[]string
	failHost string
}

func (h *healthDeployAgent) Call(ctx context.Context, method string, params any, out any) error {
	switch method {
	case "health_check":
		p := params.(agent.HealthCheckParams)
		target := p.URL
		if target == "" {
			target = p.Command
		}
		*h.events = append(*h.events, fmt.Sprintf("agent:%s:health_check:%s", h.host, target))
		if result, ok := out.(*agent.HealthCheckResult); ok {
			*result = agent.HealthCheckResult{
				OK:         h.host != h.failHost,
				StatusCode: 200,
				Output:     "checked " + h.host,
				DurationMS: 25,
			}
		}
	case "docker_inspect":
		*h.events = append(*h.events, fmt.Sprintf("agent:%s:docker_inspect", h.host))
		populateRunningDockerInspect(out)
	default:
		*h.events = append(*h.events, fmt.Sprintf("agent:%s:%s", h.host, method))
	}
	return nil
}

func TestStatusRequiresEnvironment(t *testing.T) {
	cmd := statusCmd(&options{})
	ui.ConfigureRoot(cmd)
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "missing environment") {
		t.Fatalf("err = %v", err)
	}
}
