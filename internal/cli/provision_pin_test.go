package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/watzon/ship/internal/config"
	providerpkg "github.com/watzon/ship/internal/provider"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/state"
)

// reconcilingProvider is a fake cloud provider that runs Ship's real
// reconciliation (provider.ReconcileHosts) against an in-memory server list.
// It never talks to a real API; Create only records the request. Like the
// real providers, Reconcile runs the read-only preflight first and records
// "setup" (standing in for network/firewall creation) only after it passes.
type reconcilingProvider struct {
	existing []providerpkg.Host
	created  []string
	setup    int
}

func (p *reconcilingProvider) Name() string { return "fakecloud" }

func (p *reconcilingProvider) PlanHosts(project, environment string, env config.Environment) ([]providerpkg.HostPlan, error) {
	return providerpkg.HostPlans(project, environment, env, providerpkg.HostPlanOptions{}), nil
}

func (p *reconcilingProvider) Reconcile(ctx context.Context, project, environment string, env config.Environment) (providerpkg.ReconcileResult, error) {
	plans, _ := p.PlanHosts(project, environment, env)
	if err := providerpkg.PreflightHosts(ctx, p, project, environment, plans); err != nil {
		return providerpkg.ReconcileResult{}, err
	}
	p.setup++
	return providerpkg.ReconcileHosts(ctx, project, environment, plans, p)
}

func (p *reconcilingProvider) List(context.Context, string, string) ([]providerpkg.Host, error) {
	return append([]providerpkg.Host(nil), p.existing...), nil
}

func (p *reconcilingProvider) Create(_ context.Context, plan providerpkg.HostPlan) (providerpkg.Host, error) {
	p.created = append(p.created, plan.Name)
	return providerpkg.Host{ID: "new-" + plan.Name, Name: plan.Name, Pool: plan.Pool, PublicAddress: "192.0.2.99", Labels: plan.Labels}, nil
}

func (p *reconcilingProvider) Delete(context.Context, providerpkg.Host) error {
	panic("fake provider must not delete servers in these tests")
}

func (p *reconcilingProvider) CredentialChecks(func(string) (string, bool)) []providerpkg.CredentialCheck {
	return nil
}

func writePinConfig(t *testing.T, pool string) string {
	t.Helper()
	text := strings.Replace(singleHostConfig(), "        web:\n          count: 1\n", "        web:\n"+pool, 1)
	path := filepath.Join(t.TempDir(), config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func saveFactsNextToConfig(t *testing.T, configPath string, facts []state.HostFact) {
	t.Helper()
	store := state.NewStore(filepath.Join(filepath.Dir(configPath), config.LocalStateDir))
	if err := store.SaveHostFacts("production", facts); err != nil {
		t.Fatal(err)
	}
}

func runProvision(t *testing.T, opts *options, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := provisionCmd(opts)
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestProvisionPlanShowsExistingAndNewHostsFromHostFacts(t *testing.T) {
	path := writePinConfig(t, "          count: 2\n")
	saveFactsNextToConfig(t, path, []state.HostFact{{Name: "web-1", Pool: "web", User: "root", ProviderID: "abc", IPv4: "203.0.113.10", PublicAddress: "203.0.113.10"}})

	out, err := runProvision(t, &options{configPath: path}, "plan", "production")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"- exists web-1: pool=web provider_id=abc address=203.0.113.10\n",
		"- create web-2: pool=web new server\n",
		"based on local host facts, apply would create 1 server: web-2; run with --refresh to confirm against the provider\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan missing %q:\n%s", want, out)
		}
	}

	// --dry-run apply prints the same plan.
	dry, err := runProvision(t, &options{configPath: path, dryRun: true}, "apply", "production")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dry, "- exists web-1:") || !strings.Contains(dry, "- create web-2:") {
		t.Fatalf("dry-run apply output:\n%s", dry)
	}
}

func TestProvisionPlanRefreshUsesProviderListing(t *testing.T) {
	path := writePinConfig(t, "          count: 2\n          pin:\n            web-1: 203.0.113.10\n")
	prov := &reconcilingProvider{existing: []providerpkg.Host{
		{ID: "abc", Name: "web-1", Pool: "web", PublicAddress: "203.0.113.10"},
		{ID: "old", Name: "web-9", Pool: "web", PublicAddress: "203.0.113.19"},
	}}
	installProviderHook(t, prov)

	out, err := runProvision(t, &options{configPath: path}, "plan", "production", "--refresh", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Actions []struct{ Kind, Target string } `json:"actions"`
		Summary map[string]int                  `json:"summary"`
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("invalid json %q: %v", out, err)
	}
	got := []string{}
	for _, action := range plan.Actions {
		got = append(got, action.Kind+" "+action.Target)
	}
	if strings.Join(got, ",") != "exists web-1,create web-2,extra web-9" {
		t.Fatalf("actions = %v", got)
	}
	if plan.Summary["create"] != 1 || plan.Summary["exists"] != 1 || plan.Summary["extra"] != 1 {
		t.Fatalf("summary = %+v", plan.Summary)
	}
	if len(prov.created) != 0 {
		t.Fatalf("plan created servers: %v", prov.created)
	}
}

