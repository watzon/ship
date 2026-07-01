package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/watzon/ship/internal/accessory"
	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/deployment"
	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/doctor"
	"github.com/watzon/ship/internal/ingress"
	"github.com/watzon/ship/internal/planner"
	"github.com/watzon/ship/internal/provider"
	"github.com/watzon/ship/internal/providers"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/secrets"
	"github.com/watzon/ship/internal/state"
	"github.com/watzon/ship/internal/transport"
)

type options struct {
	configPath string
	dryRun     bool
}

var newEnvironmentProvider = providers.ForEnvironment

type deployDocker interface {
	BuildImage(ctx context.Context, opts docker.BuildOptions) error
	Push(ctx context.Context, image string) error
	ResolveDigest(ctx context.Context, image string) (string, error)
}

type deployAgent interface {
	Call(ctx context.Context, method string, params any, out any) error
}

var newDeployDocker = func() deployDocker {
	return docker.Client{}
}

var newDeployAgent = func(host scheduler.Host) deployAgent {
	return agent.Client{SSH: transport.SSH{User: host.User, Host: host.ContactTarget()}}
}

var deployNow = time.Now
var deployGitRevision = docker.GitShortSHA
var readCurrentShipBinary = func() ([]byte, error) {
	path, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

type bootstrapSSH interface {
	Run(ctx context.Context, command string) (string, error)
	RunWithStdin(ctx context.Context, command, stdin string) (string, error)
}

var newBootstrapSSH = func(host scheduler.Host, dryRun bool) bootstrapSSH {
	return transport.SSH{User: host.User, Host: host.ContactTarget(), DryRun: dryRun}
}
var bootstrapMaxAttempts = 30
var bootstrapRetryDelay = 2 * time.Second

func Execute() error {
	opts := &options{}
	root := &cobra.Command{
		Use:          "ship",
		Short:        "Deploy Docker apps to ordinary servers with horizontal scaling",
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVarP(&opts.configPath, "config", "c", config.DefaultConfigFile, "path to ship.yml")
	root.PersistentFlags().BoolVar(&opts.dryRun, "dry-run", false, "print the intended operation without mutating remote state")

	root.AddCommand(initCmd(opts))
	root.AddCommand(doctorCmd(opts))
	root.AddCommand(provisionCmd(opts))
	root.AddCommand(agentCmd(opts))
	root.AddCommand(planCmd(opts))
	root.AddCommand(deployCmd(opts))
	root.AddCommand(scaleCmd(opts))
	root.AddCommand(statusCmd(opts))
	root.AddCommand(logsCmd(opts))
	root.AddCommand(inspectCmd(opts))
	root.AddCommand(eventsCmd(opts))
	root.AddCommand(rollbackCmd(opts))
	root.AddCommand(recoverCmd(opts))
	root.AddCommand(accessoryCmd(opts))
	root.AddCommand(secretsCmd(opts))

	return root.Execute()
}

func initCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create ship.yml and local Ship state",
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := os.Stat(opts.configPath); err == nil {
				return fmt.Errorf("%s already exists", opts.configPath)
			}
			if err := os.WriteFile(opts.configPath, []byte(config.Sample()), 0o644); err != nil {
				return err
			}
			if err := os.MkdirAll(config.LocalStateDir, 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(config.LocalStateDir, "secrets.example"), []byte("DATABASE_URL=\n"), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created %s and %s/\n", opts.configPath, config.LocalStateDir)
			return nil
		},
	}
}

func doctorCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Validate local tools, config, and credentials",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			var report doctor.Report
			if err != nil {
				report = doctor.ConfigLoadError(err)
			} else {
				report = doctor.Run(cmd.Context(), cfg, doctor.Options{ConfigPath: opts.configPath})
			}
			if jsonOutput {
				if err := report.WriteJSON(cmd.OutOrStdout()); err != nil {
					return err
				}
			} else {
				report.WriteText(cmd.OutOrStdout())
			}
			if report.Failed() {
				return fmt.Errorf("doctor found issues")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print doctor results as JSON")
	return cmd
}

func provisionCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{Use: "provision", Short: "Plan or apply server provisioning"}
	cmd.AddCommand(&cobra.Command{
		Use:   "plan ENV",
		Short: "Print the provisioning plan",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			plan, err := planner.ProvisionPlan(cfg, args[0])
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), plan.String())
			return nil
		},
	})
	var yes bool
	apply := &cobra.Command{
		Use:   "apply ENV",
		Short: "Create servers and bootstrap Ship",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			env, err := cfg.Environment(args[0])
			if err != nil {
				return err
			}
			if !yes && !opts.dryRun {
				return fmt.Errorf("provision apply requires --yes (or --dry-run) before creating servers")
			}
			prov, err := newEnvironmentProvider(env, opts.dryRun)
			if err != nil {
				return err
			}
			if opts.dryRun {
				plans, err := prov.PlanHosts(cfg.Project, args[0], env)
				if err != nil {
					return err
				}
				for _, host := range plans {
					fmt.Fprintf(cmd.OutOrStdout(), "would provision %s pool=%s\n", host.Name, host.Pool)
				}
				return nil
			}
			stateDir, err := localStateDirForConfig(opts.configPath)
			if err != nil {
				return err
			}
			shipBinary, err := readCurrentShipBinary()
			if err != nil {
				return fmt.Errorf("read ship binary for bootstrap: %w", err)
			}
			store := state.NewStore(stateDir)
			recordEvent(store, state.Event{Environment: args[0], Kind: "provision", Status: "started"})
			result, err := prov.Reconcile(ctx, cfg.Project, args[0], env)
			if err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "provision", Status: "failed", Message: err.Error()})
				return err
			}
			for _, host := range result.Existing {
				printProviderHost(cmd.OutOrStdout(), "exists", host)
			}
			for _, host := range result.Created {
				printProviderHost(cmd.OutOrStdout(), "created", host)
			}
			for _, host := range result.Extra {
				printProviderHostDetails(cmd.OutOrStdout(), "extra", host)
				fmt.Fprintln(cmd.OutOrStdout(), " (not deleted)")
			}
			facts := hostFactsFromReconcile(prov.Name(), result)
			if err := store.SaveHostFacts(args[0], facts); err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "provision", Status: "failed", Message: err.Error()})
				return err
			}
			hosts, err := applyHostFacts(args[0], scheduler.HostsForEnvironment(env), facts)
			if err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "provision", Status: "failed", Message: err.Error()})
				return err
			}
			for _, host := range hosts {
				if err := bootstrapHost(ctx, host, shipBinary, opts.dryRun); err != nil {
					recordEvent(store, state.Event{Environment: args[0], Kind: "provision", Status: "failed", Host: host.Name, Message: err.Error()})
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "bootstrapped %s\n", host.Name)
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "provision", Status: "succeeded", Message: fmt.Sprintf("created=%d existing=%d extra=%d", len(result.Created), len(result.Existing), len(result.Extra))})
			return nil
		},
	}
	apply.Flags().BoolVar(&yes, "yes", false, "confirm provisioning changes")
	cmd.AddCommand(apply)
	var decommissionYes bool
	decommission := &cobra.Command{
		Use:   "decommission ENV",
		Short: "Delete Ship-managed servers for an environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			env, err := cfg.Environment(args[0])
			if err != nil {
				return err
			}
			if !decommissionYes && !opts.dryRun {
				return fmt.Errorf("provision decommission requires --yes (or --dry-run) before deleting servers")
			}
			prov, err := newEnvironmentProvider(env, opts.dryRun)
			if err != nil {
				return err
			}
			stateDir, err := localStateDirForConfig(opts.configPath)
			if err != nil {
				return err
			}
			store := state.NewStore(stateDir)
			hosts, err := prov.List(ctx, cfg.Project, args[0])
			if err != nil {
				return err
			}
			if opts.dryRun {
				if len(hosts) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "would decommission no servers")
					return nil
				}
				for _, host := range hosts {
					printProviderHost(cmd.OutOrStdout(), "would decommission", host)
				}
				return nil
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "decommission", Status: "started"})
			for _, host := range hosts {
				if err := prov.Delete(ctx, host); err != nil {
					recordEvent(store, state.Event{Environment: args[0], Kind: "decommission", Status: "failed", Host: host.Name, Message: err.Error()})
					return err
				}
				printProviderHost(cmd.OutOrStdout(), "decommissioned", host)
			}
			if err := store.DeleteHostFacts(args[0]); err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "decommission", Status: "failed", Message: err.Error()})
				return err
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "decommission", Status: "succeeded", Message: fmt.Sprintf("deleted=%d", len(hosts))})
			return nil
		},
	}
	decommission.Flags().BoolVar(&decommissionYes, "yes", false, "confirm deletion of Ship-managed servers")
	cmd.AddCommand(decommission)
	return cmd
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

func printProviderHost(w io.Writer, action string, host provider.Host) {
	printProviderHostDetails(w, action, host)
	fmt.Fprintln(w)
}

func printProviderHostDetails(w io.Writer, action string, host provider.Host) {
	pool := host.Pool
	if pool == "" {
		pool = host.Labels[provider.LabelPool]
	}
	fmt.Fprintf(w, "%s %s", action, host.Name)
	if strings.Contains(action, "decommission") {
		printProviderID(w, host.ID)
		if pool != "" {
			fmt.Fprintf(w, " pool=%s", pool)
		}
	} else {
		if pool != "" {
			fmt.Fprintf(w, " pool=%s", pool)
		}
		printProviderID(w, host.ID)
	}
	if host.PublicAddress != "" {
		if ip := net.ParseIP(host.PublicAddress); ip != nil && ip.To4() != nil {
			fmt.Fprintf(w, " ipv4=%s", host.PublicAddress)
		} else {
			fmt.Fprintf(w, " public_address=%s", host.PublicAddress)
		}
	}
}

func printProviderID(w io.Writer, id string) {
	if id == "" {
		return
	}
	if _, err := strconv.ParseInt(id, 10, 64); err == nil {
		fmt.Fprintf(w, " server_id=%s", id)
		return
	}
	fmt.Fprintf(w, " provider_id=%s", id)
}

