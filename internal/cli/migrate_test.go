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
	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/deployment"
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
	agentCalls       []migrateAgentCall
	agentFailure     func(migrateAgentCall) error
	infra            *acceptanceFakeInfra
	provider         *acceptanceHetznerAPI
	initialFacts     []state.HostFact
	initialAccessory state.AccessoryState
	initialRelease   state.Release
	initialCreated   int
}

type migrateAgentCall struct {
	Host      scheduler.Host
	Method    string
	Container string
}

type migrateFailurePoint string

const (
	migrateFailCreate               migrateFailurePoint = "provider create"
	migrateFailPublicAddress        migrateFailurePoint = "public address"
	migrateFailBinaryResolve        migrateFailurePoint = "binary resolve"
	migrateFailBootstrap            migrateFailurePoint = "bootstrap"
	migrateFailReplacementPreflight migrateFailurePoint = "replacement preflight"
	migrateFailAccessoryBackup      migrateFailurePoint = "accessory backup"
	migrateFailArtifactCopy         migrateFailurePoint = "artifact copy"
	migrateFailAccessoryRestore     migrateFailurePoint = "accessory restore"
	migrateFailOldAccessoryStop     migrateFailurePoint = "old accessory stop"
	migrateFailHostFactRepoint      migrateFailurePoint = "host fact repoint"
	migrateFailHostsAfterRepoint    migrateFailurePoint = "hosts after repoint"
	migrateFailServiceRollout       migrateFailurePoint = "service rollout"
	migrateFailAccessoryRestart     migrateFailurePoint = "accessory restart"
	migrateFailOldWorkloadStop      migrateFailurePoint = "old workload stop"
	migrateFailProviderDelete       migrateFailurePoint = "provider delete"
)

type migrateFailingBootstrapSSH struct {
	delegate bootstrapSSH
	failed   *bool
}

func (s migrateFailingBootstrapSSH) Run(ctx context.Context, command string) (string, error) {
	if strings.TrimSpace(command) != "true" && !*s.failed {
		*s.failed = true
		return "", errors.New("injected bootstrap failure")
	}
	return s.delegate.Run(ctx, command)
}

func (s migrateFailingBootstrapSSH) RunWithStdin(ctx context.Context, command, stdin string) (string, error) {
	return s.delegate.RunWithStdin(ctx, command, stdin)
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
	installAcceptanceDeployHooks(t, &acceptanceDocker{events: &h.events}, h.agent)
	installBootstrapHooks(t, &h.events)
	installMigrateCopyHooks(t, &h.events)

	h.provider = newAcceptanceHetznerAPI(t)
	h.provider.events = &h.events
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
	h.events = nil
	h.agentCalls = nil
	return h
}

func (h *migrateFailureHarness) agent(host scheduler.Host) deployAgent {
	delegate := h.infra.agent(host)
	return deployAgentFunc(func(ctx context.Context, method string, params any, out any) error {
		call := migrateAgentCall{Host: host, Method: method, Container: migrateAgentContainer(method, params)}
		h.agentCalls = append(h.agentCalls, call)
		if h.agentFailure != nil {
			if err := h.agentFailure(call); err != nil {
				h.events = append(h.events, fmt.Sprintf("agent-failure:%s:%s:%s:%s", host.Name, host.ContactTarget(), method, call.Container))
				return err
			}
		}
		return delegate.Call(ctx, method, params, out)
	})
}

func migrateAgentContainer(method string, params any) string {
	switch method {
	case "run_container":
		if value, ok := params.(agent.RunContainerParams); ok {
			return value.Name
		}
	case "stop_container":
		if value, ok := params.(map[string]string); ok {
			return value["name"]
		}
	}
	return ""
}