func TestProvisionApplyDoesNotReprovisionPinnedHost(t *testing.T) {
	// A second machine: no local host facts, web-1 pinned by address.
	path := writePinConfig(t, "          count: 1\n          pin:\n            web-1: 203.0.113.10\n")
	var events []string
	installBootstrapHooks(t, &events)
	var contacts []string
	recording := newBootstrapSSH
	newBootstrapSSH = func(host scheduler.Host, dryRun bool) bootstrapSSH {
		contacts = append(contacts, host.ContactTarget())
		return recording(host, dryRun)
	}
	prov := &reconcilingProvider{existing: []providerpkg.Host{{
		ID: "abc", Name: "web-1", Pool: "web", PublicAddress: "203.0.113.10",
		Labels: map[string]string{providerpkg.LabelHost: "web-1", providerpkg.LabelPool: "web"},
	}}}
	installProviderHook(t, prov)

	out, err := runProvision(t, &options{configPath: path}, "apply", "production", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if len(prov.created) != 0 {
		t.Fatalf("pinned host was re-provisioned: %v", prov.created)
	}
	if !strings.Contains(out, "exists web-1 pool=web provider_id=abc ipv4=203.0.113.10") || !strings.Contains(out, "bootstrapped web-1") {
		t.Fatalf("output = %q", out)
	}
	for _, contact := range contacts {
		if contact != "203.0.113.10" {
			t.Fatalf("bootstrap contacted %q, want pinned address", contact)
		}
	}

	plan, err := runProvision(t, &options{configPath: path}, "plan", "production")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "- exists web-1: pool=web provider_id=abc address=203.0.113.10") || !strings.Contains(plan, "based on local host facts, apply would not create any servers") {
		t.Fatalf("plan after apply:\n%s", plan)
	}
}

func TestProvisionApplyRefusesPinnedHostMissingAtProvider(t *testing.T) {
	path := writePinConfig(t, "          count: 2\n          pin:\n            web-1: 203.0.113.10\n")
	var events []string
	installBootstrapHooks(t, &events)
	prov := &reconcilingProvider{}
	installProviderHook(t, prov)

	_, err := runProvision(t, &options{configPath: path}, "apply", "production", "--yes")
	if err == nil {
		t.Fatal("expected apply to refuse a pinned host the provider does not have")
	}
	if !strings.Contains(err.Error(), "host web-1 pool=web is pinned to 203.0.113.10 but the provider reports no server for it") {
		t.Fatalf("err = %v", err)
	}
	if len(prov.created) != 0 {
		t.Fatalf("created servers despite the bad pin (web-2 must not be created either): %v", prov.created)
	}
	if len(events) != 0 {
		t.Fatalf("bootstrapped despite the bad pin: %v", events)
	}
	if prov.setup != 0 {
		t.Fatal("provider setup ran before the pin was refused")
	}
}

func TestProvisionApplyRefusesAddressHostWithStaleLocalFacts(t *testing.T) {
	// Thorn QA P1: hosts.json has web-1 at 203.0.113.10, the provider now
	// reports 203.0.113.11, and the config lists hosts: [aaa-new, 203.0.113.10].
	// The plan showed a conflict, but apply created both hosts.
	path := writePinConfig(t, "          hosts: [aaa-new, 203.0.113.10]\n")
	saveFactsNextToConfig(t, path, []state.HostFact{{Name: "web-1", Pool: "web", User: "root", IPv4: "203.0.113.10", PublicAddress: "203.0.113.10"}})

	plan, err := runProvision(t, &options{configPath: path}, "plan", "production")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "- conflict 203.0.113.10:") || !strings.Contains(plan, "apply would refuse to run") {
		t.Fatalf("plan:\n%s", plan)
	}

	var events []string
	installBootstrapHooks(t, &events)
	prov := &reconcilingProvider{existing: []providerpkg.Host{{
		ID: "abc", Name: "web-1", Pool: "web", PublicAddress: "203.0.113.11",
		Labels: map[string]string{providerpkg.LabelHost: "web-1", providerpkg.LabelPool: "web"},
	}}}
	installProviderHook(t, prov)
	_, err = runProvision(t, &options{configPath: path}, "apply", "production", "--yes")
	if err == nil || !strings.Contains(err.Error(), "host 203.0.113.10 pool=web is an IP address") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("apply err = %v", err)
	}
	if len(prov.created) != 0 || prov.setup != 0 || len(events) != 0 {
		t.Fatalf("apply created %v / setup %d / bootstrapped %v", prov.created, prov.setup, events)
	}
}