func hostFactsFromReconcile(providerName string, result provider.ReconcileResult) []state.HostFact {
	hostsByName := map[string]provider.Host{}
	for _, host := range result.Existing {
		hostsByName[host.Name] = host
	}
	for _, host := range result.Created {
		hostsByName[host.Name] = host
	}

	facts := make([]state.HostFact, 0, len(result.Desired))
	for _, plan := range result.Desired {
		fact := state.HostFact{
			Name:     plan.Name,
			Pool:     plan.Pool,
			User:     plan.User,
			Provider: providerName,
		}
		if host, ok := hostsByName[plan.Name]; ok {
			fact.ProviderID = host.ID
			if id, err := strconv.ParseInt(host.ID, 10, 64); err == nil {
				fact.ServerID = id
			}
			fact.PublicAddress = host.PublicAddress
			if ip := net.ParseIP(host.PublicAddress); ip != nil && ip.To4() != nil {
				fact.IPv4 = host.PublicAddress
			}
		}
		facts = append(facts, fact)
	}
	return facts
}

type statusView struct {
	Environment    string                            `json:"environment"`
	CurrentRelease *state.Release                    `json:"current_release,omitempty"`
	Desired        []deployment.DesiredReplicaStatus `json:"desired"`
	Observed       []deployment.ContainerStatus      `json:"observed"`
	ExtraObserved  []deployment.ContainerStatus      `json:"extra_observed,omitempty"`
	Summary        deployment.StatusSummary          `json:"summary"`
}

type inspectView struct {
	Environment    string                            `json:"environment"`
	CurrentRelease *state.Release                    `json:"current_release,omitempty"`
	Desired        []deployment.DesiredReplicaStatus `json:"desired"`
	Observed       []deployment.ContainerStatus      `json:"observed"`
	ExtraObserved  []deployment.ContainerStatus      `json:"extra_observed,omitempty"`
	Accessories    []state.AccessoryState            `json:"accessories,omitempty"`
	Events         []state.Event                     `json:"events,omitempty"`
	Summary        deployment.StatusSummary          `json:"summary"`
}

type logsView struct {
	Environment string      `json:"environment"`
	Service     string      `json:"service"`
	Lines       int         `json:"lines"`
	Replica     int         `json:"replica,omitempty"`
	Follow      bool        `json:"follow"`
	Entries     []logsEntry `json:"entries"`
}

type logsEntry struct {
	Iteration int    `json:"iteration"`
	Host      string `json:"host"`
	Service   string `json:"service"`
	Replica   int    `json:"replica"`
	Container string `json:"container"`
	Logs      string `json:"logs"`
}

const logsFollowPolls = 3

var logsFollowInterval = 100 * time.Millisecond

func environmentContext(opts *options, envName string) (*config.Config, config.Environment, state.Store, error) {
	cfg, err := config.Load(opts.configPath)
	if err != nil {
		return nil, config.Environment{}, state.Store{}, err
	}
	env, err := cfg.Environment(envName)
	if err != nil {
		return nil, config.Environment{}, state.Store{}, err
	}
	stateDir, err := localStateDirForConfig(opts.configPath)
	if err != nil {
		return nil, config.Environment{}, state.Store{}, err
	}
	return cfg, env, state.NewStore(stateDir), nil
}

type hostFactKey struct {
	name string
	pool string
}

func resolvedHostsForEnvironment(store state.Store, envName string, env config.Environment) ([]scheduler.Host, error) {
	hosts := scheduler.HostsForEnvironment(env)
	facts, err := store.ReadHostFacts(envName)
	if errors.Is(err, os.ErrNotExist) {
		return hosts, nil
	}
	if err != nil {
		return nil, err
	}
	if len(facts) == 0 {
		return hosts, nil
	}
	return applyHostFacts(envName, hosts, facts)
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
	_ = store.RecordEvent(event)
}

func buildStatusView(ctx context.Context, cfg *config.Config, env config.Environment, envName string, store state.Store) (statusView, deployment.StatusReport, error) {
	releases, err := store.Releases(envName)
	if err != nil {
		return statusView{}, deployment.StatusReport{}, err
	}
	var currentRelease *state.Release
	desiredReleaseID := ""
	if current, err := store.CurrentRelease(envName); err == nil {
		currentRelease = &current
		desiredReleaseID = current.ID
	} else if len(releases) > 0 {
		latest := releases[len(releases)-1]
		currentRelease = &latest
	}

	hosts, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return statusView{}, deployment.StatusReport{}, err
	}
	observed, err := deployment.InspectObservedOnHosts(ctx, hosts, deploymentAgentFactory())
	if err != nil {
		return statusView{}, deployment.StatusReport{}, err
	}
	report, err := deployment.AggregateStatus(deployment.StatusInput{
		Config:         cfg,
		Environment:    env,
		Hosts:          hosts,
		EnvName:        envName,
		CurrentRelease: desiredReleaseID,
		Observed:       observed,
	})
	if err != nil {
		return statusView{}, deployment.StatusReport{}, err
	}
	view := statusView{
		Environment:    envName,
		CurrentRelease: currentRelease,
		Desired:        report.Desired,
		Observed:       report.Observed,
		ExtraObserved:  report.ExtraObserved,
		Summary:        report.Summary,
	}
	return view, report, nil
}

func renderStatusText(w io.Writer, view statusView) {
	fmt.Fprintf(w, "environment %s\n", view.Environment)
	if view.CurrentRelease == nil {
		fmt.Fprintln(w, "current release none")
	} else {
		fmt.Fprintf(w, "current release %s status=%s healthy=%t\n", view.CurrentRelease.ID, view.CurrentRelease.Status, view.CurrentRelease.Healthy)
	}
	for _, desired := range view.Desired {
		fmt.Fprintf(w, "%s.%d desired host=%s", desired.Service, desired.Replica, desired.Host)
		if desired.DesiredRelease != "" {
			fmt.Fprintf(w, " release=%s", desired.DesiredRelease)
		}
		fmt.Fprintf(w, " state=%s", desired.State)
		if len(desired.Observed) == 0 {
			fmt.Fprint(w, " observed=missing")
		}
		if len(desired.Drift) > 0 {
			fmt.Fprintf(w, " drift=%s", strings.Join(desired.Drift, "; "))
		}
		fmt.Fprintln(w)
		for _, observed := range desired.Observed {
			fmt.Fprintf(w, "  observed host=%s name=%s", observed.Host, observed.Name)
			if observed.Release != "" {
				fmt.Fprintf(w, " release=%s", observed.Release)
			}
			if observed.Status != "" {
				fmt.Fprintf(w, " status=%q", observed.Status)
			}
			fmt.Fprintln(w)
		}
	}
	if len(view.ExtraObserved) > 0 {
		fmt.Fprintln(w, "extra managed containers:")
		for _, observed := range view.ExtraObserved {
			fmt.Fprintf(w, "- host=%s name=%s kind=%s", observed.Host, observed.Name, observed.Kind)
			if observed.Service != "" {
				fmt.Fprintf(w, " service=%s.%d", observed.Service, observed.Replica)
			}
			if observed.Accessory != "" {
				fmt.Fprintf(w, " accessory=%s", observed.Accessory)
			}
			if observed.Release != "" {
				fmt.Fprintf(w, " release=%s", observed.Release)
			}
			if observed.Status != "" {
				fmt.Fprintf(w, " status=%q", observed.Status)
			}
			fmt.Fprintln(w)
		}
	}
	if view.Summary.Drift {
		fmt.Fprintf(w, "drift detected missing=%d wrong_release=%d wrong_host=%d extra=%d\n", view.Summary.Missing, view.Summary.WrongRelease, view.Summary.WrongHost, view.Summary.Extra)
		return
	}
	fmt.Fprintln(w, "status ok")
}

func renderEventsText(w io.Writer, events []state.Event) {
	if len(events) == 0 {
		fmt.Fprintln(w, "no events")
		return
	}
	for _, event := range events {
		fmt.Fprintf(w, "%s %s %s", event.Time.Format(time.RFC3339), event.Kind, event.Status)
		if event.Release != "" {
			fmt.Fprintf(w, " release=%s", event.Release)
		}
		if event.Service != "" {
			fmt.Fprintf(w, " service=%s", event.Service)
		}
		if event.Accessory != "" {
			fmt.Fprintf(w, " accessory=%s", event.Accessory)
		}
		if event.Host != "" {
			fmt.Fprintf(w, " host=%s", event.Host)
		}
		if event.Message != "" {
			fmt.Fprintf(w, " %s", event.Message)
		}
		fmt.Fprintln(w)
	}
}

func agentCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Manage or run the Ship node agent"}
	cmd.AddCommand(&cobra.Command{
		Use:    "rpc",
		Short:  "Serve one or more JSON-RPC requests over stdin/stdout",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return agent.ServeStdio(cmd.Context())
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "run",
		Short: "Run the agent service loop",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "ship agent is installed; RPC is served through `ship agent rpc`")
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "install ENV",
		Short: "Print or run host bootstrap commands for every host in an environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, env, store, err := environmentContext(opts, args[0])
			if err != nil {
				return err
			}
			hosts, err := resolvedHostsForEnvironment(store, args[0], env)
			if err != nil {
				return err
			}
			for _, host := range hosts {
				unit := systemdUnit()
				command := fmt.Sprintf("mkdir -p %s && cat >/etc/systemd/system/ship-agent.service <<'EOF'\n%s\nEOF\nsystemctl daemon-reload && systemctl enable --now ship-agent", config.RemoteStateDir, unit)
				out, err := (transport.SSH{User: host.User, Host: host.ContactTarget(), DryRun: opts.dryRun}).Run(cmd.Context(), command)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", host.Name, strings.TrimSpace(out))
			}
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "status ENV",
		Short: "Ask every host agent for status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, env, store, err := environmentContext(opts, args[0])
			if err != nil {
				return err
			}
			hosts, err := resolvedHostsForEnvironment(store, args[0], env)
			if err != nil {
				return err
			}
			for _, host := range hosts {
				var status agent.Status
				client := agent.Client{SSH: transport.SSH{User: host.User, Host: host.ContactTarget(), DryRun: opts.dryRun}}
				if err := client.Call(cmd.Context(), "status", map[string]any{}, &status); err != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "%s fail %v\n", host.Name, err)
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s docker=%t state=%s\n", host.Name, status.DockerOK, status.StateDir)
			}
			return nil
		},
	})
	return cmd
}

func planCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "plan ENV",
		Short: "Print the deployment plan",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			plan, err := planner.DeploymentPlan(cfg, args[0])
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), plan.String())
			return nil
		},
	}
}

func deployCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "deploy ENV",
		Short: "Build, push, place, and roll services",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			env, err := cfg.Environment(args[0])
			if err != nil {
				return err
			}
			plan, err := planner.DeploymentPlan(cfg, args[0])
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), plan.String())
			if opts.dryRun {
				if _, err := secrets.Verify(cfg); err != nil {
					return err
				}
				stateDir, err := localStateDirForConfig(opts.configPath)
				if err != nil {
					return err
				}
				return printIngressDryRun(cmd.OutOrStdout(), cfg, env, args[0], stateDir)
			}
			stateDir, err := localStateDirForConfig(opts.configPath)
			if err != nil {
				return err
			}
			store := state.NewStore(stateDir)
			hosts, err := resolvedHostsForEnvironment(store, args[0], env)
			if err != nil {
				return err
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "deploy", Status: "started"})
			secretFile, err := secrets.RenderEnvFile(cfg)
			if err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy", Status: "failed", Message: err.Error()})
				return err
			}
			createdAt := deployNow()
			releaseID := docker.ReleaseTag(ctx, createdAt, deployGitRevision)
			images, err := prepareDeployImages(ctx, deployDockerWithLogs(newDeployDocker(), cmd.OutOrStdout()), cfg, releaseID)
			if err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy", Status: "failed", Release: releaseID, Message: err.Error()})
				return err
			}
			release := state.Release{
				ID:            releaseID,
				Environment:   args[0],
				Images:        images,
				SecretDigests: secretFile.Digests,
				CreatedAt:     createdAt,
				Status:        state.ReleaseStatusPending,
			}
			if err := store.SaveReleaseRecord(release); err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy", Status: "failed", Release: releaseID, Message: err.Error()})
				return err
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "deploy", Status: "release_created", Release: releaseID})
			if err := syncRemoteReleaseStateWithEvents(ctx, store, args[0], hosts, "deploy_release_state_write", release); err != nil {
				if _, markErr := store.MarkReleaseFailed(releaseID, err.Error(), deployNow()); markErr != nil {
					recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_mark_failed", Status: "failed", Release: releaseID, Message: markErr.Error()})
					return fmt.Errorf("%w; additionally failed to mark release failed: %v", err, markErr)
				}
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_mark_failed", Status: "succeeded", Release: releaseID})
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy", Status: "failed", Release: releaseID, Message: err.Error()})
				return err
			}
			secretEnvFiles, secretWrites, err := serviceSecretEnvFiles(cfg, hosts, args[0], secretFile)
			if err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy", Status: "failed", Release: releaseID, Message: err.Error()})
				return err
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_secret_write", Status: "started", Release: releaseID, Message: fmt.Sprintf("writes=%d", len(secretWrites))})
			if err := writeRemoteSecretFiles(ctx, secretWrites, secretFile.Content); err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_secret_write", Status: "failed", Release: releaseID, Message: err.Error()})
				failedRelease, markErr := store.MarkReleaseFailed(releaseID, err.Error(), deployNow())
				if markErr != nil {
					recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_mark_failed", Status: "failed", Release: releaseID, Message: markErr.Error()})
					return fmt.Errorf("%w; additionally failed to mark release failed: %v", err, markErr)
				}
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_mark_failed", Status: "succeeded", Release: releaseID})
				if syncErr := syncRemoteReleaseStateWithEvents(ctx, store, args[0], hosts, "deploy_release_state_write", failedRelease); syncErr != nil {
					recordEvent(store, state.Event{Environment: args[0], Kind: "deploy", Status: "failed", Release: releaseID, Message: fmt.Sprintf("%v; additionally failed to write failed release state: %v", err, syncErr)})
					return fmt.Errorf("%w; additionally failed to write failed release state: %v", err, syncErr)
				}
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy", Status: "failed", Release: releaseID, Message: err.Error()})
				return err
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_secret_write", Status: "succeeded", Release: releaseID, Message: fmt.Sprintf("writes=%d", len(secretWrites))})
			recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_rollout", Status: "started", Release: releaseID})
			actions, err := deployment.Rollout(ctx, deployment.RolloutOptions{
				Config:         cfg,
				Environment:    env,
				Hosts:          hosts,
				EnvName:        args[0],
				ReleaseID:      releaseID,
				Images:         images,
				StateDir:       stateDir,
				SecretEnvFiles: secretEnvFiles,
				AgentFor:       deploymentAgentFactory(),
			})
			if err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_rollout", Status: "failed", Release: releaseID, Message: err.Error()})
				failedRelease, markErr := store.MarkReleaseFailed(releaseID, err.Error(), deployNow())
				if markErr != nil {
					recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_mark_failed", Status: "failed", Release: releaseID, Message: markErr.Error()})
					return fmt.Errorf("%w; additionally failed to mark release failed: %v", err, markErr)
				}
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_mark_failed", Status: "succeeded", Release: releaseID})
				if syncErr := syncRemoteReleaseStateWithEvents(ctx, store, args[0], hosts, "deploy_release_state_write", failedRelease); syncErr != nil {
					recordEvent(store, state.Event{Environment: args[0], Kind: "deploy", Status: "failed", Release: releaseID, Message: fmt.Sprintf("%v; additionally failed to write failed release state: %v", err, syncErr)})
					return fmt.Errorf("%w; additionally failed to write failed release state: %v", err, syncErr)
				}
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy", Status: "failed", Release: releaseID, Message: err.Error()})
				return err
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_rollout", Status: "succeeded", Release: releaseID})
			recordIngressEvents(store, args[0], releaseID, actions)
			completedAt := deployNow()
			healthyRelease := releaseAsHealthy(release, completedAt)
			if err := syncRemoteReleaseStateWithEvents(ctx, store, args[0], hosts, "deploy_release_state_write", healthyRelease); err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy", Status: "failed", Release: releaseID, Message: err.Error()})
				return err
			}
			if _, err := store.MarkReleaseHealthy(releaseID, completedAt); err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy", Status: "failed", Release: releaseID, Message: err.Error()})
				return err
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "deploy", Status: "succeeded", Release: releaseID})
			return nil
		},
	}
}

func prepareDeployImages(ctx context.Context, dc deployDocker, cfg *config.Config, releaseID string) (map[string]string, error) {
	images := map[string]string{}
	for _, name := range sortedServiceNames(cfg.Services) {
		svc := cfg.Services[name]
		imageRef := strings.TrimSpace(svc.Image.Ref)
		if imageRef == "" {
			tag, err := docker.ImageTag(cfg.Registry, name, releaseID)
			if err != nil {
				return nil, err
			}
			if err := dc.BuildImage(ctx, docker.BuildOptions{
				ContextDir: svc.Image.Build,
				Dockerfile: svc.Image.Dockerfile,
				Tag:        tag,
				BuildArgs:  svc.Image.BuildArgs,
				Target:     svc.Image.Target,
				Platform:   svc.Image.Platform,
			}); err != nil {
				return nil, err
			}
			if err := dc.Push(ctx, tag); err != nil {
				return nil, err
			}
			imageRef = tag
		}
		digestRef, err := dc.ResolveDigest(ctx, imageRef)
		if err != nil {
			return nil, err
		}
		images[name] = digestRef
	}
	return images, nil
}

func deployDockerWithLogs(dc deployDocker, w io.Writer) deployDocker {
	switch client := dc.(type) {
	case docker.Client:
		client.LogWriter = w
		return client
	case *docker.Client:
		copy := *client
		copy.LogWriter = w
		return copy
	default:
		return dc
	}
}

func recordIngressEvents(store state.Store, envName, releaseID string, actions []deployment.Action) {
	for _, action := range actions {
		if action.Kind != deployment.ActionIngress {
			continue
		}
		for _, host := range action.IngressHosts {
			recordEvent(store, state.Event{Environment: envName, Kind: "ingress_reload", Status: "succeeded", Release: releaseID, Host: host.Name})
		}
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

func releaseAsHealthy(release state.Release, at time.Time) state.Release {
	release.Status = state.ReleaseStatusHealthy
	release.Healthy = true
	release.Error = ""
	release.FailedAt = nil
	completedAt := at.UTC()
	release.CompletedAt = &completedAt
	return release
}

func sortedServiceNames(services map[string]config.Service) []string {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func scaleCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "scale ENV SERVICE=N [SERVICE=N...]",
		Short: "Preview deterministic manual scaling placement",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			envName := args[0]
			for _, pair := range args[1:] {
				parts := strings.SplitN(pair, "=", 2)
				if len(parts) != 2 {
					return fmt.Errorf("scale must be SERVICE=N, got %q", pair)
				}
				n, err := strconv.Atoi(parts[1])
				if err != nil || n < 0 {
					return fmt.Errorf("invalid scale %q", pair)
				}
				svc, ok := cfg.Services[parts[0]]
				if !ok {
					return fmt.Errorf("unknown service %q", parts[0])
				}
				svc.Scale = n
				cfg.Services[parts[0]] = svc
			}
			plan, err := planner.DeploymentPlan(cfg, envName)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), plan.String())
			stateDir, err := localStateDirForConfig(opts.configPath)
			if err == nil {
				recordEvent(state.NewStore(stateDir), state.Event{Environment: envName, Kind: "scale", Status: "planned", Message: strings.Join(args[1:], " ")})
			}
			return nil
		},
	}
}

func statusCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "status ENV",
		Short: "Show desired placements and release state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, env, store, err := environmentContext(opts, args[0])
			if err != nil {
				return err
			}
			view, _, err := buildStatusView(cmd.Context(), cfg, env, args[0], store)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), view)
			}
			renderStatusText(cmd.OutOrStdout(), view)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print status as JSON")
	return cmd
}

func logsCmd(opts *options) *cobra.Command {
	var lines int
	var replica int
	var follow bool
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "logs ENV SERVICE",
		Short: "Fetch service logs from placed hosts",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			env, err := cfg.Environment(args[0])
			if err != nil {
				return err
			}
			if _, ok := cfg.Services[args[1]]; !ok {
				return fmt.Errorf("unknown service %q", args[1])
			}
			if lines <= 0 {
				return fmt.Errorf("--lines must be greater than zero")
			}
			if replica < 0 {
				return fmt.Errorf("--replica cannot be negative")
			}
			stateDir, err := localStateDirForConfig(opts.configPath)
			if err != nil {
				return err
			}
			store := state.NewStore(stateDir)
			hosts, err := resolvedHostsForEnvironment(store, args[0], env)
			if err != nil {
				return err
			}
			placements, err := scheduler.PlaceServicesOnHosts(cfg, hosts)
			if err != nil {
				return err
			}
			var releaseID string
			if release, err := store.CurrentRelease(args[0]); err == nil {
				releaseID = release.ID
			}
			var targets []scheduler.Placement
			for _, placement := range placements {
				if placement.Service != args[1] {
					continue
				}
				if replica > 0 && placement.Replica != replica {
					continue
				}
				targets = append(targets, placement)
			}
			if len(targets) == 0 {
				if replica > 0 {
					return fmt.Errorf("service %q has no replica %d", args[1], replica)
				}
				return fmt.Errorf("service %q has no placed replicas", args[1])
			}
			polls := 1
			if follow {
				polls = logsFollowPolls
			}
			view := logsView{
				Environment: args[0],
				Service:     args[1],
				Lines:       lines,
				Replica:     replica,
				Follow:      follow,
			}
			for iteration := 1; iteration <= polls; iteration++ {
				if iteration > 1 {
					timer := time.NewTimer(logsFollowInterval)
					select {
					case <-cmd.Context().Done():
						timer.Stop()
						if jsonOutput {
							return writeJSON(cmd.OutOrStdout(), view)
						}
						return cmd.Context().Err()
					case <-timer.C:
					}
				}
				for _, placement := range targets {
					var out map[string]string
					name := fmt.Sprintf("ship_%s_%d", placement.Service, placement.Replica)
					if releaseID != "" {
						name = deployment.ContainerName(cfg.Project, args[0], placement.Service, placement.Replica, releaseID)
					}
					if err := newDeployAgent(placement.Host).Call(cmd.Context(), "logs", agent.LogsParams{Name: name, Lines: lines}, &out); err != nil {
						return err
					}
					entry := logsEntry{
						Iteration: iteration,
						Host:      placement.Host.Name,
						Service:   placement.Service,
						Replica:   placement.Replica,
						Container: name,
						Logs:      out["logs"],
					}
					view.Entries = append(view.Entries, entry)
					if !jsonOutput {
						fmt.Fprintf(cmd.OutOrStdout(), "==> %s/%s <==\n%s\n", placement.Host.Name, name, entry.Logs)
					}
				}
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), view)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&lines, "lines", 100, "number of log lines to fetch per replica")
	cmd.Flags().IntVar(&replica, "replica", 0, "fetch logs for one replica number")
	cmd.Flags().BoolVar(&follow, "follow", false, "poll logs repeatedly in a short V1 follow loop")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print logs as JSON")
	return cmd
}

func inspectCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "inspect ENV",
		Short: "Show structured environment release, placement, observed state, and events",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, env, store, err := environmentContext(opts, args[0])
			if err != nil {
				return err
			}
			status, _, err := buildStatusView(cmd.Context(), cfg, env, args[0], store)
			if err != nil {
				return err
			}
			accessories, err := store.AccessoryStates(args[0])
			if err != nil {
				return err
			}
			events, err := store.Events(args[0])
			if err != nil {
				return err
			}
			view := inspectView{
				Environment:    args[0],
				CurrentRelease: status.CurrentRelease,
				Desired:        status.Desired,
				Observed:       status.Observed,
				ExtraObserved:  status.ExtraObserved,
				Accessories:    accessories,
				Events:         events,
				Summary:        status.Summary,
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), view)
			}
			renderStatusText(cmd.OutOrStdout(), status)
			if len(view.Observed) > 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "observed managed containers:")
				for _, observed := range view.Observed {
					fmt.Fprintf(cmd.OutOrStdout(), "- host=%s name=%s kind=%s", observed.Host, observed.Name, observed.Kind)
					if observed.Service != "" {
						fmt.Fprintf(cmd.OutOrStdout(), " service=%s.%d", observed.Service, observed.Replica)
					}
					if observed.Accessory != "" {
						fmt.Fprintf(cmd.OutOrStdout(), " accessory=%s", observed.Accessory)
					}
					if observed.Release != "" {
						fmt.Fprintf(cmd.OutOrStdout(), " release=%s", observed.Release)
					}
					if observed.Status != "" {
						fmt.Fprintf(cmd.OutOrStdout(), " status=%q", observed.Status)
					}
					fmt.Fprintln(cmd.OutOrStdout())
				}
			}
			if len(view.Accessories) > 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "accessories:")
				for _, acc := range view.Accessories {
					fmt.Fprintf(cmd.OutOrStdout(), "- %s host=%s updated=%s\n", acc.Name, acc.Host.Name, acc.UpdatedAt.Format(time.RFC3339))
				}
			}
			fmt.Fprintln(cmd.OutOrStdout(), "events:")
			renderEventsText(cmd.OutOrStdout(), view.Events)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print inspect data as JSON")
	return cmd
}

func eventsCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "events ENV",
		Short: "Show local Ship event timeline",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _, store, err := environmentContext(opts, args[0])
			if err != nil {
				return err
			}
			events, err := store.Events(args[0])
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), events)
			}
			renderEventsText(cmd.OutOrStdout(), events)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print events as JSON")
	return cmd
}

func rollbackCmd(opts *options) *cobra.Command {
	var toRelease string
	var allowDataRollback bool
	cmd := &cobra.Command{
		Use:   "rollback ENV",
		Short: "Apply the previous healthy release",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			env, err := cfg.Environment(args[0])
			if err != nil {
				return err
			}
			stateDir, err := localStateDirForConfig(opts.configPath)
			if err != nil {
				return err
			}
			store := state.NewStore(stateDir)
			hosts, err := resolvedHostsForEnvironment(store, args[0], env)
			if err != nil {
				return err
			}
			var release state.Release
			if strings.TrimSpace(toRelease) != "" {
				release, err = store.ReadRelease(toRelease)
			} else {
				release, err = store.RollbackTarget(args[0])
			}
			if err != nil {
				return err
			}
			if release.Environment != args[0] {
				return fmt.Errorf("release %s belongs to environment %q", release.ID, release.Environment)
			}
			if release.Status == state.ReleaseStatusFailed || (!release.Healthy && release.Status != "") {
				return fmt.Errorf("release %s is not healthy", release.ID)
			}
			blockers := rollbackBlockers(cfg)
			if len(blockers) > 0 && !allowDataRollback {
				message := rollbackBlockerError(blockers)
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback", Status: "blocked", Release: release.ID, Message: message})
				return fmt.Errorf("%s", message)
			}
			currentReleaseID := ""
			if current, err := store.CurrentRelease(args[0]); err == nil {
				currentReleaseID = current.ID
			}
			if currentReleaseID == release.ID {
				return fmt.Errorf("release %s is already current", release.ID)
			}
			var observed []deployment.ObservedContainer
			useObservedRollout := false
			if !opts.dryRun {
				needsFixedPortSafety, err := rollbackNeedsFixedPortSafety(cfg, env)
				if err != nil {
					return err
				}
				if needsFixedPortSafety {
					agentFor := deploymentAgentFactory()
					observed, err = deployment.InspectObservedOnHosts(ctx, hosts, agentFor)
					if err != nil {
						return err
					}
					conflicts, err := deployment.FixedPortRollbackConflicts(deployment.PlanInput{
						Config:      cfg,
						Environment: env,
						Hosts:       hosts,
						EnvName:     args[0],
						ReleaseID:   release.ID,
						Observed:    observed,
					})
					if err != nil {
						return err
					}
					if len(conflicts) > 0 {
						message := unsafeFixedPortRollbackError(conflicts)
						recordEvent(store, state.Event{Environment: args[0], Kind: "rollback", Status: "blocked", Release: release.ID, Message: message})
						return fmt.Errorf("%s", message)
					}
					useObservedRollout = true
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "rollback %s to release %s\n", args[0], release.ID)
			for svc, image := range release.Images {
				fmt.Fprintf(cmd.OutOrStdout(), "- %s -> %s\n", svc, image)
			}
			if opts.dryRun {
				return printIngressDryRun(cmd.OutOrStdout(), cfg, env, args[0], stateDir)
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "rollback", Status: "started", Release: release.ID})
			recordEvent(store, state.Event{Environment: args[0], Kind: "rollback_attempt", Status: "started", Release: release.ID, Message: rollbackAttemptMessage(currentReleaseID)})
			secretFile, err := secrets.RenderEnvFile(cfg)
			if err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback_attempt", Status: "failed", Release: release.ID, Message: rollbackAttemptFailureMessage(currentReleaseID, err)})
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback", Status: "failed", Release: release.ID, Message: err.Error()})
				return err
			}
			secretEnvFiles, secretWrites, err := serviceSecretEnvFiles(cfg, hosts, args[0], secretFile)
			if err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback_attempt", Status: "failed", Release: release.ID, Message: rollbackAttemptFailureMessage(currentReleaseID, err)})
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback", Status: "failed", Release: release.ID, Message: err.Error()})
				return err
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "rollback_secret_write", Status: "started", Release: release.ID, Message: fmt.Sprintf("writes=%d", len(secretWrites))})
			if err := writeRemoteSecretFiles(ctx, secretWrites, secretFile.Content); err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback_secret_write", Status: "failed", Release: release.ID, Message: err.Error()})
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback_attempt", Status: "failed", Release: release.ID, Message: rollbackAttemptFailureMessage(currentReleaseID, err)})
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback", Status: "failed", Release: release.ID, Message: err.Error()})
				return err
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "rollback_secret_write", Status: "succeeded", Release: release.ID, Message: fmt.Sprintf("writes=%d", len(secretWrites))})
			recordEvent(store, state.Event{Environment: args[0], Kind: "rollback_rollout", Status: "started", Release: release.ID})
			agentFor := deploymentAgentFactory()
			var actions []deployment.Action
			if useObservedRollout {
				actions, err = deployment.BuildActions(deployment.PlanInput{
					Config:         cfg,
					Environment:    env,
					Hosts:          hosts,
					EnvName:        args[0],
					ReleaseID:      release.ID,
					Images:         release.Images,
					Observed:       observed,
					StateDir:       stateDir,
					SecretEnvFiles: secretEnvFiles,
				})
				if err == nil {
					err = deployment.ExecuteActions(ctx, actions, agentFor, nil)
				}
			} else {
				actions, err = deployment.Rollout(ctx, deployment.RolloutOptions{
					Config:         cfg,
					Environment:    env,
					Hosts:          hosts,
					EnvName:        args[0],
					ReleaseID:      release.ID,
					Images:         release.Images,
					StateDir:       stateDir,
					SecretEnvFiles: secretEnvFiles,
					AgentFor:       agentFor,
				})
			}
			if err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback_rollout", Status: "failed", Release: release.ID, Message: err.Error()})
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback_attempt", Status: "failed", Release: release.ID, Message: rollbackAttemptFailureMessage(currentReleaseID, err)})
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback", Status: "failed", Release: release.ID, Message: err.Error()})
				return err
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "rollback_rollout", Status: "succeeded", Release: release.ID})
			recordIngressEvents(store, args[0], release.ID, actions)
			completedAt := deployNow()
			healthyRelease := releaseAsHealthy(release, completedAt)
			if err := syncRemoteReleaseStateWithEvents(ctx, store, args[0], hosts, "rollback_release_state_write", healthyRelease); err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback_attempt", Status: "failed", Release: release.ID, Message: rollbackAttemptFailureMessage(currentReleaseID, err)})
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback", Status: "failed", Release: release.ID, Message: err.Error()})
				return err
			}
			if _, err := store.MarkReleaseHealthy(release.ID, completedAt); err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback_attempt", Status: "failed", Release: release.ID, Message: rollbackAttemptFailureMessage(currentReleaseID, err)})
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback", Status: "failed", Release: release.ID, Message: err.Error()})
				return err
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "rollback_attempt", Status: "succeeded", Release: release.ID, Message: rollbackAttemptMessage(currentReleaseID)})
			recordEvent(store, state.Event{Environment: args[0], Kind: "rollback", Status: "succeeded", Release: release.ID})
			return nil
		},
	}
	cmd.Flags().StringVar(&toRelease, "to", "", "specific healthy release id to apply")
	cmd.Flags().BoolVar(&allowDataRollback, "allow-data-rollback", false, "confirm rollback risk for configured stateful accessories")
	return cmd
}

