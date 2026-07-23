package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	accessorypkg "github.com/watzon/ship/internal/accessory"
	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/deployment"
	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/scheduler"
	secretspkg "github.com/watzon/ship/internal/secrets"
	"github.com/watzon/ship/internal/state"
)

func TestPlanJSON(t *testing.T) {
	path := writeConfig(t)
	var out bytes.Buffer
	cmd := planCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view struct {
		Environment string `json:"environment"`
		Actions     []struct {
			Kind    string `json:"kind"`
			Target  string `json:"target"`
			Details string `json:"details,omitempty"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("invalid json %q: %v", out.String(), err)
	}
	if view.Environment != "production" || len(view.Actions) == 0 {
		t.Fatalf("view = %+v", view)
	}
	var sawBuild, sawStart, sawIngress bool
	for _, action := range view.Actions {
		switch {
		case action.Kind == "build" && action.Target == "web":
			sawBuild = true
		case action.Kind == "start" && action.Target == "web.1":
			sawStart = true
		case action.Kind == "ingress" && action.Target == "web" && strings.Contains(action.Details, "example.com"):
			sawIngress = true
		}
	}
	if !sawBuild || !sawStart || !sawIngress {
		t.Fatalf("actions missing build/start/ingress: %+v", view.Actions)
	}
}

func TestPlanObservedReportsDriftAndRolloutActions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{
		ID:          "release-current",
		Environment: "production",
		Images:      map[string]string{"web": "registry.local/acme/web@sha256:" + strings.Repeat("1", 64)},
		CreatedAt:   time.Unix(20, 0),
	}); err != nil {
		t.Fatal(err)
	}
	observed := map[string][]docker.ContainerSummary{
		"web-1": {
			serviceContainer("web-1", "web", 1, "release-current", "Up 10 seconds"),
			serviceContainer("web-1", "worker", 1, "old-worker", "Exited"),
		},
		"web-2": {
			serviceContainer("web-2", "web", 2, "release-old", "Up 2 minutes"),
		},
	}
	var events []string
	installAgentHook(t, func(host scheduler.Host) deployAgent {
		return &observabilityAgent{host: host.Name, events: &events, observed: observed}
	})

	var out bytes.Buffer
	cmd := planCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--observed"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, needle := range []string{
		"release release-current",
		"web.2",
		"web-2",
		"wrong_release",
		"Extra containers",
		"worker.1",
		"Rollout actions",
		"start",
		"web.1",
		"web-1",
		"stop",
		"web.2",
		"worker.1",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("observed plan output missing %q:\n%s", needle, text)
		}
	}

	out.Reset()
	cmd = planCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--observed", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view observedPlanView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("invalid json %q: %v", out.String(), err)
	}
	if view.Observed.CurrentRelease != "release-current" || !view.Observed.Summary.Drift || len(view.RolloutActions) == 0 {
		t.Fatalf("observed plan view = %+v", view)
	}
	var sawPreserveStop bool
	for _, action := range view.RolloutActions {
		if action.Kind == "preserve_stop" && action.Container == deployment.ContainerName("demo", "production", "web", 2, "release-old") {
			sawPreserveStop = true
		}
	}
	if !sawPreserveStop {
		t.Fatalf("rollout actions missing preserve-stop for old web replica: %+v", view.RolloutActions)
	}
}

func TestDeployBuildsPushesResolvesBeforeAgentMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	latestTag := "registry.local/acme/demo:web-latest"
	productionTag := "registry.local/acme/demo:web-production"
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	workerDigestRef := "registry.local/acme/worker@sha256:" + strings.Repeat("3", 64)
	fakeDocker := &recordingDeployDocker{
		events: &events,
		resolved: map[string]string{
			tag:                                 digestRef,
			"registry.local/acme/worker:stable": workerDigestRef,
		},
	}
	installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})

	var out bytes.Buffer
	cmd := deployCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"agent:web-1:negotiate",
		"agent:web-2:negotiate",
		"build:" + tag,
		"push:" + tag,
		"push:" + latestTag,
		"push:" + productionTag,
		"resolve:" + tag,
		"resolve:registry.local/acme/worker:stable",
		"agent:web-1:write_release_state:" + releaseID + ":pending",
		"agent:web-2:write_release_state:" + releaseID + ":pending",
		"agent:web-1:list_ship_containers",
		"agent:web-2:list_ship_containers",
		"agent:web-1:pull:" + digestRef,
		"agent:web-1:run:" + digestRef,
		"agent:web-2:pull:" + digestRef,
		"agent:web-2:run:" + digestRef,
		"agent:web-1:write_release_state:" + releaseID + ":healthy",
		"agent:web-2:write_release_state:" + releaseID + ":healthy",
	}
	if strings.Join(events, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	if len(fakeDocker.builds) != 1 {
		t.Fatalf("builds = %#v", fakeDocker.builds)
	}
	build := fakeDocker.builds[0]
	if build.BuildArgs["RAILS_ENV"] != "production" || build.Target != "runtime" || build.Builder != "ship-cloud" || build.Platform != "linux/amd64" {
		t.Fatalf("build options = %+v", build)
	}
	if strings.Join(build.AdditionalTags, ",") != latestTag+","+productionTag {
		t.Fatalf("build additional tags = %+v", build)
	}
	if !build.Pull || build.NoCache || strings.Join(build.NoCacheFilter, ",") != "install,assets" {
		t.Fatalf("build freshness options = %+v", build)
	}
	if strings.Join(build.CacheFrom, ",") != "type=registry,ref=registry.local/acme/demo:build-cache" ||
		strings.Join(build.CacheTo, ",") != "type=registry,ref=registry.local/acme/demo:build-cache,mode=max" {
		t.Fatalf("build cache options = %+v", build)
	}
	if strings.Join(build.Secrets, ",") != "id=npm_token,env=NPM_TOKEN" || strings.Join(build.SSH, ",") != "default" {
		t.Fatalf("build secret/ssh options = %+v", build)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	release, err := store.ReadRelease(releaseID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, _, err := cfg.ResolveEnvironment("production")
	if err != nil {
		t.Fatal(err)
	}
	if release.ConfigHash == "" || release.ConfigHash != configHash(resolved) {
		t.Fatalf("release config hash = %q, want %q", release.ConfigHash, configHash(resolved))
	}
}

func TestDeployEnsuresMissingAccessoryBeforeRollout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployWithAccessoryConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	fakeDocker := &recordingDeployDocker{events: &events}
	installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})

	var out bytes.Buffer
	cmd := deployCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(events, "\n")
	for _, needle := range []string{
		"agent:web-1:pull:redis:7-alpine",
		"agent:web-1:run:redis:7-alpine",
		"agent:web-1:pull:registry.local/acme/web:stable@sha256:",
	} {
		if !strings.Contains(joined, needle) {
			t.Fatalf("events missing %q:\n%s", needle, joined)
		}
	}
	if strings.Index(joined, "agent:web-1:run:redis:7-alpine") > strings.Index(joined, "agent:web-1:pull:registry.local/acme/web:stable@sha256:") {
		t.Fatalf("accessory was not ensured before service rollout:\n%s", joined)
	}
	if !strings.Contains(out.String(), "ensured accessory redis on web-1") {
		t.Fatalf("output missing accessory ensure:\n%s", out.String())
	}
}

func TestDeploySkipsAlreadyRunningAccessory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployWithAccessoryConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	containerName := accessorypkg.ContainerName("demo", "production", "redis")
	observed := map[string][]docker.ContainerSummary{
		"web-1": {{
			Names:  containerName,
			Image:  "redis:7-alpine",
			Status: "Up 10 minutes",
			Labels: map[string]string{
				docker.LabelManagedBy:   docker.LabelManagedByValue,
				docker.LabelProject:     "demo",
				docker.LabelEnvironment: "production",
				docker.LabelAccessory:   "redis",
			},
		}},
	}

	var events []string
	fakeDocker := &recordingDeployDocker{events: &events}
	installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events, observed: observed}
	})

	var out bytes.Buffer
	cmd := deployCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(events, "\n")
	if strings.Contains(joined, "agent:web-1:pull:redis:7-alpine") || strings.Contains(joined, "agent:web-1:run:redis:7-alpine") {
		t.Fatalf("running accessory was redeployed:\n%s", joined)
	}
	if !strings.Contains(out.String(), "accessory redis already running on web-1") {
		t.Fatalf("output missing accessory skip:\n%s", out.String())
	}
	if _, err := state.NewStore(filepath.Join(dir, config.LocalStateDir)).ReadAccessoryState("production", "redis"); err != nil {
		t.Fatalf("running accessory placement was not persisted: %v", err)
	}
}

func TestPrepareDeployImagesPublishesAttestedBuilds(t *testing.T) {
	events := []string{}
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	latestTag := "registry.local/acme/demo:web-latest"
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	fakeDocker := &recordingDeployDocker{
		events: &events,
		resolved: map[string]string{
			tag: digestRef,
		},
	}
	cfg := &config.Config{
		Registry: "registry.local/acme/demo",
		Services: map[string]config.Service{
			"web": {
				Image: config.ImageSpec{
					Build:      ".",
					Tags:       []string{"latest"},
					SBOM:       config.BuildxFlag("true"),
					Provenance: config.BuildxFlag("mode=max"),
				},
			},
		},
	}

	images, err := prepareDeployImages(context.Background(), fakeDocker, cfg, releaseID)
	if err != nil {
		t.Fatal(err)
	}
	if images["web"] != digestRef {
		t.Fatalf("images = %+v, want web digest %q", images, digestRef)
	}
	wantEvents := []string{
		"build:" + tag,
		"resolve:" + tag,
	}
	if strings.Join(events, "\n") != strings.Join(wantEvents, "\n") {
		t.Fatalf("events = %#v, want %#v", events, wantEvents)
	}
	if len(fakeDocker.builds) != 1 {
		t.Fatalf("builds = %#v", fakeDocker.builds)
	}
	build := fakeDocker.builds[0]
	if !build.Push || build.SBOM != "true" || build.Provenance != "mode=max" || strings.Join(build.AdditionalTags, ",") != latestTag {
		t.Fatalf("build options = %+v", build)
	}
}

func TestPrepareDeployImagesPublishesMultiPlatformBuilds(t *testing.T) {
	events := []string{}
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	fakeDocker := &recordingDeployDocker{
		events: &events,
		resolved: map[string]string{
			tag: digestRef,
		},
	}
	cfg := &config.Config{
		Registry: "registry.local/acme/demo",
		Services: map[string]config.Service{
			"web": {
				Image: config.ImageSpec{
					Build:     ".",
					Platforms: []string{"linux/amd64", "linux/arm64"},
				},
			},
		},
	}

	images, err := prepareDeployImages(context.Background(), fakeDocker, cfg, releaseID)
	if err != nil {
		t.Fatal(err)
	}
	if images["web"] != digestRef {
		t.Fatalf("images = %+v, want web digest %q", images, digestRef)
	}
	wantEvents := []string{
		"build:" + tag,
		"resolve:" + tag,
	}
	if strings.Join(events, "\n") != strings.Join(wantEvents, "\n") {
		t.Fatalf("events = %#v, want %#v", events, wantEvents)
	}
	if len(fakeDocker.builds) != 1 {
		t.Fatalf("builds = %#v", fakeDocker.builds)
	}
	build := fakeDocker.builds[0]
	if !build.Push || strings.Join(build.Platforms, ",") != "linux/amd64,linux/arm64" {
		t.Fatalf("build options = %+v", build)
	}
}

func TestPrepareDeployImagesPassesBuildpackOptions(t *testing.T) {
	events := []string{}
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	fakeDocker := &recordingDeployDocker{
		events: &events,
		resolved: map[string]string{
			tag: digestRef,
		},
	}
	cfg := &config.Config{
		Registry: "registry.local/acme/demo",
		Services: map[string]config.Service{
			"web": {
				Image: config.ImageSpec{
					Build: ".",
					Tags:  []string{"latest"},
					Buildpack: config.BuildpackConfig{
						Builder:    "paketobuildpacks/builder-jammy-base",
						Buildpacks: []string{"paketo-buildpacks/nodejs"},
						Env: map[string]string{
							"BP_NODE_RUN_SCRIPTS": "build",
						},
						Descriptor:   "project.production.toml",
						Publish:      boolPtr(true),
						PullPolicy:   "if-not-present",
						TrustBuilder: boolPtr(true),
					},
				},
			},
		},
	}

	images, err := prepareDeployImages(context.Background(), fakeDocker, cfg, releaseID)
	if err != nil {
		t.Fatal(err)
	}
	if images["web"] != digestRef {
		t.Fatalf("images = %+v, want web digest %q", images, digestRef)
	}
	wantEvents := []string{
		"build:" + tag,
		"resolve:" + tag,
	}
	if strings.Join(events, "\n") != strings.Join(wantEvents, "\n") {
		t.Fatalf("events = %#v, want %#v", events, wantEvents)
	}
	if len(fakeDocker.builds) != 1 {
		t.Fatalf("builds = %#v", fakeDocker.builds)
	}
	build := fakeDocker.builds[0]
	if !build.Buildpack.Publish || build.Buildpack.Builder != "paketobuildpacks/builder-jammy-base" || build.Buildpack.PullPolicy != "if-not-present" || !build.Buildpack.TrustBuilder {
		t.Fatalf("buildpack options = %+v", build.Buildpack)
	}
	if strings.Join(build.Buildpack.Buildpacks, ",") != "paketo-buildpacks/nodejs" {
		t.Fatalf("buildpacks = %+v", build.Buildpack.Buildpacks)
	}
	if build.Buildpack.Env["BP_NODE_RUN_SCRIPTS"] != "build" || build.Buildpack.Descriptor != "project.production.toml" {
		t.Fatalf("buildpack options = %+v", build.Buildpack)
	}
}

func TestDeployWritesRegistryAuthBeforePull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	workerDigestRef := "registry.local/acme/worker@sha256:" + strings.Repeat("3", 64)
	fakeDocker := &recordingDeployDocker{
		events: &events,
		resolved: map[string]string{
			tag:                                 digestRef,
			"registry.local/acme/worker:stable": workerDigestRef,
		},
		auths: map[string]docker.RegistryAuth{
			digestRef: {Server: "registry.local", Auth: json.RawMessage(`{"auth":"dXNlcjp0b2tlbg=="}`)},
		},
	}
	installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(events, "\n")
	for _, host := range []string{"web-1", "web-2"} {
		authAt := strings.Index(joined, "agent:"+host+":write_registry_auth:registry.local")
		pullAt := strings.Index(joined, "agent:"+host+":pull:"+digestRef)
		if authAt < 0 || pullAt < 0 || authAt > pullAt {
			t.Fatalf("registry auth did not precede pull for %s:\n%s", host, joined)
		}
	}
	if strings.Count(joined, "write_registry_auth:registry.local") != 2 {
		t.Fatalf("registry auth was not written once per host:\n%s", joined)
	}
}

func TestDeployRunsReleaseCommandBeforeRollout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(releaseCommandDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	secretValue := "postgres://user:pass@example/db"
	t.Setenv("SHIP_TEST_DATABASE_URL", secretValue)

	var events []string
	var oneOffRuns []agent.RunOneOffContainerParams
	imageRef := "registry.local/acme/web:stable"
	digestRef := "registry.local/acme/web@sha256:" + strings.Repeat("4", 64)
	installDeployHooks(t, &recordingDeployDocker{events: &events, resolved: map[string]string{imageRef: digestRef}}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events, oneOffRuns: &oneOffRuns}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	releaseID := "abc123def456-20260630T183456.123456789Z"
	if len(oneOffRuns) != 1 {
		t.Fatalf("one-off runs = %#v", oneOffRuns)
	}
	run := oneOffRuns[0]
	if run.Image != digestRef || run.Command != "bin/rails db:migrate" || run.TimeoutSeconds != 600 {
		t.Fatalf("one-off run = %+v", run)
	}
	if run.Network != "ship-demo-production" {
		t.Fatalf("one-off network = %q", run.Network)
	}
	if !strings.Contains(strings.Join(run.Args, " "), "--env-file /var/lib/ship/secrets/production/service-web.env") {
		t.Fatalf("one-off args missing secret env file: %+v", run.Args)
	}
	if !strings.Contains(strings.Join(run.Args, " "), "--log-opt max-size=10m") {
		t.Fatalf("one-off args missing logging options: %+v", run.Args)
	}
	if !strings.Contains(strings.Join(run.Args, " "), "-v uploads:/app/uploads") {
		t.Fatalf("one-off args missing service volumes: %+v", run.Args)
	}
	if !strings.Contains(strings.Join(run.Args, " "), "--memory 512m") || !strings.Contains(strings.Join(run.Args, " "), "--cpus 1") {
		t.Fatalf("one-off args missing resource limits: %+v", run.Args)
	}
	if strings.Contains(strings.Join(run.Args, " "), "-p 3000:3000") {
		t.Fatalf("one-off args should not bind service ports: %+v", run.Args)
	}
	if strings.Contains(strings.Join(run.Args, " "), "--restart") {
		t.Fatalf("one-off args should not set restart policy: %+v", run.Args)
	}
	joined := strings.Join(events, "\n")
	oneOffAt := strings.Index(joined, "agent:web-1:run_oneoff:ship_demo_production_web_release_"+releaseID+":bin/rails db:migrate")
	rolloutAt := strings.Index(joined, "agent:web-1:list_ship_containers")
	if oneOffAt < 0 || rolloutAt < 0 || oneOffAt > rolloutAt {
		t.Fatalf("release command did not precede rollout inspection:\n%s", joined)
	}
}

func TestDeployWithSavedHostFactsPassesContactsAndPreservesLogicalEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveHostFacts("production", []state.HostFact{
		{Name: "web-1", Pool: "web", User: "deploy", SSHPort: 2222, IPv4: "203.0.113.10", PublicAddress: "198.51.100.10"},
		{Name: "web-2", Pool: "web", User: "ubuntu", IPv4: "203.0.113.20"},
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	seenContacts := map[string]string{}
	seenUsers := map[string]string{}
	seenPorts := map[string]int{}
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	workerDigestRef := "registry.local/acme/worker@sha256:" + strings.Repeat("3", 64)
	fakeDocker := &recordingDeployDocker{
		events: &events,
		resolved: map[string]string{
			tag:                                 digestRef,
			"registry.local/acme/worker:stable": workerDigestRef,
		},
	}
	installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent {
		seenContacts[host.Name] = host.Contact
		seenUsers[host.Name] = host.User
		seenPorts[host.Name] = host.SSHPort
		return recordingDeployAgent{host: host.Name, events: &events}
	})

	var out bytes.Buffer
	cmd := deployCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	if seenContacts["web-1"] != "198.51.100.10" || seenContacts["web-2"] != "203.0.113.20" {
		t.Fatalf("seen contacts = %+v", seenContacts)
	}
	if seenUsers["web-1"] != "deploy" || seenUsers["web-2"] != "ubuntu" {
		t.Fatalf("seen users = %+v", seenUsers)
	}
	if seenPorts["web-1"] != 2222 || seenPorts["web-2"] != 0 {
		t.Fatalf("seen ports = %+v", seenPorts)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "agent:web-1:run:"+digestRef) || !strings.Contains(joined, "agent:web-2:run:"+digestRef) {
		t.Fatalf("logical host events missing:\n%s", joined)
	}
	if strings.Contains(joined, "198.51.100.10") || strings.Contains(joined, "203.0.113.20") {
		t.Fatalf("contact addresses leaked into logical events:\n%s", joined)
	}
}

func TestLockUnlockCommandsManageDeployLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var out bytes.Buffer
	lock := lockCmd(&options{configPath: path})
	lock.SetOut(&out)
	lock.SetArgs([]string{"production", "--message", "database maintenance"})
	if err := lock.Execute(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	read, err := store.ReadDeployLock("production")
	if err != nil {
		t.Fatal(err)
	}
	if read.Message != "database maintenance" {
		t.Fatalf("lock = %+v", read)
	}
	if !strings.Contains(out.String(), "locked production: database maintenance") {
		t.Fatalf("lock output = %q", out.String())
	}

	out.Reset()
	unlock := unlockCmd(&options{configPath: path})
	unlock.SetOut(&out)
	unlock.SetArgs([]string{"production"})
	if err := unlock.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadDeployLock("production"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected missing lock, got %v", err)
	}
	if !strings.Contains(out.String(), "unlocked production") {
		t.Fatalf("unlock output = %q", out.String())
	}
}

func TestDeployRefusesLockedEnvironmentBeforeMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveDeployLock(state.DeployLock{Environment: "production", Message: "freeze"}); err != nil {
		t.Fatal(err)
	}

	var events []string
	installDeployHooks(t, panicDeployDocker{t: t}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "deploys are locked for production: freeze") {
		t.Fatalf("expected deploy lock error, got %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("locked deploy touched agent: %#v", events)
	}
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "deploy", "blocked", "") {
		t.Fatalf("timeline missing blocked deploy: %+v", timeline)
	}
}

func TestDeployRefusesConcurrentOperationBeforeMutation(t *testing.T) {
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
	installDeployHooks(t, panicDeployDocker{t: t}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err = cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "already busy") {
		t.Fatalf("expected busy operation error, got %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("busy deploy touched agent: %#v", events)
	}
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "deploy", "blocked", "") {
		t.Fatalf("timeline missing blocked deploy: %+v", timeline)
	}
}

func TestDeployRejectsAgentMissingRolloutLifecycleMethodsBeforeMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	var events []string
	var negotiateParams agent.NegotiateParams
	fakeAgent := &methodNegotiationAgent{host: "web-1", events: &events, supported: []string{"pull"}, negotiateParams: &negotiateParams}
	installDeployHooks(t, panicDeployDocker{t: t}, func(host scheduler.Host) deployAgent {
		return fakeAgent
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected preflight failure")
	}
	for _, method := range requiredRolloutAgentMethods {
		if !strings.Contains(err.Error(), method) {
			t.Fatalf("error %q missing method %q", err, method)
		}
	}
	want := []string{"agent:web-1:negotiate"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want read-only preflight %#v", events, want)
	}
	if negotiateParams.MinProtocolVersion != agent.AgentProtocol || negotiateParams.MaxProtocolVersion != agent.AgentProtocol {
		t.Fatalf("negotiate params = %+v, want current protocol %d", negotiateParams, agent.AgentProtocol)
	}
}

func TestDeployStopsFixedPortOldBeforeStartAndPromotesHealthyRelease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	oldRelease := state.Release{
		ID:          "old-release",
		Environment: "production",
		Images:      map[string]string{"web": "registry.local/acme/web@sha256:" + strings.Repeat("0", 64)},
		CreatedAt:   time.Unix(10, 0),
	}
	if err := store.SaveRelease(oldRelease); err != nil {
		t.Fatal(err)
	}
	var events []string
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	fakeDocker := &recordingDeployDocker{
		events:   &events,
		resolved: map[string]string{tag: digestRef},
	}
	agents := map[string]*scriptedDeployAgent{}
	installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent {
		a := agents[host.Name]
		if a == nil {
			a = &scriptedDeployAgent{
				host:   host.Name,
				events: &events,
				observed: []docker.ContainerSummary{{
					Names: "ship_demo_production_web_1_old-release",
					Labels: map[string]string{
						docker.LabelManagedBy:   docker.LabelManagedByValue,
						docker.LabelProject:     "demo",
						docker.LabelEnvironment: "production",
						docker.LabelService:     "web",
						docker.LabelReplica:     "1",
						docker.LabelRelease:     "old-release",
					},
				}},
			}
			agents[host.Name] = a
		}
		return a
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	wantOrder := []string{
		"agent:web-1:list_ship_containers",
		"agent:web-1:pull:" + digestRef,
		"agent:web-1:stop_keep:ship_demo_production_web_1_old-release",
		"agent:web-1:run:ship_demo_production_web_1_" + releaseID,
		"agent:web-1:health",
		"agent:web-1:remove:ship_demo_production_web_1_old-release",
	}
	joined := strings.Join(events, "\n")
	for _, want := range wantOrder {
		if !strings.Contains(joined, want) {
			t.Fatalf("events missing %q:\n%s", want, joined)
		}
	}
	pullAt := strings.Index(joined, "agent:web-1:pull:"+digestRef)
	stopAt := strings.Index(joined, "agent:web-1:stop_keep:ship_demo_production_web_1_old-release")
	runAt := strings.Index(joined, "agent:web-1:run:ship_demo_production_web_1_"+releaseID)
	healthAt := strings.Index(joined, "agent:web-1:health")
	removeAt := strings.Index(joined, "agent:web-1:remove:ship_demo_production_web_1_old-release")
	if !(pullAt < stopAt && stopAt < runAt && runAt < healthAt && healthAt < removeAt) {
		t.Fatalf("fixed-port deploy order is wrong:\n%s", joined)
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != releaseID || !current.Healthy || current.Status != state.ReleaseStatusHealthy {
		t.Fatalf("current release = %+v", current)
	}
	if _, err := os.Stat(filepath.Join(dir, config.LocalStateDir, "ingress", "production.Caddyfile")); err != nil {
		t.Fatalf("ingress file missing: %v", err)
	}
}

func TestDeployFromEmptyStateDoesNotStopConfiguredLiveAccessory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployWithAccessoryConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	observed := []docker.ContainerSummary{accessoryContainer("redis", "web-1", "Up 2 minutes")}
	fakeDocker := &recordingDeployDocker{events: &events}
	installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent {
		return &scriptedDeployAgent{host: host.Name, events: &events, observed: observed}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(events, "\n")
	accessoryName := accessorypkg.ContainerName("demo", "production", "redis")
	if strings.Contains(joined, "stop:"+accessoryName) {
		t.Fatalf("deploy stopped configured accessory from empty local state:\n%s", joined)
	}
	if strings.Contains(joined, "run:"+accessoryName) || strings.Contains(joined, "pull:redis:7-alpine") {
		t.Fatalf("deploy recreated already-running accessory:\n%s", joined)
	}
	if !strings.Contains(joined, "agent:web-1:run:ship_demo_production_web_1_abc123def456-20260630T183456.123456789Z") {
		t.Fatalf("deploy did not roll service:\n%s", joined)
	}
}

func TestDeployFailedPullMarksReleaseFailedAndKeepsCurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	fakeDocker := &recordingDeployDocker{
		events:   &events,
		resolved: map[string]string{tag: digestRef},
	}
	installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent {
		return &scriptedDeployAgent{host: host.Name, events: &events, failMethod: "pull"}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected deploy failure")
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "old-release" {
		t.Fatalf("current = %+v", current)
	}
	failed, err := store.ReadRelease(releaseID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != state.ReleaseStatusFailed || failed.Healthy {
		t.Fatalf("failed release = %+v", failed)
	}
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "deploy_rollout", "failed", releaseID) || !timelineContains(timeline, "deploy_mark_failed", "succeeded", releaseID) {
		t.Fatalf("timeline = %+v", timeline)
	}
}

func TestDeployFailedHealthDoesNotShiftTrafficOrCurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	fakeDocker := &recordingDeployDocker{
		events:   &events,
		resolved: map[string]string{tag: digestRef},
	}
	installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent {
		return &scriptedDeployAgent{
			host:   host.Name,
			events: &events,
			observed: []docker.ContainerSummary{{
				Names: "ship_demo_production_web_1_old-release",
				Labels: map[string]string{
					docker.LabelManagedBy:   docker.LabelManagedByValue,
					docker.LabelProject:     "demo",
					docker.LabelEnvironment: "production",
					docker.LabelService:     "web",
					docker.LabelReplica:     "1",
					docker.LabelRelease:     "old-release",
				},
			}},
			failMethod: "health_check",
		}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected deploy failure")
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "old-release" {
		t.Fatalf("current = %+v", current)
	}
	if _, err := os.Stat(filepath.Join(dir, config.LocalStateDir, "ingress", "production.Caddyfile")); !os.IsNotExist(err) {
		t.Fatalf("health failure wrote ingress: %v", err)
	}
	joined := strings.Join(events, "\n")
	stopAt := strings.Index(joined, "agent:web-1:stop_keep:ship_demo_production_web_1_old-release")
	runAt := strings.Index(joined, "agent:web-1:run:ship_demo_production_web_1_"+releaseID)
	if stopAt < 0 || runAt < 0 || stopAt > runAt {
		t.Fatalf("fixed-port health failure did not stop old before start:\n%s", joined)
	}
}

func TestDeployFailedHealthRestoresFixedPortContainerInventory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var releaseWrites []releaseStateWrite
	releaseID := "abc123def456-20260630T183456.123456789Z"
	oldName := "ship_demo_production_web_1_old-release"
	newName := "ship_demo_production_web_1_" + releaseID
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	fakeDocker := &recordingDeployDocker{events: &events, resolved: map[string]string{tag: digestRef}}
	fake := &inventoryDeployAgent{
		host:          "web-1",
		events:        &events,
		releaseWrites: &releaseWrites,
		containers: map[string]bool{
			oldName: true,
		},
		summaries: map[string]docker.ContainerSummary{
			oldName: serviceContainer("web-1", "web", 1, "old-release", "Up 1 minute"),
		},
		failHealth: true,
	}
	installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent { return fake })

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected deploy failure")
	}
	if running, ok := fake.containers[oldName]; !ok || !running {
		t.Fatalf("old container was not restored: %#v", fake.containers)
	}
	if _, ok := fake.containers[newName]; ok {
		t.Fatalf("failed candidate was not removed: %#v", fake.containers)
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "old-release" {
		t.Fatalf("current = %+v", current)
	}
	if len(releaseWrites) == 0 || releaseWrites[len(releaseWrites)-1].Release.Status != state.ReleaseStatusFailed {
		t.Fatalf("remote release writes = %+v", releaseWrites)
	}
}

func TestDeployPartialHostFailureLeavesPreviousCurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	configYAML := strings.Replace(deployBuildConfig(), "    ports: [3000]\n", "    ports: [3000]\n    health:\n      command: ./bin/health\n", 1)
	if err := os.WriteFile(path, []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var releaseWrites []releaseStateWrite
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	workerDigestRef := "registry.local/acme/worker@sha256:" + strings.Repeat("3", 64)
	fakeDocker := &recordingDeployDocker{
		events: &events,
		resolved: map[string]string{
			tag:                                 digestRef,
			"registry.local/acme/worker:stable": workerDigestRef,
		},
	}
	agents := map[string]*inventoryDeployAgent{}
	for i, host := range []string{"web-1", "web-2"} {
		replica := i + 1
		oldName := deployment.ContainerName("demo", "production", "web", replica, "old-release")
		agents[host] = &inventoryDeployAgent{
			host:          host,
			events:        &events,
			releaseWrites: &releaseWrites,
			containers:    map[string]bool{oldName: true},
			summaries: map[string]docker.ContainerSummary{
				oldName: serviceContainer(host, "web", replica, "old-release", "Up 1 minute"),
			},
			failHealth: host == "web-2",
		}
	}
	installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent { return agents[host.Name] })

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected deploy failure")
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "agent:web-1:run_container:ship_demo_production_web_1_"+releaseID) || !strings.Contains(joined, "agent:web-2:run_container:ship_demo_production_web_2_"+releaseID) {
		t.Fatalf("partial host events = %#v", events)
	}
	for i, host := range []string{"web-1", "web-2"} {
		replica := i + 1
		oldName := deployment.ContainerName("demo", "production", "web", replica, "old-release")
		newName := deployment.ContainerName("demo", "production", "web", replica, releaseID)
		if running, ok := agents[host].containers[oldName]; !ok || !running {
			t.Fatalf("%s old container was not restored: %#v", host, agents[host].containers)
		}
		if _, ok := agents[host].containers[newName]; ok {
			t.Fatalf("%s candidate was not removed: %#v", host, agents[host].containers)
		}
	}
	if strings.Contains(joined, ":write_file") {
		t.Fatalf("partial failure shifted ingress: %#v", events)
	}
	lastRemoteStatus := map[string]string{}
	for _, write := range releaseWrites {
		lastRemoteStatus[write.Host] = write.Release.Status
	}
	for _, host := range []string{"web-1", "web-2"} {
		if lastRemoteStatus[host] != state.ReleaseStatusFailed {
			t.Fatalf("remote release status on %s = %q, writes = %+v", host, lastRemoteStatus[host], releaseWrites)
		}
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "old-release" {
		t.Fatalf("current = %+v", current)
	}
	failed, err := store.ReadRelease(releaseID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != state.ReleaseStatusFailed {
		t.Fatalf("failed release = %+v", failed)
	}
}

func TestDeployFailedHealthRecordsCompensationFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	releaseID := "abc123def456-20260630T183456.123456789Z"
	oldName := deployment.ContainerName("demo", "production", "web", 1, "old-release")
	newName := deployment.ContainerName("demo", "production", "web", 1, releaseID)
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	fake := &inventoryDeployAgent{
		host:       "web-1",
		events:     &events,
		containers: map[string]bool{oldName: true},
		summaries: map[string]docker.ContainerSummary{
			oldName: serviceContainer("web-1", "web", 1, "old-release", "Up 1 minute"),
		},
		failHealth: true,
		failures: map[string]error{
			"remove_container:" + newName: errors.New("remove candidate failed"),
		},
	}
	installDeployHooks(t, &recordingDeployDocker{events: &events, resolved: map[string]string{tag: digestRef}}, func(host scheduler.Host) deployAgent { return fake })

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected deploy failure")
	}
	for _, want := range []string{"health " + newName + " on web-1 failed", "remove " + newName + " on web-1: remove candidate failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
	if running, ok := fake.containers[oldName]; !ok || !running {
		t.Fatalf("old container was not restored after cleanup failure: %#v", fake.containers)
	}
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range timeline {
		if event.Kind == "deploy_rollout" && event.Status == "failed" && strings.Contains(event.Message, "remove candidate failed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("timeline did not record compensation failure: %+v", timeline)
	}
}

func TestDeployCleanupWarningKeepsReleaseHealthyAndCurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "old-release", Environment: "production", Images: map[string]string{"web": "old"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var releaseWrites []releaseStateWrite
	releaseID := "abc123def456-20260630T183456.123456789Z"
	oldName := deployment.ContainerName("demo", "production", "web", 1, "old-release")
	newName := deployment.ContainerName("demo", "production", "web", 1, releaseID)
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	fake := &inventoryDeployAgent{
		host:          "web-1",
		events:        &events,
		releaseWrites: &releaseWrites,
		containers:    map[string]bool{oldName: true},
		summaries: map[string]docker.ContainerSummary{
			oldName: serviceContainer("web-1", "web", 1, "old-release", "Up 1 minute"),
		},
		failures: map[string]error{
			"remove_container:" + oldName: errors.New("remove preserved failed"),
		},
	}
	installDeployHooks(t, &recordingDeployDocker{events: &events, resolved: map[string]string{tag: digestRef}}, func(host scheduler.Host) deployAgent { return fake })

	var output bytes.Buffer
	cmd := deployCmd(&options{configPath: path})
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "remove preserved container "+oldName+" on web-1: remove preserved failed") {
		t.Fatalf("output = %q", output.String())
	}
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != releaseID || !current.Healthy || current.Status != state.ReleaseStatusHealthy {
		t.Fatalf("current release = %+v", current)
	}
	if running, ok := fake.containers[newName]; !ok || !running {
		t.Fatalf("candidate is not running: %#v", fake.containers)
	}
	if running, ok := fake.containers[oldName]; !ok || running {
		t.Fatalf("leaked preserved container should remain stopped: %#v", fake.containers)
	}
	if len(releaseWrites) == 0 || releaseWrites[len(releaseWrites)-1].Release.Status != state.ReleaseStatusHealthy {
		t.Fatalf("remote release writes = %+v", releaseWrites)
	}
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "deploy_cleanup", "warning", releaseID) || !timelineContains(timeline, "deploy", "succeeded", releaseID) {
		t.Fatalf("timeline = %+v", timeline)
	}
}

func TestPromoteRollsTargetEnvironmentWithSourceReleaseImages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(promoteConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	sourceImage := "registry.local/acme/demo@sha256:" + strings.Repeat("a", 64)
	if err := store.SaveRelease(state.Release{
		ID:          "staging-release",
		Environment: "staging",
		Images:      map[string]string{"web": sourceImage},
		ConfigHash:  "sha256:staging",
		CreatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRelease(state.Release{
		ID:          "production-old",
		Environment: "production",
		Images:      map[string]string{"web": "old-image"},
		ConfigHash:  "sha256:production-old",
		CreatedAt:   time.Unix(20, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var releaseWrites []releaseStateWrite
	fakeDocker := &recordingDeployDocker{events: &events}
	installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent {
		return &scriptedDeployAgent{host: host.Name, events: &events, releaseWrites: &releaseWrites}
	})

	var out bytes.Buffer
	cmd := promoteCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"staging", "production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	releaseID := "abc123def456-20260630T183456.123456789Z"
	current, err := store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != releaseID {
		t.Fatalf("production current = %+v", current)
	}
	sourceCurrent, err := store.CurrentRelease("staging")
	if err != nil {
		t.Fatal(err)
	}
	if sourceCurrent.ID != "staging-release" {
		t.Fatalf("staging current = %+v", sourceCurrent)
	}
	promoted, err := store.ReadRelease(releaseID)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Environment != "production" || promoted.Images["web"] != sourceImage || promoted.Status != state.ReleaseStatusHealthy {
		t.Fatalf("promoted release = %+v", promoted)
	}
	if len(fakeDocker.builds) != 0 {
		t.Fatalf("promote built images: %+v", fakeDocker.builds)
	}
	joined := strings.Join(events, "\n")
	for _, forbidden := range []string{"build:", "push:", "resolve:"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("promote performed build pipeline work %q:\n%s", forbidden, joined)
		}
	}
	for _, needle := range []string{
		"agent:prod-1:write_release_state:" + releaseID + ":pending",
		"agent:prod-1:pull:" + sourceImage,
		"agent:prod-1:run:ship_demo_production_web_1_" + releaseID,
		"agent:prod-1:write_release_state:" + releaseID + ":healthy",
	} {
		if !strings.Contains(joined, needle) {
			t.Fatalf("promote events missing %q:\n%s", needle, joined)
		}
	}
	if len(releaseWrites) != 2 || releaseWrites[0].Release.Status != state.ReleaseStatusPending || releaseWrites[1].Release.Status != state.ReleaseStatusHealthy {
		t.Fatalf("release writes = %+v", releaseWrites)
	}
	if !strings.Contains(out.String(), "promote staging release staging-release to production as "+releaseID) {
		t.Fatalf("promote output = %q", out.String())
	}
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "promote", "release_created", releaseID) || !timelineContains(timeline, "promote", "succeeded", releaseID) {
		t.Fatalf("timeline missing promote events: %+v", timeline)
	}
}

func TestDeployImageFailuresStopBeforeAgentMutation(t *testing.T) {
	for _, stage := range []string{"build", "push", "resolve"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, config.DefaultConfigFile)
			if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)

			var events []string
			fakeDocker := &recordingDeployDocker{events: &events, failStage: stage}
			installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent {
				return recordingDeployAgent{host: host.Name, events: &events}
			})

			cmd := deployCmd(&options{configPath: path})
			cmd.SetArgs([]string{"production"})
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected deploy failure")
			}
			for _, event := range events {
				// The protocol preflight (negotiate) is read-only and runs
				// before builds; only mutating agent calls matter here.
				if strings.HasPrefix(event, "agent:") && !strings.HasSuffix(event, ":negotiate") {
					t.Fatalf("agent mutated after %s failure: %#v", stage, events)
				}
			}
		})
	}
}

func TestDeployDryRunDoesNotBuildResolveOrCallAgents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	installDeployHooks(t, panicDeployDocker{t: t}, func(host scheduler.Host) deployAgent {
		t.Fatalf("unexpected agent client for %s", host.Name)
		return nil
	})

	var out bytes.Buffer
	cmd := deployCmd(&options{configPath: path, dryRun: true})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "resolve web: pushed image -> immutable digest") {
		t.Fatalf("dry-run output missing immutable resolve intent:\n%s", out.String())
	}
}

func TestDeployDryRunShowsIngressReloadTargetsWithoutAgents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	installDeployHooks(t, panicDeployDocker{t: t}, func(host scheduler.Host) deployAgent {
		t.Fatalf("unexpected agent client for %s", host.Name)
		return nil
	})

	var out bytes.Buffer
	cmd := deployCmd(&options{configPath: path, dryRun: true})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, needle := range []string{
		"ingress web: example.com",
		".ship/ingress/production.Caddyfile",
		"reload caddy on web-1 after validation",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("dry-run output missing %q:\n%s", needle, text)
		}
	}
}

func TestDeployPreservesMaintenanceIngress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(rollingDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	stateDir := filepath.Join(dir, config.LocalStateDir)
	if err := writeMaintenanceView(stateDir, maintenanceView{
		Environment: "production",
		Enabled:     true,
		Message:     "Back soon",
		UpdatedAt:   time.Unix(10, 0),
		Hosts:       []string{"web-1"},
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var writes []agent.WriteFileParams
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events, writes: &writes}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var caddyWrites []string
	for _, write := range writes {
		if strings.Contains(write.Path, "production.Caddyfile") {
			caddyWrites = append(caddyWrites, write.Content)
		}
	}
	if len(caddyWrites) < 2 {
		t.Fatalf("expected normal and maintenance caddy writes, got %#v", caddyWrites)
	}
	if !strings.Contains(caddyWrites[len(caddyWrites)-1], `respond "Back soon" 503`) || strings.Contains(caddyWrites[len(caddyWrites)-1], "reverse_proxy") {
		t.Fatalf("last caddy write did not preserve maintenance:\n%s", caddyWrites[len(caddyWrites)-1])
	}
	timeline, err := state.NewStore(stateDir).Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "maintenance", "preserved", "") {
		t.Fatalf("timeline missing maintenance preservation: %+v", timeline)
	}
}

func TestDeployDryRunValidatesMissingSecretsWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(secretDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	unsetEnv(t, "SHIP_TEST_DATABASE_URL")

	installDeployHooks(t, panicDeployDocker{t: t}, func(host scheduler.Host) deployAgent {
		t.Fatalf("unexpected agent client for %s", host.Name)
		return nil
	})

	var out bytes.Buffer
	cmd := deployCmd(&options{configPath: path, dryRun: true})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "missing secrets: SHIP_TEST_DATABASE_URL") {
		t.Fatalf("expected missing secret error, got %v", err)
	}
	if strings.Contains(out.String(), "postgres://") {
		t.Fatalf("dry-run leaked secret value:\n%s", out.String())
	}
}

func TestDeployMissingSecretsStopBeforeBuildResolveOrAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(secretDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	unsetEnv(t, "SHIP_TEST_DATABASE_URL")

	var events []string
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "missing secrets: SHIP_TEST_DATABASE_URL") {
		t.Fatalf("expected missing secret error, got %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("missing secret deploy touched docker or agent: %#v", events)
	}
}

func TestDeployWritesRemoteSecretFileAndStoresOnlyDigests(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(secretDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	storedSecret := "postgres://stored"
	secretValue := "postgres://user:pass@example/db"
	identityFile := writeEncryptedSecretStore(t, path, "production", map[string]string{
		"SHIP_TEST_DATABASE_URL": storedSecret,
	})
	t.Setenv("SHIP_TEST_DATABASE_URL", secretValue)

	var events []string
	var writes []agent.WriteFileParams
	var runs []agent.RunContainerParams
	imageRef := "registry.local/acme/web:stable"
	digestRef := "registry.local/acme/web@sha256:" + strings.Repeat("4", 64)
	installDeployHooks(t, &recordingDeployDocker{events: &events, resolved: map[string]string{imageRef: digestRef}}, func(host scheduler.Host) deployAgent {
		return &secretDeployAgent{host: host.Name, events: &events, writes: &writes, runs: &runs}
	})

	var out bytes.Buffer
	cmd := deployCmd(&options{configPath: path, secretsIdentityFile: identityFile})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "warning: process environment overrides encrypted store secrets: SHIP_TEST_DATABASE_URL") {
		t.Fatalf("deploy output missing secret override warning:\n%s", out.String())
	}

	secretPath := "/var/lib/ship/secrets/production/service-web.env"
	wantEvents := []string{
		"agent:web-1:negotiate",
		"resolve:" + imageRef,
		"agent:web-1:write_release_state:abc123def456-20260630T183456.123456789Z:pending",
		"agent:web-1:write_file:" + secretPath + ":0600",
		"agent:web-1:list_ship_containers",
		"agent:web-1:pull:" + digestRef,
		"agent:web-1:run:ship_demo_production_web_1_abc123def456-20260630T183456.123456789Z",
		"agent:web-1:write_release_state:abc123def456-20260630T183456.123456789Z:healthy",
	}
	if strings.Join(events, "\n") != strings.Join(wantEvents, "\n") {
		t.Fatalf("events = %#v, want %#v", events, wantEvents)
	}
	if len(writes) != 1 {
		t.Fatalf("writes = %#v", writes)
	}
	if writes[0].Path != secretPath || writes[0].Mode != 0o600 {
		t.Fatalf("write params = %+v", writes[0])
	}
	if writes[0].Content != "SHIP_TEST_DATABASE_URL="+secretValue+"\n" {
		t.Fatalf("secret file content = %q", writes[0].Content)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %#v", runs)
	}
	joinedArgs := strings.Join(runs[0].Args, " ")
	for _, needle := range []string{"-e RACK_ENV=production", "--env-file " + secretPath} {
		if !strings.Contains(joinedArgs, needle) {
			t.Fatalf("run args %q missing %q", joinedArgs, needle)
		}
	}
	releaseID := "abc123def456-20260630T183456.123456789Z"
	data, err := os.ReadFile(filepath.Join(dir, config.LocalStateDir, "releases", releaseID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secretValue) {
		t.Fatalf("release metadata leaked secret value: %s", data)
	}
	var release state.Release
	if err := json.Unmarshal(data, &release); err != nil {
		t.Fatal(err)
	}
	if release.SecretDigests["service-web:SHIP_TEST_DATABASE_URL"] != secretspkg.Digest(secretValue) {
		t.Fatalf("secret digests = %+v", release.SecretDigests)
	}
}

func TestDeployWritesRemoteReleaseStateToEveryHostWithoutSecretValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(secretMultiHostDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	secretValue := "postgres://user:pass@example/db"
	t.Setenv("SHIP_TEST_DATABASE_URL", secretValue)

	var events []string
	var releaseWrites []releaseStateWrite
	imageRef := "registry.local/acme/web:stable"
	digestRef := "registry.local/acme/web@sha256:" + strings.Repeat("5", 64)
	installDeployHooks(t, &recordingDeployDocker{events: &events, resolved: map[string]string{imageRef: digestRef}}, func(host scheduler.Host) deployAgent {
		return &secretDeployAgent{host: host.Name, events: &events, releaseWrites: &releaseWrites}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	seen := map[string]map[string]bool{}
	for _, write := range releaseWrites {
		if seen[write.Host] == nil {
			seen[write.Host] = map[string]bool{}
		}
		seen[write.Host][write.Release.Status] = true
		data, err := json.Marshal(write.Release)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), secretValue) {
			t.Fatalf("remote release metadata leaked secret value: %s", data)
		}
		if write.Release.SecretDigests["service-web:SHIP_TEST_DATABASE_URL"] != secretspkg.Digest(secretValue) {
			t.Fatalf("secret digests = %+v", write.Release.SecretDigests)
		}
		if write.Release.Images["web"] != digestRef {
			t.Fatalf("images = %+v", write.Release.Images)
		}
	}
	for _, host := range []string{"web-1", "web-2"} {
		if !seen[host][state.ReleaseStatusPending] || !seen[host][state.ReleaseStatusHealthy] {
			t.Fatalf("release writes by host = %+v", seen)
		}
	}
}

func TestRestartRecreatesCurrentReleaseReplicaAndRecordsEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(restartConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "release-1", Environment: "production", Images: map[string]string{"web": "registry.local/acme/web@sha256:" + strings.Repeat("1", 64)}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var runs []agent.RunContainerParams
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &restartDeployAgent{host: host.Name, events: &events, runs: &runs}
	})

	var out bytes.Buffer
	cmd := restartCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "web", "--replica", "2"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	wantName := deployment.ContainerName("demo", "production", "web", 2, "release-1")
	if len(runs) != 1 || runs[0].Name != wantName || runs[0].Image != "registry.local/acme/web@sha256:"+strings.Repeat("1", 64) {
		t.Fatalf("restart runs = %+v", runs)
	}
	if runs[0].Labels["com.example.team"] != "platform" || runs[0].Labels[docker.LabelService] != "web" {
		t.Fatalf("restart labels = %+v", runs[0].Labels)
	}
	if runs[0].Network != "ship-demo-production" {
		t.Fatalf("restart network = %q", runs[0].Network)
	}
	if strings.Join(runs[0].NetworkAliases, ",") != "app,web" {
		t.Fatalf("restart network aliases = %+v", runs[0].NetworkAliases)
	}
	args := strings.Join(runs[0].Args, " ")
	if !strings.Contains(args, "--env-file /var/lib/ship/secrets/production/service-web.env") || !strings.Contains(args, "-p 3000:3000") {
		t.Fatalf("restart args = %+v", runs[0].Args)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "agent:web-2:run:"+wantName) || !strings.Contains(joined, "agent:web-2:health_check:http://127.0.0.1:3000/up") {
		t.Fatalf("restart events = %#v", events)
	}
	if !strings.Contains(out.String(), "restarted "+wantName+" on web-2") {
		t.Fatalf("restart output = %q", out.String())
	}
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "restart", "started", "release-1") || !timelineContains(timeline, "restart", "succeeded", "release-1") {
		t.Fatalf("timeline missing restart events: %+v", timeline)
	}
}

func TestRestartAcceptsShorthandAndFullContainerName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(restartConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "release-1", Environment: "production", Images: map[string]string{"web": "registry.local/acme/web@sha256:" + strings.Repeat("1", 64)}, CreatedAt: time.Unix(10, 0)}); err != nil {
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
			var runs []agent.RunContainerParams
			installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
				return &restartDeployAgent{host: host.Name, events: &events, runs: &runs}
			})

			cmd := restartCmd(&options{configPath: path})
			cmd.SetArgs([]string{"production", tc.service})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if len(runs) != 1 || runs[0].Name != wantName {
				t.Fatalf("restart runs = %+v, want single run against %q", runs, wantName)
			}
		})
	}
}

func TestRestartUsesRemoteCurrentReleaseWhenLocalStateIsStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(restartConfig()), 0o644); err != nil {
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
		"web-2": {serviceContainer("web-2", "web", 2, "release-new", "Up 10 seconds")},
	}

	var events []string
	var runs []agent.RunContainerParams
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &restartDeployAgent{host: host.Name, events: &events, observed: observed, current: &remote, runs: &runs}
	})

	var out bytes.Buffer
	cmd := restartCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "web", "--replica", "1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	wantName := deployment.ContainerName("demo", "production", "web", 1, "release-new")
	if len(runs) != 1 || runs[0].Name != wantName || runs[0].Image != remote.Images["web"] {
		t.Fatalf("restart runs = %+v", runs)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "agent:web-1:read_release_state") || !strings.Contains(joined, "agent:web-2:read_release_state") {
		t.Fatalf("remote release state was not read:\n%s", joined)
	}
	if strings.Contains(joined, "release-old") || strings.Contains(out.String(), "release-old") {
		t.Fatalf("restart used stale local release:\nevents=%s\nout=%s", joined, out.String())
	}
}

func TestRestartStaleLocalReleaseFailsBeforeRunWhenRemoteCurrentUnknown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(restartConfig()), 0o644); err != nil {
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
	observed := map[string][]docker.ContainerSummary{
		"web-1": {serviceContainer("web-1", "web", 1, "release-new", "Up 10 seconds")},
	}

	var events []string
	var runs []agent.RunContainerParams
	installDeployHooks(t, &recordingDeployDocker{events: &events}, func(host scheduler.Host) deployAgent {
		return &restartDeployAgent{host: host.Name, events: &events, observed: observed, runs: &runs}
	})

	cmd := restartCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production", "web", "--replica", "1"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "could not determine current release") {
		t.Fatalf("expected current release resolution error, got %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("restart ran containers despite unknown current release: %+v", runs)
	}
	if strings.Contains(strings.Join(events, "\n"), ":run:") {
		t.Fatalf("restart mutated agents despite unknown current release: %#v", events)
	}
}

func TestRestartDryRunInspectsStateWithoutMutatingAgents(t *testing.T) {
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
		return recordingDeployAgent{host: host.Name, events: &events}
	})

	var out bytes.Buffer
	cmd := restartCmd(&options{configPath: path, dryRun: true})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "agent:web-1:list_ship_containers") || !strings.Contains(joined, "agent:web-2:list_ship_containers") {
		t.Fatalf("dry-run restart did not inspect observed state: %#v", events)
	}
	if strings.Contains(joined, ":run:") || strings.Contains(joined, ":health_check:") {
		t.Fatalf("dry-run restart mutated agents: %#v", events)
	}
	if !strings.Contains(out.String(), "would restart "+deployment.ContainerName("demo", "production", "web", 1, "release-1")) ||
		!strings.Contains(out.String(), "would restart "+deployment.ContainerName("demo", "production", "web", 2, "release-1")) {
		t.Fatalf("dry-run output = %q", out.String())
	}
}

func promoteConfig() string {
	return `project: demo
registry: registry.local/acme/demo

environments:
  staging:
    provider:
      manual: {}
    hosts:
      pools:
        web:
          hosts:
            - staging-1
  production:
    provider:
      manual: {}
    hosts:
      pools:
        web:
          hosts:
            - prod-1

services:
  web:
    image:
      build: .
    command: ./bin/server
    pool: web
    scale: 1
    ports: [3000]
`
}

func releaseCommandDeployConfig() string {
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
          count: 1

services:
  web:
    image:
      ref: registry.local/acme/web:stable
    command: ./bin/server
    pool: web
    scale: 1
    ports: [3000]
    logging:
      options:
        max-size: 10m
    volumes:
      - uploads:/app/uploads
    resources:
      cpus: "1"
      memory: 512m
    secrets:
      - SHIP_TEST_DATABASE_URL
    release:
      command: bin/rails db:migrate
      timeout_seconds: 600
`
}

func secretMultiHostDeployConfig() string {
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
    secrets:
      - SHIP_TEST_DATABASE_URL

secrets:
  - SHIP_TEST_DATABASE_URL
`
}

type methodNegotiationAgent struct {
	host            string
	events          *[]string
	supported       []string
	negotiateParams *agent.NegotiateParams
}

func (a *methodNegotiationAgent) Call(ctx context.Context, method string, params any, out any) error {
	*a.events = append(*a.events, fmt.Sprintf("agent:%s:%s", a.host, method))
	if method != "negotiate" {
		return fmt.Errorf("unexpected mutation %s", method)
	}
	if a.negotiateParams != nil {
		*a.negotiateParams = params.(agent.NegotiateParams)
	}
	if result, ok := out.(*agent.NegotiateResult); ok {
		*result = agent.NegotiateResult{
			AgentVersion:     agent.Version(),
			ProtocolVersion:  agent.AgentProtocol,
			SupportedMethods: append([]string(nil), a.supported...),
		}
	}
	return nil
}

type restartDeployAgent struct {
	host     string
	events   *[]string
	observed map[string][]docker.ContainerSummary
	current  *state.Release
	runs     *[]agent.RunContainerParams
}

func (r *restartDeployAgent) Call(ctx context.Context, method string, params any, out any) error {
	switch method {
	case "list_ship_containers":
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:list_ship_containers", r.host))
		if containers, ok := out.(*[]docker.ContainerSummary); ok {
			*containers = append([]docker.ContainerSummary(nil), r.observed[r.host]...)
		}
	case "read_release_state":
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:read_release_state", r.host))
		if r.current != nil {
			if release, ok := out.(*state.Release); ok {
				*release = *r.current
			}
		}
	case "ensure_network":
		return nil
	case "run_container":
		p := params.(agent.RunContainerParams)
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:run:%s", r.host, p.Name))
		if r.runs != nil {
			*r.runs = append(*r.runs, p)
		}
	case "health_check":
		p := params.(agent.HealthCheckParams)
		target := p.URL
		if target == "" {
			target = p.Command
		}
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:health_check:%s", r.host, target))
		if result, ok := out.(*agent.HealthCheckResult); ok {
			*result = agent.HealthCheckResult{OK: true}
		}
	default:
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:%s", r.host, method))
	}
	return nil
}

type inventoryDeployAgent struct {
	host          string
	events        *[]string
	containers    map[string]bool
	summaries     map[string]docker.ContainerSummary
	failHealth    bool
	failures      map[string]error
	releaseWrites *[]releaseStateWrite
}

func (a *inventoryDeployAgent) Call(ctx context.Context, method string, params any, out any) error {
	name := ""
	switch method {
	case "run_container":
		name = params.(agent.RunContainerParams).Name
	case "stop_container", "stop_container_keep", "start_container", "remove_container":
		name = params.(map[string]string)["name"]
	}
	event := fmt.Sprintf("agent:%s:%s", a.host, method)
	if name != "" {
		event += ":" + name
	}
	*a.events = append(*a.events, event)
	if err := a.failures[method+":"+name]; err != nil {
		return err
	}
	switch method {
	case "negotiate":
		if result, ok := out.(*agent.NegotiateResult); ok {
			*result = agent.NegotiateResult{
				AgentVersion:    agent.Version(),
				ProtocolVersion: agent.AgentProtocol,
				SupportedMethods: []string{
					"remove_container", "start_container", "stop_container_keep",
				},
			}
		}
	case "write_release_state":
		if a.releaseWrites != nil {
			p := params.(agent.WriteReleaseStateParams)
			*a.releaseWrites = append(*a.releaseWrites, releaseStateWrite{Host: a.host, Release: p.Release})
		}
	case "list_ship_containers":
		if containers, ok := out.(*[]docker.ContainerSummary); ok {
			for containerName, summary := range a.summaries {
				if running, exists := a.containers[containerName]; exists {
					if running {
						summary.Status = "Up 1 minute"
					} else {
						summary.Status = "Exited"
					}
					*containers = append(*containers, summary)
				}
			}
		}
	case "pull", "ensure_network", "write_file", "run_oneoff_container":
		return nil
	case "read_file":
		if result, ok := out.(*agent.ReadFileResult); ok {
			*result = agent.ReadFileResult{Exists: false}
		}
	case "docker_inspect":
		populateRunningDockerInspect(out)
	case "logs":
		if result, ok := out.(*map[string]string); ok {
			*result = map[string]string{"logs": ""}
		}
	case "run_container":
		p := params.(agent.RunContainerParams)
		a.containers[p.Name] = true
		a.summaries[p.Name] = docker.ContainerSummary{Names: p.Name, Image: p.Image, Status: "Up", Labels: p.Labels}
	case "health_check":
		if result, ok := out.(*agent.HealthCheckResult); ok {
			*result = agent.HealthCheckResult{OK: !a.failHealth}
		}
	case "stop_container":
		delete(a.containers, name)
		delete(a.summaries, name)
	case "stop_container_keep":
		if _, ok := a.containers[name]; ok {
			a.containers[name] = false
		}
	case "start_container":
		if _, ok := a.containers[name]; !ok {
			return fmt.Errorf("container %s does not exist", name)
		}
		a.containers[name] = true
	case "remove_container":
		delete(a.containers, name)
		delete(a.summaries, name)
	default:
		return fmt.Errorf("unexpected method %s", method)
	}
	return nil
}
