package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	accessorypkg "github.com/watzon/ship/internal/accessory"
	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/cli/ui"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/deployment"
	"github.com/watzon/ship/internal/docker"
	providerpkg "github.com/watzon/ship/internal/provider"
	"github.com/watzon/ship/internal/provider/hetzner"
	"github.com/watzon/ship/internal/scheduler"
	secretspkg "github.com/watzon/ship/internal/secrets"
	"github.com/watzon/ship/internal/state"
)

type recordingBootstrapSSH struct {
	host   scheduler.Host
	events *[]string
}

func (r recordingBootstrapSSH) Run(_ context.Context, command string) (string, error) {
	*r.events = append(*r.events, "bootstrap:"+r.host.Name+":run:"+firstCommandLine(command))
	switch strings.TrimSpace(command) {
	case "uname -s":
		return "Linux", nil
	case "uname -m":
		return "x86_64", nil
	default:
		return "ok", nil
	}
}

func (r recordingBootstrapSSH) RunWithStdin(_ context.Context, command, stdin string) (string, error) {
	*r.events = append(*r.events, fmt.Sprintf("bootstrap:%s:upload:%s:%d", r.host.Name, firstCommandLine(command), len(stdin)))
	return "ok", nil
}

func firstCommandLine(command string) string {
	if line, _, ok := strings.Cut(command, "\n"); ok {
		return line
	}
	return command
}

func installBootstrapHooks(t *testing.T, events *[]string) {
	t.Helper()
	originalBinary := readCurrentShipBinary
	originalResolve := resolveShipBinaryForHost
	originalSSH := newBootstrapSSH
	originalAttempts := bootstrapMaxAttempts
	originalDelay := bootstrapRetryDelay
	readCurrentShipBinary = func() ([]byte, error) {
		return []byte("ship-test-binary"), nil
	}
	resolveShipBinaryForHost = func(ctx context.Context, host scheduler.Host, opts *options) ([]byte, error) {
		return readCurrentShipBinary()
	}
	newBootstrapSSH = func(host scheduler.Host, dryRun bool) bootstrapSSH {
		return recordingBootstrapSSH{host: host, events: events}
	}
	bootstrapMaxAttempts = 2
	bootstrapRetryDelay = 0
	t.Cleanup(func() {
		readCurrentShipBinary = originalBinary
		resolveShipBinaryForHost = originalResolve
		newBootstrapSSH = originalSSH
		bootstrapMaxAttempts = originalAttempts
		bootstrapRetryDelay = originalDelay
	})
}

func installUpgradeSSHHook(t *testing.T, events *[]string) {
	t.Helper()
	originalSSH := newBootstrapSSH
	newBootstrapSSH = func(host scheduler.Host, dryRun bool) bootstrapSSH {
		return recordingBootstrapSSH{host: host, events: events}
	}
	t.Cleanup(func() { newBootstrapSSH = originalSSH })
}

func installShipBinaryReader(t *testing.T, content []byte) {
	t.Helper()
	originalBinary := readCurrentShipBinary
	originalResolve := resolveShipBinaryForHost
	readCurrentShipBinary = func() ([]byte, error) {
		return append([]byte(nil), content...), nil
	}
	resolveShipBinaryForHost = func(ctx context.Context, host scheduler.Host, opts *options) ([]byte, error) {
		return readCurrentShipBinary()
	}
	t.Cleanup(func() {
		readCurrentShipBinary = originalBinary
		resolveShipBinaryForHost = originalResolve
	})
}

type recordingProvider struct {
	name      string
	plans     []providerpkg.HostPlan
	reconcile providerpkg.ReconcileResult
	listed    []providerpkg.Host
	deleted   []string
}

func (p *recordingProvider) Name() string {
	if p.name != "" {
		return p.name
	}
	return "recording"
}

func (p *recordingProvider) PlanHosts(string, string, config.Environment) ([]providerpkg.HostPlan, error) {
	return append([]providerpkg.HostPlan(nil), p.plans...), nil
}

func (p *recordingProvider) Reconcile(context.Context, string, string, config.Environment) (providerpkg.ReconcileResult, error) {
	return p.reconcile, nil
}

func (p *recordingProvider) List(context.Context, string, string) ([]providerpkg.Host, error) {
	return append([]providerpkg.Host(nil), p.listed...), nil
}

func (p *recordingProvider) Delete(_ context.Context, host providerpkg.Host) error {
	p.deleted = append(p.deleted, host.ID)
	return nil
}

func (p *recordingProvider) CredentialChecks(func(string) (string, bool)) []providerpkg.CredentialCheck {
	return nil
}

func installProviderHook(t *testing.T, provider providerpkg.Provider) {
	t.Helper()
	original := newEnvironmentProvider
	newEnvironmentProvider = func(config.Environment, bool) (providerpkg.Provider, error) {
		return provider, nil
	}
	t.Cleanup(func() {
		newEnvironmentProvider = original
	})
}

func TestProvisionApplyRequiresYes(t *testing.T) {
	path := writeConfig(t)
	cmd := provisionCmd(&options{configPath: path})
	cmd.SetArgs([]string{"apply", "production"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected confirmation error")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error = %v", err)
	}
}

func TestConfigCmdRendersResolvedYAML(t *testing.T) {
	path := writeConfig(t)
	var out bytes.Buffer
	cmd := configCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, needle := range []string{
		"project: example",
		"registry: ghcr.io/acme/example",
		"production:",
		"image: caddy:2",
		"domains:",
		"- example.com",
		"redirects:",
		"to: https://example.com",
		"SESSION_SECRET",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("config output missing %q:\n%s", needle, text)
		}
	}
	for _, unexpected := range []string{
		"port: 0",
		"identity_file: \"\"",
		"vultr: null",
		"services: {}",
	} {
		if strings.Contains(text, unexpected) {
			t.Fatalf("config output included empty default %q:\n%s", unexpected, text)
		}
	}
}

func TestConfigCmdRendersResolvedJSON(t *testing.T) {
	path := writeConfig(t)
	var out bytes.Buffer
	cmd := configCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("invalid json %q: %v", out.String(), err)
	}
	if view["project"] != "example" || view["registry"] != "ghcr.io/acme/example" {
		t.Fatalf("top-level config = %+v", view)
	}
	envs := view["environments"].(map[string]any)
	production := envs["production"].(map[string]any)
	provider := production["provider"].(map[string]any)
	hetzner := provider["hetzner"].(map[string]any)
	if hetzner["server_type"] != "cx23" {
		t.Fatalf("hetzner config = %+v", hetzner)
	}
	ssh, hasSSH := view["ssh"]
	if hasSSH {
		t.Fatalf("empty ssh config should be omitted: %+v", ssh)
	}
}

func TestHostsCmdUsesConfiguredInventoryBeforeProvisioning(t *testing.T) {
	path := writeConfig(t)
	var out bytes.Buffer
	cmd := hostsCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, needle := range []string{
		"environment production",
		"source config",
		"ingress-1",
		"web-1",
		"worker-1",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("hosts output missing %q:\n%s", needle, text)
		}
	}
}

func TestHostsCmdUsesSavedHostFacts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveHostFacts("production", []state.HostFact{{
		Name:           "web-1",
		Pool:           "web",
		User:           "deploy",
		SSHPort:        2222,
		IdentityFile:   "~/.ssh/ship",
		KnownHostsFile: ".ship/known_hosts",
		JumpHost:       "bastion.example.com",
		SSHOptions:     map[string]string{"IdentitiesOnly": "yes"},
		PublicAddress:  "198.51.100.10",
		Provider:       "fake",
		ProviderID:     "instance-abc",
	}}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := hostsCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view hostsView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("invalid json %q: %v", out.String(), err)
	}
	if view.Environment != "production" || view.Source != "state" || len(view.Hosts) != 1 {
		t.Fatalf("view = %+v", view)
	}
	host := view.Hosts[0]
	if host.Name != "web-1" || host.Pool != "web" || host.User != "deploy" || host.Contact != "198.51.100.10" || host.SSHPort != 2222 {
		t.Fatalf("host = %+v", host)
	}
	if host.IdentityFile != "~/.ssh/ship" || host.KnownHostsFile != ".ship/known_hosts" || host.JumpHost != "bastion.example.com" || host.SSHOptions["IdentitiesOnly"] != "yes" {
		t.Fatalf("host ssh metadata = %+v", host)
	}
}