func (h *migrateFailureHarness) inject(point migrateFailurePoint, hostName string) {
	h.t.Helper()
	oldContact := h.initialFact(hostName).PublicAddress
	switch point {
	case migrateFailCreate:
		h.provider.failCreate = true
	case migrateFailPublicAddress:
		h.provider.createWithoutPublicAddress = true
	case migrateFailBinaryResolve:
		original := resolveShipBinaryForHost
		resolveShipBinaryForHost = func(context.Context, scheduler.Host, *options) ([]byte, error) {
			return nil, errors.New("injected binary resolution failure")
		}
		h.t.Cleanup(func() { resolveShipBinaryForHost = original })
	case migrateFailBootstrap:
		original := newBootstrapSSH
		failed := false
		newBootstrapSSH = func(host scheduler.Host, dryRun bool) bootstrapSSH {
			return migrateFailingBootstrapSSH{delegate: original(host, dryRun), failed: &failed}
		}
		h.t.Cleanup(func() { newBootstrapSSH = original })
	case migrateFailReplacementPreflight:
		h.agentFailure = func(call migrateAgentCall) error {
			if call.Method == "negotiate" && call.Host.ContactTarget() != oldContact {
				return errors.New("injected replacement preflight failure")
			}
			return nil
		}
	case migrateFailAccessoryBackup:
		h.agentFailure = func(call migrateAgentCall) error {
			if call.Method == "accessory_backup" && call.Host.ContactTarget() == oldContact {
				return errors.New("injected accessory backup failure")
			}
			return nil
		}
	case migrateFailArtifactCopy:
		original := copyRemoteArtifact
		copyRemoteArtifact = func(context.Context, scheduler.Host, string, scheduler.Host, string, bool) error {
			h.events = append(h.events, "copy-artifact:injected-failure")
			return errors.New("injected artifact copy failure")
		}
		h.t.Cleanup(func() { copyRemoteArtifact = original })
	case migrateFailAccessoryRestore:
		h.agentFailure = func(call migrateAgentCall) error {
			if call.Method == "accessory_restore" && call.Host.ContactTarget() != oldContact {
				return errors.New("injected accessory restore failure")
			}
			return nil
		}
	case migrateFailOldAccessoryStop:
		containerName := accessorypkg.ContainerName("sample", "production", "postgres")
		h.agentFailure = func(call migrateAgentCall) error {
			if call.Method == "stop_container" && call.Container == containerName && call.Host.ContactTarget() == oldContact {
				return errors.New("injected old accessory stop failure")
			}
			return nil
		}
	case migrateFailHostFactRepoint:
		original := migrateRepointHostFact
		migrateRepointHostFact = func(state.Store, string, scheduler.Host, string, providerpkg.Host) (state.HostFact, error) {
			return state.HostFact{}, errors.New("injected host-fact repoint failure")
		}
		h.t.Cleanup(func() { migrateRepointHostFact = original })
	case migrateFailHostsAfterRepoint:
		original := migrateResolvedHostsForEnvironment
		calls := 0
		migrateResolvedHostsForEnvironment = func(store state.Store, envName string, env config.Environment) ([]scheduler.Host, error) {
			calls++
			if calls == 2 {
				return nil, errors.New("injected host re-resolution failure")
			}
			return original(store, envName, env)
		}
		h.t.Cleanup(func() { migrateResolvedHostsForEnvironment = original })
	case migrateFailServiceRollout:
		h.agentFailure = func(call migrateAgentCall) error {
			if call.Method == "run_container" && call.Host.ContactTarget() != oldContact && strings.Contains(call.Container, "_web_1_") {
				return errors.New("injected service rollout failure")
			}
			return nil
		}
	case migrateFailAccessoryRestart:
		h.agentFailure = func(call migrateAgentCall) error {
			if call.Method == "run_container" && strings.Contains(call.Container, "_web_") {
				return errors.New("injected accessory restart failure")
			}
			return nil
		}
	case migrateFailOldWorkloadStop:
		h.agentFailure = func(call migrateAgentCall) error {
			if call.Method == "stop_container" && call.Host.ContactTarget() == oldContact && strings.Contains(call.Container, "_web_1_") {
				return errors.New("injected old workload stop failure")
			}
			return nil
		}
	case migrateFailProviderDelete:
		h.provider.failDelete = true
	default:
		h.t.Fatalf("unknown migration failure point %q", point)
	}
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

func (h *migrateFailureHarness) replacement(hostName string) (hetzner.Server, bool) {
	h.t.Helper()
	for name, server := range h.provider.servers {
		if strings.HasPrefix(name, hostName+"-m") {
			return server, true
		}
	}
	return hetzner.Server{}, false
}

func (h *migrateFailureHarness) assertInitialFacts() {
	h.t.Helper()
	facts, err := h.store.ReadHostFacts("production")
	if err != nil {
		h.t.Fatal(err)
	}
	if !reflect.DeepEqual(facts, h.initialFacts) {
		h.t.Fatalf("host facts changed: got %+v, want %+v", facts, h.initialFacts)
	}
}

func (h *migrateFailureHarness) assertReleaseUnchanged() {
	h.t.Helper()
	current, err := h.store.CurrentRelease("production")
	if err != nil {
		h.t.Fatal(err)
	}
	if !reflect.DeepEqual(current, h.initialRelease) {
		h.t.Fatalf("current release changed: got %+v, want %+v", current, h.initialRelease)
	}
}

func (h *migrateFailureHarness) assertAccessoryHost(contact string, wantOldRunning, wantReplacementRunning bool) {
	h.t.Helper()
	saved, err := h.store.ReadAccessoryState("production", "postgres")
	if err != nil {
		h.t.Fatal(err)
	}
	if saved.Host.PublicAddress != contact {
		h.t.Fatalf("accessory state contact = %q, want %q: %+v", saved.Host.PublicAddress, contact, saved)
	}
	containerName := accessorypkg.ContainerName("sample", "production", "postgres")
	oldContact := h.initialAccessory.Host.PublicAddress
	if got := h.hasContainer(oldContact, containerName); got != wantOldRunning {
		h.t.Fatalf("old accessory running on %s = %t, want %t", oldContact, got, wantOldRunning)
	}
	if replacement, ok := h.replacement("data-1"); ok && replacement.PublicNet.IPv4.IP != "" {
		newContact := replacement.PublicNet.IPv4.IP
		if got := h.hasContainer(newContact, containerName); got != wantReplacementRunning {
			h.t.Fatalf("replacement accessory running on %s = %t, want %t", newContact, got, wantReplacementRunning)
		}
	} else if wantReplacementRunning {
		h.t.Fatal("replacement accessory should be running, but no addressed replacement exists")
	}
}

func (h *migrateFailureHarness) assertMigrateEvent(status, message string) {
	h.t.Helper()
	events, err := h.store.Events("production")
	if err != nil {
		h.t.Fatal(err)
	}
	var matches []state.Event
	for _, event := range events {
		if event.Kind == "migrate" {
			matches = append(matches, event)
		}
	}
	if len(matches) != 2 {
		h.t.Fatalf("migrate events = %+v, want started and %s", matches, status)
	}
	if matches[0].Status != "started" || matches[1].Status != status {
		h.t.Fatalf("migrate event statuses = %+v, want started then %s", matches, status)
	}
	if message != "" && !strings.Contains(matches[1].Message, message) {
		h.t.Fatalf("migrate event message = %q, want %q", matches[1].Message, message)
	}
}

func (h *migrateFailureHarness) assertNoProviderDelete() {
	h.t.Helper()
	if len(h.provider.deletedIDs) != 0 {
		h.t.Fatalf("provider deleted server IDs: %v", h.provider.deletedIDs)
	}
	if eventIndexContaining(h.events, "provider:delete-attempt:") >= 0 {
		h.t.Fatalf("provider delete was attempted: %v", h.events)
	}
}

func (h *migrateFailureHarness) assertProviderBothRemain(hostName string) hetzner.Server {
	h.t.Helper()
	if _, ok := h.provider.servers[hostName]; !ok {
		h.t.Fatalf("old provider server %s is absent: %+v", hostName, h.provider.servers)
	}
	replacement, ok := h.replacement(hostName)
	if !ok {
		h.t.Fatalf("replacement provider server for %s is absent: %+v", hostName, h.provider.servers)
	}
	if got, want := len(h.provider.createdIDs), h.initialCreated+1; got != want {
		h.t.Fatalf("created provider server IDs = %v, want %d entries", h.provider.createdIDs, want)
	}
	return replacement
}

func (h *migrateFailureHarness) serviceContainer(service string, replica int) string {
	h.t.Helper()
	return deployment.ContainerName("sample", "production", service, replica, h.initialRelease.ID)
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

func TestMigrateFailureBeforeRepoint(t *testing.T) {
	tests := []struct {
		name                 string
		point                migrateFailurePoint
		host                 string
		wantError            string
		wantReplacement      bool
		wantBootstrap        bool
		wantReplacementAgent bool
		wantReplacementAcc   bool
	}{
		{name: "provider create", point: migrateFailCreate, host: "web-1", wantError: "create replacement server"},
		{name: "missing public address", point: migrateFailPublicAddress, host: "web-1", wantError: "has no public address", wantReplacement: true},
		{name: "binary resolution", point: migrateFailBinaryResolve, host: "web-1", wantError: "injected binary resolution failure", wantReplacement: true},
		{name: "bootstrap", point: migrateFailBootstrap, host: "web-1", wantError: "injected bootstrap failure", wantReplacement: true, wantBootstrap: true},
		{name: "replacement preflight", point: migrateFailReplacementPreflight, host: "web-1", wantError: "injected replacement preflight failure", wantReplacement: true, wantBootstrap: true, wantReplacementAgent: true},
		{name: "accessory backup", point: migrateFailAccessoryBackup, host: "data-1", wantError: "injected accessory backup failure", wantReplacement: true, wantBootstrap: true, wantReplacementAgent: true},
		{name: "artifact copy", point: migrateFailArtifactCopy, host: "data-1", wantError: "injected artifact copy failure", wantReplacement: true, wantBootstrap: true, wantReplacementAgent: true},
		{name: "accessory restore", point: migrateFailAccessoryRestore, host: "data-1", wantError: "injected accessory restore failure", wantReplacement: true, wantBootstrap: true, wantReplacementAgent: true, wantReplacementAcc: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newMigrateFailureHarness(t)
			h.inject(tt.point, tt.host)

			out, err := runAcceptanceCommandError(t, migrateCmd(&options{configPath: h.path}), "production", tt.host, "--yes")
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("migration error = %v, want %q\n%s", err, tt.wantError, out)
			}
			h.assertInitialFacts()
			h.assertReleaseUnchanged()
			h.assertMigrateEvent("failed", tt.wantError)
			h.assertNoProviderDelete()
			h.assertAccessoryHost(h.initialAccessory.Host.PublicAddress, true, tt.wantReplacementAcc)
			if _, ok := h.provider.servers[tt.host]; !ok {
				t.Fatalf("old provider server %s is absent: %+v", tt.host, h.provider.servers)
			}
			baselineWebContainer := h.serviceContainer("web", 1)
			baselineWebContact := h.initialFact("web-1").PublicAddress
			if !h.hasContainer(baselineWebContact, baselineWebContainer) {
				t.Fatalf("baseline service container %s changed before repoint", baselineWebContainer)
			}

			oldFact := h.initialFact(tt.host)
			if tt.host == "web-1" {
				oldContainer := h.serviceContainer("web", 1)
				if !h.hasContainer(oldFact.PublicAddress, oldContainer) {
					t.Fatalf("old service container %s was moved or stopped", oldContainer)
				}
			}

			replacement, replacementExists := h.replacement(tt.host)
			if replacementExists != tt.wantReplacement {
				t.Fatalf("replacement exists = %t, want %t: %+v", replacementExists, tt.wantReplacement, h.provider.servers)
			}
			if tt.wantReplacement {
				h.assertProviderBothRemain(tt.host)
				note := outputLineWithPrefix(out, "note: replacement server ")
				if !strings.Contains(note, replacement.Name) || !strings.Contains(note, "delete it there before retrying") {
					t.Fatalf("recovery note does not identify the disposable replacement:\n%s", out)
				}
				if tt.host == "web-1" && replacement.PublicNet.IPv4.IP != "" && h.hasContainer(replacement.PublicNet.IPv4.IP, h.serviceContainer("web", 1)) {
					t.Fatalf("service moved to replacement before repoint: %v", h.events)
				}
			} else {
				if got := len(h.provider.createdIDs); got != h.initialCreated {
					t.Fatalf("provider created IDs = %v after create failure", h.provider.createdIDs)
				}
				if got := len(h.provider.servers); got != h.initialCreated {
					t.Fatalf("provider inventory size = %d after create failure, want %d", got, h.initialCreated)
				}
				if strings.Contains(out, "note: replacement server") {
					t.Fatalf("create failure claimed a replacement exists:\n%s", out)
				}
			}

			if eventIndexContaining(h.events, "bootstrap:"+tt.host+":") >= 0 != tt.wantBootstrap {
				t.Fatalf("bootstrap trace mismatch for %s: %v", tt.point, h.events)
			}
			var sourcePreflight, replacementPreflight bool
			for _, call := range h.agentCalls {
				if call.Method != "negotiate" || call.Host.Name != tt.host {
					continue
				}
				if call.Host.ContactTarget() == oldFact.PublicAddress {
					sourcePreflight = true
				} else {
					replacementPreflight = true
				}
			}
			if !sourcePreflight || replacementPreflight != tt.wantReplacementAgent {
				t.Fatalf("protocol preflight calls = %+v, want source=true replacement=%t", h.agentCalls, tt.wantReplacementAgent)
			}
		})
	}
}

