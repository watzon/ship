package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/cli/ui"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/deployment"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/secrets"
	"github.com/watzon/ship/internal/shipbinary"
	"github.com/watzon/ship/internal/state"
	"github.com/watzon/ship/internal/transport"
)

// addAgentBinaryOverrideFlags registers the airgap overrides on every command
// that can place an agent binary on a host (directly or via
// --auto-upgrade-agents).
func addAgentBinaryOverrideFlags(cmd *cobra.Command, opts *options) {
	cmd.Flags().StringVar(&opts.agentBinaryPath, "agent-binary", "", "prebuilt ship binary (or release .tar.gz) to install on hosts instead of cross-compiling or downloading")
	cmd.Flags().StringVar(&opts.agentReleaseDir, "agent-release-dir", "", "local mirror of release assets (ship_*_<os>_<arch>.tar.gz + checksums.txt) for hosts without GitHub access")
}

// agentBinaryOverrides resolves the airgap overrides: explicit flags first,
// then the environment. Both being set is rejected inside shipbinary.Resolve.
func agentBinaryOverrides(opts *options) shipbinary.Options {
	overrides := shipbinary.Options{BinaryPath: opts.agentBinaryPath, ReleaseDir: opts.agentReleaseDir}
	if overrides.BinaryPath == "" {
		overrides.BinaryPath = os.Getenv("SHIP_AGENT_BINARY")
	}
	if overrides.ReleaseDir == "" {
		overrides.ReleaseDir = os.Getenv("SHIP_AGENT_RELEASE_DIR")
	}
	return overrides
}

func agentBinaryOverridden(opts *options) bool {
	overrides := agentBinaryOverrides(opts)
	return overrides.BinaryPath != "" || overrides.ReleaseDir != ""
}

type deployAgent interface {
	Call(ctx context.Context, method string, params any, out any) error
}

var newDeployAgent = func(host scheduler.Host) deployAgent {
	return agent.Client{SSH: sshForHost(host, false)}
}

var deployNow = time.Now

var eventWarningWriter io.Writer = os.Stderr

var readCurrentShipBinary = func() ([]byte, error) {
	path, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

var resolveShipBinaryForHost = shipBinaryForHost

type bootstrapSSH interface {
	Run(ctx context.Context, command string) (string, error)
	RunWithStdin(ctx context.Context, command, stdin string) (string, error)
}

var newBootstrapSSH = func(host scheduler.Host, dryRun bool) bootstrapSSH {
	return sshForHost(host, dryRun)
}

var bootstrapMaxAttempts = 30

var bootstrapRetryDelay = 2 * time.Second

func sshForHost(host scheduler.Host, dryRun bool) transport.SSH {
	return transport.SSH{
		User:           host.User,
		Host:           host.ContactTarget(),
		DryRun:         dryRun,
		Port:           host.SSHPort,
		IdentityFile:   host.IdentityFile,
		KnownHostsFile: host.KnownHostsFile,
		JumpHost:       host.JumpHost,
		Options:        host.SSHOptions,
	}
}

func localStateDirForConfig(configPath string) (string, error) {
	if strings.TrimSpace(configPath) == "" {
		configPath = config.DefaultConfigFile
	}
	absPath, err := filepath.Abs(configPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(absPath), config.LocalStateDir), nil
}

func environmentContext(opts *options, envName string) (*config.Config, config.Environment, state.Store, error) {
	cfg, err := config.Load(opts.configPath)
	if err != nil {
		return nil, config.Environment{}, state.Store{}, err
	}
	resolved, env, err := cfg.ResolveEnvironment(envName)
	if err != nil {
		return nil, config.Environment{}, state.Store{}, err
	}
	stateDir, err := localStateDirForConfig(opts.configPath)
	if err != nil {
		return nil, config.Environment{}, state.Store{}, err
	}
	return resolved, env, state.NewStore(stateDir), nil
}

func secretSourceOptions(opts *options, envName string) (secrets.SourceOptions, error) {
	stateDir, err := localStateDirForConfig(opts.configPath)
	if err != nil {
		return secrets.SourceOptions{}, err
	}
	return secrets.SourceOptions{
		EnvName:      envName,
		ConfigPath:   opts.configPath,
		StateDir:     stateDir,
		EnvFiles:     append([]string(nil), opts.envFiles...),
		IdentityFile: opts.secretsIdentityFile,
	}, nil
}

type hostFactKey struct {
	name string
	pool string
}

func resolvedHostsForEnvironment(store state.Store, envName string, env config.Environment) ([]scheduler.Host, error) {
	hosts := scheduler.HostsForEnvironment(env)
	facts, err := store.ReadHostFacts(envName)
	if errors.Is(err, os.ErrNotExist) {
		return dedupeHosts(hosts), nil
	}
	if err != nil {
		return nil, err
	}
	if len(facts) == 0 {
		return dedupeHosts(hosts), nil
	}
	hosts, err = applyHostFacts(envName, hosts, facts)
	if err != nil {
		return nil, err
	}
	return dedupeHosts(hosts), nil
}

func dedupeHosts(hosts []scheduler.Host) []scheduler.Host {
	seen := map[string]struct{}{}
	out := make([]scheduler.Host, 0, len(hosts))
	for _, host := range hosts {
		key := host.User + "\x00" + host.ContactTarget()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, host)
	}
	return out
}