type rollbackBlocker struct {
	Accessory string
	Reason    string
}

func rollbackBlockers(cfg *config.Config) []rollbackBlocker {
	if cfg == nil {
		return nil
	}
	var blockers []rollbackBlocker
	for _, name := range accessory.SortedNames(cfg, "") {
		acc := cfg.Accessories[name]
		switch {
		case acc.Primary && acc.Backup.Required:
			blockers = append(blockers, rollbackBlocker{Accessory: name, Reason: "primary backup-required accessory"})
		case acc.Primary:
			blockers = append(blockers, rollbackBlocker{Accessory: name, Reason: "primary accessory"})
		case acc.Backup.Required:
			blockers = append(blockers, rollbackBlocker{Accessory: name, Reason: "backup-required accessory"})
		}
	}
	return blockers
}

func rollbackBlockerError(blockers []rollbackBlocker) string {
	messages := rollbackBlockerMessages(blockers)
	return "rollback may be unsafe for stateful data: " + strings.Join(messages, ", ") + "; rerun with --allow-data-rollback after confirming app/data compatibility"
}

func rollbackBlockerMessages(blockers []rollbackBlocker) []string {
	messages := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		messages = append(messages, fmt.Sprintf("accessory %s (%s)", blocker.Accessory, blocker.Reason))
	}
	return messages
}

func rollbackNeedsFixedPortSafety(cfg *config.Config, env config.Environment) (bool, error) {
	placements, err := scheduler.PlaceServices(cfg, env)
	if err != nil {
		return false, err
	}
	for _, placement := range placements {
		if len(cfg.Services[placement.Service].Ports) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func unsafeFixedPortRollbackError(conflicts []deployment.FixedPortRollbackConflict) string {
	details := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		release := conflict.ContainerRelease
		if strings.TrimSpace(release) == "" {
			release = "unknown"
		}
		details = append(details, fmt.Sprintf(
			"service %s.%d on %s fixed port(s) %s would require stopping %s (release %s) before %s is healthy",
			conflict.Service,
			conflict.Replica,
			conflict.Host.Name,
			formatPortList(conflict.Ports),
			conflict.ContainerName,
			release,
			conflict.TargetContainerName,
		))
	}
	return "unsafe fixed-port rollback: refusing to avoid downtime: " + strings.Join(details, "; ")
}

func formatPortList(ports []int) string {
	values := make([]string, 0, len(ports))
	for _, port := range ports {
		values = append(values, strconv.Itoa(port))
	}
	if len(values) == 0 {
		return "unknown"
	}
	return strings.Join(values, ",")
}

func rollbackAttemptMessage(currentReleaseID string) string {
	if strings.TrimSpace(currentReleaseID) == "" {
		return "from unknown current release"
	}
	return "from " + currentReleaseID
}

func rollbackAttemptFailureMessage(currentReleaseID string, err error) string {
	return rollbackAttemptMessage(currentReleaseID) + ": " + err.Error()
}

type recoveryView struct {
	Environment      string          `json:"environment"`
	CurrentRelease   *state.Release  `json:"current_release,omitempty"`
	FailedReleases   []state.Release `json:"failed_releases,omitempty"`
	RollbackTarget   *state.Release  `json:"rollback_target,omitempty"`
	RollbackError    string          `json:"rollback_error,omitempty"`
	RollbackBlockers []string        `json:"rollback_blockers,omitempty"`
	SuggestedCommand string          `json:"suggested_command,omitempty"`
	Events           []state.Event   `json:"events,omitempty"`
}

func recoverCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:     "recover ENV",
		Aliases: []string{"recovery"},
		Short:   "Show failed deploy recovery information from local state",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, store, err := environmentContext(opts, args[0])
			if err != nil {
				return err
			}
			view, err := buildRecoveryView(cfg, args[0], store)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), view)
			}
			renderRecoveryText(cmd.OutOrStdout(), view)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print recovery information as JSON")
	return cmd
}

func buildRecoveryView(cfg *config.Config, envName string, store state.Store) (recoveryView, error) {
	releases, err := store.Releases(envName)
	if err != nil {
		return recoveryView{}, err
	}
	events, err := store.Events(envName)
	if err != nil {
		return recoveryView{}, err
	}
	view := recoveryView{
		Environment:      envName,
		FailedReleases:   failedReleases(releases),
		RollbackBlockers: rollbackBlockerMessages(rollbackBlockers(cfg)),
		Events:           recoveryEvents(events, 8),
	}
	if current, err := store.CurrentRelease(envName); err == nil {
		view.CurrentRelease = &current
	}
	if target, err := store.RollbackTarget(envName); err == nil {
		view.RollbackTarget = &target
		view.SuggestedCommand = suggestedRollbackCommand(envName, target.ID, len(view.RollbackBlockers) > 0)
	} else {
		view.RollbackError = err.Error()
	}
	return view, nil
}

func failedReleases(releases []state.Release) []state.Release {
	var failed []state.Release
	for _, release := range releases {
		if release.Status == state.ReleaseStatusFailed || (!release.Healthy && release.Status == state.ReleaseStatusFailed) {
			failed = append(failed, release)
		}
	}
	return failed
}

func recoveryEvents(events []state.Event, limit int) []state.Event {
	if limit <= 0 {
		return nil
	}
	var selected []state.Event
	for i := len(events) - 1; i >= 0 && len(selected) < limit; i-- {
		status := events[i].Status
		if status != "failed" && status != "blocked" {
			continue
		}
		selected = append(selected, events[i])
	}
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}
	return selected
}

func suggestedRollbackCommand(envName, releaseID string, allowDataRollback bool) string {
	command := fmt.Sprintf("ship rollback %s --to %s", envName, releaseID)
	if allowDataRollback {
		command += " --allow-data-rollback"
	}
	return command
}

func renderRecoveryText(w io.Writer, view recoveryView) {
	fmt.Fprintf(w, "recovery %s\n", view.Environment)
	if view.CurrentRelease == nil {
		fmt.Fprintln(w, "current release none")
	} else {
		fmt.Fprintf(w, "current release %s status=%s healthy=%t\n", view.CurrentRelease.ID, view.CurrentRelease.Status, view.CurrentRelease.Healthy)
	}
	if len(view.FailedReleases) == 0 {
		fmt.Fprintln(w, "failed releases none")
	} else {
		fmt.Fprintln(w, "failed releases:")
		for _, release := range view.FailedReleases {
			fmt.Fprintf(w, "- %s", release.ID)
			if release.Error != "" {
				fmt.Fprintf(w, " error=%q", release.Error)
			}
			fmt.Fprintln(w)
		}
	}
	if view.RollbackTarget != nil {
		fmt.Fprintf(w, "rollback target %s\n", view.RollbackTarget.ID)
	} else if view.RollbackError != "" {
		fmt.Fprintf(w, "rollback target unavailable: %s\n", view.RollbackError)
	}
	if len(view.RollbackBlockers) > 0 {
		fmt.Fprintln(w, "rollback blockers:")
		for _, blocker := range view.RollbackBlockers {
			fmt.Fprintf(w, "- %s\n", blocker)
		}
	}
	if view.SuggestedCommand != "" {
		fmt.Fprintf(w, "suggested rollback: %s\n", view.SuggestedCommand)
	}
	if len(view.Events) > 0 {
		fmt.Fprintln(w, "recent failure events:")
		renderEventsText(w, view.Events)
	}
}

