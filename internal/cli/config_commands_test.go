package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/state"
)

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
