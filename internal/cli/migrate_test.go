package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/watzon/ship/internal/config"
	providerpkg "github.com/watzon/ship/internal/provider"
	"github.com/watzon/ship/internal/provider/hetzner"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/state"
)

func writeConfigContent(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

type creatingProvider struct {
	*recordingProvider
	createdPlans []providerpkg.HostPlan
	createHost   providerpkg.Host
}

func (p *creatingProvider) CreateHost(_ context.Context, _, _ string, _ config.Environment, plan providerpkg.HostPlan) (providerpkg.Host, error) {
	p.createdPlans = append(p.createdPlans, plan)
	host := p.createHost
	if host.Name == "" {
		host.Name = plan.Name
	}
	if host.Labels == nil {
		host.Labels = plan.Labels
	}
	return host, nil
}

func TestMigrateRequiresYes(t *testing.T) {
	path := writeConfigContent(t, singleHostConfig())
	cmd := migrateCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production", "web-1"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v, want --yes requirement", err)
	}
}

func TestMigrateUnknownHostFails(t *testing.T) {
	path := writeConfigContent(t, singleHostConfig())
	prov := &creatingProvider{recordingProvider: &recordingProvider{
		plans: []providerpkg.HostPlan{{Name: "web-1", Pool: "web"}},
	}}
	installProviderHook(t, prov)
	cmd := migrateCmd(&options{configPath: path, dryRun: true})
	cmd.SetArgs([]string{"production", "nope-1"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "not in the production inventory") {
		t.Fatalf("err = %v, want unknown host error", err)
	}
}

func TestMigrateRequiresCreateCapableProvider(t *testing.T) {
	path := writeConfigContent(t, manualHostConfig())
	cmd := migrateCmd(&options{configPath: path, dryRun: true})
	cmd.SetArgs([]string{"production", "web-1.example.com"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "cannot create servers") {
		t.Fatalf("err = %v, want create capability error", err)
	}
}

func TestMigrateDryRunPrintsPlanWithoutCreating(t *testing.T) {
	path := writeConfigContent(t, singleHostConfig())
	prov := &creatingProvider{recordingProvider: &recordingProvider{
		plans: []providerpkg.HostPlan{{Name: "web-1", Pool: "web", Location: "ash", Size: "cpx31", Image: "ubuntu-24.04"}},
	}}
	installProviderHook(t, prov)
	out, err := runAcceptanceCommandError(t, migrateCmd(&options{configPath: path, dryRun: true}), "production", "web-1")
	if err != nil {
		t.Fatalf("dry-run migrate failed: %v\n%s", err, out)
	}
	assertAcceptanceOutput(t, out,
		"would provision replacement server web-1-m",
		"would start 1 replica(s) of web from the current release",
		"would stop Ship-managed workloads on the old server",
		"would delete the old server for host web-1",
	)
	if len(prov.createdPlans) != 0 {
		t.Fatalf("dry-run created servers: %+v", prov.createdPlans)
	}
}

func TestParseAccessoryArtifacts(t *testing.T) {
	parsed, err := parseAccessoryArtifacts([]string{"postgres=/tmp/pg.backup"}, []string{"postgres"})
	if err != nil || parsed["postgres"] != "/tmp/pg.backup" {
		t.Fatalf("parsed = %v err = %v", parsed, err)
	}
	if _, err := parseAccessoryArtifacts([]string{"postgres"}, []string{"postgres"}); err == nil {
		t.Fatal("expected error for missing path")
	}
	if _, err := parseAccessoryArtifacts([]string{"redis=/tmp/r.backup"}, []string{"postgres"}); err == nil {
		t.Fatal("expected error for accessory not on host")
	}
}

func TestValidateMigrateAccessories(t *testing.T) {
	yes := true
	cfg := &config.Config{Accessories: map[string]config.Accessory{
		"good": {Primary: &yes, Backup: config.BackupSpec{
			Required:       &yes,
			Command:        "pg_dumpall",
			RestoreCommand: "psql",
		}},
		"bad": {},
	}}
	if err := validateMigrateAccessories(cfg, []string{"good"}); err != nil {
		t.Fatalf("good accessory blocked: %v", err)
	}
	err := validateMigrateAccessories(cfg, []string{"good", "bad"})
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("err = %v, want bad accessory blocked", err)
	}
}

func TestHostFactsFromReconcileMatchesReplacementByLabel(t *testing.T) {
	facts := hostFactsFromReconcile("hetzner", providerpkg.ReconcileResult{
		Desired: []providerpkg.HostPlan{{Name: "web-1", Pool: "web", User: "root"}},
		Existing: []providerpkg.Host{{
			ID:            "105",
			Name:          "web-1-m20260630180101",
			PublicAddress: "192.0.2.5",
			Labels:        map[string]string{providerpkg.LabelHost: "web-1"},
		}},
	})
	if len(facts) != 1 {
		t.Fatalf("facts = %+v", facts)
	}
	fact := facts[0]
	if fact.Name != "web-1" || fact.ProviderName != "web-1-m20260630180101" || fact.PublicAddress != "192.0.2.5" || fact.ServerID != 105 {
		t.Fatalf("fact = %+v", fact)
	}
}

func installMigrateCopyHooks(t *testing.T, events *[]string) {
	t.Helper()
	originalCopy := copyRemoteArtifact
	originalUpload := uploadLocalArtifact
	copyRemoteArtifact = func(_ context.Context, source scheduler.Host, _ string, dst scheduler.Host, _ string, _ bool) error {
		*events = append(*events, fmt.Sprintf("copy-artifact:%s:%s->%s", source.Name, source.Contact, dst.Contact))
		return nil
	}
	uploadLocalArtifact = func(_ context.Context, dst scheduler.Host, localPath, _ string, _ bool) error {
		*events = append(*events, fmt.Sprintf("upload-artifact:%s:%s", dst.Name, localPath))
		return nil
	}
	t.Cleanup(func() {
		copyRemoteArtifact = originalCopy
		uploadLocalArtifact = originalUpload
	})
}

func TestAcceptanceMigrateHostWorkflow(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(acceptanceFakeInfraConfigYAML(sampleAppPath(t))), 0o644); err != nil {
		t.Fatal(err)
	}

	var events []string
	fakeDocker := &acceptanceDocker{events: &events}
	fakeInfra := newAcceptanceFakeInfra(t, &events)
	installAcceptanceDeployHooks(t, fakeDocker, fakeInfra.agent)
	installBootstrapHooks(t, &events)
	installMigrateCopyHooks(t, &events)

	fakeHetzner := newAcceptanceHetznerAPI(t)
	originalNewEnvironmentProvider := newEnvironmentProvider
	newEnvironmentProvider = func(_ config.Environment, dryRun bool) (providerpkg.Provider, error) {
		return hetzner.Client{
			Token:        "acceptance-token",
			DryRun:       dryRun,
			HTTP:         fakeHetzner.server.Client(),
			BaseURL:      fakeHetzner.server.URL,
			PollInterval: time.Nanosecond,
		}, nil
	}
	t.Cleanup(func() {
		newEnvironmentProvider = originalNewEnvironmentProvider
	})

	runAcceptanceCommand(t, provisionCmd(&options{configPath: path}), "apply", "production", "--yes")
	runAcceptanceCommand(t, deployCmd(&options{configPath: path}), "production")
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	release := currentAcceptanceRelease(t, store)
	serversAfterDeploy := len(fakeHetzner.createdNames())

	// Migrate a service host: replacement provisioned, replicas restarted on
	// it from the current release, old server deleted.
	out := runAcceptanceCommand(t, migrateCmd(&options{configPath: path}), "production", "web-1", "--yes")
	assertAcceptanceOutput(t, out,
		"provisioned replacement server web-1-m",
		"bootstrapped web-1-m",
		"started release "+release.ID+" replicas on web-1",
		"deleted old server web-1",
		"migrated host web-1: 192.0.2.2 ->",
	)
	facts, err := store.ReadHostFacts("production")
	if err != nil {
		t.Fatal(err)
	}
	var webFact state.HostFact
	for _, fact := range facts {
		if fact.Name == "web-1" {
			webFact = fact
		}
	}
	if !strings.HasPrefix(webFact.ProviderName, "web-1-m") {
		t.Fatalf("web-1 fact not repointed: %+v", webFact)
	}
	if webFact.PublicAddress == "" || webFact.PublicAddress == "192.0.2.2" {
		t.Fatalf("web-1 fact address not updated: %+v", webFact)
	}
	assertAcceptanceEvent(t, events, "agent:web-1:contact:"+webFact.PublicAddress)
	assertAcceptanceEvent(t, events, "agent:web-1:run:ship_sample_production_web_1_"+release.ID)
	assertAcceptanceEvent(t, events, "agent:web-1:stop:ship_sample_production_web_1_"+release.ID)
	if !slices.Contains(fakeHetzner.deleted, "web-1") {
		t.Fatalf("old web-1 server not deleted: %v", fakeHetzner.deleted)
	}

	// Migrate the accessory host: fresh backup on the old server, artifact
	// transferred, restore on the replacement, old accessory stopped.
	out = runAcceptanceCommand(t, migrateCmd(&options{configPath: path}), "production", "data-1", "--yes")
	assertAcceptanceOutput(t, out,
		"provisioned replacement server data-1-m",
		"transferred backup artifact for accessory postgres",
		"moved accessory postgres to the replacement server",
		"deleted old server data-1",
	)
	assertAcceptanceEvent(t, events, "agent:data-1:accessory_backup")
	assertAcceptanceEvent(t, events, "copy-artifact:data-1:192.0.2.1->")
	assertAcceptanceEvent(t, events, "agent:data-1:accessory_restore")
	if !slices.Contains(fakeHetzner.deleted, "data-1") {
		t.Fatalf("old data-1 server not deleted: %v", fakeHetzner.deleted)
	}
	accessoryState, err := store.ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if accessoryState.Host.Name != "data-1" || accessoryState.Host.PublicAddress == "192.0.2.1" {
		t.Fatalf("accessory placement not moved: %+v", accessoryState.Host)
	}
	if accessoryState.LastRestore == nil {
		t.Fatalf("accessory restore not recorded: %+v", accessoryState)
	}

	// Reconciliation keeps matching the replacements to their logical hosts,
	// so a later provision apply must not create duplicate servers.
	out = runAcceptanceCommand(t, provisionCmd(&options{configPath: path}), "apply", "production", "--yes")
	if got := len(fakeHetzner.createdNames()); got != serversAfterDeploy+2 {
		t.Fatalf("provision apply after migrate created servers: %v", fakeHetzner.createdNames())
	}
	assertAcceptanceOutput(t, out, "exists web-1-m", "exists data-1-m", "exists web-2", "exists worker-1")
}