func deploymentAgentFactory() deployment.AgentFactory {
	return func(host scheduler.Host) deployment.Agent {
		return newDeployAgent(host)
	}
}

type remoteSecretFile struct {
	Host scheduler.Host
	Path string
}

func serviceSecretEnvFiles(cfg *config.Config, hosts []scheduler.Host, envName string, rendered secrets.RenderedEnvFile) (map[string]string, []remoteSecretFile, error) {
	if len(rendered.Digests) == 0 {
		return nil, nil, nil
	}
	placements, err := scheduler.PlaceServicesOnHosts(cfg, hosts)
	if err != nil {
		return nil, nil, err
	}
	envFiles := map[string]string{}
	writesByKey := map[string]remoteSecretFile{}
	for _, placement := range placements {
		path := secrets.RemoteEnvFilePath(envName, "service-"+placement.Service)
		envFiles[placement.Service] = path
		key := placement.Host.Name + "\x00" + path
		writesByKey[key] = remoteSecretFile{Host: placement.Host, Path: path}
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

func accessorySecretEnvFile(envName, name string, rendered secrets.RenderedEnvFile) string {
	if len(rendered.Digests) == 0 {
		return ""
	}
	return secrets.RemoteEnvFilePath(envName, "accessory-"+name)
}

func writeRemoteSecretFiles(ctx context.Context, writes []remoteSecretFile, content string) error {
	for _, write := range writes {
		if err := writeRemoteSecretFile(ctx, write.Host, write.Path, content); err != nil {
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

func printIngressDryRun(w io.Writer, cfg *config.Config, env config.Environment, envName, stateDir string) error {
	placements, err := scheduler.PlaceServices(cfg, env)
	if err != nil {
		return err
	}
	caddyfile := ingress.GenerateCaddyfile(cfg, placements)
	if strings.TrimSpace(caddyfile) == "" {
		return nil
	}
	fmt.Fprintf(w, "- ingress config %s\n", filepath.Join(stateDir, "ingress", envName+".Caddyfile"))
	for _, host := range ingress.HostsForEnvironment(cfg, env, placements) {
		fmt.Fprintf(w, "- reload caddy on %s after validation\n", host.Name)
	}
	return nil
}

func accessoryCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{Use: "accessory", Short: "Manage stateful single-primary accessories"}
	cmd.AddCommand(&cobra.Command{
		Use:   "deploy ENV [NAME]",
		Short: "Deploy one accessory container per accessory",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, env, store, err := accessoryContext(opts, args[0])
			if err != nil {
				return err
			}
			names, err := accessoryTargets(cfg, args[1:])
			if err != nil {
				return err
			}
			return runAccessoryDeploy(cmd.Context(), cmd.OutOrStdout(), opts, cfg, env, args[0], store, names)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "status ENV [NAME]",
		Short: "Show accessory placement and observed containers",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, env, store, err := accessoryContext(opts, args[0])
			if err != nil {
				return err
			}
			names, err := accessoryTargets(cfg, args[1:])
			if err != nil {
				return err
			}
			return runAccessoryStatus(cmd.Context(), cmd.OutOrStdout(), cfg, env, args[0], store, names)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "backup ENV [NAME]",
		Short: "Run accessory backup commands on placed hosts",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, env, store, err := accessoryContext(opts, args[0])
			if err != nil {
				return err
			}
			names, err := accessoryTargets(cfg, args[1:])
			if err != nil {
				return err
			}
			return runAccessoryBackup(cmd.Context(), cmd.OutOrStdout(), opts, cfg, env, args[0], store, names)
		},
	})
	var restoreArtifact string
	var restoreYes bool
	restore := &cobra.Command{
		Use:   "restore ENV NAME",
		Short: "Restore one accessory from an explicit backup artifact",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, env, store, err := accessoryContext(opts, args[0])
			if err != nil {
				return err
			}
			if _, ok := cfg.Accessories[args[1]]; !ok {
				return fmt.Errorf("unknown accessory %q", args[1])
			}
			return runAccessoryRestore(cmd.Context(), cmd.OutOrStdout(), opts, cfg, env, args[0], store, args[1], restoreArtifact, restoreYes)
		},
	}
	restore.Flags().StringVar(&restoreArtifact, "artifact", "", "remote backup artifact path to restore")
	restore.Flags().BoolVar(&restoreYes, "yes", false, "confirm destructive restore")
	cmd.AddCommand(restore)
	var failoverTarget string
	var failoverArtifact string
	var failoverYes bool
	failover := &cobra.Command{
		Use:   "failover ENV NAME",
		Short: "Move a single-primary accessory to another eligible host after backup/restore checks",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, env, store, err := accessoryContext(opts, args[0])
			if err != nil {
				return err
			}
			if _, ok := cfg.Accessories[args[1]]; !ok {
				return fmt.Errorf("unknown accessory %q", args[1])
			}
			return runAccessoryFailover(cmd.Context(), cmd.OutOrStdout(), opts, cfg, env, args[0], store, args[1], failoverTarget, failoverArtifact, failoverYes)
		},
	}
	failover.Flags().StringVar(&failoverTarget, "to", "", "eligible host name to promote")
	failover.Flags().StringVar(&failoverArtifact, "artifact", "", "remote backup artifact path to restore; defaults to the last recorded backup")
	failover.Flags().BoolVar(&failoverYes, "yes", false, "confirm failover")
	cmd.AddCommand(failover)
	return cmd
}

type accessoryObservation struct {
	Host      scheduler.Host
	Container docker.ContainerSummary
}

func accessoryContext(opts *options, envName string) (*config.Config, config.Environment, state.Store, error) {
	cfg, err := config.Load(opts.configPath)
	if err != nil {
		return nil, config.Environment{}, state.Store{}, err
	}
	env, err := cfg.Environment(envName)
	if err != nil {
		return nil, config.Environment{}, state.Store{}, err
	}
	stateDir, err := localStateDirForConfig(opts.configPath)
	if err != nil {
		return nil, config.Environment{}, state.Store{}, err
	}
	return cfg, env, state.NewStore(stateDir), nil
}

func accessoryTargets(cfg *config.Config, args []string) ([]string, error) {
	name := ""
	if len(args) > 0 {
		name = args[0]
		if _, ok := cfg.Accessories[name]; !ok {
			return nil, fmt.Errorf("unknown accessory %q", name)
		}
	}
	names := accessory.SortedNames(cfg, name)
	if len(names) == 0 {
		return nil, fmt.Errorf("no accessories configured")
	}
	return names, nil
}

func runAccessoryDeploy(ctx context.Context, w io.Writer, opts *options, cfg *config.Config, env config.Environment, envName string, store state.Store, names []string) error {
	var secretFile secrets.RenderedEnvFile
	var err error
	if !opts.dryRun {
		secretFile, err = secrets.RenderEnvFile(cfg)
		if err != nil {
			return err
		}
	}
	hosts, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return err
	}
	for _, name := range names {
		acc := cfg.Accessories[name]
		if err := accessory.ValidateDeploy(acc); err != nil {
			return fmt.Errorf("accessory %q: %w", name, err)
		}
		placement, err := accessory.PlacementForHosts(cfg, hosts, envName, name, store)
		if err != nil {
			return err
		}
		if opts.dryRun {
			fmt.Fprintf(w, "would deploy accessory %s on %s image=%s\n", name, placement.Host.Name, acc.Image)
			continue
		}
		observed, err := collectAccessoryObservations(ctx, cfg, hosts, envName, []string{name})
		if err != nil {
			return err
		}
		containerName := accessory.ContainerName(cfg.Project, envName, name)
		if err := validateSingleAccessory(name, placement, containerName, observed[name]); err != nil {
			return err
		}
		placement, err = accessory.EnsurePlacementForHosts(cfg, hosts, envName, name, store, deployNow())
		if err != nil {
			return err
		}
		client := newDeployAgent(placement.Host)
		secretEnvFile := accessorySecretEnvFile(envName, name, secretFile)
		if err := writeRemoteSecretFile(ctx, placement.Host, secretEnvFile, secretFile.Content); err != nil {
			return err
		}
		if err := client.Call(ctx, "pull", map[string]string{"image": acc.Image}, nil); err != nil {
			return fmt.Errorf("pull accessory %s on %s: %w", name, placement.Host.Name, err)
		}
		for _, volume := range accessory.NamedVolumes(acc) {
			params := agent.EnsureVolumeParams{Name: volume, Owner: acc.VolumeOwner}
			if err := client.Call(ctx, "ensure_volume", params, nil); err != nil {
				return fmt.Errorf("ensure volume %s for accessory %s on %s: %w", volume, name, placement.Host.Name, err)
			}
		}
		params := agent.RunContainerParams{
			Name:   containerName,
			Image:  acc.Image,
			Args:   accessory.DockerArgs(acc, secretEnvFile),
			Labels: accessory.ContainerLabels(cfg.Project, envName, name),
		}
		if err := client.Call(ctx, "run_container", params, nil); err != nil {
			return fmt.Errorf("deploy accessory %s on %s: %w", name, placement.Host.Name, err)
		}
		fmt.Fprintf(w, "deployed accessory %s on %s image=%s\n", name, placement.Host.Name, acc.Image)
	}
	return nil
}