func TestProvisionPlanFromStaleFactsDoesNotPromiseNoCreates(t *testing.T) {
	// Thorn QA P1: local facts say web-1 exists but the provider deleted it.
	path := writePinConfig(t, "          count: 1\n")
	saveFactsNextToConfig(t, path, []state.HostFact{{Name: "web-1", Pool: "web", User: "root", ProviderID: "abc", IPv4: "203.0.113.10", PublicAddress: "203.0.113.10"}})
	prov := &reconcilingProvider{}
	installProviderHook(t, prov)

	local, err := runProvision(t, &options{configPath: path}, "plan", "production")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(local, "apply will not create any servers") || !strings.Contains(local, "not confirmed against the provider, so run with --refresh before apply") {
		t.Fatalf("local plan:\n%s", local)
	}

	live, err := runProvision(t, &options{configPath: path}, "plan", "production", "--refresh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(live, "- create web-1: pool=web new server") || !strings.Contains(live, "apply will create 1 server: web-1") {
		t.Fatalf("refreshed plan:\n%s", live)
	}
	if len(prov.created) != 0 {
		t.Fatalf("plan created servers: %v", prov.created)
	}
}

func TestProvisionApplyRefusesPinWithoutProviderAddress(t *testing.T) {
	// Thorn QA P1: web-1 pinned, provider lists web-1 with no address, web-2
	// missing. web-2 was created; the unverifiable pin must fail closed.
	path := writePinConfig(t, "          count: 2\n          pin:\n            web-1: 203.0.113.10\n")
	var events []string
	installBootstrapHooks(t, &events)
	prov := &reconcilingProvider{existing: []providerpkg.Host{{
		ID: "abc", Name: "web-1", Pool: "web",
		Labels: map[string]string{providerpkg.LabelHost: "web-1", providerpkg.LabelPool: "web"},
	}}}
	installProviderHook(t, prov)

	live, err := runProvision(t, &options{configPath: path}, "plan", "production", "--refresh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(live, "- conflict web-1:") || !strings.Contains(live, "apply will refuse to run and will not create any servers") {
		t.Fatalf("refreshed plan:\n%s", live)
	}

	_, err = runProvision(t, &options{configPath: path}, "apply", "production", "--yes")
	if err == nil || !strings.Contains(err.Error(), "reports no address for it, so the pin cannot be verified") {
		t.Fatalf("apply err = %v", err)
	}
	if len(prov.created) != 0 || prov.setup != 0 || len(events) != 0 {
		t.Fatalf("apply created %v / setup %d / bootstrapped %v", prov.created, prov.setup, events)
	}
}

func TestResolvedHostsMismatchHintPointsAtPinning(t *testing.T) {
	// Issue #5: count: 1 replaced by hosts: [<ipv4 of web-1>].
	path := writePinConfig(t, "          hosts: [203.0.113.10]\n")
	saveFactsNextToConfig(t, path, []state.HostFact{{Name: "web-1", Pool: "web", User: "root", IPv4: "203.0.113.10", PublicAddress: "203.0.113.10"}})
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(filepath.Join(filepath.Dir(path), config.LocalStateDir))
	_, err = resolvedHostsForEnvironment(store, "production", cfg.Environments["production"])
	if err == nil {
		t.Fatal("expected mismatch error")
	}
	for _, want := range []string{
		"missing 203.0.113.10 pool=web; extra web-1 pool=web",
		"set hosts.pools.web.pin.web-1: 203.0.113.10",
		"run ship provision plan production",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "provision apply") || strings.Contains(err.Error(), "--yes") {
		t.Fatalf("hint must not suggest provision apply: %v", err)
	}

	// The plan must not read as "create a server for this address".
	plan, err := runProvision(t, &options{configPath: path}, "plan", "production")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "- conflict 203.0.113.10: pool=web is the address of existing host web-1") || strings.Contains(plan, "- create") {
		t.Fatalf("plan:\n%s", plan)
	}

	// And apply refuses before creating anything.
	var events []string
	installBootstrapHooks(t, &events)
	prov := &reconcilingProvider{existing: []providerpkg.Host{{
		ID: "abc", Name: "web-1", Pool: "web", PublicAddress: "203.0.113.10",
		Labels: map[string]string{providerpkg.LabelHost: "web-1", providerpkg.LabelPool: "web"},
	}}}
	installProviderHook(t, prov)
	if _, err := runProvision(t, &options{configPath: path}, "apply", "production", "--yes"); err == nil || !strings.Contains(err.Error(), "is the address of existing server web-1") {
		t.Fatalf("apply err = %v", err)
	}
	if len(prov.created) != 0 || len(events) != 0 {
		t.Fatalf("apply created %v / bootstrapped %v", prov.created, events)
	}
}