func applyHostFacts(envName string, hosts []scheduler.Host, facts []state.HostFact) ([]scheduler.Host, error) {
	factsByKey := map[hostFactKey]state.HostFact{}
	for _, fact := range facts {
		key := hostFactKey{name: strings.TrimSpace(fact.Name), pool: strings.TrimSpace(fact.Pool)}
		if key.name == "" || key.pool == "" {
			return nil, fmt.Errorf("host facts for %s contain a host without name or pool", envName)
		}
		if _, exists := factsByKey[key]; exists {
			return nil, fmt.Errorf("host facts for %s contain duplicate host %s in pool %s", envName, key.name, key.pool)
		}
		factsByKey[key] = fact
	}

	resolved := append([]scheduler.Host(nil), hosts...)
	matched := map[hostFactKey]struct{}{}
	var missing []string
	for i, host := range resolved {
		key := hostFactKey{name: host.Name, pool: host.Pool}
		fact, ok := factsByKey[key]
		if !ok {
			missing = append(missing, fmt.Sprintf("%s pool=%s", host.Name, host.Pool))
			continue
		}
		if user := strings.TrimSpace(fact.User); user != "" {
			resolved[i].User = user
		}
		if fact.SSHPort > 0 {
			resolved[i].SSHPort = fact.SSHPort
		}
		if strings.TrimSpace(fact.IdentityFile) != "" {
			resolved[i].IdentityFile = fact.IdentityFile
		}
		if strings.TrimSpace(fact.KnownHostsFile) != "" {
			resolved[i].KnownHostsFile = fact.KnownHostsFile
		}
		if strings.TrimSpace(fact.JumpHost) != "" {
			resolved[i].JumpHost = fact.JumpHost
		}
		if len(fact.SSHOptions) > 0 {
			resolved[i].SSHOptions = mergeStringMap(resolved[i].SSHOptions, fact.SSHOptions)
		}
		resolved[i].Contact = hostFactContact(fact)
		matched[key] = struct{}{}
	}

	var extra []string
	for key := range factsByKey {
		if _, ok := matched[key]; ok {
			continue
		}
		extra = append(extra, fmt.Sprintf("%s pool=%s", key.name, key.pool))
	}
	if len(missing) > 0 || len(extra) > 0 {
		sort.Strings(missing)
		sort.Strings(extra)
		parts := []string{}
		if len(missing) > 0 {
			parts = append(parts, "missing "+strings.Join(missing, ", "))
		}
		if len(extra) > 0 {
			parts = append(parts, "extra "+strings.Join(extra, ", "))
		}
		return nil, fmt.Errorf("host facts for %s do not match configured hosts: %s; run ship provision apply %s --yes", envName, strings.Join(parts, "; "), envName)
	}
	return resolved, nil
}

func hostFactContact(fact state.HostFact) string {
	if contact := strings.TrimSpace(fact.PublicAddress); contact != "" {
		return contact
	}
	return strings.TrimSpace(fact.IPv4)
}

func copyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func mergeStringMap(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := copyStringMap(base)
	if out == nil {
		out = map[string]string{}
	}
	for key, value := range override {
		out[key] = value
	}
	return out
}

func shipBinaryForHost(ctx context.Context, host scheduler.Host, opts *options) ([]byte, error) {
	if opts.dryRun {
		return readCurrentShipBinary()
	}
	ssh := newBootstrapSSH(host, opts.dryRun)
	// OS detection is the first SSH contact with a freshly provisioned server,
	// which may still be booting — wait for readiness like bootstrapHost does.
	if err := waitForSSH(ctx, host, ssh); err != nil {
		return nil, err
	}
	sysname, err := ssh.Run(ctx, "uname -s")
	if err != nil {
		return nil, fmt.Errorf("detect remote OS on %s: %w", host.Name, err)
	}
	machine, err := ssh.Run(ctx, "uname -m")
	if err != nil {
		return nil, fmt.Errorf("detect remote architecture on %s: %w", host.Name, err)
	}
	target, err := shipbinary.ParseUname(sysname, machine)
	if err != nil {
		return nil, fmt.Errorf("unsupported remote platform on %s: %w", host.Name, err)
	}
	return shipbinary.Resolve(ctx, target, agentBinaryOverrides(opts))
}