func TestVersionCmdLocal(t *testing.T) {
	var out bytes.Buffer
	cmd := versionCmd(&options{})
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, needle := range []string{
		agent.AgentVersion,
		fmt.Sprintf("%d-%d", agent.AgentMinProtocol, agent.AgentProtocol),
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("version output missing %q:\n%s", needle, text)
		}
	}
}

func TestVersionCmdEnvironmentJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls []string
	installAgentHook(t, func(host scheduler.Host) deployAgent {
		return deployAgentFunc(func(ctx context.Context, method string, params any, out any) error {
			calls = append(calls, host.Name+":"+method)
			if method != "status" {
				return fmt.Errorf("unexpected method %s", method)
			}
			status, ok := out.(*agent.Status)
			if !ok {
				return fmt.Errorf("unexpected status output %T", out)
			}
			*status = agent.Status{
				Hostname:         "node-" + host.Name,
				StateDir:         config.RemoteStateDir,
				DockerOK:         true,
				AgentVersion:     agent.Version(),
				ProtocolVersion:  agent.AgentProtocol,
				SupportedMethods: []string{"status", "pull"},
			}
			return nil
		})
	})

	var out bytes.Buffer
	cmd := versionCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"production", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var view versionView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("invalid json %q: %v", out.String(), err)
	}
	if view.ShipVersion != agent.Version() || view.MinAgentProtocol != agent.AgentMinProtocol || view.MaxAgentProtocol != agent.AgentProtocol {
		t.Fatalf("local version fields = %+v", view)
	}
	if view.Environment != "production" || len(view.Hosts) != 1 {
		t.Fatalf("view = %+v", view)
	}
	host := view.Hosts[0]
	if host.Name != "web-1" || host.Pool != "web" || host.AgentVersion != agent.Version() || host.AgentProtocol != agent.AgentProtocol || !host.DockerOK {
		t.Fatalf("host version = %+v", host)
	}
	if strings.Join(host.SupportedMethods, ",") != "status,pull" {
		t.Fatalf("supported methods = %+v", host.SupportedMethods)
	}
	if strings.Join(calls, ",") != "web-1:status" {
		t.Fatalf("agent calls = %+v", calls)
	}
}

func TestAgentUpgradeUploadsCurrentBinary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := []byte("ship-upgrade-binary")
	installShipBinaryReader(t, binary)
	wantSum := fmt.Sprintf("%x", sha256.Sum256(binary))
	var sshEvents []string
	installUpgradeSSHHook(t, &sshEvents)
	var calls []agent.InstallBinaryParams
	installAgentHook(t, func(host scheduler.Host) deployAgent {
		return deployAgentFunc(func(ctx context.Context, method string, params any, out any) error {
			switch method {
			case "install_binary":
				p := params.(agent.InstallBinaryParams)
				calls = append(calls, p)
				result, ok := out.(*agent.InstallBinaryResult)
				if !ok {
					return fmt.Errorf("unexpected install output %T", out)
				}
				*result = agent.InstallBinaryResult{Path: p.Path, Installed: true, SHA256: p.SHA256}
				return nil
			case "status":
				status, ok := out.(*agent.Status)
				if !ok {
					return fmt.Errorf("unexpected status output %T", out)
				}
				*status = agent.Status{AgentVersion: agent.Version(), ProtocolVersion: agent.AgentProtocol, DockerOK: true}
				return nil
			default:
				return fmt.Errorf("unexpected method %s", method)
			}
		})
	})

	var out bytes.Buffer
	cmd := agentCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"upgrade", "production", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("install calls = %+v", calls)
	}
	if calls[0].Path != config.RemoteBinaryPath || calls[0].SHA256 != wantSum || calls[0].Mode != 0o755 || calls[0].ContentBase64 == "" {
		t.Fatalf("install params = %+v, want sha %s", calls[0], wantSum)
	}
	var view agentUpgradeView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("invalid json %q: %v", out.String(), err)
	}
	if view.Environment != "production" || view.SHA256 != wantSum || len(view.Hosts) != 1 {
		t.Fatalf("upgrade view = %+v", view)
	}
	host := view.Hosts[0]
	if host.Name != "web-1" || !host.Installed || host.Path != config.RemoteBinaryPath || host.SHA256 != wantSum {
		t.Fatalf("host upgrade = %+v", host)
	}
	timeline, err := state.NewStore(filepath.Join(dir, config.LocalStateDir)).Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "agent_upgrade", "started", "") || !timelineContains(timeline, "agent_upgrade", "succeeded", "") {
		t.Fatalf("timeline missing agent upgrade events: %+v", timeline)
	}
}

func TestSystemdUnitIsOneshotMarker(t *testing.T) {
	unit := systemdUnit()
	for _, needle := range []string{
		"Type=oneshot",
		"RemainAfterExit=yes",
		config.RemoteBinaryPath + " agent run",
	} {
		if !strings.Contains(unit, needle) {
			t.Fatalf("systemd unit missing %q:\n%s", needle, unit)
		}
	}
	// `ship agent run` exits immediately by design; a Restart= directive
	// would loop the unit forever.
	if strings.Contains(unit, "Restart=") {
		t.Fatalf("systemd unit must not restart a oneshot command:\n%s", unit)
	}
}

func TestAgentUpgradeWithOverrideToleratesVersionSkew(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	installShipBinaryReader(t, []byte("older-release-binary"))
	var sshEvents []string
	installUpgradeSSHHook(t, &sshEvents)
	installAgentHook(t, func(host scheduler.Host) deployAgent {
		return deployAgentFunc(func(ctx context.Context, method string, params any, out any) error {
			switch method {
			case "install_binary":
				p := params.(agent.InstallBinaryParams)
				result := out.(*agent.InstallBinaryResult)
				*result = agent.InstallBinaryResult{Path: p.Path, Installed: true, SHA256: p.SHA256}
				return nil
			case "status":
				status := out.(*agent.Status)
				// An explicitly overridden binary may be a different release
				// than the CLI; that must not trigger a rollback.
				*status = agent.Status{AgentVersion: "0.0.1-not-the-cli-version", ProtocolVersion: agent.AgentProtocol, DockerOK: true}
				return nil
			default:
				return fmt.Errorf("unexpected method %s", method)
			}
		})
	})

	var out bytes.Buffer
	cmd := agentCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"upgrade", "production", "--agent-binary", "/mirror/ship_0.4.6_linux_amd64.tar.gz"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("override upgrade rolled back on version skew: %v", err)
	}
}

func TestAgentUpgradeDryRunDoesNotContactAgents(t *testing.T) {
	path := writeConfig(t)
	installShipBinaryReader(t, []byte("ship-upgrade-binary"))
	installAgentHook(t, func(host scheduler.Host) deployAgent {
		t.Fatalf("dry-run upgrade contacted agent on %s", host.Name)
		return nil
	})

	var out bytes.Buffer
	cmd := agentCmd(&options{configPath: path, dryRun: true})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"upgrade", "production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, needle := range []string{
		"environment production",
		"dry-run",
		"planned",
		config.RemoteBinaryPath,
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("upgrade dry-run output missing %q:\n%s", needle, text)
		}
	}
}

func TestProvisionApplyDryRunDoesNotRequireYesOrToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(config.Sample()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("HCLOUD_TOKEN", "")

	var out bytes.Buffer
	cmd := provisionCmd(&options{configPath: path, dryRun: true})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"apply", "production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "would provision web-1 pool=web") {
		t.Fatalf("output = %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, config.LocalStateDir, "environments", "production", "hosts.json")); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote hosts.json: %v", err)
	}
}