func runAccessoryStatus(ctx context.Context, w io.Writer, cfg *config.Config, env config.Environment, envName string, store state.Store, names []string) error {
	hosts, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return err
	}
	observed, err := collectAccessoryObservations(ctx, cfg, hosts, envName, names)
	if err != nil {
		return err
	}
	for _, name := range names {
		placement := "unplaced"
		if saved, err := store.ReadAccessoryState(envName, name); err == nil {
			placement = saved.Host.Name
		} else if !os.IsNotExist(err) {
			return err
		}
		items := observed[name]
		switch len(items) {
		case 0:
			fmt.Fprintf(w, "accessory %s placement=%s status=missing\n", name, placement)
		case 1:
			item := items[0]
			fmt.Fprintf(w, "accessory %s placement=%s host=%s image=%s status=%s\n", name, placement, item.Host.Name, item.Container.Image, item.Container.Status)
		default:
			fmt.Fprintf(w, "accessory %s placement=%s status=replicated hosts=%s\n", name, placement, observationHosts(items))
		}
	}
	return nil
}

func runAccessoryBackup(ctx context.Context, w io.Writer, opts *options, cfg *config.Config, env config.Environment, envName string, store state.Store, names []string) error {
	hosts, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return err
	}
	for _, name := range names {
		recordEvent(store, state.Event{Environment: envName, Kind: "accessory_backup", Status: "started", Accessory: name})
		fail := func(err error) error {
			recordEvent(store, state.Event{Environment: envName, Kind: "accessory_backup", Status: "failed", Accessory: name, Message: err.Error()})
			return err
		}
		acc := cfg.Accessories[name]
		if strings.TrimSpace(acc.Backup.Command) == "" {
			return fail(fmt.Errorf("accessory %q backup.command is required", name))
		}
		artifact := accessory.BackupArtifactPath(acc, envName, name, deployNow())
		placement, err := accessory.PlacementForHosts(cfg, hosts, envName, name, store)
		if err != nil {
			return fail(err)
		}
		if opts.dryRun {
			recordEvent(store, state.Event{Environment: envName, Kind: "accessory_backup", Status: "planned", Accessory: name, Host: placement.Host.Name, Message: "dry-run"})
			fmt.Fprintf(w, "would backup accessory %s on %s artifact=%s\n", name, placement.Host.Name, artifact)
			continue
		}
		if !placement.Persisted {
			return fail(fmt.Errorf("accessory %q is not deployed; run accessory deploy first", name))
		}
		observed, err := collectAccessoryObservations(ctx, cfg, hosts, envName, []string{name})
		if err != nil {
			return fail(err)
		}
		containerName := accessory.ContainerName(cfg.Project, envName, name)
		if err := validatePlacedAccessory(name, placement, containerName, observed[name]); err != nil {
			return fail(err)
		}
		command, err := accessory.BackupCommand(acc, artifact)
		if err != nil {
			return fail(err)
		}
		host := placement.Host
		var result agent.CommandResult
		if err := newDeployAgent(host).Call(ctx, "accessory_backup", agent.AccessoryCommandParams{
			Name:           name,
			Command:        command,
			TimeoutSeconds: accessory.BackupTimeoutSeconds(acc),
		}, &result); err != nil {
			return fail(fmt.Errorf("backup accessory %s on %s: %w", name, host.Name, err))
		}
		if _, err := store.RecordAccessoryBackup(envName, name, state.AccessoryBackup{
			Artifact:  artifact,
			Host:      host.Name,
			Output:    result.Output,
			CreatedAt: deployNow().UTC(),
		}); err != nil {
			return fail(err)
		}
		recordEvent(store, state.Event{Environment: envName, Kind: "accessory_backup", Status: "succeeded", Accessory: name, Host: host.Name, Message: artifact})
		fmt.Fprintf(w, "backed up accessory %s on %s artifact=%s\n", name, host.Name, artifact)
	}
	return nil
}

func runAccessoryRestore(ctx context.Context, w io.Writer, opts *options, cfg *config.Config, env config.Environment, envName string, store state.Store, name, artifact string, yes bool) error {
	hosts, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return err
	}
	recordEvent(store, state.Event{Environment: envName, Kind: "accessory_restore", Status: "started", Accessory: name})
	fail := func(err error) error {
		recordEvent(store, state.Event{Environment: envName, Kind: "accessory_restore", Status: "failed", Accessory: name, Message: err.Error()})
		return err
	}
	acc := cfg.Accessories[name]
	if err := accessory.ValidateRestore(acc); err != nil {
		return fail(err)
	}
	artifact = strings.TrimSpace(artifact)
	if artifact == "" {
		return fail(fmt.Errorf("restore requires --artifact"))
	}
	artifact, err = accessory.ValidateRestoreArtifact(acc, envName, name, artifact)
	if err != nil {
		return fail(err)
	}
	if !yes && !opts.dryRun {
		return fail(fmt.Errorf("accessory restore requires --yes to confirm destructive restore"))
	}
	placement, err := accessory.PlacementForHosts(cfg, hosts, envName, name, store)
	if err != nil {
		return fail(err)
	}
	if opts.dryRun {
		recordEvent(store, state.Event{Environment: envName, Kind: "accessory_restore", Status: "planned", Accessory: name, Host: placement.Host.Name, Message: "dry-run"})
		fmt.Fprintf(w, "would restore accessory %s on %s artifact=%s\n", name, placement.Host.Name, artifact)
		return nil
	}
	if !placement.Persisted {
		return fail(fmt.Errorf("accessory %q is not deployed; run accessory deploy first", name))
	}
	observed, err := collectAccessoryObservations(ctx, cfg, hosts, envName, []string{name})
	if err != nil {
		return fail(err)
	}
	containerName := accessory.ContainerName(cfg.Project, envName, name)
	if err := validatePlacedAccessory(name, placement, containerName, observed[name]); err != nil {
		return fail(err)
	}
	checkCommand, err := accessory.RestoreCheckCommand(acc, envName, name, artifact)
	if err != nil {
		return fail(err)
	}
	host := placement.Host
	var check agent.HealthCheckResult
	if err := newDeployAgent(host).Call(ctx, "health_check", agent.HealthCheckParams{
		Command:        checkCommand,
		TimeoutSeconds: 30,
	}, &check); err != nil {
		return fail(fmt.Errorf("verify backup artifact for accessory %s on %s: %w", name, host.Name, err))
	}
	if !check.OK {
		return fail(fmt.Errorf("verify backup artifact for accessory %s on %s failed", name, host.Name))
	}
	restoreCommand, err := accessory.RestoreCommand(acc, artifact)
	if err != nil {
		return fail(err)
	}
	var result agent.CommandResult
	if err := newDeployAgent(host).Call(ctx, "accessory_restore", agent.AccessoryCommandParams{
		Name:           name,
		Command:        restoreCommand,
		TimeoutSeconds: accessory.BackupTimeoutSeconds(acc),
	}, &result); err != nil {
		return fail(fmt.Errorf("restore accessory %s on %s: %w", name, host.Name, err))
	}
	if _, err := store.RecordAccessoryRestore(envName, name, state.AccessoryRestore{
		Artifact:  artifact,
		Host:      host.Name,
		Output:    result.Output,
		CreatedAt: deployNow().UTC(),
	}); err != nil {
		return fail(err)
	}
	recordEvent(store, state.Event{Environment: envName, Kind: "accessory_restore", Status: "succeeded", Accessory: name, Host: host.Name, Message: artifact})
	fmt.Fprintf(w, "restored accessory %s on %s artifact=%s\n", name, host.Name, artifact)
	return nil
}

