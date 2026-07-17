package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	accessorypkg "github.com/watzon/ship/internal/accessory"
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

func TestRepointHostFactUpdatesEveryPoolFact(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveHostFacts("production", []state.HostFact{
		{Name: "npxray-production", Pool: "app", User: "root", PublicAddress: "203.0.113.10", Provider: "manual"},
		{Name: "npxray-production", Pool: "ingress", User: "root", PublicAddress: "203.0.113.10", Provider: "manual"},
	}); err != nil {
		t.Fatal(err)
	}
	source := scheduler.Host{Name: "npxray-production", Pool: "app", User: "root"}
	created := providerpkg.Host{ID: "42", Name: "npxray-production-m20260716", PublicAddress: "192.0.2.9"}
	oldFact, err := repointHostFact(store, "production", source, "vultr", created)
	if err != nil {
		t.Fatal(err)
	}
	if oldFact.PublicAddress != "203.0.113.10" {
		t.Fatalf("old fact = %+v", oldFact)
	}
	facts, err := store.ReadHostFacts("production")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("facts = %+v", facts)
	}
	for _, fact := range facts {
		if fact.PublicAddress != "192.0.2.9" {
			t.Fatalf("pool %s fact still points at %s", fact.Pool, fact.PublicAddress)
		}
	}
}

func TestRepointHostFactCreatesFactWhenNoneExist(t *testing.T) {
	dir := t.TempDir()
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	source := scheduler.Host{Name: "butterbase-staging", Pool: "web", User: "root"}
	created := providerpkg.Host{ID: "7", Name: "butterbase-staging-m20260716", PublicAddress: "192.0.2.7"}
	if _, err := repointHostFact(store, "staging", source, "vultr", created); err != nil {
		t.Fatal(err)
	}
	facts, err := store.ReadHostFacts("staging")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].Name != "butterbase-staging" || facts[0].PublicAddress != "192.0.2.7" {
		t.Fatalf("facts = %+v", facts)
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

type migrateFailureHarness struct {
	t                *testing.T
	path             string
	store            state.Store
	events           []string
	infra            *acceptanceFakeInfra
	provider         *acceptanceHetznerAPI
	initialFacts     []state.HostFact
	initialAccessory state.AccessoryState
	initialRelease   state.Release
	initialCreated   int
}

func newMigrateFailureHarness(t *testing.T) *migrateFailureHarness {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(acceptanceFakeInfraConfigYAML(sampleAppPath(t))), 0o644); err != nil {
		t.Fatal(err)
	}

	h := &migrateFailureHarness{t: t, path: path}
	h.infra = newAcceptanceFakeInfra(t, &h.events)
	installAcceptanceDeployHooks(t, &acceptanceDocker{events: &h.events}, h.infra.agent)
	installBootstrapHooks(t, &h.events)
	installMigrateCopyHooks(t, &h.events)

	h.provider = newAcceptanceHetznerAPI(t)
	originalProvider := newEnvironmentProvider
	newEnvironmentProvider = func(_ config.Environment, dryRun bool) (providerpkg.Provider, error) {
		return hetzner.Client{
			Token:        "acceptance-token",
			DryRun:       dryRun,
			HTTP:         h.provider.server.Client(),
			BaseURL:      h.provider.server.URL,
			PollInterval: time.Nanosecond,
		}, nil
	}
	t.Cleanup(func() { newEnvironmentProvider = originalProvider })

	runAcceptanceCommand(t, provisionCmd(&options{configPath: path}), "apply", "production", "--yes")
	runAcceptanceCommand(t, deployCmd(&options{configPath: path}), "production")
	h.store = state.NewStore(filepath.Join(dir, config.LocalStateDir))
	h.initialRelease = currentAcceptanceRelease(t, h.store)
	var err error
	h.initialFacts, err = h.store.ReadHostFacts("production")
	if err != nil {
		t.Fatal(err)
	}
	h.initialAccessory, err = h.store.ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	h.initialCreated = len(h.provider.createdIDs)
	return h
}

func (h *migrateFailureHarness) fact(name string) state.HostFact {
	h.t.Helper()
	facts, err := h.store.ReadHostFacts("production")
	if err != nil {
		h.t.Fatal(err)
	}
	for _, fact := range facts {
		if fact.Name == name {
			return fact
		}
	}
	h.t.Fatalf("host fact %q not found in %+v", name, facts)
	return state.HostFact{}
}

func (h *migrateFailureHarness) initialFact(name string) state.HostFact {
	h.t.Helper()
	for _, fact := range h.initialFacts {
		if fact.Name == name {
			return fact
		}
	}
	h.t.Fatalf("initial host fact %q not found in %+v", name, h.initialFacts)
	return state.HostFact{}
}

func (h *migrateFailureHarness) hasContainer(contact, name string) bool {
	h.t.Helper()
	for _, container := range h.infra.containersFor(contact) {
		if container.Names == name {
			return true
		}
	}
	return false
}