func bootstrapHost(ctx context.Context, host scheduler.Host, shipBinary []byte, dryRun bool) error {
	ssh := newBootstrapSSH(host, dryRun)
	if err := waitForSSH(ctx, host, ssh); err != nil {
		return err
	}
	if _, err := ssh.Run(ctx, bootstrapPrerequisitesCommand()); err != nil {
		return fmt.Errorf("bootstrap prerequisites on %s: %w", host.Name, err)
	}
	if _, err := ssh.RunWithStdin(ctx, uploadShipBinaryCommand(), string(shipBinary)); err != nil {
		return fmt.Errorf("upload ship binary to %s: %w", host.Name, err)
	}
	if _, err := ssh.Run(ctx, installAgentServiceCommand()); err != nil {
		return fmt.Errorf("install agent service on %s: %w", host.Name, err)
	}
	return nil
}

func waitForSSH(ctx context.Context, host scheduler.Host, ssh bootstrapSSH) error {
	attempts := bootstrapMaxAttempts
	if attempts <= 0 {
		attempts = 1
	}
	delay := bootstrapRetryDelay
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if _, err := ssh.Run(ctx, "true"); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if attempt == attempts {
			break
		}
		if delay <= 0 {
			continue
		}
		timer := time.NewTimer(delayForAttempt(delay, attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("wait for SSH readiness on %s: %w", host.Name, ctx.Err())
		case <-timer.C:
		}
	}
	return fmt.Errorf("wait for SSH readiness on %s after %d attempts: %w", host.Name, attempts, lastErr)
}