func TestProvisionApplyDryRunUsesConfiguredProvider(t *testing.T) {
	path := writeConfig(t)
	prov := &recordingProvider{plans: []providerpkg.HostPlan{
		{Name: "fake-1", Pool: "fake"},
	}}
	installProviderHook(t, prov)

	var out bytes.Buffer
	cmd := provisionCmd(&options{configPath: path, dryRun: true})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"apply", "production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "would provision fake-1 pool=fake") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestProvisionPlanJSON(t *testing.T) {
	path := writeConfig(t)
	var out bytes.Buffer
	cmd := provisionCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"plan", "production", "--json"})
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
	var sawWeb bool
	for _, action := range view.Actions {
		if action.Kind == "provision" && action.Target == "web-1" && action.Details == "pool=web" {
			sawWeb = true
		}
	}
	if !sawWeb {
		t.Fatalf("missing web provision action: %+v", view.Actions)
	}
}

func TestProvisionApplyWithConfigWritesHostFactsNextToConfig(t *testing.T) {
	projectDir := t.TempDir()
	cwd := t.TempDir()
	path := filepath.Join(projectDir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	var bootstrapEvents []string
	installBootstrapHooks(t, &bootstrapEvents)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/networks":
			_ = json.NewEncoder(w).Encode(map[string]any{"networks": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/networks":
			_ = json.NewEncoder(w).Encode(map[string]any{"network": map[string]any{"id": 300, "name": "ship-demo-production-network"}})
		case r.Method == http.MethodGet && r.URL.Path == "/firewalls":
			_ = json.NewEncoder(w).Encode(map[string]any{"firewalls": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/firewalls":
			_ = json.NewEncoder(w).Encode(map[string]any{"firewall": map[string]any{"id": 400, "name": "ship-demo-production-firewall"}})
		case r.Method == http.MethodGet && r.URL.Path == "/servers":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"servers": []any{},
				"meta": map[string]any{
					"pagination": map[string]any{"next_page": nil},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/servers":
			var req struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"server": map[string]any{
					"id":     42,
					"name":   req.Name,
					"labels": req.Labels,
					"public_net": map[string]any{
						"ipv4": map[string]string{"ip": "203.0.113.10"},
					},
				},
				"action": map[string]any{"id": 99, "status": "running"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/actions/99":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"action": map[string]any{"id": 99, "status": "success"},
			})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)

	originalNewEnvironmentProvider := newEnvironmentProvider
	newEnvironmentProvider = func(_ config.Environment, dryRun bool) (providerpkg.Provider, error) {
		return hetzner.Client{
			Token:        "test-token",
			DryRun:       dryRun,
			HTTP:         api.Client(),
			BaseURL:      api.URL,
			PollInterval: time.Nanosecond,
		}, nil
	}
	t.Cleanup(func() {
		newEnvironmentProvider = originalNewEnvironmentProvider
	})

	var out bytes.Buffer
	cmd := provisionCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"apply", "production", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "bootstrapped web-1") {
		t.Fatalf("bootstrap output missing: %q", out.String())
	}
	wantEvents := []string{
		"bootstrap:web-1:run:true",
		"bootstrap:web-1:run:set -eu",
		"bootstrap:web-1:upload:set -eu:16",
		"bootstrap:web-1:run:set -eu",
	}
	if strings.Join(bootstrapEvents, "\n") != strings.Join(wantEvents, "\n") {
		t.Fatalf("bootstrap events = %#v, want %#v", bootstrapEvents, wantEvents)
	}

	wantPath := filepath.Join(projectDir, config.LocalStateDir, "environments", "production", "hosts.json")
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read host facts from config dir: %v", err)
	}
	var hosts []state.HostFact
	if err := json.Unmarshal(data, &hosts); err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 {
		t.Fatalf("hosts = %+v", hosts)
	}
	if hosts[0].Name != "web-1" || hosts[0].IPv4 != "203.0.113.10" || hosts[0].ServerID != 42 {
		t.Fatalf("host fact = %+v", hosts[0])
	}
	cwdPath := filepath.Join(cwd, config.LocalStateDir, "environments", "production", "hosts.json")
	if _, err := os.Stat(cwdPath); !os.IsNotExist(err) {
		t.Fatalf("host facts written in cwd: %v", err)
	}
}

func TestProvisionApplyWritesGenericProviderHostFacts(t *testing.T) {
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	var bootstrapEvents []string
	installBootstrapHooks(t, &bootstrapEvents)
	prov := &recordingProvider{
		name: "fake",
		reconcile: providerpkg.ReconcileResult{
			Desired: []providerpkg.HostPlan{{Name: "web-1", Pool: "web", User: "root"}},
			Created: []providerpkg.Host{{
				ID:            "instance-abc",
				Name:          "web-1",
				Pool:          "web",
				PublicAddress: "host.example.test",
			}},
		},
	}
	installProviderHook(t, prov)

	var out bytes.Buffer
	cmd := provisionCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"apply", "production", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "created web-1 pool=web provider_id=instance-abc public_address=host.example.test") {
		t.Fatalf("output = %q", out.String())
	}
	store := state.NewStore(filepath.Join(projectDir, config.LocalStateDir))
	facts, err := store.ReadHostFacts("production")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("facts = %+v", facts)
	}
	if facts[0].Provider != "fake" || facts[0].ProviderID != "instance-abc" || facts[0].ServerID != 0 || facts[0].IPv4 != "" || facts[0].PublicAddress != "host.example.test" {
		t.Fatalf("host fact = %+v", facts[0])
	}
}

func TestProvisionApplyManualProviderBootstrapsExistingHosts(t *testing.T) {
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(manualHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	var bootstrapEvents []string
	installBootstrapHooks(t, &bootstrapEvents)

	var out bytes.Buffer
	cmd := provisionCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"apply", "production", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "exists web-1.example.com pool=web provider_id=web-1.example.com public_address=web-1.example.com") {
		t.Fatalf("output = %q", out.String())
	}
	if !strings.Contains(out.String(), "bootstrapped web-1.example.com") {
		t.Fatalf("bootstrap output missing: %q", out.String())
	}
	wantEvents := []string{
		"bootstrap:web-1.example.com:run:true",
		"bootstrap:web-1.example.com:run:set -eu",
		"bootstrap:web-1.example.com:upload:set -eu:16",
		"bootstrap:web-1.example.com:run:set -eu",
	}
	if strings.Join(bootstrapEvents, "\n") != strings.Join(wantEvents, "\n") {
		t.Fatalf("bootstrap events = %#v, want %#v", bootstrapEvents, wantEvents)
	}
	store := state.NewStore(filepath.Join(projectDir, config.LocalStateDir))
	facts, err := store.ReadHostFacts("production")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("facts = %+v", facts)
	}
	if facts[0].Provider != config.ProviderManual || facts[0].Name != "web-1.example.com" || facts[0].User != "deploy" || facts[0].PublicAddress != "web-1.example.com" {
		t.Fatalf("host fact = %+v", facts[0])
	}
}

func TestProvisionDecommissionRequiresYes(t *testing.T) {
	path := writeConfig(t)
	cmd := provisionCmd(&options{configPath: path})
	cmd.SetArgs([]string{"decommission", "production"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected confirmation error")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error = %v", err)
	}
}

func TestProvisionDecommissionUsesConfiguredProvider(t *testing.T) {
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(filepath.Join(projectDir, config.LocalStateDir))
	if err := store.SaveHostFacts("production", []state.HostFact{{Name: "web-1", Pool: "web", User: "root"}}); err != nil {
		t.Fatal(err)
	}
	prov := &recordingProvider{listed: []providerpkg.Host{{
		ID:   "instance-abc",
		Name: "web-1",
		Pool: "web",
	}}}
	installProviderHook(t, prov)

	var out bytes.Buffer
	cmd := provisionCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"decommission", "production", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(prov.deleted, ","); got != "instance-abc" {
		t.Fatalf("deleted = %q", got)
	}
	if !strings.Contains(out.String(), "decommissioned web-1 provider_id=instance-abc pool=web") {
		t.Fatalf("output = %q", out.String())
	}
	if _, err := store.ReadHostFacts("production"); !os.IsNotExist(err) {
		t.Fatalf("host facts should be removed, err=%v", err)
	}
}