func eventIndexContaining(events []string, needle string) int {
	for i, event := range events {
		if strings.Contains(event, needle) {
			return i
		}
	}
	return -1
}

func outputLineWithPrefix(output, prefix string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

func TestMigrateAccessoryFailureAfterStateSaveReportsResidualState(t *testing.T) {
	h := newMigrateFailureHarness(t)
	migrationEventsStart := len(h.events)

	originalRepoint := migrateRepointHostFact
	migrateRepointHostFact = func(state.Store, string, scheduler.Host, string, providerpkg.Host) (state.HostFact, error) {
		return state.HostFact{}, errors.New("injected host-fact repoint failure")
	}
	t.Cleanup(func() { migrateRepointHostFact = originalRepoint })

	out, err := runAcceptanceCommandError(t, migrateCmd(&options{configPath: h.path}), "production", "data-1", "--yes")
	if err == nil || !strings.Contains(err.Error(), "injected host-fact repoint failure") {
		t.Fatalf("migration error = %v, want injected host-fact repoint failure\n%s", err, out)
	}

	if got, want := len(h.provider.createdIDs), h.initialCreated+1; got != want {
		t.Fatalf("created provider server IDs = %v, want one replacement after %d baseline servers", h.provider.createdIDs, h.initialCreated)
	}
	if len(h.provider.deletedIDs) != 0 {
		t.Fatalf("provider deleted server IDs before host-fact repoint completed: %v", h.provider.deletedIDs)
	}
	if _, ok := h.provider.servers["data-1"]; !ok {
		t.Fatalf("old provider server is absent: %+v", h.provider.servers)
	}
	var replacementName string
	for name := range h.provider.servers {
		if strings.HasPrefix(name, "data-1-m") {
			replacementName = name
			break
		}
	}
	if replacementName == "" {
		t.Fatalf("replacement provider server is absent: %+v", h.provider.servers)
	}
	replacement := h.provider.servers[replacementName]
	newContact := replacement.PublicNet.IPv4.IP
	oldFact := h.initialFact("data-1")
	if got := h.fact("data-1"); got.PublicAddress != oldFact.PublicAddress || got.ProviderID != oldFact.ProviderID || got.ProviderName != oldFact.ProviderName {
		t.Fatalf("host fact changed before repoint completed: got %+v, want old fact %+v", got, oldFact)
	}

	accessoryState, err := h.store.ReadAccessoryState("production", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if accessoryState.Host.PublicAddress != newContact || accessoryState.LastRestore == nil || accessoryState.LastRestore.Host != "data-1" {
		t.Fatalf("accessory state does not point at restored replacement %s: %+v", newContact, accessoryState)
	}
	containerName := accessorypkg.ContainerName("sample", "production", "postgres")
	if h.hasContainer(oldFact.PublicAddress, containerName) {
		t.Fatalf("old accessory container %s is still running on %s", containerName, oldFact.PublicAddress)
	}
	if !h.hasContainer(newContact, containerName) {
		t.Fatalf("replacement accessory container %s is not running on %s", containerName, newContact)
	}

	current, err := h.store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current, h.initialRelease) {
		t.Fatalf("current release changed after migration failure: got %+v, want %+v", current, h.initialRelease)
	}
	stateEvents, err := h.store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	var migrateFailed, accessorySucceeded bool
	for _, event := range stateEvents {
		if event.Kind == "migrate" && event.Status == "failed" && strings.Contains(event.Message, "injected host-fact repoint failure") {
			migrateFailed = true
		}
		if event.Kind == "migrate_accessory" && event.Status == "succeeded" && event.Accessory == "postgres" {
			accessorySucceeded = true
		}
	}
	if !migrateFailed || !accessorySucceeded {
		t.Fatalf("migration events do not record the residual transition: %+v", stateEvents)
	}

	migrationEvents := h.events[migrationEventsStart:]
	backupAt := eventIndexContaining(migrationEvents, "agent:data-1:accessory_backup")
	copyAt := eventIndexContaining(migrationEvents, "copy-artifact:data-1:")
	restoreAt := eventIndexContaining(migrationEvents, "agent:data-1:accessory_restore")
	stopAt := eventIndexContaining(migrationEvents, "agent:data-1:stop:"+containerName)
	if backupAt < 0 || copyAt <= backupAt || restoreAt <= copyAt || stopAt <= restoreAt {
		t.Fatalf("accessory migration order = %v", migrationEvents)
	}

	note := outputLineWithPrefix(out, "note: replacement server ")
	if note == "" {
		t.Fatalf("migration output has no recovery note:\n%s", out)
	}
	if !strings.Contains(note, "accessory postgres") || !strings.Contains(note, oldFact.PublicAddress) || strings.Contains(note, "delete it there before retrying") {
		t.Fatalf("cleanup note is unsafe for the saved residual state; accessory postgres points at %s, its old container on %s is stopped, and the note says %q", newContact, oldFact.PublicAddress, note)
	}
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