func delayForAttempt(base time.Duration, attempt int) time.Duration {
	if attempt <= 1 {
		return base
	}
	delay := base
	for i := 1; i < attempt && delay < 30*time.Second; i++ {
		delay *= 2
	}
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func bootstrapPrerequisitesCommand() string {
	return strings.Join([]string{
		"set -eu",
		"mkdir -p " + config.RemoteStateDir,
		"if ! command -v docker >/dev/null 2>&1; then",
		"  if command -v apt-get >/dev/null 2>&1; then",
		"    export DEBIAN_FRONTEND=noninteractive",
		"    apt-get update",
		"    apt-get install -y docker.io ca-certificates curl",
		"  elif command -v dnf >/dev/null 2>&1; then",
		"    dnf install -y docker",
		"  elif command -v yum >/dev/null 2>&1; then",
		"    yum install -y docker",
		"  else",
		"    echo 'no supported package manager found for Docker install' >&2",
		"    exit 1",
		"  fi",
		"fi",
		"if command -v systemctl >/dev/null 2>&1; then systemctl enable --now docker >/dev/null 2>&1 || true; fi",
		"test -w " + config.RemoteStateDir,
	}, "\n")
}

func uploadShipBinaryCommand() string {
	return strings.Join([]string{
		"set -eu",
		"install -d -m 0755 " + filepath.Dir(config.RemoteBinaryPath),
		"tmp=$(mktemp /tmp/ship.XXXXXX)",
		"cat > \"$tmp\"",
		"install -m 0755 \"$tmp\" " + config.RemoteBinaryPath,
		"rm -f \"$tmp\"",
	}, "\n")
}

func installAgentServiceCommand() string {
	return fmt.Sprintf("set -eu\nmkdir -p %s\ncat >/etc/systemd/system/ship-agent.service <<'EOF'\n%s\nEOF\nsystemctl daemon-reload\nsystemctl enable --now ship-agent", config.RemoteStateDir, systemdUnit())
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func recordEvent(store state.Store, event state.Event) {
	if event.Time.IsZero() {
		event.Time = deployNow().UTC()
	}
	if err := store.RecordEvent(event); err != nil {
		fmt.Fprintf(eventWarningWriter, "warning: could not persist event environment=%q kind=%q status=%q: %v\n",
			strings.TrimSpace(event.Environment), strings.TrimSpace(event.Kind), strings.TrimSpace(event.Status), err)
	}
}

func configHash(cfg *config.Config) string {
	data, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum[:])
}

func remoteCurrentRelease(ctx context.Context, hosts []scheduler.Host, envName string) (*state.Release, []string, error) {
	byID := map[string]state.Release{}
	var failures []string
	for _, host := range hosts {
		var release state.Release
		err := newDeployAgent(host).Call(ctx, "read_release_state", agent.ReadReleaseStateParams{Environment: envName}, &release)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", host.Name, err))
			continue
		}
		if strings.TrimSpace(release.ID) != "" {
			byID[release.ID] = release
		}
	}
	var warnings []string
	if len(failures) > 0 {
		warnings = append(warnings, fmt.Sprintf("remote release state unavailable on %d/%d hosts", len(failures), len(hosts)))
	}
	if len(byID) == 0 {
		return nil, warnings, nil
	}
	if len(byID) > 1 {
		ids := make([]string, 0, len(byID))
		for id := range byID {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		warnings = append(warnings, "remote release state disagrees across hosts: "+strings.Join(ids, ", "))
		return nil, warnings, nil
	}
	for _, release := range byID {
		return &release, warnings, nil
	}
	return nil, warnings, nil
}

func currentReleaseForServiceMutation(ctx context.Context, cfg *config.Config, envName string, store state.Store, hosts []scheduler.Host) (state.Release, error) {
	var local state.Release
	var localErr error
	localID := ""
	if release, err := store.CurrentRelease(envName); err == nil {
		local = release
		localID = release.ID
	} else {
		localErr = err
	}

	observed, err := deployment.InspectObservedOnHosts(ctx, hosts, deploymentAgentFactory())
	if err != nil {
		return state.Release{}, fmt.Errorf("determine current release for %q: %w", envName, err)
	}
	runningReleases := observedRunningServiceReleases(cfg, envName, observed)
	if localErr != nil && len(runningReleases) == 0 {
		return state.Release{}, localErr
	}
	if !shouldUseRemoteReleaseState(localID, cfg, envName, observed) {
		if localErr == nil {
			return local, nil
		}
		return state.Release{}, localErr
	}

	remote, warnings, err := remoteCurrentRelease(ctx, hosts, envName)
	if err != nil {
		return state.Release{}, fmt.Errorf("determine current release for %q from host state: %w", envName, err)
	}
	if remote != nil {
		return *remote, nil
	}
	detail := "host release state is unavailable"
	if len(warnings) > 0 {
		detail = strings.Join(warnings, "; ")
	}
	if localID != "" {
		return state.Release{}, fmt.Errorf("could not determine current release for %q: local release %s is not running and %s", envName, localID, detail)
	}
	return state.Release{}, fmt.Errorf("could not determine current release for %q: %s", envName, detail)
}

type agentUpgradeView struct {
	Environment string             `json:"environment"`
	ShipVersion string             `json:"ship_version"`
	SHA256      string             `json:"sha256"`
	DryRun      bool               `json:"dry_run,omitempty"`
	Hosts       []agentUpgradeHost `json:"hosts"`
}

type agentUpgradeHost struct {
	Name      string `json:"name"`
	Pool      string `json:"pool"`
	Contact   string `json:"contact"`
	Path      string `json:"path,omitempty"`
	Installed bool   `json:"installed,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Error     string `json:"error,omitempty"`
}

func upgradeAgents(ctx context.Context, opts *options, envName string) (agentUpgradeView, error) {
	_, env, store, err := environmentContext(opts, envName)
	if err != nil {
		return agentUpgradeView{}, err
	}
	hosts, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return agentUpgradeView{}, err
	}
	view := agentUpgradeView{
		Environment: envName,
		ShipVersion: agent.Version(),
		DryRun:      opts.dryRun,
	}
	for _, host := range hosts {
		shipBinary, err := resolveShipBinaryForHost(ctx, host, opts)
		if err != nil {
			return agentUpgradeView{}, fmt.Errorf("resolve ship binary for %s: %w", host.Name, err)
		}
		sum := sha256.Sum256(shipBinary)
		digest := fmt.Sprintf("%x", sum[:])
		entry := agentUpgradeHost{
			Name:    host.Name,
			Pool:    host.Pool,
			Contact: host.ContactTarget(),
			Path:    config.RemoteBinaryPath,
			SHA256:  digest,
		}
		if opts.dryRun {
			view.Hosts = append(view.Hosts, entry)
			continue
		}
		if len(view.Hosts) == 0 {
			recordEvent(store, state.Event{Environment: envName, Kind: "agent_upgrade", Status: "started", Message: fmt.Sprintf("hosts=%d", len(hosts))})
		}
		ssh := newBootstrapSSH(host, opts.dryRun)
		// Restore point beside the target; best-effort since a first install
		// has nothing to back up.
		_, _ = ssh.Run(ctx, backupShipBinaryCommand())
		result, err := installAgentBinary(ctx, host, ssh, shipBinary, digest)
		if err != nil {
			entry.Error = err.Error()
			recordEvent(store, state.Event{Environment: envName, Kind: "agent_upgrade", Status: "failed", Host: host.Name, Message: err.Error()})
			view.Hosts = append(view.Hosts, entry)
			continue
		}
		entry.Path = result.Path
		entry.Installed = result.Installed
		entry.SHA256 = result.SHA256
		if err := verifyUpgradedAgent(ctx, host, view.ShipVersion, agentBinaryOverridden(opts)); err != nil {
			restoreMsg := "previous binary restored from " + config.RemoteBinaryPath + ".bak"
			if _, restoreErr := ssh.Run(ctx, restoreShipBinaryCommand()); restoreErr != nil {
				restoreMsg = fmt.Sprintf("restore of previous binary also failed: %v", restoreErr)
			}
			entry.Error = fmt.Sprintf("post-upgrade check failed: %v (%s)", err, restoreMsg)
			recordEvent(store, state.Event{Environment: envName, Kind: "agent_upgrade", Status: "failed", Host: host.Name, Message: entry.Error})
			view.Hosts = append(view.Hosts, entry)
			continue
		}
		if view.SHA256 == "" {
			view.SHA256 = digest
		}
		status := "unchanged"
		if result.Installed {
			status = "installed"
		}
		recordEvent(store, state.Event{Environment: envName, Kind: "agent_upgrade", Status: "succeeded", Host: host.Name, Message: fmt.Sprintf("%s sha256=%s", status, result.SHA256)})
		view.Hosts = append(view.Hosts, entry)
	}
	return view, nil
}

// installAgentBinary installs through the agent RPC, falling back to a plain
// SSH upload when the host agent predates the install_binary method.
func installAgentBinary(ctx context.Context, host scheduler.Host, ssh bootstrapSSH, shipBinary []byte, digest string) (agent.InstallBinaryResult, error) {
	var result agent.InstallBinaryResult
	err := newDeployAgent(host).Call(ctx, "install_binary", agent.InstallBinaryParams{
		Path:          config.RemoteBinaryPath,
		ContentBase64: base64.StdEncoding.EncodeToString(shipBinary),
		SHA256:        digest,
		Mode:          0o755,
	}, &result)
	if err == nil {
		return result, nil
	}
	var remote agent.RemoteError
	if errors.As(err, &remote) && remote.Code == agent.ErrorUnknownMethod {
		if _, sshErr := ssh.RunWithStdin(ctx, uploadShipBinaryCommand(), string(shipBinary)); sshErr != nil {
			return agent.InstallBinaryResult{}, fmt.Errorf("%w; SSH upload fallback also failed: %v", err, sshErr)
		}
		return agent.InstallBinaryResult{Path: config.RemoteBinaryPath, Installed: true, SHA256: digest}, nil
	}
	return agent.InstallBinaryResult{}, err
}

// verifyUpgradedAgent exercises the freshly installed binary — every RPC
// execs it anew over SSH — so a broken upgrade is caught while the .bak
// restore point still exists. When an explicit --agent-binary or release-dir
// override supplied the bytes, its version may legitimately differ from this
// CLI's, so only the does-it-answer check applies.
func verifyUpgradedAgent(ctx context.Context, host scheduler.Host, wantVersion string, versionOverridden bool) error {
	var status agent.Status
	if err := newDeployAgent(host).Call(ctx, "status", map[string]any{}, &status); err != nil {
		return fmt.Errorf("upgraded agent did not answer status: %w", err)
	}
	if !versionOverridden && status.AgentVersion != wantVersion {
		return fmt.Errorf("upgraded agent reports version %s, expected %s", status.AgentVersion, wantVersion)
	}
	return nil
}

// preflightAgentProtocols negotiates with every host agent before a rollout
// touches anything. Compatible agents proceed (older-but-in-window with an
// info line); incompatible or pre-negotiation agents stop the operation with
// the one command that fixes it, or are upgraded inline when autoUpgrade is
// set.
var requiredRolloutAgentMethods = []string{"remove_container", "start_container", "stop_container_keep"}

func preflightAgentProtocols(ctx context.Context, w io.Writer, opts *options, envName string, hosts []scheduler.Host, autoUpgrade bool) error {
	var incompatible []string
	for _, host := range hosts {
		var result agent.NegotiateResult
		err := newDeployAgent(host).Call(ctx, "negotiate", agent.NegotiateParams{
			ClientVersion:      agent.Version(),
			MinProtocolVersion: agent.AgentProtocol,
			MaxProtocolVersion: agent.AgentProtocol,
		}, &result)
		if err != nil {
			var remote agent.RemoteError
			switch {
			case errors.As(err, &remote) && remote.Code == agent.ErrorUnknownMethod:
				incompatible = append(incompatible, host.Name+": agent predates protocol negotiation")
			case errors.As(err, &remote) && remote.Code == agent.ErrorIncompatibleProtocol:
				incompatible = append(incompatible, host.Name+": "+remote.Message)
			default:
				return fmt.Errorf("agent preflight on %s: %w", host.Name, err)
			}
			continue
		}
		if result.SupportedMethods != nil {
			missing := missingAgentMethods(result.SupportedMethods, requiredRolloutAgentMethods)
			if len(missing) > 0 {
				incompatible = append(incompatible, host.Name+": missing required rollout methods: "+strings.Join(missing, ", "))
				continue
			}
		}
		if result.AgentVersion != "" && result.AgentVersion != agent.Version() {
			fmt.Fprintf(w, "agent on %s is version %s (CLI %s); protocol %d is compatible\n", host.Name, result.AgentVersion, agent.Version(), result.ProtocolVersion)
		}
	}
	if len(incompatible) == 0 {
		return nil
	}
	if autoUpgrade {
		fmt.Fprintf(w, "upgrading %d agent(s) before rollout\n", len(incompatible))
		view, err := upgradeAgents(ctx, opts, envName)
		if err != nil {
			return fmt.Errorf("auto-upgrade agents: %w", err)
		}
		if failed := countAgentUpgradeFailures(view); failed > 0 {
			return fmt.Errorf("auto-upgrade agents failed on %d/%d hosts", failed, len(view.Hosts))
		}
		return preflightAgentProtocols(ctx, w, opts, envName, hosts, false)
	}
	return fmt.Errorf("incompatible agents:\n  %s\n\nFix: ship agent upgrade %s", strings.Join(incompatible, "\n  "), envName)
}

func missingAgentMethods(supported, required []string) []string {
	available := make(map[string]struct{}, len(supported))
	for _, method := range supported {
		available[method] = struct{}{}
	}
	var missing []string
	for _, method := range required {
		if _, ok := available[method]; !ok {
			missing = append(missing, method)
		}
	}
	return missing
}

func backupShipBinaryCommand() string {
	path := config.RemoteBinaryPath
	return fmt.Sprintf("set -eu\nif [ -f %s ]; then cp -p %s %s.bak; fi", path, path, path)
}

func restoreShipBinaryCommand() string {
	path := config.RemoteBinaryPath
	return fmt.Sprintf("set -eu\ntest -f %s.bak\ncp -p %s.bak %s", path, path, path)
}

func recordRolloutCleanupWarnings(w io.Writer, store state.Store, envName, kind, releaseID string, warnings []deployment.CleanupWarning) {
	for _, warning := range warnings {
		message := warning.Error()
		recordEvent(store, state.Event{
			Environment: envName,
			Kind:        kind,
			Status:      "warning",
			Release:     releaseID,
			Host:        warning.Host.Name,
			Message:     message,
		})
		ui.PrintWarn(w, message)
	}
}

func syncRemoteReleaseStateWithEvents(ctx context.Context, store state.Store, envName string, hosts []scheduler.Host, kind string, release state.Release) error {
	recordEvent(store, state.Event{Environment: envName, Kind: kind, Status: "started", Release: release.ID, Message: fmt.Sprintf("status=%s hosts=%d", release.Status, len(hosts))})
	if err := syncRemoteReleaseState(ctx, hosts, release); err != nil {
		recordEvent(store, state.Event{Environment: envName, Kind: kind, Status: "failed", Release: release.ID, Message: err.Error()})
		return err
	}
	recordEvent(store, state.Event{Environment: envName, Kind: kind, Status: "succeeded", Release: release.ID, Message: fmt.Sprintf("status=%s hosts=%d", release.Status, len(hosts))})
	return nil
}

func syncRemoteReleaseState(ctx context.Context, hosts []scheduler.Host, release state.Release) error {
	var failures []string
	for _, host := range hosts {
		if err := newDeployAgent(host).Call(ctx, "write_release_state", agent.WriteReleaseStateParams{Release: release}, nil); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", host.Name, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("write release state %s failed on %d/%d hosts: %s", release.ID, len(failures), len(hosts), strings.Join(failures, "; "))
	}
	return nil
}

// resolveServiceReplica accepts a user-supplied SERVICE argument in any of
// three forms: a bare service name from ship.yml, the "service.N" shorthand
// shown in `ship ps` output, or a full container name (also copyable from
// `ship ps`). It returns the config service name and a replica number (0 if
// the argument didn't specify one).
func resolveServiceReplica(cfg *config.Config, envName, arg string) (string, int, error) {
	arg = strings.TrimSpace(arg)
	if _, ok := cfg.Services[arg]; ok {
		return arg, 0, nil
	}
	if svc, replica, ok := splitServiceReplica(arg); ok {
		if _, exists := cfg.Services[svc]; exists {
			return svc, replica, nil
		}
	}
	if svc, replica, ok := deployment.ParseContainerName(cfg.Project, envName, cfg.Services, arg); ok {
		return svc, replica, nil
	}
	return "", 0, fmt.Errorf("unknown service %q", arg)
}

// splitServiceReplica splits the "service.N" shorthand on its last dot.
func splitServiceReplica(arg string) (service string, replica int, ok bool) {
	idx := strings.LastIndex(arg, ".")
	if idx <= 0 || idx == len(arg)-1 {
		return "", 0, false
	}
	n, err := strconv.Atoi(arg[idx+1:])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return arg[:idx], n, true
}

// mergeResolvedReplica reconciles a replica number parsed from the SERVICE
// argument (e.g. "web.2") with an explicit --replica flag, erroring if the
// two disagree instead of silently preferring one.
func mergeResolvedReplica(cmd *cobra.Command, flagValue, parsedValue int, arg string) (int, error) {
	if parsedValue <= 0 {
		return flagValue, nil
	}
	if cmd.Flags().Changed("replica") && flagValue != parsedValue {
		return 0, fmt.Errorf("--replica %d conflicts with replica %d in %q", flagValue, parsedValue, arg)
	}
	return parsedValue, nil
}

func restartTargets(cfg *config.Config, hosts []scheduler.Host, serviceName string, replica int) ([]scheduler.Placement, error) {
	placements, err := scheduler.PlaceServicesOnHosts(cfg, hosts)
	if err != nil {
		return nil, err
	}
	var targets []scheduler.Placement
	for _, placement := range placements {
		if serviceName != "" && placement.Service != serviceName {
			continue
		}
		if replica > 0 && placement.Replica != replica {
			continue
		}
		targets = append(targets, placement)
	}
	if len(targets) == 0 {
		if serviceName != "" && replica > 0 {
			return nil, fmt.Errorf("service %q has no replica %d", serviceName, replica)
		}
		if serviceName != "" {
			return nil, fmt.Errorf("service %q has no placed replicas", serviceName)
		}
		return nil, errors.New("no placed service replicas")
	}
	return targets, nil
}

func deployedServiceSecretEnvFiles(cfg *config.Config, envName string) (map[string]string, error) {
	scopes, err := secrets.RequiredScopes(cfg)
	if err != nil {
		return nil, err
	}
	envFiles := map[string]string{}
	for scope := range scopes {
		service, ok := strings.CutPrefix(scope, "service-")
		if !ok {
			continue
		}
		envFiles[service] = secrets.RemoteEnvFilePath(envName, scope)
	}
	return envFiles, nil
}

func restartActions(cfg *config.Config, envName string, release state.Release, targets []scheduler.Placement, secretEnvFiles map[string]string) ([]deployment.Action, error) {
	var actions []deployment.Action
	networkName := deployment.DockerNetworkName(cfg, envName)
	networkDriver := deployment.DockerNetworkDriver(cfg)
	for _, target := range targets {
		svc := cfg.Services[target.Service]
		image := strings.TrimSpace(release.Images[target.Service])
		if image == "" {
			return nil, fmt.Errorf("current release %s has no image for service %q", release.ID, target.Service)
		}
		name := deployment.ContainerName(cfg.Project, envName, target.Service, target.Replica, release.ID)
		actions = append(actions, deployment.Action{
			Kind:           deployment.ActionStart,
			Host:           target.Host,
			Service:        target.Service,
			Replica:        target.Replica,
			Release:        release.ID,
			ContainerName:  name,
			Image:          image,
			Command:        svc.Command,
			Args:           deployment.DockerArgs(svc, secretEnvFiles[target.Service]),
			Labels:         deployment.ContainerLabels(cfg.Project, envName, target.Service, target.Replica, release.ID, svc.Labels),
			Network:        networkName,
			NetworkDriver:  networkDriver,
			NetworkAliases: deployment.ServiceNetworkAliases(target.Service, svc),
		})
		health, ok, err := deployment.HealthCheck(svc, name)
		if err != nil {
			return nil, fmt.Errorf("service %q health check: %w", target.Service, err)
		}
		if ok {
			actions = append(actions, deployment.Action{
				Kind:           deployment.ActionHealth,
				Host:           target.Host,
				Service:        target.Service,
				Replica:        target.Replica,
				Release:        release.ID,
				ContainerName:  name,
				Health:         health,
				HealthRetries:  svc.Rolling.HealthRetries,
				HealthInterval: deployment.HealthRetryInterval(svc),
			})
		}
	}
	return actions, nil
}

func restartCurrentServicesAfterAccessoryChange(ctx context.Context, w io.Writer, cfg *config.Config, envName string, store state.Store, hosts []scheduler.Host) error {
	release, err := currentReleaseForServiceMutation(ctx, cfg, envName, store, hosts)
	if err != nil {
		if strings.Contains(err.Error(), "no current release") {
			return nil
		}
		return err
	}
	targets, err := restartTargets(cfg, hosts, "", 0)
	if err != nil {
		return nil
	}
	secretEnvFiles, err := deployedServiceSecretEnvFiles(cfg, envName)
	if err != nil {
		return err
	}
	actions, err := restartActions(cfg, envName, release, targets, secretEnvFiles)
	if err != nil {
		return err
	}
	recordEvent(store, state.Event{Environment: envName, Kind: "deploy_accessory_restart", Status: "started", Release: release.ID, Message: fmt.Sprintf("containers=%d", len(targets))})
	if err := deployment.ExecuteActions(ctx, actions, deploymentAgentFactory(), nil); err != nil {
		return err
	}
	for _, target := range targets {
		name := deployment.ContainerName(cfg.Project, envName, target.Service, target.Replica, release.ID)
		fmt.Fprintf(w, "restarted %s on %s after accessory change\n", name, target.Host.Name)
	}
	recordEvent(store, state.Event{Environment: envName, Kind: "deploy_accessory_restart", Status: "succeeded", Release: release.ID, Message: fmt.Sprintf("containers=%d", len(targets))})
	return nil
}

func deploymentAgentFactory() deployment.AgentFactory {
	return func(host scheduler.Host) deployment.Agent {
		return newDeployAgent(host)
	}
}

type remoteSecretFile struct {
	Host    scheduler.Host
	Path    string
	Content string
}

func serviceSecretEnvFiles(cfg *config.Config, hosts []scheduler.Host, envName string, rendered secrets.ScopedRenderedEnvFiles) (map[string]string, []remoteSecretFile, error) {
	if len(rendered.Scopes) == 0 {
		return nil, nil, nil
	}
	placements, err := scheduler.PlaceServicesOnHosts(cfg, hosts)
	if err != nil {
		return nil, nil, err
	}
	envFiles := map[string]string{}
	writesByKey := map[string]remoteSecretFile{}
	for _, placement := range placements {
		scope := "service-" + placement.Service
		file, ok := rendered.Scopes[scope]
		if !ok {
			continue
		}
		path := secrets.RemoteEnvFilePath(envName, scope)
		envFiles[placement.Service] = path
		key := placement.Host.Name + "\x00" + path
		writesByKey[key] = remoteSecretFile{Host: placement.Host, Path: path, Content: file.Content}
	}
	writes := make([]remoteSecretFile, 0, len(writesByKey))
	for _, write := range writesByKey {
		writes = append(writes, write)
	}
	sort.Slice(writes, func(i, j int) bool {
		if writes[i].Host.Name != writes[j].Host.Name {
			return writes[i].Host.Name < writes[j].Host.Name
		}
		return writes[i].Path < writes[j].Path
	})
	return envFiles, writes, nil
}

func accessorySecretEnvFile(envName, name string, rendered secrets.ScopedRenderedEnvFiles) (string, string) {
	scope := "accessory-" + name
	file, ok := rendered.Scopes[scope]
	if !ok {
		return "", ""
	}
	return secrets.RemoteEnvFilePath(envName, scope), file.Content
}

func writeRemoteSecretFiles(ctx context.Context, writes []remoteSecretFile) error {
	for _, write := range writes {
		if err := writeRemoteSecretFile(ctx, write.Host, write.Path, write.Content); err != nil {
			return err
		}
	}
	return nil
}

func writeRemoteSecretFile(ctx context.Context, host scheduler.Host, path string, content string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	params := agent.WriteFileParams{
		Path:    path,
		Content: content,
		Mode:    0o600,
	}
	if err := newDeployAgent(host).Call(ctx, "write_file", params, nil); err != nil {
		return fmt.Errorf("write secrets on %s: %w", host.Name, err)
	}
	return nil
}