func runAccessoryFailover(ctx context.Context, w io.Writer, opts *options, cfg *config.Config, env config.Environment, envName string, store state.Store, name, targetName, artifact string, yes bool) error {
	hosts, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return err
	}
	recordEvent(store, state.Event{Environment: envName, Kind: "accessory_failover", Status: "started", Accessory: name})
	fail := func(err error) error {
		recordEvent(store, state.Event{Environment: envName, Kind: "accessory_failover", Status: "failed", Accessory: name, Message: err.Error()})
		return err
	}
	acc := cfg.Accessories[name]
	if err := accessory.ValidateRestore(acc); err != nil {
		return fail(err)
	}
	targetName = strings.TrimSpace(targetName)
	if targetName == "" {
		return fail(fmt.Errorf("accessory failover requires --to"))
	}
	if !yes && !opts.dryRun {
		return fail(fmt.Errorf("accessory failover requires --yes to confirm primary movement"))
	}
	current, err := accessory.PlacementForHosts(cfg, hosts, envName, name, store)
	if err != nil {
		return fail(err)
	}
	if !current.Persisted {
		return fail(fmt.Errorf("accessory %q is not deployed; run accessory deploy first", name))
	}
	target, err := accessoryTargetHost(hosts, acc.Pool, targetName)
	if err != nil {
		return fail(err)
	}
	if target.Name == current.Host.Name {
		return fail(fmt.Errorf("accessory %q is already placed on %s", name, target.Name))
	}
	saved, err := store.ReadAccessoryState(envName, name)
	if err != nil {
		return fail(err)
	}
	if strings.TrimSpace(artifact) == "" {
		if saved.LastBackup == nil || strings.TrimSpace(saved.LastBackup.Artifact) == "" {
			return fail(fmt.Errorf("accessory failover requires --artifact or a recorded backup"))
		}
		artifact = saved.LastBackup.Artifact
	}
	artifact, err = accessory.ValidateRestoreArtifact(acc, envName, name, artifact)
	if err != nil {
		return fail(err)
	}
	if opts.dryRun {
		recordEvent(store, state.Event{Environment: envName, Kind: "accessory_failover", Status: "planned", Accessory: name, Host: target.Name, Message: "dry-run"})
		fmt.Fprintf(w, "would failover accessory %s from %s to %s artifact=%s\n", name, current.Host.Name, target.Name, artifact)
		return nil
	}

	secretFile, err := secrets.RenderEnvFile(cfg)
	if err != nil {
		return fail(err)
	}
	observed, err := collectAccessoryObservations(ctx, cfg, hosts, envName, []string{name})
	if err != nil {
		return fail(err)
	}
	containerName := accessory.ContainerName(cfg.Project, envName, name)
	if err := validatePlacedAccessory(name, current, containerName, observed[name]); err != nil {
		return fail(err)
	}
	for _, item := range observed[name] {
		if item.Host.Name == target.Name {
			return fail(fmt.Errorf("accessory %q already has a managed container on failover target %s", name, target.Name))
		}
	}

	targetClient := newDeployAgent(target)
	secretEnvFile := accessorySecretEnvFile(envName, name, secretFile)
	if err := writeRemoteSecretFile(ctx, target, secretEnvFile, secretFile.Content); err != nil {
		return fail(err)
	}
	if err := targetClient.Call(ctx, "pull", map[string]string{"image": acc.Image}, nil); err != nil {
		return fail(fmt.Errorf("pull accessory %s on %s: %w", name, target.Name, err))
	}
	for _, volume := range accessory.NamedVolumes(acc) {
		params := agent.EnsureVolumeParams{Name: volume, Owner: acc.VolumeOwner}
		if err := targetClient.Call(ctx, "ensure_volume", params, nil); err != nil {
			return fail(fmt.Errorf("ensure volume %s for accessory %s on %s: %w", volume, name, target.Name, err))
		}
	}
	params := agent.RunContainerParams{
		Name:   containerName,
		Image:  acc.Image,
		Args:   accessory.DockerArgs(acc, secretEnvFile),
		Labels: accessory.ContainerLabels(cfg.Project, envName, name),
	}
	if err := targetClient.Call(ctx, "run_container", params, nil); err != nil {
		return fail(fmt.Errorf("start failover accessory %s on %s: %w", name, target.Name, err))
	}
	checkCommand, err := accessory.RestoreCheckCommand(acc, envName, name, artifact)
	if err != nil {
		return fail(err)
	}
	var check agent.HealthCheckResult
	if err := targetClient.Call(ctx, "health_check", agent.HealthCheckParams{Command: checkCommand, TimeoutSeconds: 30}, &check); err != nil {
		return fail(fmt.Errorf("verify backup artifact for accessory %s on %s: %w", name, target.Name, err))
	}
	if !check.OK {
		return fail(fmt.Errorf("verify backup artifact for accessory %s on %s failed", name, target.Name))
	}
	restoreCommand, err := accessory.RestoreCommand(acc, artifact)
	if err != nil {
		return fail(err)
	}
	var result agent.CommandResult
	if err := targetClient.Call(ctx, "accessory_restore", agent.AccessoryCommandParams{
		Name:           name,
		Command:        restoreCommand,
		TimeoutSeconds: accessory.BackupTimeoutSeconds(acc),
	}, &result); err != nil {
		return fail(fmt.Errorf("restore accessory %s on %s: %w", name, target.Name, err))
	}
	if err := newDeployAgent(current.Host).Call(ctx, "stop_container", map[string]string{"name": containerName}, nil); err != nil {
		return fail(fmt.Errorf("stop old accessory %s on %s: %w", name, current.Host.Name, err))
	}
	saved.Host = accessory.HostFact(target)
	saved.LastRestore = &state.AccessoryRestore{
		Artifact:  artifact,
		Host:      target.Name,
		Output:    result.Output,
		CreatedAt: deployNow().UTC(),
	}
	saved.UpdatedAt = deployNow().UTC()
	if err := store.SaveAccessoryState(saved); err != nil {
		return fail(err)
	}
	recordEvent(store, state.Event{Environment: envName, Kind: "accessory_failover", Status: "succeeded", Accessory: name, Host: target.Name, Message: artifact})
	fmt.Fprintf(w, "failed over accessory %s from %s to %s artifact=%s\n", name, current.Host.Name, target.Name, artifact)
	return nil
}

func accessoryTargetHost(hosts []scheduler.Host, pool, targetName string) (scheduler.Host, error) {
	for _, host := range hosts {
		if host.Pool == pool && host.Name == targetName {
			return host, nil
		}
	}
	return scheduler.Host{}, fmt.Errorf("target host %q is not eligible in pool %q", targetName, pool)
}

func collectAccessoryObservations(ctx context.Context, cfg *config.Config, hosts []scheduler.Host, envName string, names []string) (map[string][]accessoryObservation, error) {
	targets := map[string]struct{}{}
	for _, name := range names {
		targets[name] = struct{}{}
	}
	observed := map[string][]accessoryObservation{}
	for _, host := range hosts {
		var containers []docker.ContainerSummary
		if err := newDeployAgent(host).Call(ctx, "list_ship_containers", map[string]any{}, &containers); err != nil {
			return nil, fmt.Errorf("inspect accessories on %s: %w", host.Name, err)
		}
		for _, container := range containers {
			for name := range targets {
				if accessory.MatchesLabels(cfg, envName, name, container.Labels) {
					observed[name] = append(observed[name], accessoryObservation{Host: host, Container: container})
				}
			}
		}
	}
	return observed, nil
}

func validateSingleAccessory(name string, placement accessory.Placement, containerName string, observed []accessoryObservation) error {
	return validateAccessoryTopology(name, placement, containerName, observed, false)
}

func validatePlacedAccessory(name string, placement accessory.Placement, containerName string, observed []accessoryObservation) error {
	return validateAccessoryTopology(name, placement, containerName, observed, true)
}

func validateAccessoryTopology(name string, placement accessory.Placement, containerName string, observed []accessoryObservation, requireExisting bool) error {
	if len(observed) == 0 {
		if requireExisting {
			return fmt.Errorf("accessory %q has no managed container on saved placement host %s", name, placement.Host.Name)
		}
		return nil
	}
	if len(observed) > 1 {
		return fmt.Errorf("accessory %q has multiple managed containers on hosts %s", name, observationHosts(observed))
	}
	for _, item := range observed {
		if item.Host.Name != placement.Host.Name {
			return fmt.Errorf("accessory %q already has a managed container on %s; saved placement is %s", name, item.Host.Name, placement.Host.Name)
		}
		if item.Container.Names != containerName {
			return fmt.Errorf("accessory %q already has managed container %s on %s; expected %s", name, item.Container.Names, item.Host.Name, containerName)
		}
	}
	return nil
}

func observationHosts(items []accessoryObservation) string {
	hosts := make([]string, 0, len(items))
	for _, item := range items {
		hosts = append(hosts, item.Host.Name)
	}
	sort.Strings(hosts)
	return strings.Join(hosts, ",")
}

func secretsCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{Use: "secrets", Short: "Verify environment-backed secrets"}
	cmd.AddCommand(&cobra.Command{
		Use:   "verify",
		Short: "Check required secrets exist in the local environment",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			checks, err := secrets.Verify(cfg)
			for _, check := range checks {
				if check.Present {
					fmt.Fprintf(cmd.OutOrStdout(), "ok   %s digest=%s\n", check.Name, check.Digest)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "fail %s missing\n", check.Name)
				}
			}
			return err
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "diff ENV",
		Short: "Compare local secret digests with the current release",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			if _, err := cfg.Environment(args[0]); err != nil {
				return err
			}
			rendered, err := secrets.RenderEnvFile(cfg)
			if err != nil {
				return err
			}
			stateDir, err := localStateDirForConfig(opts.configPath)
			if err != nil {
				return err
			}
			release, err := state.NewStore(stateDir).CurrentRelease(args[0])
			if err != nil {
				return fmt.Errorf("current release for %s: %w", args[0], err)
			}
			diff := secrets.Diff(rendered.Digests, release.SecretDigests)
			if diff.Empty() {
				fmt.Fprintf(cmd.OutOrStdout(), "secrets match current release %s\n", release.ID)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "secret drift against release %s\n", release.ID)
			for _, name := range diff.Missing {
				fmt.Fprintf(cmd.OutOrStdout(), "missing %s\n", name)
			}
			for _, name := range diff.Changed {
				fmt.Fprintf(cmd.OutOrStdout(), "changed %s\n", name)
			}
			for _, name := range diff.Extra {
				fmt.Fprintf(cmd.OutOrStdout(), "extra %s\n", name)
			}
			return fmt.Errorf("secret drift detected")
		},
	})
	var renderDryRun bool
	render := &cobra.Command{
		Use:   "render ENV",
		Short: "Render redacted remote secret env files",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !renderDryRun && !opts.dryRun {
				return fmt.Errorf("secrets render only supports --dry-run in V1")
			}
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			env, err := cfg.Environment(args[0])
			if err != nil {
				return err
			}
			rendered, err := secrets.RenderEnvFile(cfg)
			if err != nil {
				return err
			}
			if len(rendered.Digests) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no required secrets")
				return nil
			}
			for _, scope := range secretRenderScopes(cfg, env, args[0]) {
				fmt.Fprintf(cmd.OutOrStdout(), "# %s\n%s\n", scope, rendered.Redacted)
			}
			return nil
		},
	}
	render.Flags().BoolVar(&renderDryRun, "dry-run", false, "print redacted env-file output without exposing values")
	cmd.AddCommand(render)
	return cmd
}

func secretRenderScopes(cfg *config.Config, env config.Environment, envName string) []string {
	scopesByPath := map[string]struct{}{}
	if placements, err := scheduler.PlaceServices(cfg, env); err == nil {
		for _, placement := range placements {
			scopesByPath[secrets.RemoteEnvFilePath(envName, "service-"+placement.Service)] = struct{}{}
		}
	}
	for _, name := range accessory.SortedNames(cfg, "") {
		scopesByPath[secrets.RemoteEnvFilePath(envName, "accessory-"+name)] = struct{}{}
	}
	scopes := make([]string, 0, len(scopesByPath))
	for path := range scopesByPath {
		scopes = append(scopes, path)
	}
	sort.Strings(scopes)
	return scopes
}

func systemdUnit() string {
	return `[Unit]
Description=Ship node agent
After=docker.service
Requires=docker.service

[Service]
ExecStart=/usr/local/bin/ship agent run
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target`
}