func TestMigrateAccessoryFailureAfterStateSaveReportsResidualState(t *testing.T) {
	h := newMigrateFailureHarness(t)
	migrationEventsStart := len(h.events)

	h.inject(migrateFailHostFactRepoint, "data-1")

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

func TestMigrateAccessoryFailureAfterRestoreBeforeStateSave(t *testing.T) {
	h := newMigrateFailureHarness(t)
	h.inject(migrateFailOldAccessoryStop, "data-1")

	out, err := runAcceptanceCommandError(t, migrateCmd(&options{configPath: h.path}), "production", "data-1", "--yes")
	if err == nil || !strings.Contains(err.Error(), "injected old accessory stop failure") {
		t.Fatalf("migration error = %v, want old accessory stop failure\n%s", err, out)
	}
	h.assertInitialFacts()
	h.assertReleaseUnchanged()
	h.assertMigrateEvent("failed", "injected old accessory stop failure")
	h.assertNoProviderDelete()
	replacement := h.assertProviderBothRemain("data-1")
	h.assertAccessoryHost(h.initialAccessory.Host.PublicAddress, true, true)

	containerName := accessorypkg.ContainerName("sample", "production", "postgres")
	restoreAt := eventIndexContaining(h.events, "agent:data-1:accessory_restore")
	stopFailureAt := eventIndexContaining(h.events, "agent-failure:data-1:"+h.initialAccessory.Host.PublicAddress+":stop_container:"+containerName)
	if restoreAt < 0 || stopFailureAt <= restoreAt {
		t.Fatalf("restore and old stop ordering = %v", h.events)
	}
	note := outputLineWithPrefix(out, "note: replacement server ")
	if !strings.Contains(note, replacement.Name) || !strings.Contains(note, "delete it there before retrying") {
		t.Fatalf("recovery output does not direct cleanup of the disposable restored replacement:\n%s", out)
	}
}

func TestMigrateAccessoryResidualHint(t *testing.T) {
	created := providerpkg.Host{Name: "data-1-m20260717010101", PublicAddress: "192.0.2.5"}
	source := scheduler.Host{Contact: "192.0.2.1"}

	tests := []struct {
		name        string
		accessories []string
		want        string
	}{
		{
			name:        "one accessory",
			accessories: []string{"postgres"},
			want:        "note: replacement server data-1-m20260717010101 (192.0.2.5) is running migrated accessory postgres; its old container on 192.0.2.1 is stopped while host facts still point to the old server. Do not delete the replacement or retry `ship migrate`; inspect the saved accessory state and both servers, then manually converge host facts and workloads before deleting either server",
		},
		{
			name:        "multiple accessories are sorted",
			accessories: []string{"redis", "postgres"},
			want:        "note: replacement server data-1-m20260717010101 (192.0.2.5) is running migrated accessories postgres, redis; their old containers on 192.0.2.1 are stopped while host facts still point to the old server. Do not delete the replacement or retry `ship migrate`; inspect the saved accessory state and both servers, then manually converge host facts and workloads before deleting either server",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := migrateAccessoryResidualHint(created, source, tt.accessories); got != tt.want {
				t.Fatalf("migrateAccessoryResidualHint() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMigrateFailureAfterRepoint(t *testing.T) {
	tests := []struct {
		name      string
		point     migrateFailurePoint
		host      string
		wantError string
	}{
		{name: "host re-resolution", point: migrateFailHostsAfterRepoint, host: "web-1", wantError: "injected host re-resolution failure"},
		{name: "service rollout", point: migrateFailServiceRollout, host: "web-1", wantError: "injected service rollout failure"},
		{name: "service restart after accessory move", point: migrateFailAccessoryRestart, host: "data-1", wantError: "injected accessory restart failure"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newMigrateFailureHarness(t)
			h.inject(tt.point, tt.host)
			oldContact := h.initialFact(tt.host).PublicAddress

			out, err := runAcceptanceCommandError(t, migrateCmd(&options{configPath: h.path}), "production", tt.host, "--yes")
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("migration error = %v, want %q\n%s", err, tt.wantError, out)
			}
			replacement := h.assertProviderBothRemain(tt.host)
			newContact := replacement.PublicNet.IPv4.IP
			fact := h.fact(tt.host)
			if fact.PublicAddress != newContact || fact.ProviderName != replacement.Name {
				t.Fatalf("host fact does not point at replacement: fact=%+v replacement=%+v", fact, replacement)
			}
			h.assertReleaseUnchanged()
			h.assertMigrateEvent("failed", tt.wantError)
			h.assertNoProviderDelete()

			if tt.host == "data-1" {
				h.assertAccessoryHost(newContact, false, true)
			} else {
				h.assertAccessoryHost(h.initialAccessory.Host.PublicAddress, true, false)
			}

			oldServiceHost := h.initialFact("web-1").PublicAddress
			oldService := h.serviceContainer("web", 1)
			if !h.hasContainer(oldServiceHost, oldService) {
				t.Fatalf("old workload %s stopped before %s completed: %v", oldService, tt.point, h.events)
			}
			if tt.host == "web-1" && h.hasContainer(newContact, oldService) {
				t.Fatalf("failed boundary left a completed replacement rollout for %s", oldService)
			}

			note := outputLineWithPrefix(out, "note: host ")
			for _, want := range []string{"ship deploy production", oldContact, newContact} {
				if !strings.Contains(note, want) {
					t.Fatalf("post-repoint recovery note missing %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestMigrateFinalCleanup(t *testing.T) {
	t.Run("old workload stop warns and deletion continues", func(t *testing.T) {
		h := newMigrateFailureHarness(t)
		h.inject(migrateFailOldWorkloadStop, "web-1")
		oldFact := h.initialFact("web-1")
		oldServerID := h.provider.servers["web-1"].ID

		out, err := runAcceptanceCommandError(t, migrateCmd(&options{configPath: h.path}), "production", "web-1", "--yes")
		if err != nil {
			t.Fatalf("migration failed after old workload warning: %v\n%s", err, out)
		}
		replacement, ok := h.replacement("web-1")
		if !ok {
			t.Fatalf("replacement missing: %+v", h.provider.servers)
		}
		if _, ok := h.provider.servers["web-1"]; ok {
			t.Fatalf("old provider server remains after warning-only stop failure: %+v", h.provider.servers)
		}
		if !reflect.DeepEqual(h.provider.deletedIDs, []int64{oldServerID}) {
			t.Fatalf("deleted provider IDs = %v, want [%d]", h.provider.deletedIDs, oldServerID)
		}
		fact := h.fact("web-1")
		if fact.PublicAddress != replacement.PublicNet.IPv4.IP {
			t.Fatalf("host fact = %+v, want replacement %s", fact, replacement.PublicNet.IPv4.IP)
		}
		h.assertReleaseUnchanged()
		h.assertAccessoryHost(h.initialAccessory.Host.PublicAddress, true, false)
		h.assertMigrateEvent("succeeded", "server "+oldFact.PublicAddress+" -> "+replacement.PublicNet.IPv4.IP)

		containerName := h.serviceContainer("web", 1)
		if !h.hasContainer(oldFact.PublicAddress, containerName) || !h.hasContainer(replacement.PublicNet.IPv4.IP, containerName) {
			t.Fatalf("warning residual containers are wrong: old=%t new=%t", h.hasContainer(oldFact.PublicAddress, containerName), h.hasContainer(replacement.PublicNet.IPv4.IP, containerName))
		}
		for _, want := range []string{"warning: stop container " + containerName, "deleted old server web-1", "migrated host web-1"} {
			if !strings.Contains(out, want) {
				t.Fatalf("cleanup output missing %q:\n%s", want, out)
			}
		}
		runAt := eventIndexContaining(h.events, "agent:web-1:run:"+containerName)
		stopFailureAt := eventIndexContaining(h.events, "agent-failure:web-1:"+oldFact.PublicAddress+":stop_container:"+containerName)
		deleteAt := eventIndexContaining(h.events, fmt.Sprintf("provider:deleted:%d:web-1", oldServerID))
		if runAt < 0 || stopFailureAt <= runAt || deleteAt <= stopFailureAt {
			t.Fatalf("rollout, stop warning, delete order = %v", h.events)
		}
	})

	t.Run("provider deletion failure preserves both servers", func(t *testing.T) {
		h := newMigrateFailureHarness(t)
		h.inject(migrateFailProviderDelete, "web-1")
		oldFact := h.initialFact("web-1")
		oldServerID := h.provider.servers["web-1"].ID

		out, err := runAcceptanceCommandError(t, migrateCmd(&options{configPath: h.path}), "production", "web-1", "--yes")
		if err == nil || !strings.Contains(err.Error(), "delete old server") {
			t.Fatalf("migration error = %v, want provider delete failure\n%s", err, out)
		}
		replacement := h.assertProviderBothRemain("web-1")
		if len(h.provider.deletedIDs) != 0 {
			t.Fatalf("provider deleted IDs despite injected failure: %v", h.provider.deletedIDs)
		}
		fact := h.fact("web-1")
		if fact.PublicAddress != replacement.PublicNet.IPv4.IP {
			t.Fatalf("host fact = %+v, want replacement %s", fact, replacement.PublicNet.IPv4.IP)
		}
		h.assertReleaseUnchanged()
		h.assertAccessoryHost(h.initialAccessory.Host.PublicAddress, true, false)
		h.assertMigrateEvent("failed", "delete old server")

		containerName := h.serviceContainer("web", 1)
		if h.hasContainer(oldFact.PublicAddress, containerName) || !h.hasContainer(replacement.PublicNet.IPv4.IP, containerName) {
			t.Fatalf("delete failure workload state is wrong: old=%t new=%t", h.hasContainer(oldFact.PublicAddress, containerName), h.hasContainer(replacement.PublicNet.IPv4.IP, containerName))
		}
		note := outputLineWithPrefix(out, "note: host ")
		for _, want := range []string{"ship deploy production", oldFact.PublicAddress, replacement.PublicNet.IPv4.IP} {
			if !strings.Contains(note, want) {
				t.Fatalf("delete failure recovery note missing %q:\n%s", want, out)
			}
		}
		stopAt := eventIndexContaining(h.events, "agent:web-1:stop:"+containerName)
		deleteAttemptAt := eventIndexContaining(h.events, fmt.Sprintf("provider:delete-attempt:%d:web-1", oldServerID))
		if stopAt < 0 || deleteAttemptAt <= stopAt {
			t.Fatalf("old workload stop and failed delete order = %v", h.events)
		}
	})

	t.Run("keep server stops workloads without deletion", func(t *testing.T) {
		h := newMigrateFailureHarness(t)
		oldFact := h.initialFact("web-1")

		out, err := runAcceptanceCommandError(t, migrateCmd(&options{configPath: h.path}), "production", "web-1", "--yes", "--keep-server")
		if err != nil {
			t.Fatalf("keep-server migration failed: %v\n%s", err, out)
		}
		replacement := h.assertProviderBothRemain("web-1")
		h.assertNoProviderDelete()
		fact := h.fact("web-1")
		if fact.PublicAddress != replacement.PublicNet.IPv4.IP {
			t.Fatalf("host fact = %+v, want replacement %s", fact, replacement.PublicNet.IPv4.IP)
		}
		h.assertReleaseUnchanged()
		h.assertAccessoryHost(h.initialAccessory.Host.PublicAddress, true, false)
		h.assertMigrateEvent("succeeded", "server "+oldFact.PublicAddress+" -> "+replacement.PublicNet.IPv4.IP)

		containerName := h.serviceContainer("web", 1)
		if h.hasContainer(oldFact.PublicAddress, containerName) || !h.hasContainer(replacement.PublicNet.IPv4.IP, containerName) {
			t.Fatalf("keep-server workload state is wrong: old=%t new=%t", h.hasContainer(oldFact.PublicAddress, containerName), h.hasContainer(replacement.PublicNet.IPv4.IP, containerName))
		}
		for _, want := range []string{"keeping old server web-1 (" + oldFact.PublicAddress + ")", "will report it as extra", "migrated host web-1"} {
			if !strings.Contains(out, want) {
				t.Fatalf("keep-server output missing %q:\n%s", want, out)
			}
		}
	})
}

func TestMigrateFailureRecovery(t *testing.T) {
	h := newMigrateFailureHarness(t)
	h.inject(migrateFailServiceRollout, "web-1")
	oldFact := h.initialFact("web-1")
	oldContainer := h.serviceContainer("web", 1)

	migrateOut, err := runAcceptanceCommandError(t, migrateCmd(&options{configPath: h.path}), "production", "web-1", "--yes")
	if err == nil || !strings.Contains(err.Error(), "injected service rollout failure") {
		t.Fatalf("migration error = %v, want rollout failure\n%s", err, migrateOut)
	}
	replacement := h.assertProviderBothRemain("web-1")
	newContact := replacement.PublicNet.IPv4.IP
	if fact := h.fact("web-1"); fact.PublicAddress != newContact {
		t.Fatalf("post-failure fact = %+v, want replacement %s", fact, newContact)
	}
	h.assertReleaseUnchanged()
	h.assertAccessoryHost(h.initialAccessory.Host.PublicAddress, true, false)
	h.assertMigrateEvent("failed", "injected service rollout failure")
	h.assertNoProviderDelete()
	if !h.hasContainer(oldFact.PublicAddress, oldContainer) || h.hasContainer(newContact, oldContainer) {
		t.Fatalf("pre-recovery containers are wrong: old=%t new=%t", h.hasContainer(oldFact.PublicAddress, oldContainer), h.hasContainer(newContact, oldContainer))
	}
	note := outputLineWithPrefix(migrateOut, "note: host ")
	for _, want := range []string{"ship deploy production", oldFact.PublicAddress, newContact} {
		if !strings.Contains(note, want) {
			t.Fatalf("migration recovery note missing %q:\n%s", want, migrateOut)
		}
	}

	h.agentFailure = nil
	recoveryOut, err := runAcceptanceCommandError(t, deployCmd(&options{configPath: h.path}), "production")
	if err != nil {
		t.Fatalf("documented recovery deploy failed: %v\n%s", err, recoveryOut)
	}
	current, err := h.store.CurrentRelease("production")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID == h.initialRelease.ID || current.Status != state.ReleaseStatusHealthy {
		t.Fatalf("recovery release = %+v, want a new healthy release after %s", current, h.initialRelease.ID)
	}
	if fact := h.fact("web-1"); fact.PublicAddress != newContact || fact.ProviderName != replacement.Name {
		t.Fatalf("recovery changed replacement host fact: %+v", fact)
	}
	newContainer := deployment.ContainerName("sample", "production", "web", 1, current.ID)
	if !h.hasContainer(newContact, newContainer) {
		t.Fatalf("recovery did not start %s on replacement %s: %v", newContainer, newContact, h.events)
	}
	if h.hasContainer(oldFact.PublicAddress, newContainer) {
		t.Fatalf("recovery started current container %s on old server %s", newContainer, oldFact.PublicAddress)
	}
	if !h.hasContainer(oldFact.PublicAddress, oldContainer) {
		t.Fatalf("recovery unexpectedly mutated unreachable old server %s", oldFact.PublicAddress)
	}
	if _, ok := h.provider.servers["web-1"]; !ok {
		t.Fatalf("recovery deleted old provider server: %+v", h.provider.servers)
	}
	if _, ok := h.replacement("web-1"); !ok {
		t.Fatalf("recovery deleted replacement provider server: %+v", h.provider.servers)
	}
	h.assertNoProviderDelete()
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