func TestProvisionDecommissionDeletesServersAndClearsHostFacts(t *testing.T) {
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(filepath.Join(projectDir, config.LocalStateDir))
	if err := store.SaveHostFacts("production", []state.HostFact{{
		Name:          "web-1",
		Pool:          "web",
		User:          "root",
		IPv4:          "203.0.113.10",
		PublicAddress: "203.0.113.10",
		ServerID:      42,
	}}); err != nil {
		t.Fatal(err)
	}

	var deleted []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/servers":
			if got := r.URL.Query().Get("label_selector"); got != "managed-by=ship,project=demo,environment=production" {
				t.Fatalf("label selector = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"servers": []map[string]any{{
					"id":     42,
					"name":   "web-1",
					"labels": map[string]string{"managed-by": "ship", "project": "example", "environment": "production", "pool": "web"},
				}},
				"meta": map[string]any{"pagination": map[string]any{"next_page": nil}},
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/servers/42":
			deleted = append(deleted, "42")
			_ = json.NewEncoder(w).Encode(map[string]any{"action": map[string]any{"id": 99, "status": "running"}})
		case r.Method == http.MethodGet && r.URL.Path == "/actions/99":
			_ = json.NewEncoder(w).Encode(map[string]any{"action": map[string]any{"id": 99, "status": "success"}})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)

	originalNewEnvironmentProvider := newEnvironmentProvider
	newEnvironmentProvider = func(_ config.Environment, dryRun bool) (providerpkg.Provider, error) {
		return hetzner.Client{
			Token:        "test-token",
			DryRun:       dryRun,
			HTTP:         api.Client(),
			BaseURL:      api.URL,
			PollInterval: time.Nanosecond,
		}, nil
	}
	t.Cleanup(func() {
		newEnvironmentProvider = originalNewEnvironmentProvider
	})

	var out bytes.Buffer
	cmd := provisionCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"decommission", "production", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(deleted, ","); got != "42" {
		t.Fatalf("deleted = %q", got)
	}
	if !strings.Contains(out.String(), "decommissioned web-1 server_id=42 pool=web") {
		t.Fatalf("output = %q", out.String())
	}
	if _, err := store.ReadHostFacts("production"); !os.IsNotExist(err) {
		t.Fatalf("host facts should be removed, err=%v", err)
	}
}

func TestAgentInstallDryRunUsesSavedHostFactContact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(singleHostConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveHostFacts("production", []state.HostFact{{
		Name:          "web-1",
		Pool:          "web",
		User:          "root",
		IPv4:          "203.0.113.10",
		PublicAddress: "198.51.100.10",
	}}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := agentCmd(&options{configPath: path, dryRun: true})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"install", "production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "web-1 ssh root@198.51.100.10 ") {
		t.Fatalf("dry-run output did not use saved public address:\n%s", got)
	}
	if strings.Contains(got, "root@web-1 ") {
		t.Fatalf("dry-run output used synthesized host name:\n%s", got)
	}
}

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
	var sawStop bool
	for _, action := range view.RolloutActions {
		if action.Kind == "stop" && action.Container == deployment.ContainerName("demo", "production", "web", 2, "release-old") {
			sawStop = true
		}
	}
	if !sawStop {
		t.Fatalf("rollout actions missing stop for old web replica: %+v", view.RolloutActions)
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

func TestDeployRunsLifecycleHooksInOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(hookDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	var hookRuns []hookRun
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	installDeployHooks(t, &recordingDeployDocker{events: &events, resolved: map[string]string{tag: digestRef}}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})
	installLocalHookRunner(t, &hookRuns, "", &events)

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	wantHooks := []string{
		"pre_deploy:echo root-pre",
		"pre_deploy:echo env-pre",
		"pre_build:echo pre-build",
		"post_deploy:echo post-deploy",
	}
	var gotHooks []string
	for _, run := range hookRuns {
		gotHooks = append(gotHooks, run.Context.Hook+":"+run.Command)
		if run.Context.Project != "demo" || run.Context.Environment != "production" || run.Context.ReleaseID != releaseID || run.Context.ConfigPath != path {
			t.Fatalf("hook context = %+v", run.Context)
		}
	}
	if strings.Join(gotHooks, "\n") != strings.Join(wantHooks, "\n") {
		t.Fatalf("hooks = %#v, want %#v", gotHooks, wantHooks)
	}
	joined := strings.Join(events, "\n")
	preBuildAt := strings.Index(joined, "hook:pre_build:echo pre-build")
	buildAt := strings.Index(joined, "build:"+tag)
	postDeployAt := strings.Index(joined, "hook:post_deploy:echo post-deploy")
	healthyAt := strings.Index(joined, "agent:web-1:write_release_state:"+releaseID+":healthy")
	if preBuildAt < 0 || buildAt < 0 || preBuildAt > buildAt {
		t.Fatalf("pre-build hook did not precede build:\n%s", joined)
	}
	if postDeployAt < 0 || healthyAt < 0 || postDeployAt > healthyAt {
		t.Fatalf("post-deploy hook did not precede healthy promotion:\n%s", joined)
	}
	if hookRuns[3].Hook.Env["SMOKE"] != "1" || hookRuns[3].Hook.TimeoutSeconds != 3 {
		t.Fatalf("post deploy hook config = %+v", hookRuns[3].Hook)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "deploy_hook", "started", releaseID) || !timelineContains(timeline, "deploy_hook", "succeeded", releaseID) {
		t.Fatalf("timeline missing deploy hook events: %+v", timeline)
	}
}

func TestDeployRunsFailureHookWhenLifecycleHookFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(hookDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	var hookRuns []hookRun
	installDeployHooks(t, panicDeployDocker{t: t}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})
	installLocalHookRunner(t, &hookRuns, "pre_build", &events)

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "pre_build hook failed") {
		t.Fatalf("expected pre_build hook failure, got %v", err)
	}
	var gotHooks []string
	for _, run := range hookRuns {
		gotHooks = append(gotHooks, run.Context.Hook+":"+run.Command)
	}
	wantHooks := []string{
		"pre_deploy:echo root-pre",
		"pre_deploy:echo env-pre",
		"pre_build:echo pre-build",
		"deploy_failed:echo failed",
	}
	if strings.Join(gotHooks, "\n") != strings.Join(wantHooks, "\n") {
		t.Fatalf("hooks = %#v, want %#v", gotHooks, wantHooks)
	}
	if !strings.Contains(hookRuns[len(hookRuns)-1].Context.Failure, "pre_build hook failed") {
		t.Fatalf("failure hook context = %+v", hookRuns[len(hookRuns)-1].Context)
	}
	var mutating []string
	for _, event := range events {
		if !strings.HasSuffix(event, ":negotiate") {
			mutating = append(mutating, event)
		}
	}
	if len(mutating) != 4 {
		t.Fatalf("unexpected deploy events after hook failure: %#v", events)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "deploy_hook", "failed", "abc123def456-20260630T183456.123456789Z") || !timelineContains(timeline, "deploy", "failed", "abc123def456-20260630T183456.123456789Z") {
		t.Fatalf("timeline missing failed hook/deploy: %+v", timeline)
	}
}

func TestDeploySendsWebhookNotifications(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(notificationDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	var deliveries []webhookDelivery
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	installDeployHooks(t, &recordingDeployDocker{events: &events, resolved: map[string]string{tag: digestRef}}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})
	installWebhookNotifier(t, &deliveries, nil)

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	if len(deliveries) != 2 {
		t.Fatalf("deliveries = %+v", deliveries)
	}
	if deliveries[0].Webhook.URL != "https://hooks.example/deploys" || deliveries[1].Webhook.URLEnv != "SHIP_NOTIFY_WEBHOOK" {
		t.Fatalf("notification webhook order = %+v", deliveries)
	}
	payload := deliveries[0].Payload
	if payload.Project != "demo" || payload.Environment != "production" || payload.Operation != "deploy" || payload.Status != "succeeded" || payload.Release != releaseID {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Images["web"] != digestRef {
		t.Fatalf("payload images = %+v", payload.Images)
	}
	if !payload.Time.Equal(deployNow().UTC()) {
		t.Fatalf("payload time = %s", payload.Time)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "notification", "succeeded", releaseID) {
		t.Fatalf("timeline missing notification success: %+v", timeline)
	}
}

func TestDeployFailureSendsWebhookNotificationWithoutMaskingError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(notificationDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	var deliveries []webhookDelivery
	installDeployHooks(t, &recordingDeployDocker{events: &events, failStage: "build"}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})
	installWebhookNotifier(t, &deliveries, nil)

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "build failed") {
		t.Fatalf("expected build failure, got %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %+v", deliveries)
	}
	payload := deliveries[0].Payload
	if payload.Operation != "deploy" || payload.Status != "failed" || !strings.Contains(payload.Message, "build failed") {
		t.Fatalf("payload = %+v", payload)
	}
	if len(payload.Images) != 0 {
		t.Fatalf("failed payload images = %+v", payload.Images)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	timeline, timelineErr := store.Events("production")
	if timelineErr != nil {
		t.Fatal(timelineErr)
	}
	if !timelineContains(timeline, "notification", "succeeded", payload.Release) {
		t.Fatalf("timeline missing notification success: %+v", timeline)
	}
}

func TestDeployWebhookNotificationFailureIsRecordedButNonFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(notificationDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	var deliveries []webhookDelivery
	releaseID := "abc123def456-20260630T183456.123456789Z"
	tag := "registry.local/acme/demo:web-" + releaseID
	digestRef := "registry.local/acme/demo@sha256:" + strings.Repeat("1", 64)
	installDeployHooks(t, &recordingDeployDocker{events: &events, resolved: map[string]string{tag: digestRef}}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events}
	})
	installWebhookNotifier(t, &deliveries, errors.New("webhook unavailable"))

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 2 {
		t.Fatalf("deliveries = %+v", deliveries)
	}
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	timeline, err := store.Events("production")
	if err != nil {
		t.Fatal(err)
	}
	if !timelineContains(timeline, "deploy", "succeeded", releaseID) || !timelineContains(timeline, "notification", "failed", releaseID) {
		t.Fatalf("timeline missing deploy success/notification failure: %+v", timeline)
	}
}

func TestDefaultSendWebhookNotificationPostsJSON(t *testing.T) {
	var gotMethod string
	var gotContentType string
	var gotAuth string
	var gotPayload notificationPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	t.Setenv("SHIP_WEBHOOK_URL", server.URL)

	payload := notificationPayload{
		Project:     "demo",
		Environment: "production",
		Operation:   "deploy",
		Status:      "succeeded",
		Release:     "release-1",
		Images:      map[string]string{"web": "image"},
		Time:        time.Unix(10, 0).UTC(),
	}
	err := defaultSendWebhookNotification(context.Background(), config.WebhookNotification{
		URLEnv:         "SHIP_WEBHOOK_URL",
		Headers:        map[string]string{"Authorization": "Bearer test"},
		TimeoutSeconds: 1,
	}, payload)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotContentType != "application/json" || gotAuth != "Bearer test" {
		t.Fatalf("request method/content-type/auth = %q %q %q", gotMethod, gotContentType, gotAuth)
	}
	if gotPayload.Project != payload.Project || gotPayload.Images["web"] != "image" {
		t.Fatalf("payload = %+v", gotPayload)
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

func TestDeploySyncsServiceSchedulesToCurrentReleaseContainers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(scheduledDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var events []string
	var syncs []agent.SyncCronFilesParams
	imageRef := "registry.local/acme/web:stable"
	digestRef := "registry.local/acme/web@sha256:" + strings.Repeat("4", 64)
	installDeployHooks(t, &recordingDeployDocker{events: &events, resolved: map[string]string{imageRef: digestRef}}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events, cronSyncs: &syncs}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	if len(syncs) != 2 {
		t.Fatalf("cron syncs = %#v", syncs)
	}
	var scheduled agent.CronFile
	for _, sync := range syncs {
		if sync.Prefix != "ship-demo-production-" {
			t.Fatalf("prefix = %q", sync.Prefix)
		}
		if len(sync.Files) == 1 {
			scheduled = sync.Files[0]
		}
	}
	if scheduled.Name != "ship-demo-production-web-cleanup" {
		t.Fatalf("scheduled file = %+v", scheduled)
	}
	releaseID := "abc123def456-20260630T183456.123456789Z"
	container := deployment.ContainerName("demo", "production", "web", 2, releaseID)
	for _, needle := range []string{
		"17 * * * * root timeout 300s docker exec '" + container + "' sh -lc 'bin/rails cleanup'",
		">> '/var/log/ship-demo-production-web-cleanup.log' 2>&1",
	} {
		if !strings.Contains(scheduled.Content, needle) {
			t.Fatalf("cron content missing %q:\n%s", needle, scheduled.Content)
		}
	}
	joined := strings.Join(events, "\n")
	scheduleAt := strings.Index(joined, "agent:web-2:sync_cron_files:ship-demo-production-:1")
	healthyAt := strings.Index(joined, "agent:web-2:write_release_state:"+releaseID+":healthy")
	if scheduleAt < 0 || healthyAt < 0 || scheduleAt > healthyAt {
		t.Fatalf("schedule sync did not precede healthy release state:\n%s", joined)
	}
}

func TestDeploySyncsAccessoryBackupSchedules(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(scheduledAccessoryBackupConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveAccessoryState(state.AccessoryState{
		Environment: "production",
		Name:        "postgres",
		Host:        state.HostFact{Name: "data-1", Pool: "data", User: "root"},
		UpdatedAt:   time.Unix(10, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var events []string
	var syncs []agent.SyncCronFilesParams
	imageRef := "registry.local/acme/web:stable"
	digestRef := "registry.local/acme/web@sha256:" + strings.Repeat("5", 64)
	installDeployHooks(t, &recordingDeployDocker{events: &events, resolved: map[string]string{imageRef: digestRef}}, func(host scheduler.Host) deployAgent {
		return recordingDeployAgent{host: host.Name, events: &events, cronSyncs: &syncs}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	if len(syncs) != 1 {
		t.Fatalf("cron syncs = %#v", syncs)
	}
	if syncs[0].Prefix != "ship-demo-production-" || len(syncs[0].Files) != 1 {
		t.Fatalf("cron sync = %+v", syncs[0])
	}
	scheduled := syncs[0].Files[0]
	if scheduled.Name != "ship-demo-production-accessory-postgres-backup" {
		t.Fatalf("scheduled file = %+v", scheduled)
	}
	for _, needle := range []string{
		"13 3 * * * root",
		"timeout 600s",
		"artifact=\"$artifact_dir/postgres-$(date -u +\\%Y\\%m\\%dT\\%H\\%M\\%S.000000000Z).backup\"",
		"pg_dumpall",
		`SHIP_BACKUP_ARTIFACT="$artifact"; export SHIP_BACKUP_ARTIFACT; printf "s3://ship/\%s\n" "$(basename "$SHIP_BACKUP_ARTIFACT")"`,
		"/var/log/ship-demo-production-accessory-postgres-backup.log",
	} {
		if !strings.Contains(scheduled.Content, needle) {
			t.Fatalf("accessory backup schedule missing %q:\n%s", needle, scheduled.Content)
		}
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
		"agent:web-1:stop:ship_demo_production_web_1_old-release",
		"agent:web-1:run:ship_demo_production_web_1_" + releaseID,
		"agent:web-1:health",
	}
	joined := strings.Join(events, "\n")
	for _, want := range wantOrder {
		if !strings.Contains(joined, want) {
			t.Fatalf("events missing %q:\n%s", want, joined)
		}
	}
	pullAt := strings.Index(joined, "agent:web-1:pull:"+digestRef)
	stopAt := strings.Index(joined, "agent:web-1:stop:ship_demo_production_web_1_old-release")
	runAt := strings.Index(joined, "agent:web-1:run:ship_demo_production_web_1_"+releaseID)
	healthAt := strings.Index(joined, "agent:web-1:health")
	if !(pullAt < stopAt && stopAt < runAt && runAt < healthAt) {
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
	stopAt := strings.Index(joined, "agent:web-1:stop:ship_demo_production_web_1_old-release")
	runAt := strings.Index(joined, "agent:web-1:run:ship_demo_production_web_1_"+releaseID)
	if stopAt < 0 || runAt < 0 || stopAt > runAt {
		t.Fatalf("fixed-port health failure did not stop old before start:\n%s", joined)
	}
}

func TestDeployPartialHostFailureLeavesPreviousCurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
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
	workerDigestRef := "registry.local/acme/worker@sha256:" + strings.Repeat("3", 64)
	fakeDocker := &recordingDeployDocker{
		events: &events,
		resolved: map[string]string{
			tag:                                 digestRef,
			"registry.local/acme/worker:stable": workerDigestRef,
		},
	}
	installDeployHooks(t, fakeDocker, func(host scheduler.Host) deployAgent {
		failMethod := ""
		if host.Name == "web-2" {
			failMethod = "run_container"
		}
		return &scriptedDeployAgent{host: host.Name, events: &events, failMethod: failMethod}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected deploy failure")
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "agent:web-1:run:ship_demo_production_web_1_"+releaseID) || !strings.Contains(joined, "agent:web-2:run:ship_demo_production_web_2_"+releaseID) {
		t.Fatalf("partial host events = %#v", events)
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
	secretValue := "postgres://user:pass@example/db"
	t.Setenv("SHIP_TEST_DATABASE_URL", secretValue)

	var events []string
	var writes []agent.WriteFileParams
	var runs []agent.RunContainerParams
	imageRef := "registry.local/acme/web:stable"
	digestRef := "registry.local/acme/web@sha256:" + strings.Repeat("4", 64)
	installDeployHooks(t, &recordingDeployDocker{events: &events, resolved: map[string]string{imageRef: digestRef}}, func(host scheduler.Host) deployAgent {
		return &secretDeployAgent{host: host.Name, events: &events, writes: &writes, runs: &runs}
	})

	cmd := deployCmd(&options{configPath: path})
	cmd.SetArgs([]string{"production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
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

func TestSecretsRenderDryRunRedactsValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(secretDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	secretValue := "postgres://user:pass@example/db"
	t.Setenv("SHIP_TEST_DATABASE_URL", secretValue)

	var out bytes.Buffer
	cmd := secretsCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"render", "production", "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "/var/lib/ship/secrets/production/service-web.env") {
		t.Fatalf("render output missing remote path:\n%s", text)
	}
	if !strings.Contains(text, "SHIP_TEST_DATABASE_URL=<redacted:") {
		t.Fatalf("render output missing redacted secret:\n%s", text)
	}
	if strings.Contains(text, secretValue) {
		t.Fatalf("render output leaked secret value:\n%s", text)
	}
}

func TestSecretsInitSetListExportWorkflow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	identityFile := filepath.Join(dir, "ship-secrets.identity")
	if err := os.WriteFile(identityFile, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := &options{configPath: path, secretsIdentityFile: identityFile}
	runSecrets := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		cmd := secretsCmd(opts)
		cmd.SetOut(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("ship secrets %s: %v\n%s", strings.Join(args, " "), err, out.String())
		}
		return out.String()
	}

	initOut := runSecrets("init", "production", "--recipient", identity.Recipient().String())
	if !strings.Contains(initOut, filepath.Join(dir, ".ship", "secrets", "production.age")) {
		t.Fatalf("init output = %q", initOut)
	}
	t.Setenv("DATABASE_URL", "postgres://user:pass@example/db")
	runSecrets("set", "production", "DATABASE_URL")
	runSecrets("set", "production", "SESSION_SECRET", "--value", "keyboard-cat")

	listOut := runSecrets("list", "production")
	if listOut != "DATABASE_URL\nSESSION_SECRET\n" {
		t.Fatalf("list output = %q", listOut)
	}
	exportOut := runSecrets("export", "production")
	for _, needle := range []string{"DATABASE_URL=postgres://user:pass@example/db", "SESSION_SECRET=keyboard-cat"} {
		if !strings.Contains(exportOut, needle) {
			t.Fatalf("export output missing %q:\n%s", needle, exportOut)
		}
	}
	redactedOut := runSecrets("export", "production", "--redacted")
	if strings.Contains(redactedOut, "postgres://") || strings.Contains(redactedOut, "keyboard-cat") {
		t.Fatalf("redacted export leaked secret values:\n%s", redactedOut)
	}
	for _, needle := range []string{"DATABASE_URL=<redacted:", "SESSION_SECRET=<redacted:"} {
		if !strings.Contains(redactedOut, needle) {
			t.Fatalf("redacted export missing %q:\n%s", needle, redactedOut)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".ship", "secrets", "production.recipients")); err != nil {
		t.Fatalf("recipients file missing: %v", err)
	}
}

func TestSecretsDiffReportsDriftWithoutValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(secretDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	secretValue := "new-secret"
	t.Setenv("SHIP_TEST_DATABASE_URL", secretValue)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{
		ID:          "release-1",
		Environment: "production",
		Images:      map[string]string{"web": "registry.local/acme/web@sha256:" + strings.Repeat("1", 64)},
		SecretDigests: map[string]string{
			"service-web:SHIP_TEST_DATABASE_URL": secretspkg.Digest("old-secret"),
			"service-web:OLD_SECRET":             secretspkg.Digest("old"),
		},
		CreatedAt: time.Unix(30, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := secretsCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"diff", "production"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "secret drift detected") {
		t.Fatalf("expected drift error, got %v", err)
	}
	text := out.String()
	for _, needle := range []string{"changed service-web:SHIP_TEST_DATABASE_URL", "extra service-web:OLD_SECRET"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("diff output missing %q:\n%s", needle, text)
		}
	}
	if strings.Contains(text, secretValue) || strings.Contains(text, "old-secret") {
		t.Fatalf("diff output leaked secret value:\n%s", text)
	}
}

func writeConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(config.Sample()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func singleHostConfig() string {
	return `project: demo
registry: ghcr.io/acme/demo

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
      ref: ghcr.io/acme/demo:web
    pool: web
    scale: 1
`
}

func manualHostConfig() string {
	return `project: demo
registry: ghcr.io/acme/demo

environments:
  production:
    provider:
      manual: {}
    hosts:
      pools:
        web:
          user: deploy
          hosts:
            - web-1.example.com

services:
  web:
    image:
      ref: ghcr.io/acme/demo:web
    pool: web
    scale: 1
`
}

func deployBuildConfig() string {
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
      build: .
      dockerfile: Dockerfile
      tags:
        - latest
        - production
      build_args:
        RAILS_ENV: production
      target: runtime
      builder: ship-cloud
      platform: linux/amd64
      pull: true
      no_cache_filter:
        - install
        - assets
      cache_from:
        - type=registry,ref=registry.local/acme/demo:build-cache
      cache_to:
        - type=registry,ref=registry.local/acme/demo:build-cache,mode=max
      secrets:
        - id=npm_token,env=NPM_TOKEN
      ssh:
        - default
    command: ./bin/server
    pool: web
    scale: 2
    ports: [3000]

  worker:
    image:
      ref: registry.local/acme/worker:stable
    pool: web
    scale: 0
`
}

func deployWithAccessoryConfig() string {
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

accessories:
  redis:
    image: redis:7-alpine
    pool: web
`
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

func hookDeployConfig() string {
	return `project: demo
registry: registry.local/acme/demo

hooks:
  pre_deploy:
    - echo root-pre
  pre_build:
    - echo pre-build
  deploy_failed:
    - echo failed

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
    hooks:
      pre_deploy:
        - echo env-pre
      post_deploy:
        - command: echo post-deploy
          timeout_seconds: 3
          env:
            SMOKE: "1"

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

func notificationDeployConfig() string {
	return `project: demo
registry: registry.local/acme/demo

notifications:
  webhooks:
    - url: https://hooks.example/deploys
      events: [deploy:*]
      headers:
        X-Ship: root

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
    notifications:
      webhooks:
        - url_env: SHIP_NOTIFY_WEBHOOK
          events: [deploy:succeeded]
          timeout_seconds: 3
          headers:
            X-Env: production

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

func restartConfig() string {
	return `project: demo
registry: registry.local/acme/demo
secrets:
  - DATABASE_URL

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
    labels:
      com.example.team: platform
    network_aliases:
      - app
    command: ./bin/server
    pool: web
    scale: 2
    ports: [3000]
    health:
      http: /up
    secrets:
      - DATABASE_URL
    rolling:
      health_retries: 1
      health_interval_seconds: 1
`
}

func scheduledDeployConfig() string {
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
    schedules:
      cleanup:
        cron: "17 * * * *"
        command: bin/rails cleanup
        replica: 2
        timeout_seconds: 300
`
}

func scheduledAccessoryBackupConfig() string {
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
          count: 1

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
    backup:
      command: pg_dumpall
      export_command: 'printf "s3://ship/%s\n" "$(basename "$SHIP_BACKUP_ARTIFACT")"'
      required: true
      schedule:
        cron: "13 3 * * *"
        timeout_seconds: 600
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

func serviceAccessoryStatusConfig() string {
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

accessories:
  postgres:
    image: postgres:17
    pool: web
    primary: true
`
}

func secretDeployConfig() string {
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
    env:
      - RACK_ENV=production
    secrets:
      - SHIP_TEST_DATABASE_URL

secrets:
  - SHIP_TEST_DATABASE_URL
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

func rollingDeployConfig() string {
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
      build: .
      dockerfile: Dockerfile
    command: ./bin/server
    pool: web
    scale: 1
    ports: [3000]
    health:
      http: /up
    ingress:
      domains:
        - example.com
`
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

func accessoryContainer(name, host, status string) docker.ContainerSummary {
	return docker.ContainerSummary{
		ID:     host + "-" + name,
		Image:  "postgres:17",
		Names:  accessorypkg.ContainerName("demo", "production", name),
		Status: status,
		Labels: map[string]string{
			docker.LabelManagedBy:   docker.LabelManagedByValue,
			docker.LabelProject:     "demo",
			docker.LabelEnvironment: "production",
			docker.LabelAccessory:   name,
		},
	}
}

func serviceContainer(host, service string, replica int, release, status string) docker.ContainerSummary {
	return docker.ContainerSummary{
		ID:     fmt.Sprintf("%s-%s-%d", host, service, replica),
		Image:  "registry.local/acme/" + service + ":" + release,
		Names:  deployment.ContainerName("demo", "production", service, replica, release),
		Status: status,
		Labels: map[string]string{
			docker.LabelManagedBy:   docker.LabelManagedByValue,
			docker.LabelProject:     "demo",
			docker.LabelEnvironment: "production",
			docker.LabelService:     service,
			docker.LabelReplica:     strconv.Itoa(replica),
			docker.LabelRelease:     release,
		},
	}
}

func caddyContainer(host, status string) docker.ContainerSummary {
	return docker.ContainerSummary{
		ID:     host + "-caddy",
		Image:  "caddy:2",
		Names:  deployment.CaddyContainerName("demo", "production"),
		Status: status,
		Labels: map[string]string{
			docker.LabelManagedBy:   docker.LabelManagedByValue,
			docker.LabelProject:     "demo",
			docker.LabelEnvironment: "production",
			docker.LabelService:     "caddy",
		},
	}
}

func timelineContains(events []state.Event, kind, status, release string) bool {
	for _, event := range events {
		if event.Kind == kind && event.Status == status && event.Release == release {
			return true
		}
	}
	return false
}

type releaseStateWrite struct {
	Host    string
	Release state.Release
}

type deployAgentFunc func(context.Context, string, any, any) error

func (f deployAgentFunc) Call(ctx context.Context, method string, params any, out any) error {
	return f(ctx, method, params, out)
}

func installAgentHook(t *testing.T, agentFactory func(scheduler.Host) deployAgent) {
	t.Helper()
	originalAgent := newDeployAgent
	newDeployAgent = agentFactory
	t.Cleanup(func() {
		newDeployAgent = originalAgent
	})
}

type observabilityAgent struct {
	host      string
	events    *[]string
	observed  map[string][]docker.ContainerSummary
	logCalls  *[]agent.LogsParams
	execCalls *[]agent.ExecContainerParams
}

func (o *observabilityAgent) Call(ctx context.Context, method string, params any, out any) error {
	switch method {
	case "list_ship_containers":
		*o.events = append(*o.events, fmt.Sprintf("agent:%s:list_ship_containers", o.host))
		if containers, ok := out.(*[]docker.ContainerSummary); ok {
			*containers = append([]docker.ContainerSummary(nil), o.observed[o.host]...)
		}
	case "logs":
		p := params.(agent.LogsParams)
		*o.events = append(*o.events, fmt.Sprintf("agent:%s:logs:%s:%d", o.host, p.Name, p.Lines))
		if o.logCalls != nil {
			*o.logCalls = append(*o.logCalls, p)
		}
		if result, ok := out.(*map[string]string); ok {
			*result = map[string]string{"logs": "logs from " + o.host}
		}
	case "exec_container":
		p := params.(agent.ExecContainerParams)
		*o.events = append(*o.events, fmt.Sprintf("agent:%s:exec:%s:%s", o.host, p.Name, p.Command))
		if o.execCalls != nil {
			*o.execCalls = append(*o.execCalls, p)
		}
		if result, ok := out.(*agent.CommandResult); ok {
			*result = agent.CommandResult{Output: "exec from " + o.host}
		}
	default:
		*o.events = append(*o.events, fmt.Sprintf("agent:%s:%s", o.host, method))
	}
	return nil
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

func installDeployHooks(t *testing.T, dockerClient deployDocker, agentFactory func(scheduler.Host) deployAgent) {
	t.Helper()
	originalDocker := newDeployDocker
	originalAgent := newDeployAgent
	originalNow := deployNow
	originalGitRevision := deployGitRevision
	originalReleaseID := newReleaseID
	newDeployDocker = func() deployDocker { return dockerClient }
	newDeployAgent = agentFactory
	deployNow = func() time.Time {
		return time.Date(2026, 6, 30, 12, 34, 56, 123456789, time.FixedZone("MDT", -6*60*60))
	}
	deployGitRevision = func(context.Context) (string, error) {
		return "abc123def456", nil
	}
	newReleaseID = func() (string, error) {
		return "abc123def456-20260630T183456.123456789Z", nil
	}
	t.Cleanup(func() {
		newDeployDocker = originalDocker
		newDeployAgent = originalAgent
		deployNow = originalNow
		deployGitRevision = originalGitRevision
		newReleaseID = originalReleaseID
	})
}

type hookRun struct {
	Command   string
	Context   hookContext
	Hook      config.HookCommand
	ReleaseID string
}

func installLocalHookRunner(t *testing.T, runs *[]hookRun, failHook string, events *[]string) {
	t.Helper()
	originalRunner := runLocalHookCommand
	runLocalHookCommand = func(ctx context.Context, hook config.HookCommand, hctx hookContext, w io.Writer) error {
		*runs = append(*runs, hookRun{Command: hook.Command, Context: hctx, Hook: hook, ReleaseID: hctx.ReleaseID})
		if events != nil {
			*events = append(*events, "hook:"+hctx.Hook+":"+hook.Command)
		}
		if hctx.Hook == failHook {
			return fmt.Errorf("%s hook failed", hctx.Hook)
		}
		return nil
	}
	t.Cleanup(func() {
		runLocalHookCommand = originalRunner
	})
}

type webhookDelivery struct {
	Webhook config.WebhookNotification
	Payload notificationPayload
}

func installWebhookNotifier(t *testing.T, deliveries *[]webhookDelivery, err error) {
	t.Helper()
	originalNotifier := sendWebhookNotification
	sendWebhookNotification = func(ctx context.Context, webhook config.WebhookNotification, payload notificationPayload) error {
		*deliveries = append(*deliveries, webhookDelivery{Webhook: webhook, Payload: payload})
		return err
	}
	t.Cleanup(func() {
		sendWebhookNotification = originalNotifier
	})
}

func unsetEnv(t *testing.T, name string) {
	t.Helper()
	value, ok := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(name, value)
			return
		}
		_ = os.Unsetenv(name)
	})
}

type recordingDeployDocker struct {
	events    *[]string
	builds    []docker.BuildOptions
	resolved  map[string]string
	auths     map[string]docker.RegistryAuth
	failStage string
}

func (r *recordingDeployDocker) BuildImage(ctx context.Context, opts docker.BuildOptions) error {
	r.builds = append(r.builds, opts)
	*r.events = append(*r.events, "build:"+opts.Tag)
	if r.failStage == "build" {
		return errors.New("build failed")
	}
	return nil
}

func (r *recordingDeployDocker) Push(ctx context.Context, image string) error {
	*r.events = append(*r.events, "push:"+image)
	if r.failStage == "push" {
		return errors.New("push failed")
	}
	return nil
}

func (r *recordingDeployDocker) ResolveDigest(ctx context.Context, image string) (string, error) {
	*r.events = append(*r.events, "resolve:"+image)
	if r.failStage == "resolve" {
		return "", errors.New("resolve failed")
	}
	if resolved := r.resolved[image]; resolved != "" {
		return resolved, nil
	}
	return image + "@sha256:" + strings.Repeat("2", 64), nil
}

func (r *recordingDeployDocker) RegistryAuth(ctx context.Context, image string) (docker.RegistryAuth, bool, error) {
	if r.failStage == "registry_auth" {
		*r.events = append(*r.events, "registry_auth:"+image)
		return docker.RegistryAuth{}, false, errors.New("registry auth failed")
	}
	auth, ok := r.auths[image]
	if ok {
		*r.events = append(*r.events, "registry_auth:"+image)
	}
	return auth, ok, nil
}

type recordingDeployAgent struct {
	host          string
	events        *[]string
	observed      map[string][]docker.ContainerSummary
	releaseWrites *[]releaseStateWrite
	current       *state.Release
	cronSyncs     *[]agent.SyncCronFilesParams
	oneOffRuns    *[]agent.RunOneOffContainerParams
	writes        *[]agent.WriteFileParams
}

func (r recordingDeployAgent) Call(ctx context.Context, method string, params any, out any) error {
	switch method {
	case "write_release_state":
		p := params.(agent.WriteReleaseStateParams)
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:write_release_state:%s:%s", r.host, p.Release.ID, p.Release.Status))
		if r.releaseWrites != nil {
			*r.releaseWrites = append(*r.releaseWrites, releaseStateWrite{Host: r.host, Release: p.Release})
		}
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
	case "pull":
		image := params.(map[string]string)["image"]
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:pull:%s", r.host, image))
	case "write_registry_auth":
		p := params.(agent.WriteRegistryAuthParams)
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:write_registry_auth:%s", r.host, p.Server))
	case "write_file":
		p := params.(agent.WriteFileParams)
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:write_file:%s", r.host, p.Path))
		if r.writes != nil {
			*r.writes = append(*r.writes, p)
		}
	case "sync_cron_files":
		p := params.(agent.SyncCronFilesParams)
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:sync_cron_files:%s:%d", r.host, p.Prefix, len(p.Files)))
		if r.cronSyncs != nil {
			*r.cronSyncs = append(*r.cronSyncs, p)
		}
	case "ensure_network":
		return nil
	case "run_container":
		p := params.(agent.RunContainerParams)
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:run:%s", r.host, p.Image))
	case "health_check":
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:health_check", r.host))
		if result, ok := out.(*agent.HealthCheckResult); ok {
			*result = agent.HealthCheckResult{OK: true}
		}
	case "run_oneoff_container":
		p := params.(agent.RunOneOffContainerParams)
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:run_oneoff:%s:%s", r.host, p.Name, p.Command))
		if r.oneOffRuns != nil {
			*r.oneOffRuns = append(*r.oneOffRuns, p)
		}
		if result, ok := out.(*agent.CommandResult); ok {
			*result = agent.CommandResult{Output: "one-off complete"}
		}
	case "docker_inspect":
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:docker_inspect", r.host))
		populateRunningDockerInspect(out)
	default:
		*r.events = append(*r.events, fmt.Sprintf("agent:%s:%s", r.host, method))
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

type secretDeployAgent struct {
	host          string
	events        *[]string
	writes        *[]agent.WriteFileParams
	runs          *[]agent.RunContainerParams
	releaseWrites *[]releaseStateWrite
}

func (s *secretDeployAgent) Call(ctx context.Context, method string, params any, out any) error {
	switch method {
	case "write_release_state":
		p := params.(agent.WriteReleaseStateParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:write_release_state:%s:%s", s.host, p.Release.ID, p.Release.Status))
		if s.releaseWrites != nil {
			*s.releaseWrites = append(*s.releaseWrites, releaseStateWrite{Host: s.host, Release: p.Release})
		}
	case "write_file":
		p := params.(agent.WriteFileParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:write_file:%s:%04o", s.host, p.Path, p.Mode))
		if s.writes != nil {
			*s.writes = append(*s.writes, p)
		}
	case "list_ship_containers":
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:list_ship_containers", s.host))
		if containers, ok := out.(*[]docker.ContainerSummary); ok {
			*containers = nil
		}
	case "pull":
		image := params.(map[string]string)["image"]
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:pull:%s", s.host, image))
	case "ensure_network":
		return nil
	case "run_container":
		p := params.(agent.RunContainerParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:run:%s", s.host, p.Name))
		if s.runs != nil {
			*s.runs = append(*s.runs, p)
		}
	default:
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:%s", s.host, method))
	}
	return nil
}

type scriptedDeployAgent struct {
	host          string
	events        *[]string
	observed      []docker.ContainerSummary
	failMethod    string
	releaseWrites *[]releaseStateWrite
}

func (s *scriptedDeployAgent) Call(ctx context.Context, method string, params any, out any) error {
	switch method {
	case "write_release_state":
		p := params.(agent.WriteReleaseStateParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:write_release_state:%s:%s", s.host, p.Release.ID, p.Release.Status))
		if s.releaseWrites != nil {
			*s.releaseWrites = append(*s.releaseWrites, releaseStateWrite{Host: s.host, Release: p.Release})
		}
	case "list_ship_containers":
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:list_ship_containers", s.host))
		if containers, ok := out.(*[]docker.ContainerSummary); ok {
			*containers = append([]docker.ContainerSummary(nil), s.observed...)
		}
	case "pull":
		image := params.(map[string]string)["image"]
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:pull:%s", s.host, image))
	case "ensure_network", "write_file", "run_oneoff_container":
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:%s", s.host, method))
	case "docker_inspect":
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:docker_inspect", s.host))
		populateRunningDockerInspect(out)
	case "run_container":
		p := params.(agent.RunContainerParams)
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:run:%s", s.host, p.Name))
	case "health_check":
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:health", s.host))
		if result, ok := out.(*agent.HealthCheckResult); ok {
			*result = agent.HealthCheckResult{OK: true}
		}
	case "stop_container":
		name := params.(map[string]string)["name"]
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:stop:%s", s.host, name))
	default:
		*s.events = append(*s.events, fmt.Sprintf("agent:%s:%s", s.host, method))
	}
	if method == s.failMethod {
		return fmt.Errorf("%s failed", method)
	}
	return nil
}

func populateRunningDockerInspect(out any) {
	if result, ok := out.(*agent.DockerInspectResult); ok {
		result.Inspect = json.RawMessage(`[{"State":{"Running":true}}]`)
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

type panicDeployDocker struct {
	t *testing.T
}

func (p panicDeployDocker) BuildImage(context.Context, docker.BuildOptions) error {
	p.t.Fatal("unexpected build")
	return nil
}

func (p panicDeployDocker) Push(context.Context, string) error {
	p.t.Fatal("unexpected push")
	return nil
}

func (p panicDeployDocker) ResolveDigest(context.Context, string) (string, error) {
	p.t.Fatal("unexpected digest resolve")
	return "", nil
}

func (p panicDeployDocker) RegistryAuth(context.Context, string) (docker.RegistryAuth, bool, error) {
	p.t.Fatal("unexpected registry auth")
	return docker.RegistryAuth{}, false, nil
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

func boolPtr(value bool) *bool {
	return &value
}
