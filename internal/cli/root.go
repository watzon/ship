package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/watzon/ship/internal/accessory"
	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/cli/ui"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/deployment"
	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/doctor"
	"github.com/watzon/ship/internal/ingress"
	"github.com/watzon/ship/internal/planner"
	"github.com/watzon/ship/internal/provider"
	"github.com/watzon/ship/internal/provider/providers"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/secrets"
	"github.com/watzon/ship/internal/shipbinary"
	"github.com/watzon/ship/internal/state"
	"github.com/watzon/ship/internal/transport"
	"gopkg.in/yaml.v3"
)

type options struct {
	configPath          string
	dryRun              bool
	envFiles            []string
	secretsIdentityFile string
	agentBinaryPath     string
	agentReleaseDir     string
}

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

var newEnvironmentProvider = providers.ForEnvironment

type deployDocker interface {
	BuildImage(ctx context.Context, opts docker.BuildOptions) error
	Push(ctx context.Context, image string) error
	ResolveDigest(ctx context.Context, image string) (string, error)
	RegistryAuth(ctx context.Context, registry string) (docker.RegistryAuth, bool, error)
}

type deployAgent interface {
	Call(ctx context.Context, method string, params any, out any) error
}

var newDeployDocker = func() deployDocker {
	return docker.Client{}
}

var newDeployAgent = func(host scheduler.Host) deployAgent {
	return agent.Client{SSH: sshForHost(host, false)}
}

var deployNow = time.Now
var deployGitRevision = docker.GitShortSHA
var newReleaseID = docker.NewReleaseID
var runLocalHookCommand = defaultRunLocalHookCommand
var sendWebhookNotification = defaultSendWebhookNotification
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

type hookContext struct {
	Project     string
	Environment string
	Hook        string
	ReleaseID   string
	ConfigPath  string
	ConfigDir   string
	Failure     string
}

type notificationPayload struct {
	Project     string            `json:"project"`
	Environment string            `json:"environment"`
	Operation   string            `json:"operation"`
	Status      string            `json:"status"`
	Release     string            `json:"release,omitempty"`
	Message     string            `json:"message,omitempty"`
	Images      map[string]string `json:"images,omitempty"`
	Time        time.Time         `json:"time"`
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

func Execute() error {
	opts := &options{}
	root := &cobra.Command{
		Use:           "ship",
		Short:         "Deploy Docker apps to ordinary servers with horizontal scaling",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&opts.configPath, "config", "c", config.DefaultConfigFile, "path to ship.yml")
	root.PersistentFlags().BoolVar(&opts.dryRun, "dry-run", false, "print the intended operation without mutating remote state")
	root.PersistentFlags().StringArrayVar(&opts.envFiles, "env-file", nil, "load secrets from a dotenv file (repeatable)")
	root.PersistentFlags().StringVar(&opts.secretsIdentityFile, "secrets-identity-file", "", "age identity file for encrypted Ship secrets")
	ui.ConfigureRoot(root)

	init := initCmd(opts)
	init.GroupID = ui.GroupSetup
	doctor := doctorCmd(opts)
	doctor.GroupID = ui.GroupSetup
	cfg := configCmd(opts)
	cfg.GroupID = ui.GroupPlan
	hosts := hostsCmd(opts)
	hosts.GroupID = ui.GroupPlan
	plan := planCmd(opts)
	plan.GroupID = ui.GroupPlan
	scale := scaleCmd(opts)
	scale.GroupID = ui.GroupPlan
	provision := provisionCmd(opts)
	provision.GroupID = ui.GroupInfra
	migrate := migrateCmd(opts)
	migrate.GroupID = ui.GroupInfra
	agent := agentCmd(opts)
	agent.GroupID = ui.GroupInfra
	version := versionCmd(opts)
	version.GroupID = ui.GroupInfra
	release := releaseCmd()
	release.GroupID = ui.GroupInfra
	deploy := deployCmd(opts)
	deploy.GroupID = ui.GroupDeploy
	promote := promoteCmd(opts)
	promote.GroupID = ui.GroupDeploy
	status := statusCmd(opts)
	status.GroupID = ui.GroupOperate
	ps := psCmd(opts)
	ps.GroupID = ui.GroupOperate
	health := healthCmd(opts)
	health.GroupID = ui.GroupOperate
	maintenance := maintenanceCmd(opts)
	maintenance.GroupID = ui.GroupOperate
	logs := logsCmd(opts)
	logs.GroupID = ui.GroupOperate
	restart := restartCmd(opts)
	restart.GroupID = ui.GroupOperate
	execSvc := execServiceCmd(opts)
	execSvc.GroupID = ui.GroupOperate
	inspect := inspectCmd(opts)
	inspect.GroupID = ui.GroupOperate
	support := supportCmd(opts)
	support.GroupID = ui.GroupOperate
	events := eventsCmd(opts)
	events.GroupID = ui.GroupOperate
	releases := releasesCmd(opts)
	releases.GroupID = ui.GroupOperate
	lock := lockCmd(opts)
	lock.GroupID = ui.GroupOperate
	unlock := unlockCmd(opts)
	unlock.GroupID = ui.GroupOperate
	prune := pruneCmd(opts)
	prune.GroupID = ui.GroupOperate
	recover := recoverCmd(opts)
	recover.GroupID = ui.GroupRecovery
	rollback := rollbackCmd(opts)
	rollback.GroupID = ui.GroupRecovery
	accessory := accessoryCmd(opts)
	accessory.GroupID = ui.GroupAccessories
	secrets := secretsCmd(opts)
	secrets.GroupID = ui.GroupSecrets

	root.AddCommand(
		init, doctor,
		cfg, hosts, plan, scale,
		provision, migrate, agent, version, release,
		deploy, promote,
		status, ps, health, logs, execSvc, restart, inspect, support, events, releases, lock, unlock, maintenance, prune,
		accessory,
		secrets,
		recover, rollback,
	)

	if err := root.Execute(); err != nil {
		ui.PrintError(os.Stderr, err)
		return err
	}
	return nil
}

func configCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "config ENV",
		Short: "Show the resolved Ship config for an environment",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			value, err := resolvedConfigValue(cfg, args[0])
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), value)
			}
			doc, err := yaml.Marshal(value)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(doc)
			return err
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print resolved config as JSON")
	return cmd
}

func resolvedConfigValue(cfg *config.Config, envName string) (map[string]any, error) {
	resolved, _, err := cfg.ResolveEnvironment(envName)
	if err != nil {
		return nil, err
	}
	value, ok := compactConfigValue(reflect.ValueOf(resolved), false)
	if !ok {
		return map[string]any{}, nil
	}
	out, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("resolved config rendered as %T", value)
	}
	return out, nil
}

func compactConfigValue(value reflect.Value, keepZero bool) (any, bool) {
	if !value.IsValid() {
		return nil, false
	}
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil, false
		}
		value = value.Elem()
		keepZero = true
	}
	switch value.Kind() {
	case reflect.Struct:
		out := map[string]any{}
		t := value.Type()
		for i := 0; i < value.NumField(); i++ {
			field := t.Field(i)
			if field.PkgPath != "" {
				continue
			}
			name := yamlFieldName(field)
			if name == "" {
				continue
			}
			if child, ok := compactConfigValue(value.Field(i), false); ok {
				out[name] = child
			}
		}
		if len(out) == 0 && !keepZero {
			return nil, false
		}
		return out, true
	case reflect.Map:
		if value.Len() == 0 && !keepZero {
			return nil, false
		}
		out := map[string]any{}
		keys := value.MapKeys()
		sort.Slice(keys, func(i, j int) bool {
			return fmt.Sprint(keys[i].Interface()) < fmt.Sprint(keys[j].Interface())
		})
		for _, key := range keys {
			if child, ok := compactConfigValue(value.MapIndex(key), false); ok {
				out[fmt.Sprint(key.Interface())] = child
			}
		}
		if len(out) == 0 && !keepZero {
			return nil, false
		}
		return out, true
	case reflect.Slice, reflect.Array:
		if value.Len() == 0 && !keepZero {
			return nil, false
		}
		out := make([]any, 0, value.Len())
		for i := 0; i < value.Len(); i++ {
			if child, ok := compactConfigValue(value.Index(i), false); ok {
				out = append(out, child)
			}
		}
		if len(out) == 0 && !keepZero {
			return nil, false
		}
		return out, true
	case reflect.String:
		if value.String() == "" && !keepZero {
			return nil, false
		}
		return value.String(), true
	case reflect.Bool:
		if !value.Bool() && !keepZero {
			return nil, false
		}
		return value.Bool(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if value.Int() == 0 && !keepZero {
			return nil, false
		}
		return value.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if value.Uint() == 0 && !keepZero {
			return nil, false
		}
		return value.Uint(), true
	case reflect.Float32, reflect.Float64:
		if value.Float() == 0 && !keepZero {
			return nil, false
		}
		return value.Float(), true
	default:
		if value.IsZero() && !keepZero {
			return nil, false
		}
		return value.Interface(), true
	}
}

func yamlFieldName(field reflect.StructField) string {
	tag := field.Tag.Get("yaml")
	if tag == "-" {
		return ""
	}
	if tag != "" {
		name, _, _ := strings.Cut(tag, ",")
		return name
	}
	return strings.ToLower(field.Name)
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
			if err := os.MkdirAll(filepath.Join(config.LocalStateDir, "secrets"), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(config.LocalStateDir, "secrets.example"), []byte("DATABASE_URL=\n"), 0o644); err != nil {
				return err
			}
			if err := ensureShipGitignore(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created %s and %s/\n", opts.configPath, config.LocalStateDir)
			return nil
		},
	}
}

func ensureShipGitignore() error {
	const block = `
.ship/*
!.ship/secrets/
!.ship/secrets/*.age
!.ship/secrets/*.recipients
.ship/secrets/*.env
.ship/secrets/*.identity
.ship/secrets/*key*
`
	data, err := os.ReadFile(".gitignore")
	if errors.Is(err, os.ErrNotExist) {
		return os.WriteFile(".gitignore", []byte(strings.TrimPrefix(block, "\n")), 0o644)
	}
	if err != nil {
		return err
	}
	if strings.Contains(string(data), "!.ship/secrets/*.age") {
		return nil
	}
	f, err := os.OpenFile(".gitignore", os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if len(data) > 0 && data[len(data)-1] != '\n' {
		if _, err := f.WriteString("\n"); err != nil {
			return err
		}
	}
	_, err = f.WriteString(block)
	return err
}

func doctorCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "doctor [ENV]",
		Short: "Validate local tools, config, and credentials",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			var report doctor.Report
			envName := ""
			if len(args) > 0 {
				envName = args[0]
			}
			if err != nil {
				report = doctor.ConfigLoadError(err)
			} else {
				report = doctor.Run(cmd.Context(), cfg, doctor.Options{ConfigPath: opts.configPath, Environment: envName})
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
	var planJSON bool
	plan := &cobra.Command{
		Use:   "plan ENV",
		Short: "Print the provisioning plan",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			plan, err := planner.ProvisionPlan(cfg, args[0])
			if err != nil {
				return err
			}
			if planJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(plan)
			}
			fmt.Fprint(cmd.OutOrStdout(), plan.String())
			return nil
		},
	}
	plan.Flags().BoolVar(&planJSON, "json", false, "print the provisioning plan as JSON")
	cmd.AddCommand(plan)
	var yes bool
	apply := &cobra.Command{
		Use:   "apply ENV",
		Short: "Create servers and bootstrap Ship",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			resolved, env, err := cfg.ResolveEnvironment(args[0])
			if err != nil {
				return err
			}
			cfg = resolved
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
				shipBinary, err := resolveShipBinaryForHost(ctx, host, opts)
				if err != nil {
					recordEvent(store, state.Event{Environment: args[0], Kind: "provision", Status: "failed", Host: host.Name, Message: err.Error()})
					return fmt.Errorf("resolve ship binary for %s: %w", host.Name, err)
				}
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
	addAgentBinaryOverrideFlags(apply, opts)
	cmd.AddCommand(apply)
	var decommissionYes bool
	decommission := &cobra.Command{
		Use:   "decommission ENV",
		Short: "Delete Ship-managed servers for an environment",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			resolved, env, err := cfg.ResolveEnvironment(args[0])
			if err != nil {
				return err
			}
			cfg = resolved
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
	hostsByLogical := map[string]provider.Host{}
	for _, host := range result.Existing {
		hostsByLogical[provider.LogicalName(host)] = host
	}
	for _, host := range result.Created {
		hostsByLogical[provider.LogicalName(host)] = host
	}

	facts := make([]state.HostFact, 0, len(result.Desired))
	for _, plan := range result.Desired {
		fact := state.HostFact{
			Name:     plan.Name,
			Pool:     plan.Pool,
			User:     plan.User,
			Provider: providerName,
		}
		if host, ok := hostsByLogical[plan.Name]; ok {
			applyProviderHostToFact(&fact, host)
		}
		facts = append(facts, fact)
	}
	return facts
}

func applyProviderHostToFact(fact *state.HostFact, host provider.Host) {
	fact.ProviderID = host.ID
	if id, err := strconv.ParseInt(host.ID, 10, 64); err == nil {
		fact.ServerID = id
	}
	if host.Name != fact.Name {
		fact.ProviderName = host.Name
	} else {
		fact.ProviderName = ""
	}
	fact.SSHPort = host.SSHPort
	fact.IdentityFile = host.IdentityFile
	fact.KnownHostsFile = host.KnownHostsFile
	fact.JumpHost = host.JumpHost
	fact.SSHOptions = copyStringMap(host.SSHOptions)
	fact.PublicAddress = host.PublicAddress
	fact.IPv4 = ""
	if ip := net.ParseIP(host.PublicAddress); ip != nil && ip.To4() != nil {
		fact.IPv4 = host.PublicAddress
	}
}

type statusView struct {
	Environment    string                            `json:"environment"`
	CurrentRelease *state.Release                    `json:"current_release,omitempty"`
	CurrentConfig  string                            `json:"current_config_hash,omitempty"`
	DeployedConfig string                            `json:"deployed_config_hash,omitempty"`
	ConfigDrift    bool                              `json:"config_drift,omitempty"`
	Warnings       []string                          `json:"warnings,omitempty"`
	Desired        []deployment.DesiredReplicaStatus `json:"desired"`
	Observed       []deployment.ContainerStatus      `json:"observed"`
	ExtraObserved  []deployment.ContainerStatus      `json:"extra_observed,omitempty"`
	Summary        deployment.StatusSummary          `json:"summary"`
}

type inspectView struct {
	Environment    string                            `json:"environment"`
	CurrentRelease *state.Release                    `json:"current_release,omitempty"`
	CurrentConfig  string                            `json:"current_config_hash,omitempty"`
	DeployedConfig string                            `json:"deployed_config_hash,omitempty"`
	ConfigDrift    bool                              `json:"config_drift,omitempty"`
	Desired        []deployment.DesiredReplicaStatus `json:"desired"`
	Observed       []deployment.ContainerStatus      `json:"observed"`
	ExtraObserved  []deployment.ContainerStatus      `json:"extra_observed,omitempty"`
	Accessories    []state.AccessoryState            `json:"accessories,omitempty"`
	Events         []state.Event                     `json:"events,omitempty"`
	Summary        deployment.StatusSummary          `json:"summary"`
}

type logsView struct {
	Environment string      `json:"environment"`
	Service     string      `json:"service,omitempty"`
	Accessory   string      `json:"accessory,omitempty"`
	Release     string      `json:"release,omitempty"`
	Lines       int         `json:"lines"`
	Replica     int         `json:"replica,omitempty"`
	Follow      bool        `json:"follow"`
	Entries     []logsEntry `json:"entries"`
}

type logsEntry struct {
	Iteration int    `json:"iteration"`
	Host      string `json:"host"`
	Service   string `json:"service,omitempty"`
	Accessory string `json:"accessory,omitempty"`
	Replica   int    `json:"replica,omitempty"`
	Release   string `json:"release,omitempty"`
	Container string `json:"container"`
	Logs      string `json:"logs"`
}

type execView struct {
	Environment string      `json:"environment"`
	Service     string      `json:"service,omitempty"`
	Accessory   string      `json:"accessory,omitempty"`
	Command     string      `json:"command"`
	All         bool        `json:"all,omitempty"`
	Replica     int         `json:"replica,omitempty"`
	Entries     []execEntry `json:"entries"`
}

type execEntry struct {
	Host      string `json:"host"`
	Service   string `json:"service,omitempty"`
	Accessory string `json:"accessory,omitempty"`
	Replica   int    `json:"replica,omitempty"`
	Container string `json:"container"`
	Output    string `json:"output,omitempty"`
}

type accessoryEnsureMode string

const (
	accessoryEnsureOnly  accessoryEnsureMode = "ensure"
	accessoryForceDeploy accessoryEnsureMode = "force"
)

type accessoryEnsureResult struct {
	Name    string
	Host    scheduler.Host
	Changed bool
}

type healthView struct {
	Environment string        `json:"environment"`
	Current     string        `json:"current_release,omitempty"`
	OK          bool          `json:"ok"`
	Checks      []healthEntry `json:"checks"`
}

type healthEntry struct {
	Host       string `json:"host"`
	Service    string `json:"service"`
	Replica    int    `json:"replica"`
	Container  string `json:"container"`
	Status     string `json:"status"`
	Checked    bool   `json:"checked"`
	OK         bool   `json:"ok"`
	URL        string `json:"url,omitempty"`
	Command    string `json:"command,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
	Output     string `json:"output,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Error      string `json:"error,omitempty"`
}

type maintenanceView struct {
	Environment string    `json:"environment"`
	Enabled     bool      `json:"enabled"`
	Message     string    `json:"message,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
	Hosts       []string  `json:"hosts,omitempty"`
}

const logsFollowPolls = 3

var logsFollowInterval = 100 * time.Millisecond

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

type hostsView struct {
	Environment string      `json:"environment"`
	Source      string      `json:"source"`
	Hosts       []hostEntry `json:"hosts"`
}

type hostEntry struct {
	Name           string            `json:"name"`
	Pool           string            `json:"pool"`
	User           string            `json:"user"`
	Contact        string            `json:"contact"`
	SSHPort        int               `json:"ssh_port,omitempty"`
	IdentityFile   string            `json:"identity_file,omitempty"`
	KnownHostsFile string            `json:"known_hosts_file,omitempty"`
	JumpHost       string            `json:"jump_host,omitempty"`
	SSHOptions     map[string]string `json:"ssh_options,omitempty"`
}

func hostsCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "hosts ENV",
		Short: "Show resolved host inventory for an environment",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			_, env, err := cfg.ResolveEnvironment(args[0])
			if err != nil {
				return err
			}
			stateDir, err := localStateDirForConfig(opts.configPath)
			if err != nil {
				return err
			}
			view, err := buildHostsView(state.NewStore(stateDir), args[0], env)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), view)
			}
			renderHostsText(cmd.OutOrStdout(), view)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print hosts as JSON")
	return cmd
}

func buildHostsView(store state.Store, envName string, env config.Environment) (hostsView, error) {
	source := "config"
	if _, err := store.ReadHostFacts(envName); err == nil {
		source = "state"
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return hostsView{}, err
	}
	hosts, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return hostsView{}, err
	}
	view := hostsView{Environment: envName, Source: source}
	for _, host := range hosts {
		view.Hosts = append(view.Hosts, hostEntry{
			Name:           host.Name,
			Pool:           host.Pool,
			User:           host.User,
			Contact:        host.ContactTarget(),
			SSHPort:        host.SSHPort,
			IdentityFile:   host.IdentityFile,
			KnownHostsFile: host.KnownHostsFile,
			JumpHost:       host.JumpHost,
			SSHOptions:     copyStringMap(host.SSHOptions),
		})
	}
	return view, nil
}

func renderHostsText(w io.Writer, view hostsView) {
	ui.PrintHeader(w, view.Environment, ui.HeaderField{Label: "source", Value: view.Source, Accent: true})
	if len(view.Hosts) == 0 {
		ui.PrintNotice(w, "no hosts")
		return
	}
	table := ui.NewTable(w)
	table.SetHeaders("NAME", "POOL", "USER", "CONTACT", "SSH", "NOTES")
	for _, host := range view.Hosts {
		ssh := "-"
		if host.SSHPort > 0 {
			ssh = fmt.Sprintf(":%d", host.SSHPort)
		}
		var notes []string
		if host.IdentityFile != "" {
			notes = append(notes, "identity="+host.IdentityFile)
		}
		if host.KnownHostsFile != "" {
			notes = append(notes, "known_hosts="+host.KnownHostsFile)
		}
		if host.JumpHost != "" {
			notes = append(notes, "jump="+host.JumpHost)
		}
		if len(host.SSHOptions) > 0 {
			notes = append(notes, "options="+formatStringMap(host.SSHOptions))
		}
		table.AddRow(host.Name, host.Pool, host.User, host.Contact, ssh, ui.Dash(strings.Join(notes, " ")))
	}
	ui.RenderTable(w, table)
}

func formatStringMap(values map[string]string) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+values[key])
	}
	return strings.Join(parts, ",")
}

type versionView struct {
	ShipVersion      string             `json:"ship_version"`
	MinAgentProtocol int                `json:"min_agent_protocol"`
	MaxAgentProtocol int                `json:"max_agent_protocol"`
	Environment      string             `json:"environment,omitempty"`
	Hosts            []versionHostEntry `json:"hosts,omitempty"`
}

type versionHostEntry struct {
	Name             string   `json:"name"`
	Pool             string   `json:"pool"`
	Contact          string   `json:"contact"`
	Hostname         string   `json:"hostname,omitempty"`
	DockerOK         bool     `json:"docker_ok,omitempty"`
	StateDir         string   `json:"state_dir,omitempty"`
	AgentVersion     string   `json:"agent_version,omitempty"`
	AgentProtocol    int      `json:"agent_protocol,omitempty"`
	SupportedMethods []string `json:"supported_methods,omitempty"`
	Error            string   `json:"error,omitempty"`
}

func versionCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "version [ENV]",
		Short: "Show local Ship and remote agent versions",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			view := localVersionView()
			if len(args) == 1 {
				_, env, store, err := environmentContext(opts, args[0])
				if err != nil {
					return err
				}
				view, err = buildVersionView(cmd.Context(), store, args[0], env)
				if err != nil {
					return err
				}
			}
			if jsonOutput {
				if err := writeJSON(cmd.OutOrStdout(), view); err != nil {
					return err
				}
			} else {
				renderVersionText(cmd.OutOrStdout(), view)
			}
			if failed := countVersionFailures(view); failed > 0 {
				return fmt.Errorf("version check failed on %d/%d hosts", failed, len(view.Hosts))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print version information as JSON")
	return cmd
}

func localVersionView() versionView {
	return versionView{
		ShipVersion:      agent.Version(),
		MinAgentProtocol: agent.AgentMinProtocol,
		MaxAgentProtocol: agent.AgentProtocol,
	}
}

func buildVersionView(ctx context.Context, store state.Store, envName string, env config.Environment) (versionView, error) {
	hosts, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return versionView{}, err
	}
	view := localVersionView()
	view.Environment = envName
	for _, host := range hosts {
		entry := versionHostEntry{
			Name:    host.Name,
			Pool:    host.Pool,
			Contact: host.ContactTarget(),
		}
		var status agent.Status
		if err := newDeployAgent(host).Call(ctx, "status", map[string]any{}, &status); err != nil {
			entry.Error = err.Error()
			view.Hosts = append(view.Hosts, entry)
			continue
		}
		entry.Hostname = status.Hostname
		entry.DockerOK = status.DockerOK
		entry.StateDir = status.StateDir
		entry.AgentVersion = status.AgentVersion
		entry.AgentProtocol = status.ProtocolVersion
		entry.SupportedMethods = append([]string(nil), status.SupportedMethods...)
		view.Hosts = append(view.Hosts, entry)
	}
	return view, nil
}

func renderVersionText(w io.Writer, view versionView) {
	style := ui.NewStyle(w)
	fmt.Fprint(w, style.Teal("ship "))
	fmt.Fprint(w, style.White(view.ShipVersion))
	fmt.Fprint(w, style.Gray("  protocol "))
	fmt.Fprintln(w, style.White(fmt.Sprintf("%d-%d", view.MinAgentProtocol, view.MaxAgentProtocol)))
	if view.Environment == "" {
		return
	}
	ui.PrintHeader(w, view.Environment)
	if len(view.Hosts) == 0 {
		ui.PrintNotice(w, "no hosts")
		return
	}
	table := ui.NewTable(w)
	table.SetHeaders("HOST", "POOL", "CONTACT", "AGENT", "DOCKER", "STATE", "DETAIL")
	for _, host := range view.Hosts {
		if host.Error != "" {
			table.AddRow(host.Name, host.Pool, host.Contact, "-", "-", "-", host.Error)
			continue
		}
		agentVersion := host.AgentVersion
		if agentVersion == "" {
			agentVersion = "unknown"
		}
		if host.AgentProtocol > 0 {
			agentVersion = fmt.Sprintf("%s (%d)", agentVersion, host.AgentProtocol)
		}
		detail := ui.Dash(host.Hostname)
		if len(host.SupportedMethods) > 0 {
			detail = strings.Join(host.SupportedMethods, ",")
		}
		table.AddRow(host.Name, host.Pool, host.Contact, agentVersion, fmt.Sprintf("%t", host.DockerOK), ui.Dash(host.StateDir), detail)
	}
	ui.RenderTable(w, table)
}

func countVersionFailures(view versionView) int {
	var failed int
	for _, host := range view.Hosts {
		if host.Error != "" {
			failed++
		}
	}
	return failed
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
	_ = store.RecordEvent(event)
}

func runDeployHooks(ctx context.Context, w io.Writer, store state.Store, cfg *config.Config, envName, hookName, releaseID, failure, configPath string) error {
	hooks := deployHooksFor(cfg.Hooks, hookName)
	if len(hooks) == 0 {
		return nil
	}
	configDir := "."
	if strings.TrimSpace(configPath) != "" {
		configDir = filepath.Dir(configPath)
		if abs, err := filepath.Abs(configDir); err == nil {
			configDir = abs
		}
	}
	hctx := hookContext{
		Project:     cfg.Project,
		Environment: envName,
		Hook:        hookName,
		ReleaseID:   releaseID,
		ConfigPath:  configPath,
		ConfigDir:   configDir,
		Failure:     failure,
	}
	for i, hook := range hooks {
		message := fmt.Sprintf("hook=%s index=%d command=%q", hookName, i, hook.Command)
		recordEvent(store, state.Event{Environment: envName, Kind: "deploy_hook", Status: "started", Release: releaseID, Message: message})
		if err := runLocalHookCommand(ctx, hook, hctx, w); err != nil {
			recordEvent(store, state.Event{Environment: envName, Kind: "deploy_hook", Status: "failed", Release: releaseID, Message: message + ": " + err.Error()})
			return err
		}
		recordEvent(store, state.Event{Environment: envName, Kind: "deploy_hook", Status: "succeeded", Release: releaseID, Message: message})
	}
	return nil
}

func runNotifications(ctx context.Context, store state.Store, cfg *config.Config, envName, operation, status, releaseID, message string, images map[string]string) {
	webhooks := cfg.Notifications.Webhooks
	if len(webhooks) == 0 {
		return
	}
	payload := notificationPayload{
		Project:     cfg.Project,
		Environment: envName,
		Operation:   operation,
		Status:      status,
		Release:     releaseID,
		Message:     message,
		Images:      copyStringMap(images),
		Time:        deployNow().UTC(),
	}
	eventName := operation + ":" + status
	for i, webhook := range webhooks {
		if !webhookWantsEvent(webhook, eventName) {
			continue
		}
		label := fmt.Sprintf("operation=%s status=%s index=%d", operation, status, i)
		if err := sendWebhookNotification(ctx, webhook, payload); err != nil {
			recordEvent(store, state.Event{Environment: envName, Kind: "notification", Status: "failed", Release: releaseID, Message: label + ": " + err.Error()})
			continue
		}
		recordEvent(store, state.Event{Environment: envName, Kind: "notification", Status: "succeeded", Release: releaseID, Message: label})
	}
}

func webhookWantsEvent(webhook config.WebhookNotification, eventName string) bool {
	if len(webhook.Events) == 0 {
		return true
	}
	for _, raw := range webhook.Events {
		event := strings.TrimSpace(raw)
		if event == "*" || event == eventName {
			return true
		}
		if strings.HasSuffix(event, ":*") && strings.HasPrefix(eventName, strings.TrimSuffix(event, "*")) {
			return true
		}
	}
	return false
}

func defaultSendWebhookNotification(ctx context.Context, webhook config.WebhookNotification, payload notificationPayload) error {
	url := strings.TrimSpace(webhook.URL)
	if url == "" {
		envName := strings.TrimSpace(webhook.URLEnv)
		if envName == "" {
			return fmt.Errorf("webhook url or url_env is required")
		}
		value, ok := os.LookupEnv(envName)
		if !ok || strings.TrimSpace(value) == "" {
			return fmt.Errorf("webhook url_env %s is not set", envName)
		}
		url = strings.TrimSpace(value)
	}
	if webhook.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(webhook.TimeoutSeconds)*time.Second)
		defer cancel()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range webhook.Headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func deployHooksFor(hooks config.Hooks, hookName string) []config.HookCommand {
	switch hookName {
	case "pre_deploy":
		return hooks.PreDeploy
	case "pre_build":
		return hooks.PreBuild
	case "post_deploy":
		return hooks.PostDeploy
	case "deploy_failed":
		return hooks.DeployFailed
	default:
		return nil
	}
}

func defaultRunLocalHookCommand(ctx context.Context, hook config.HookCommand, hctx hookContext, w io.Writer) error {
	if hook.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(hook.TimeoutSeconds)*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "sh", "-lc", hook.Command)
	cmd.Dir = hctx.ConfigDir
	cmd.Env = hookEnv(os.Environ(), hook, hctx)
	output, err := cmd.CombinedOutput()
	if len(output) > 0 {
		_, _ = w.Write(output)
	}
	if err != nil {
		if len(output) > 0 {
			return fmt.Errorf("hook %s command %q failed: %w: %s", hctx.Hook, hook.Command, err, strings.TrimSpace(string(output)))
		}
		return fmt.Errorf("hook %s command %q failed: %w", hctx.Hook, hook.Command, err)
	}
	return nil
}

func hookEnv(base []string, hook config.HookCommand, hctx hookContext) []string {
	env := append([]string{}, base...)
	shipValues := map[string]string{
		"SHIP_PROJECT":     hctx.Project,
		"SHIP_ENVIRONMENT": hctx.Environment,
		"SHIP_HOOK":        hctx.Hook,
		"SHIP_RELEASE":     hctx.ReleaseID,
		"SHIP_CONFIG":      hctx.ConfigPath,
		"SHIP_CONFIG_DIR":  hctx.ConfigDir,
		"SHIP_FAILURE":     hctx.Failure,
	}
	for _, key := range sortedMapKeys(shipValues) {
		env = append(env, key+"="+shipValues[key])
	}
	for _, key := range sortedMapKeys(hook.Env) {
		env = append(env, key+"="+hook.Env[key])
	}
	return env
}

func configHash(cfg *config.Config) string {
	data, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum[:])
}

func buildStatusView(ctx context.Context, cfg *config.Config, env config.Environment, envName string, store state.Store) (statusView, deployment.StatusReport, error) {
	releases, err := store.Releases(envName)
	if err != nil {
		return statusView{}, deployment.StatusReport{}, err
	}
	var currentRelease *state.Release
	desiredReleaseID := ""
	deployedConfigHash := ""
	if current, err := store.CurrentRelease(envName); err == nil {
		currentRelease = &current
		desiredReleaseID = current.ID
		deployedConfigHash = current.ConfigHash
	}

	hosts, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return statusView{}, deployment.StatusReport{}, err
	}
	observed, err := deployment.InspectObservedOnHosts(ctx, hosts, deploymentAgentFactory())
	if err != nil {
		return statusView{}, deployment.StatusReport{}, err
	}
	var warnings []string
	if shouldUseRemoteReleaseState(desiredReleaseID, cfg, envName, observed) {
		if remote, remoteWarnings, err := remoteCurrentRelease(ctx, hosts, envName); err != nil {
			warnings = append(warnings, err.Error())
		} else {
			warnings = append(warnings, remoteWarnings...)
			if remote != nil {
				currentRelease = remote
				desiredReleaseID = remote.ID
				deployedConfigHash = remote.ConfigHash
			}
		}
	}
	if currentRelease == nil && len(releases) > 0 {
		latest := releases[len(releases)-1]
		currentRelease = &latest
		deployedConfigHash = latest.ConfigHash
	}
	currentConfigHash := configHash(cfg)
	configDrift := deployedConfigHash != "" && currentConfigHash != "" && deployedConfigHash != currentConfigHash
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
		CurrentConfig:  currentConfigHash,
		DeployedConfig: deployedConfigHash,
		ConfigDrift:    configDrift,
		Warnings:       warnings,
		Desired:        report.Desired,
		Observed:       report.Observed,
		ExtraObserved:  report.ExtraObserved,
		Summary:        report.Summary,
	}
	return view, report, nil
}

func shouldUseRemoteReleaseState(localRelease string, cfg *config.Config, envName string, observed []deployment.ObservedContainer) bool {
	if strings.TrimSpace(localRelease) == "" {
		return true
	}
	running := observedRunningServiceReleases(cfg, envName, observed)
	if len(running) == 0 {
		return false
	}
	_, ok := running[localRelease]
	return !ok
}

func observedRunningServiceReleases(cfg *config.Config, envName string, observed []deployment.ObservedContainer) map[string]struct{} {
	releases := map[string]struct{}{}
	for _, item := range observed {
		labels := item.Container.Labels
		if labels[docker.LabelManagedBy] != docker.LabelManagedByValue ||
			labels[docker.LabelProject] != statusLabelValue(cfg.Project) ||
			labels[docker.LabelEnvironment] != statusLabelValue(envName) ||
			strings.TrimSpace(labels[docker.LabelService]) == "" ||
			!strings.HasPrefix(item.Container.Status, "Up ") {
			continue
		}
		if release := strings.TrimSpace(labels[docker.LabelRelease]); release != "" {
			releases[release] = struct{}{}
		}
	}
	return releases
}

func statusLabelValue(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		allowed := r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' ||
			r == '_' || r == '.' || r == '-'
		if allowed {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), ".-_")
	if out == "" {
		return "unknown"
	}
	return out
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

func renderStatusText(w io.Writer, view statusView) {
	var fields []ui.HeaderField
	if view.CurrentRelease == nil {
		fields = append(fields, ui.HeaderField{Label: "release", Value: "none"})
	} else {
		release := fmt.Sprintf("%s (%s)", view.CurrentRelease.ID, view.CurrentRelease.Status)
		if view.CurrentRelease.Healthy {
			release += ", healthy"
		} else {
			release += ", unhealthy"
		}
		fields = append(fields, ui.HeaderField{Label: "release", Value: release, Accent: true})
	}
	ui.PrintHeader(w, view.Environment, fields...)
	if view.ConfigDrift {
		ui.PrintWarn(w, fmt.Sprintf("config drift  current=%s  deployed=%s", view.CurrentConfig, view.DeployedConfig))
	}
	for _, warning := range view.Warnings {
		ui.PrintWarn(w, warning)
	}
	if len(view.Desired) == 0 {
		ui.PrintNotice(w, "no placements")
	} else {
		table := ui.NewTable(w)
		table.SetHeaders("SERVICE", "HOST", "RELEASE", "STATE", "CONTAINER", "STATUS", "DRIFT")
		for _, desired := range view.Desired {
			container := "-"
			status := "missing"
			if len(desired.Observed) > 0 {
				obs := desired.Observed[0]
				container = obs.Name
				status = ui.Dash(obs.Status)
			}
			table.AddRow(
				fmt.Sprintf("%s.%d", desired.Service, desired.Replica),
				desired.Host,
				ui.Dash(desired.DesiredRelease),
				desired.State,
				container,
				status,
				ui.Dash(strings.Join(desired.Drift, "; ")),
			)
		}
		ui.RenderTable(w, table)
	}
	if len(view.ExtraObserved) > 0 {
		ui.PrintSection(w, "Extra containers")
		table := ui.NewTable(w)
		table.SetHeaders("HOST", "NAME", "KIND", "SERVICE", "RELEASE", "STATUS")
		for _, observed := range view.ExtraObserved {
			service := ""
			if observed.Service != "" {
				service = fmt.Sprintf("%s.%d", observed.Service, observed.Replica)
			} else if observed.Accessory != "" {
				service = observed.Accessory
			}
			table.AddRow(
				observed.Host,
				observed.Name,
				observed.Kind,
				ui.Dash(service),
				ui.Dash(observed.Release),
				ui.Dash(observed.Status),
			)
		}
		ui.RenderTable(w, table)
	}
	if view.Summary.Drift {
		ui.PrintWarn(w, fmt.Sprintf("drift detected  missing=%d  wrong_release=%d  wrong_host=%d  extra=%d",
			view.Summary.Missing, view.Summary.WrongRelease, view.Summary.WrongHost, view.Summary.Extra))
		return
	}
	ui.PrintOK(w, "status ok")
}

func renderEventsText(w io.Writer, events []state.Event) {
	if len(events) == 0 {
		ui.PrintNotice(w, "no events")
		return
	}
	table := ui.NewTable(w)
	table.SetHeaders("TIME", "KIND", "STATUS", "RELEASE", "HOST", "DETAIL")
	for _, event := range events {
		detail := event.Message
		if event.Service != "" {
			detail = strings.TrimSpace("service=" + event.Service + " " + detail)
		}
		if event.Accessory != "" {
			detail = strings.TrimSpace("accessory=" + event.Accessory + " " + detail)
		}
		table.AddRow(
			event.Time.Format(time.RFC3339),
			event.Kind,
			event.Status,
			ui.Dash(event.Release),
			ui.Dash(event.Host),
			ui.Dash(detail),
		)
	}
	table.Render(w)
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
		Short: "Report agent install status (RPC is served on demand, not by a daemon)",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "ship agent is installed; RPC is served on demand through `ship agent rpc` over SSH, so no daemon runs")
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "install ENV",
		Short: "Print or run host bootstrap commands for every host in an environment",
		Args:  ui.ExactArgs(ui.Env),
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
				out, err := sshForHost(host, opts.dryRun).Run(cmd.Context(), command)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", host.Name, strings.TrimSpace(out))
			}
			return nil
		},
	})
	var upgradeJSON bool
	upgrade := &cobra.Command{
		Use:   "upgrade ENV",
		Short: "Upload the current Ship binary to every host agent",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			view, err := upgradeAgents(cmd.Context(), opts, args[0])
			if upgradeJSON {
				if writeErr := writeJSON(cmd.OutOrStdout(), view); writeErr != nil {
					return writeErr
				}
			} else {
				renderAgentUpgradeText(cmd.OutOrStdout(), view)
			}
			if err != nil {
				return err
			}
			if failed := countAgentUpgradeFailures(view); failed > 0 {
				return fmt.Errorf("agent upgrade failed on %d/%d hosts", failed, len(view.Hosts))
			}
			return nil
		},
	}
	upgrade.Flags().BoolVar(&upgradeJSON, "json", false, "print upgrade results as JSON")
	addAgentBinaryOverrideFlags(upgrade, opts)
	cmd.AddCommand(upgrade)
	cmd.AddCommand(&cobra.Command{
		Use:   "status ENV",
		Short: "Ask every host agent for status",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, env, store, err := environmentContext(opts, args[0])
			if err != nil {
				return err
			}
			hosts, err := resolvedHostsForEnvironment(store, args[0], env)
			if err != nil {
				return err
			}
			renderAgentStatusText(cmd.OutOrStdout(), args[0], hosts, func(host scheduler.Host) (agent.Status, error) {
				var status agent.Status
				client := agent.Client{SSH: sshForHost(host, opts.dryRun)}
				if err := client.Call(cmd.Context(), "status", map[string]any{}, &status); err != nil {
					return agent.Status{}, err
				}
				return status, nil
			})
			return nil
		},
	})
	return cmd
}

func releaseCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "release", Short: "Inspect Ship's own release assets"}
	var jsonOutput bool
	check := &cobra.Command{
		Use:   "check VERSION",
		Short: "Verify a Ship release published binaries for every supported platform",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			statuses, allOK, err := shipbinary.CheckRelease(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if jsonOutput {
				if err := writeJSON(cmd.OutOrStdout(), statuses); err != nil {
					return err
				}
			} else {
				table := ui.NewTable(cmd.OutOrStdout())
				table.SetHeaders("ASSET", "STATUS", "DETAIL")
				for _, status := range statuses {
					result := "ok"
					if !status.OK {
						result = "missing"
					}
					table.AddRow(status.Name, result, ui.Dash(status.Detail))
				}
				ui.RenderTable(cmd.OutOrStdout(), table)
			}
			if !allOK {
				return fmt.Errorf("release v%s is missing assets; do not pin it for agent installs", strings.TrimPrefix(strings.TrimSpace(args[0]), "v"))
			}
			return nil
		},
	}
	check.Flags().BoolVar(&jsonOutput, "json", false, "print asset statuses as JSON")
	cmd.AddCommand(check)
	return cmd
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
func preflightAgentProtocols(ctx context.Context, w io.Writer, opts *options, envName string, hosts []scheduler.Host, autoUpgrade bool) error {
	var incompatible []string
	for _, host := range hosts {
		var result agent.NegotiateResult
		err := newDeployAgent(host).Call(ctx, "negotiate", agent.NegotiateParams{
			ClientVersion:      agent.Version(),
			MinProtocolVersion: agent.AgentMinProtocol,
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
		return nil
	}
	return fmt.Errorf("incompatible agents:\n  %s\n\nFix: ship agent upgrade %s", strings.Join(incompatible, "\n  "), envName)
}

func backupShipBinaryCommand() string {
	path := config.RemoteBinaryPath
	return fmt.Sprintf("set -eu\nif [ -f %s ]; then cp -p %s %s.bak; fi", path, path, path)
}

func restoreShipBinaryCommand() string {
	path := config.RemoteBinaryPath
	return fmt.Sprintf("set -eu\ntest -f %s.bak\ncp -p %s.bak %s", path, path, path)
}

func renderAgentStatusText(w io.Writer, envName string, hosts []scheduler.Host, fetch func(scheduler.Host) (agent.Status, error)) {
	ui.PrintHeader(w, envName)
	if len(hosts) == 0 {
		ui.PrintNotice(w, "no hosts")
		return
	}
	table := ui.NewTable(w)
	table.SetHeaders("HOST", "DOCKER", "STATE DIR", "AGENT", "DETAIL")
	for _, host := range hosts {
		status, err := fetch(host)
		if err != nil {
			table.AddRow(host.Name, "-", "-", "-", err.Error())
			continue
		}
		table.AddRow(host.Name, fmt.Sprintf("%t", status.DockerOK), ui.Dash(status.StateDir), ui.Dash(status.AgentVersion), ui.Dash(status.Hostname))
	}
	ui.RenderTable(w, table)
}

func renderAgentUpgradeText(w io.Writer, view agentUpgradeView) {
	fields := []ui.HeaderField{
		{Label: "version", Value: view.ShipVersion, Accent: true},
		{Label: "sha256", Value: view.SHA256},
	}
	if view.DryRun {
		fields = append(fields, ui.HeaderField{Label: "mode", Value: "dry-run"})
	}
	ui.PrintHeader(w, view.Environment, fields...)
	if len(view.Hosts) == 0 {
		ui.PrintNotice(w, "no hosts")
		return
	}
	table := ui.NewTable(w)
	table.SetHeaders("HOST", "POOL", "CONTACT", "RESULT", "PATH", "SHA256")
	for _, host := range view.Hosts {
		result := "unchanged"
		if host.Error != "" {
			result = "failed"
		} else if view.DryRun {
			result = "planned"
		} else if host.Installed {
			result = "installed"
		}
		detail := ui.Dash(host.Error)
		if host.Error == "" && view.DryRun {
			detail = "would install"
		}
		if host.Error != "" {
			table.AddRow(host.Name, host.Pool, host.Contact, result, ui.Dash(host.Path), detail)
			continue
		}
		table.AddRow(host.Name, host.Pool, host.Contact, result, ui.Dash(host.Path), ui.Dash(host.SHA256))
	}
	ui.RenderTable(w, table)
}

func countAgentUpgradeFailures(view agentUpgradeView) int {
	var failed int
	for _, host := range view.Hosts {
		if host.Error != "" {
			failed++
		}
	}
	return failed
}

func planCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	var observedOutput bool
	cmd := &cobra.Command{
		Use:   "plan ENV",
		Short: "Print the deployment plan",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			plan, err := planner.DeploymentPlan(cfg, args[0])
			if err != nil {
				return err
			}
			if observedOutput {
				view, err := buildObservedPlanView(cmd.Context(), opts, cfg, args[0], plan)
				if err != nil {
					return err
				}
				if jsonOutput {
					return writeJSON(cmd.OutOrStdout(), view)
				}
				renderObservedPlanText(cmd.OutOrStdout(), view)
				return nil
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(plan)
			}
			fmt.Fprint(cmd.OutOrStdout(), plan.String())
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print the deployment plan as JSON")
	cmd.Flags().BoolVar(&observedOutput, "observed", false, "inspect hosts and include observed drift plus rollout actions")
	return cmd
}

const plannedReleaseID = "<next-release>"

type observedPlanView struct {
	Environment    string                  `json:"environment"`
	Plan           planner.Plan            `json:"plan"`
	Observed       deployment.StatusReport `json:"observed"`
	RolloutActions []rolloutActionView     `json:"rollout_actions"`
}

type rolloutActionView struct {
	Kind      string `json:"kind"`
	Service   string `json:"service,omitempty"`
	Replica   int    `json:"replica,omitempty"`
	Host      string `json:"host,omitempty"`
	Release   string `json:"release,omitempty"`
	Container string `json:"container,omitempty"`
	Image     string `json:"image,omitempty"`
	Target    string `json:"target,omitempty"`
	Details   string `json:"details,omitempty"`
}

func buildObservedPlanView(ctx context.Context, opts *options, cfg *config.Config, envName string, plan planner.Plan) (observedPlanView, error) {
	resolved, env, err := cfg.ResolveEnvironment(envName)
	if err != nil {
		return observedPlanView{}, err
	}
	stateDir, err := localStateDirForConfig(opts.configPath)
	if err != nil {
		return observedPlanView{}, err
	}
	store := state.NewStore(stateDir)
	hosts, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return observedPlanView{}, err
	}
	observed, err := deployment.InspectObservedOnHosts(ctx, hosts, deploymentAgentFactory())
	if err != nil {
		return observedPlanView{}, err
	}
	currentRelease := ""
	if current, err := store.CurrentRelease(envName); err == nil {
		currentRelease = current.ID
	} else if !errors.Is(err, os.ErrNotExist) {
		return observedPlanView{}, err
	}
	report, err := deployment.AggregateStatus(deployment.StatusInput{
		Config:         resolved,
		Environment:    env,
		Hosts:          hosts,
		EnvName:        envName,
		CurrentRelease: currentRelease,
		Observed:       observed,
	})
	if err != nil {
		return observedPlanView{}, err
	}
	actions, err := deployment.BuildActions(deployment.PlanInput{
		Config:      resolved,
		Environment: env,
		Hosts:       hosts,
		EnvName:     envName,
		ReleaseID:   plannedReleaseID,
		Images:      plannedImages(resolved),
		Observed:    observed,
		StateDir:    stateDir,
	})
	if err != nil {
		return observedPlanView{}, err
	}
	return observedPlanView{
		Environment:    envName,
		Plan:           plan,
		Observed:       report,
		RolloutActions: rolloutActionViews(actions),
	}, nil
}

func plannedImages(cfg *config.Config) map[string]string {
	images := map[string]string{}
	serviceNames := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)
	for _, name := range serviceNames {
		svc := cfg.Services[name]
		if strings.TrimSpace(svc.Image.Ref) != "" {
			images[name] = svc.Image.Ref + "@<digest>"
			continue
		}
		tag, err := docker.ImageTag(cfg.Registry, name, plannedReleaseID)
		if err != nil {
			images[name] = cfg.Registry + ":" + name + "-" + plannedReleaseID
			continue
		}
		images[name] = tag + "@<digest>"
	}
	return images
}

func rolloutActionViews(actions []deployment.Action) []rolloutActionView {
	views := make([]rolloutActionView, 0, len(actions))
	for _, action := range actions {
		view := rolloutActionView{
			Kind:      string(action.Kind),
			Service:   action.Service,
			Replica:   action.Replica,
			Host:      action.Host.Name,
			Release:   action.Release,
			Container: action.ContainerName,
			Image:     action.Image,
		}
		switch action.Kind {
		case deployment.ActionIngress:
			view.Target = "ingress"
			view.Details = planIngressDetails(action)
		case deployment.ActionHealth:
			view.Details = planHealthDetails(action)
		case deployment.ActionDrain:
			view.Details = action.DrainTimeout.String()
		case deployment.ActionCanary:
			view.Details = action.PauseDuration.String()
		}
		views = append(views, view)
	}
	return views
}

func renderObservedPlanText(w io.Writer, view observedPlanView) {
	fmt.Fprint(w, view.Plan.String())
	ui.PrintHeader(w, view.Environment,
		ui.HeaderField{Label: "release", Value: emptyAsNone(view.Observed.CurrentRelease), Accent: true},
		ui.HeaderField{Label: "drift", Value: fmt.Sprintf("%t", view.Observed.Summary.Drift)},
	)
	summary := ui.NewTable(w)
	summary.SetHeaders("DESIRED", "OBSERVED", "EXTRA")
	summary.AddRow(
		strconv.Itoa(view.Observed.Summary.Desired),
		strconv.Itoa(view.Observed.Summary.Observed),
		strconv.Itoa(view.Observed.Summary.Extra),
	)
	ui.RenderTable(w, summary)

	var driftRows []deployment.DesiredReplicaStatus
	for _, desired := range view.Observed.Desired {
		if desired.State == "ok" {
			continue
		}
		driftRows = append(driftRows, desired)
	}
	if len(driftRows) > 0 {
		ui.PrintSection(w, "Drift")
		table := ui.NewTable(w)
		table.SetHeaders("SERVICE", "HOST", "STATE", "DETAIL")
		for _, desired := range driftRows {
			table.AddRow(
				fmt.Sprintf("%s.%d", desired.Service, desired.Replica),
				desired.Host,
				desired.State,
				ui.Dash(strings.Join(desired.Drift, "; ")),
			)
		}
		ui.RenderTable(w, table)
	}
	if len(view.Observed.ExtraObserved) > 0 {
		ui.PrintSection(w, "Extra containers")
		table := ui.NewTable(w)
		table.SetHeaders("HOST", "NAME", "SERVICE", "RELEASE", "STATUS")
		for _, observed := range view.Observed.ExtraObserved {
			service := observed.Service
			if observed.Replica > 0 {
				service = fmt.Sprintf("%s.%d", observed.Service, observed.Replica)
			}
			table.AddRow(observed.Host, observed.Name, ui.Dash(service), ui.Dash(observed.Release), ui.Dash(observed.Status))
		}
		ui.RenderTable(w, table)
	}
	ui.PrintSection(w, "Rollout actions")
	if len(view.RolloutActions) == 0 {
		ui.PrintNotice(w, "no changes")
		return
	}
	table := ui.NewTable(w)
	table.SetHeaders("KIND", "TARGET", "HOST", "IMAGE", "DETAIL")
	for _, action := range view.RolloutActions {
		target := action.Target
		if target == "" && action.Service != "" && action.Replica > 0 {
			target = fmt.Sprintf("%s.%d", action.Service, action.Replica)
		}
		if target == "" && action.Container != "" {
			target = action.Container
		}
		if target == "" {
			target = action.Kind
		}
		table.AddRow(action.Kind, target, ui.Dash(action.Host), ui.Dash(action.Image), ui.Dash(action.Details))
	}
	ui.RenderTable(w, table)
}

func planIngressDetails(action deployment.Action) string {
	hosts := make([]string, 0, len(action.IngressHosts))
	for _, host := range action.IngressHosts {
		hosts = append(hosts, host.Name)
	}
	sort.Strings(hosts)
	if strings.TrimSpace(action.IngressConfig) == "" {
		return "clear on " + strings.Join(hosts, ",")
	}
	return "reload on " + strings.Join(hosts, ",")
}

func planHealthDetails(action deployment.Action) string {
	if action.Health.URL != "" {
		return action.Health.URL
	}
	return action.Health.Command
}

func emptyAsNone(value string) string {
	if strings.TrimSpace(value) == "" {
		return "none"
	}
	return value
}

func firstNonEmptyLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func deployCmd(opts *options) *cobra.Command {
	var ignoreLock bool
	var autoUpgradeAgents bool
	cmd := &cobra.Command{
		Use:   "deploy ENV",
		Short: "Build, push, place, and roll services",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			ctx := cmd.Context()
			envName := args[0]
			var hookCfg *config.Config
			var hookStore state.Store
			var hookReleaseID string
			var hookImages map[string]string
			defer func() {
				if runErr == nil || hookCfg == nil || hookStore.Dir == "" {
					return
				}
				if err := runDeployHooks(ctx, cmd.OutOrStdout(), hookStore, hookCfg, envName, "deploy_failed", hookReleaseID, runErr.Error(), opts.configPath); err != nil {
					runErr = fmt.Errorf("%w; additionally deploy_failed hook failed: %v", runErr, err)
				}
				runNotifications(ctx, hookStore, hookCfg, envName, "deploy", "failed", hookReleaseID, runErr.Error(), hookImages)
			}()
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			resolved, env, err := cfg.ResolveEnvironment(envName)
			if err != nil {
				return err
			}
			cfg = resolved
			plan, err := planner.DeploymentPlan(cfg, envName)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), plan.String())
			secretOpts, err := secretSourceOptions(opts, envName)
			if err != nil {
				return err
			}
			if opts.dryRun {
				if _, err := secrets.VerifyForEnv(cfg, secretOpts); err != nil {
					return err
				}
				stateDir, err := localStateDirForConfig(opts.configPath)
				if err != nil {
					return err
				}
				return printIngressDryRun(cmd.OutOrStdout(), cfg, env, envName, stateDir)
			}
			stateDir, err := localStateDirForConfig(opts.configPath)
			if err != nil {
				return err
			}
			store := state.NewStore(stateDir)
			operationLock, err := store.AcquireOperationLock(envName, "deploy")
			if err != nil {
				recordEvent(store, state.Event{Environment: envName, Kind: "deploy", Status: "blocked", Message: err.Error()})
				return err
			}
			defer operationLock.Unlock()
			if !ignoreLock {
				if lock, err := store.ReadDeployLock(envName); err == nil {
					message := deployLockMessage(lock)
					recordEvent(store, state.Event{Environment: envName, Kind: "deploy", Status: "blocked", Message: message})
					return fmt.Errorf("%s; rerun with --ignore-lock to override", message)
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
			} else {
				recordEvent(store, state.Event{Environment: envName, Kind: "deploy_lock", Status: "ignored"})
			}
			hosts, err := resolvedHostsForEnvironment(store, envName, env)
			if err != nil {
				return err
			}
			recordEvent(store, state.Event{Environment: envName, Kind: "deploy", Status: "started"})
			createdAt := deployNow()
			releaseID, err := newReleaseID()
			if err != nil {
				return err
			}
			gitRevision, _ := deployGitRevision(ctx)
			hookReleaseID = releaseID
			hookCfg = cfg
			hookStore = store
			if err := runDeployHooks(ctx, cmd.OutOrStdout(), store, cfg, envName, "pre_deploy", releaseID, "", opts.configPath); err != nil {
				recordEvent(store, state.Event{Environment: envName, Kind: "deploy", Status: "failed", Release: releaseID, Message: err.Error()})
				return err
			}
			secretFiles, err := secrets.RenderScopedForEnv(cfg, secretOpts)
			if err != nil {
				recordEvent(store, state.Event{Environment: envName, Kind: "deploy", Status: "failed", Release: releaseID, Message: err.Error()})
				return err
			}
			// Local validation is done; check CLI/agent compatibility before
			// spending time on builds or touching remote state.
			if err := preflightAgentProtocols(ctx, cmd.OutOrStdout(), opts, envName, hosts, autoUpgradeAgents); err != nil {
				recordEvent(store, state.Event{Environment: envName, Kind: "deploy", Status: "blocked", Release: releaseID, Message: err.Error()})
				return err
			}
			deployClient := deployDockerWithLogs(newDeployDocker(), cmd.OutOrStdout())
			if err := runDeployHooks(ctx, cmd.OutOrStdout(), store, cfg, envName, "pre_build", releaseID, "", opts.configPath); err != nil {
				recordEvent(store, state.Event{Environment: envName, Kind: "deploy", Status: "failed", Release: releaseID, Message: err.Error()})
				return err
			}
			images, err := prepareDeployImages(ctx, deployClient, cfg, releaseID)
			if err != nil {
				recordEvent(store, state.Event{Environment: envName, Kind: "deploy", Status: "failed", Release: releaseID, Message: err.Error()})
				return err
			}
			hookImages = images
			release := state.Release{
				ID:            releaseID,
				Environment:   args[0],
				Images:        images,
				SecretDigests: secretFiles.Digests,
				ConfigHash:    configHash(cfg),
				GitRevision:   gitRevision,
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
			accessoryNames := accessory.SortedNames(cfg, "")
			if len(accessoryNames) > 0 {
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_accessory_ensure", Status: "started", Release: releaseID, Message: fmt.Sprintf("accessories=%d", len(accessoryNames))})
				results, err := ensureAccessories(ctx, cmd.OutOrStdout(), opts, cfg, env, envName, store, accessoryNames, accessoryEnsureOnly)
				if err != nil {
					recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_accessory_ensure", Status: "failed", Release: releaseID, Message: err.Error()})
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
				changed := countChangedAccessories(results)
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_accessory_ensure", Status: "succeeded", Release: releaseID, Message: fmt.Sprintf("changed=%d", changed)})
				if changed > 0 {
					if err := restartCurrentServicesAfterAccessoryChange(ctx, cmd.OutOrStdout(), cfg, envName, store, hosts); err != nil {
						recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_accessory_restart", Status: "failed", Release: releaseID, Message: err.Error()})
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
				}
			}
			secretEnvFiles, secretWrites, err := serviceSecretEnvFiles(cfg, hosts, args[0], secretFiles)
			if err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy", Status: "failed", Release: releaseID, Message: err.Error()})
				return err
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_secret_write", Status: "started", Release: releaseID, Message: fmt.Sprintf("writes=%d", len(secretWrites))})
			if err := writeRemoteSecretFiles(ctx, secretWrites); err != nil {
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
			registryImages := deployRegistryImages(cfg, images)
			recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_registry_auth", Status: "started", Release: releaseID, Message: fmt.Sprintf("images=%d hosts=%d", len(registryImages), len(hosts))})
			if err := syncRemoteRegistryAuth(ctx, deployClient, hosts, registryImages); err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_registry_auth", Status: "failed", Release: releaseID, Message: err.Error()})
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
			recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_registry_auth", Status: "succeeded", Release: releaseID, Message: fmt.Sprintf("images=%d hosts=%d", len(registryImages), len(hosts))})
			if hasReleaseCommands(cfg) {
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_release_command", Status: "started", Release: releaseID})
				if err := runReleaseCommands(ctx, cfg, hosts, args[0], releaseID, images, secretEnvFiles); err != nil {
					recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_release_command", Status: "failed", Release: releaseID, Message: err.Error()})
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
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_release_command", Status: "succeeded", Release: releaseID})
			}
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
			if err := preserveMaintenanceIngress(ctx, cfg, args[0], stateDir, hosts, store); err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "maintenance", Status: "failed", Release: releaseID, Message: err.Error()})
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
			if hasManagedSchedules(cfg) {
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_schedules", Status: "started", Release: releaseID})
				if err := syncManagedSchedules(ctx, cfg, hosts, args[0], releaseID, store); err != nil {
					recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_schedules", Status: "failed", Release: releaseID, Message: err.Error()})
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
				recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_schedules", Status: "succeeded", Release: releaseID})
			}
			if err := runDeployHooks(ctx, cmd.OutOrStdout(), store, cfg, envName, "post_deploy", releaseID, "", opts.configPath); err != nil {
				recordEvent(store, state.Event{Environment: envName, Kind: "deploy", Status: "failed", Release: releaseID, Message: err.Error()})
				failedRelease, markErr := store.MarkReleaseFailed(releaseID, err.Error(), deployNow())
				if markErr != nil {
					recordEvent(store, state.Event{Environment: envName, Kind: "deploy_mark_failed", Status: "failed", Release: releaseID, Message: markErr.Error()})
					return fmt.Errorf("%w; additionally failed to mark release failed: %v", err, markErr)
				}
				recordEvent(store, state.Event{Environment: envName, Kind: "deploy_mark_failed", Status: "succeeded", Release: releaseID})
				if syncErr := syncRemoteReleaseStateWithEvents(ctx, store, envName, hosts, "deploy_release_state_write", failedRelease); syncErr != nil {
					recordEvent(store, state.Event{Environment: envName, Kind: "deploy", Status: "failed", Release: releaseID, Message: fmt.Sprintf("%v; additionally failed to write failed release state: %v", err, syncErr)})
					return fmt.Errorf("%w; additionally failed to write failed release state: %v", err, syncErr)
				}
				return err
			}
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
			runNotifications(ctx, store, cfg, envName, "deploy", "succeeded", releaseID, "", images)
			return nil
		},
	}
	cmd.Flags().BoolVar(&ignoreLock, "ignore-lock", false, "deploy even when the environment has a deploy lock")
	cmd.Flags().BoolVar(&autoUpgradeAgents, "auto-upgrade-agents", false, "upgrade incompatible host agents inline instead of stopping the deploy")
	addAgentBinaryOverrideFlags(cmd, opts)
	return cmd
}

func promoteCmd(opts *options) *cobra.Command {
	var sourceReleaseID string
	var ignoreLock bool
	var autoUpgradeAgents bool
	cmd := &cobra.Command{
		Use:   "promote SOURCE_ENV TARGET_ENV",
		Short: "Promote an existing release image set into another environment",
		Args:  ui.ExactArgs(ui.SourceEnv, ui.TargetEnv),
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			ctx := cmd.Context()
			sourceEnv := args[0]
			targetEnv := args[1]
			if sourceEnv == targetEnv {
				return fmt.Errorf("source and target environments must differ")
			}
			var hookCfg *config.Config
			var hookStore state.Store
			var hookReleaseID string
			var hookImages map[string]string
			defer func() {
				if runErr == nil || hookCfg == nil || hookStore.Dir == "" {
					return
				}
				if err := runDeployHooks(ctx, cmd.OutOrStdout(), hookStore, hookCfg, targetEnv, "deploy_failed", hookReleaseID, runErr.Error(), opts.configPath); err != nil {
					runErr = fmt.Errorf("%w; additionally deploy_failed hook failed: %v", runErr, err)
				}
				runNotifications(ctx, hookStore, hookCfg, targetEnv, "promote", "failed", hookReleaseID, runErr.Error(), hookImages)
			}()
			loaded, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			if _, _, err := loaded.ResolveEnvironment(sourceEnv); err != nil {
				return err
			}
			cfg, env, err := loaded.ResolveEnvironment(targetEnv)
			if err != nil {
				return err
			}
			stateDir, err := localStateDirForConfig(opts.configPath)
			if err != nil {
				return err
			}
			store := state.NewStore(stateDir)
			sourceRelease, err := promotionSourceRelease(store, sourceEnv, sourceReleaseID)
			if err != nil {
				return err
			}
			images, err := promotionImages(sourceRelease, cfg)
			if err != nil {
				return err
			}
			hookImages = images
			secretOpts, err := secretSourceOptions(opts, targetEnv)
			if err != nil {
				return err
			}
			createdAt := deployNow()
			releaseID, err := newReleaseID()
			if err != nil {
				return err
			}
			gitRevision, _ := deployGitRevision(ctx)
			hookReleaseID = releaseID
			fmt.Fprintf(cmd.OutOrStdout(), "promote %s release %s to %s as %s\n", sourceEnv, sourceRelease.ID, targetEnv, releaseID)
			for _, service := range sortedMapKeys(images) {
				fmt.Fprintf(cmd.OutOrStdout(), "- %s -> %s\n", service, images[service])
			}
			if opts.dryRun {
				if _, err := secrets.VerifyForEnv(cfg, secretOpts); err != nil {
					return err
				}
				return printIngressDryRun(cmd.OutOrStdout(), cfg, env, targetEnv, stateDir)
			}
			operationLock, err := store.AcquireOperationLock(targetEnv, "promote")
			if err != nil {
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "blocked", Release: releaseID, Message: err.Error()})
				return err
			}
			defer operationLock.Unlock()
			if !ignoreLock {
				if lock, err := store.ReadDeployLock(targetEnv); err == nil {
					message := deployLockMessage(lock)
					recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "blocked", Release: releaseID, Message: message})
					return fmt.Errorf("%s; rerun with --ignore-lock to override", message)
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
			} else {
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "deploy_lock", Status: "ignored"})
			}
			hosts, err := resolvedHostsForEnvironment(store, targetEnv, env)
			if err != nil {
				return err
			}
			hookCfg = cfg
			hookStore = store
			recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "started", Release: releaseID, Message: fmt.Sprintf("source=%s source_release=%s", sourceEnv, sourceRelease.ID)})
			if err := runDeployHooks(ctx, cmd.OutOrStdout(), store, cfg, targetEnv, "pre_deploy", releaseID, "", opts.configPath); err != nil {
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "failed", Release: releaseID, Message: err.Error()})
				return err
			}
			secretFiles, err := secrets.RenderScopedForEnv(cfg, secretOpts)
			if err != nil {
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "failed", Release: releaseID, Message: err.Error()})
				return err
			}
			// Local validation is done; check CLI/agent compatibility before
			// touching remote state.
			if err := preflightAgentProtocols(ctx, cmd.OutOrStdout(), opts, targetEnv, hosts, autoUpgradeAgents); err != nil {
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "blocked", Release: releaseID, Message: err.Error()})
				return err
			}
			release := state.Release{
				ID:            releaseID,
				Environment:   targetEnv,
				Images:        images,
				SecretDigests: secretFiles.Digests,
				ConfigHash:    configHash(cfg),
				GitRevision:   gitRevision,
				CreatedAt:     createdAt,
				Status:        state.ReleaseStatusPending,
			}
			if err := store.SaveReleaseRecord(release); err != nil {
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "failed", Release: releaseID, Message: err.Error()})
				return err
			}
			recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "release_created", Release: releaseID, Message: fmt.Sprintf("source=%s source_release=%s", sourceEnv, sourceRelease.ID)})
			if err := syncRemoteReleaseStateWithEvents(ctx, store, targetEnv, hosts, "promote_release_state_write", release); err != nil {
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "failed", Release: releaseID, Message: err.Error()})
				return failPromotedRelease(ctx, store, targetEnv, releaseID, hosts, err)
			}
			secretEnvFiles, secretWrites, err := serviceSecretEnvFiles(cfg, hosts, targetEnv, secretFiles)
			if err != nil {
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "failed", Release: releaseID, Message: err.Error()})
				return failPromotedRelease(ctx, store, targetEnv, releaseID, hosts, err)
			}
			recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote_secret_write", Status: "started", Release: releaseID, Message: fmt.Sprintf("writes=%d", len(secretWrites))})
			if err := writeRemoteSecretFiles(ctx, secretWrites); err != nil {
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote_secret_write", Status: "failed", Release: releaseID, Message: err.Error()})
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "failed", Release: releaseID, Message: err.Error()})
				return failPromotedRelease(ctx, store, targetEnv, releaseID, hosts, err)
			}
			recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote_secret_write", Status: "succeeded", Release: releaseID, Message: fmt.Sprintf("writes=%d", len(secretWrites))})
			deployClient := newDeployDocker()
			registryImages := deployRegistryImages(cfg, images)
			recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote_registry_auth", Status: "started", Release: releaseID, Message: fmt.Sprintf("images=%d hosts=%d", len(registryImages), len(hosts))})
			if err := syncRemoteRegistryAuth(ctx, deployClient, hosts, registryImages); err != nil {
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote_registry_auth", Status: "failed", Release: releaseID, Message: err.Error()})
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "failed", Release: releaseID, Message: err.Error()})
				return failPromotedRelease(ctx, store, targetEnv, releaseID, hosts, err)
			}
			recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote_registry_auth", Status: "succeeded", Release: releaseID, Message: fmt.Sprintf("images=%d hosts=%d", len(registryImages), len(hosts))})
			if hasReleaseCommands(cfg) {
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote_release_command", Status: "started", Release: releaseID})
				if err := runReleaseCommands(ctx, cfg, hosts, targetEnv, releaseID, images, secretEnvFiles); err != nil {
					recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote_release_command", Status: "failed", Release: releaseID, Message: err.Error()})
					recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "failed", Release: releaseID, Message: err.Error()})
					return failPromotedRelease(ctx, store, targetEnv, releaseID, hosts, err)
				}
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote_release_command", Status: "succeeded", Release: releaseID})
			}
			recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote_rollout", Status: "started", Release: releaseID})
			actions, err := deployment.Rollout(ctx, deployment.RolloutOptions{
				Config:         cfg,
				Environment:    env,
				Hosts:          hosts,
				EnvName:        targetEnv,
				ReleaseID:      releaseID,
				Images:         images,
				StateDir:       stateDir,
				SecretEnvFiles: secretEnvFiles,
				AgentFor:       deploymentAgentFactory(),
			})
			if err != nil {
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote_rollout", Status: "failed", Release: releaseID, Message: err.Error()})
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "failed", Release: releaseID, Message: err.Error()})
				return failPromotedRelease(ctx, store, targetEnv, releaseID, hosts, err)
			}
			recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote_rollout", Status: "succeeded", Release: releaseID})
			recordIngressEvents(store, targetEnv, releaseID, actions)
			if err := preserveMaintenanceIngress(ctx, cfg, targetEnv, stateDir, hosts, store); err != nil {
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "maintenance", Status: "failed", Release: releaseID, Message: err.Error()})
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "failed", Release: releaseID, Message: err.Error()})
				return failPromotedRelease(ctx, store, targetEnv, releaseID, hosts, err)
			}
			if hasManagedSchedules(cfg) {
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote_schedules", Status: "started", Release: releaseID})
				if err := syncManagedSchedules(ctx, cfg, hosts, targetEnv, releaseID, store); err != nil {
					recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote_schedules", Status: "failed", Release: releaseID, Message: err.Error()})
					recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "failed", Release: releaseID, Message: err.Error()})
					return failPromotedRelease(ctx, store, targetEnv, releaseID, hosts, err)
				}
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote_schedules", Status: "succeeded", Release: releaseID})
			}
			if err := runDeployHooks(ctx, cmd.OutOrStdout(), store, cfg, targetEnv, "post_deploy", releaseID, "", opts.configPath); err != nil {
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "failed", Release: releaseID, Message: err.Error()})
				return failPromotedRelease(ctx, store, targetEnv, releaseID, hosts, err)
			}
			completedAt := deployNow()
			healthyRelease := releaseAsHealthy(release, completedAt)
			if err := syncRemoteReleaseStateWithEvents(ctx, store, targetEnv, hosts, "promote_release_state_write", healthyRelease); err != nil {
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "failed", Release: releaseID, Message: err.Error()})
				return err
			}
			if _, err := store.MarkReleaseHealthy(releaseID, completedAt); err != nil {
				recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "failed", Release: releaseID, Message: err.Error()})
				return err
			}
			recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote", Status: "succeeded", Release: releaseID, Message: fmt.Sprintf("source=%s source_release=%s", sourceEnv, sourceRelease.ID)})
			runNotifications(ctx, store, cfg, targetEnv, "promote", "succeeded", releaseID, fmt.Sprintf("source=%s source_release=%s", sourceEnv, sourceRelease.ID), images)
			return nil
		},
	}
	cmd.Flags().StringVar(&sourceReleaseID, "release", "", "source release id to promote; defaults to SOURCE_ENV current release")
	cmd.Flags().BoolVar(&ignoreLock, "ignore-lock", false, "promote even when the target environment has a deploy lock")
	cmd.Flags().BoolVar(&autoUpgradeAgents, "auto-upgrade-agents", false, "upgrade incompatible host agents inline instead of stopping the promote")
	addAgentBinaryOverrideFlags(cmd, opts)
	return cmd
}

func promotionSourceRelease(store state.Store, sourceEnv, releaseID string) (state.Release, error) {
	var release state.Release
	var err error
	if strings.TrimSpace(releaseID) != "" {
		release, err = store.ReadRelease(releaseID)
	} else {
		release, err = store.CurrentRelease(sourceEnv)
	}
	if err != nil {
		return state.Release{}, err
	}
	if release.Environment != sourceEnv {
		return state.Release{}, fmt.Errorf("release %s belongs to environment %q", release.ID, release.Environment)
	}
	if release.Status == state.ReleaseStatusFailed || (!release.Healthy && release.Status != "") {
		return state.Release{}, fmt.Errorf("release %s is not healthy", release.ID)
	}
	return release, nil
}

func promotionImages(source state.Release, cfg *config.Config) (map[string]string, error) {
	images := map[string]string{}
	var missing []string
	for _, service := range sortedMapKeys(cfg.Services) {
		image := strings.TrimSpace(source.Images[service])
		if image == "" {
			missing = append(missing, service)
			continue
		}
		images[service] = image
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("source release %s is missing image(s) for target service(s): %s", source.ID, strings.Join(missing, ", "))
	}
	return images, nil
}

func failPromotedRelease(ctx context.Context, store state.Store, envName, releaseID string, hosts []scheduler.Host, err error) error {
	failedRelease, markErr := store.MarkReleaseFailed(releaseID, err.Error(), deployNow())
	if markErr != nil {
		recordEvent(store, state.Event{Environment: envName, Kind: "promote_mark_failed", Status: "failed", Release: releaseID, Message: markErr.Error()})
		return fmt.Errorf("%w; additionally failed to mark promoted release failed: %v", err, markErr)
	}
	recordEvent(store, state.Event{Environment: envName, Kind: "promote_mark_failed", Status: "succeeded", Release: releaseID})
	if syncErr := syncRemoteReleaseStateWithEvents(ctx, store, envName, hosts, "promote_release_state_write", failedRelease); syncErr != nil {
		return fmt.Errorf("%w; additionally failed to write failed promoted release state: %v", err, syncErr)
	}
	return err
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
			aliasTags, err := docker.ImageAliasTags(cfg.Registry, name, svc.Image.Tags)
			if err != nil {
				return nil, err
			}
			buildOpts := docker.BuildOptions{
				ContextDir:     svc.Image.Build,
				Dockerfile:     svc.Image.Dockerfile,
				Tag:            tag,
				AdditionalTags: aliasTags,
				BuildArgs:      svc.Image.BuildArgs,
				Target:         svc.Image.Target,
				Builder:        svc.Image.Builder,
				Buildpack: docker.BuildpackOptions{
					Builder:      svc.Image.Buildpack.Builder,
					Buildpacks:   svc.Image.Buildpack.Buildpacks,
					Env:          svc.Image.Buildpack.Env,
					Descriptor:   svc.Image.Buildpack.Descriptor,
					Publish:      svc.Image.Buildpack.PublishEnabled(),
					PullPolicy:   svc.Image.Buildpack.PullPolicy,
					TrustBuilder: svc.Image.Buildpack.TrustBuilderEnabled(),
				},
				Platform:      svc.Image.Platform,
				Platforms:     svc.Image.Platforms,
				Pull:          svc.Image.PullEnabled(),
				NoCache:       svc.Image.NoCacheEnabled(),
				NoCacheFilter: svc.Image.NoCacheFilter,
				CacheFrom:     svc.Image.CacheFrom,
				CacheTo:       svc.Image.CacheTo,
				Secrets:       svc.Image.Secrets,
				SSH:           svc.Image.SSH,
				SBOM:          svc.Image.SBOM.Value(),
				Provenance:    svc.Image.Provenance.Value(),
			}
			if docker.BuildPublishesImage(buildOpts) {
				buildOpts.Push = true
			}
			if err := dc.BuildImage(ctx, buildOpts); err != nil {
				return nil, err
			}
			if !docker.BuildPublishesImage(buildOpts) {
				if err := dc.Push(ctx, tag); err != nil {
					return nil, err
				}
				for _, aliasTag := range aliasTags {
					if err := dc.Push(ctx, aliasTag); err != nil {
						return nil, err
					}
				}
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

func deployRegistryImages(cfg *config.Config, images map[string]string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, service := range sortedServiceNames(cfg.Services) {
		image := strings.TrimSpace(images[service])
		if image == "" {
			continue
		}
		if _, ok := seen[image]; ok {
			continue
		}
		seen[image] = struct{}{}
		out = append(out, image)
	}
	if deploymentHasIngress(cfg) {
		image := strings.TrimSpace(deploymentCaddyImage(cfg))
		if image != "" {
			if _, ok := seen[image]; !ok {
				out = append(out, image)
			}
		}
	}
	return out
}

func deploymentHasIngress(cfg *config.Config) bool {
	for _, svc := range cfg.Services {
		if svc.Ingress != nil {
			return true
		}
	}
	return false
}

func deploymentCaddyImage(cfg *config.Config) string {
	if strings.TrimSpace(cfg.Ingress.Caddy.Image) != "" {
		return cfg.Ingress.Caddy.Image
	}
	return config.DefaultCaddyImage
}

func syncRemoteRegistryAuth(ctx context.Context, dc deployDocker, hosts []scheduler.Host, images []string) error {
	auths, err := registryAuthsForImages(ctx, dc, images)
	if err != nil {
		return err
	}
	if len(auths) == 0 {
		return nil
	}
	var failures []string
	for _, host := range hosts {
		client := newDeployAgent(host)
		for _, auth := range auths {
			params := agent.WriteRegistryAuthParams{Server: auth.Server, Auth: auth.Auth}
			if err := client.Call(ctx, "write_registry_auth", params, nil); err != nil {
				failures = append(failures, fmt.Sprintf("%s:%s: %v", host.Name, auth.Server, err))
			}
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("write registry auth failed on %d host/registry pairs: %s", len(failures), strings.Join(failures, "; "))
	}
	return nil
}

func registryAuthsForImages(ctx context.Context, dc deployDocker, images []string) ([]docker.RegistryAuth, error) {
	seenImages := map[string]struct{}{}
	seenServers := map[string]struct{}{}
	var auths []docker.RegistryAuth
	for _, image := range images {
		image = strings.TrimSpace(image)
		if image == "" {
			continue
		}
		if _, ok := seenImages[image]; ok {
			continue
		}
		seenImages[image] = struct{}{}
		auth, ok, err := dc.RegistryAuth(ctx, image)
		if err != nil {
			return nil, fmt.Errorf("registry auth for %s: %w", image, err)
		}
		if !ok {
			continue
		}
		if _, exists := seenServers[auth.Server]; exists {
			continue
		}
		seenServers[auth.Server] = struct{}{}
		auths = append(auths, auth)
	}
	return auths, nil
}

func hasManagedSchedules(cfg *config.Config) bool {
	for _, svc := range cfg.Services {
		if len(svc.Schedules) > 0 {
			return true
		}
	}
	for _, acc := range cfg.Accessories {
		if strings.TrimSpace(acc.Backup.Schedule.Cron) != "" {
			return true
		}
	}
	return false
}

func hasReleaseCommands(cfg *config.Config) bool {
	for _, svc := range cfg.Services {
		if strings.TrimSpace(svc.Release.Command) != "" {
			return true
		}
	}
	return false
}

func runReleaseCommands(ctx context.Context, cfg *config.Config, hosts []scheduler.Host, envName, releaseID string, images map[string]string, secretEnvFiles map[string]string) error {
	placements, err := scheduler.PlaceServicesOnHosts(cfg, hosts)
	if err != nil {
		return err
	}
	placementByServiceReplica := map[string]scheduler.Placement{}
	for _, placement := range placements {
		placementByServiceReplica[schedulePlacementKey(placement.Service, placement.Replica)] = placement
	}
	for _, serviceName := range sortedServiceNames(cfg.Services) {
		svc := cfg.Services[serviceName]
		if strings.TrimSpace(svc.Release.Command) == "" {
			continue
		}
		replica := svc.Release.Replica
		if replica == 0 {
			replica = 1
		}
		placement, ok := placementByServiceReplica[schedulePlacementKey(serviceName, replica)]
		if !ok {
			return fmt.Errorf("release command for service %q references unplaced replica %d", serviceName, replica)
		}
		image := strings.TrimSpace(images[serviceName])
		if image == "" {
			return fmt.Errorf("release command for service %q missing image", serviceName)
		}
		client := newDeployAgent(placement.Host)
		if err := client.Call(ctx, "pull", map[string]string{"image": image}, nil); err != nil {
			return fmt.Errorf("pull release image for service %s on %s: %w", serviceName, placement.Host.Name, err)
		}
		networkName := deployment.DockerNetworkName(cfg, envName)
		if err := ensureManagedDockerNetwork(ctx, client, cfg, envName); err != nil {
			return fmt.Errorf("ensure release network %s on %s: %w", networkName, placement.Host.Name, err)
		}
		params := agent.RunOneOffContainerParams{
			Name:           releaseCommandContainerName(cfg.Project, envName, serviceName, releaseID),
			Image:          image,
			Command:        svc.Release.Command,
			Args:           releaseCommandDockerArgs(svc, secretEnvFiles[serviceName]),
			Labels:         deployment.ContainerLabels(cfg.Project, envName, serviceName, replica, releaseID, svc.Labels),
			Network:        networkName,
			TimeoutSeconds: svc.Release.TimeoutSeconds,
		}
		var result agent.CommandResult
		if err := client.Call(ctx, "run_oneoff_container", params, &result); err != nil {
			return fmt.Errorf("release command for service %s on %s: %w", serviceName, placement.Host.Name, err)
		}
	}
	return nil
}

func releaseCommandDockerArgs(svc config.Service, envFiles ...string) []string {
	withoutPorts := svc
	withoutPorts.Ports = nil
	return deployment.DockerOneOffArgs(withoutPorts, envFiles...)
}

func releaseCommandContainerName(project, envName, service, releaseID string) string {
	parts := []string{"ship", safeCronName(project), safeCronName(envName), safeCronName(service), "release", safeCronName(releaseID)}
	return strings.Join(parts, "_")
}

func ensureManagedDockerNetwork(ctx context.Context, client deployAgent, cfg *config.Config, envName string) error {
	networkName := deployment.DockerNetworkName(cfg, envName)
	if strings.TrimSpace(networkName) == "" {
		return nil
	}
	return client.Call(ctx, "ensure_network", agent.EnsureNetworkParams{Name: networkName, Driver: deployment.DockerNetworkDriver(cfg)}, nil)
}

func syncManagedSchedules(ctx context.Context, cfg *config.Config, hosts []scheduler.Host, envName, releaseID string, store state.Store) error {
	prefix := scheduleFilePrefix(cfg.Project, envName)
	filesByHost, err := managedScheduleFiles(cfg, hosts, envName, releaseID, prefix, store)
	if err != nil {
		return err
	}
	var failures []string
	for _, host := range hosts {
		params := agent.SyncCronFilesParams{
			Prefix: prefix,
			Files:  filesByHost[host.Name],
		}
		if err := newDeployAgent(host).Call(ctx, "sync_cron_files", params, nil); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", host.Name, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("sync schedules failed on %d/%d hosts: %s", len(failures), len(hosts), strings.Join(failures, "; "))
	}
	return nil
}

func managedScheduleFiles(cfg *config.Config, hosts []scheduler.Host, envName, releaseID, prefix string, store state.Store) (map[string][]agent.CronFile, error) {
	filesByHost := map[string][]agent.CronFile{}
	for _, host := range hosts {
		filesByHost[host.Name] = nil
	}
	if err := addServiceScheduleFiles(filesByHost, cfg, hosts, envName, releaseID, prefix); err != nil {
		return nil, err
	}
	if err := addAccessoryBackupScheduleFiles(filesByHost, cfg, hosts, envName, prefix, store); err != nil {
		return nil, err
	}
	return filesByHost, nil
}

func addServiceScheduleFiles(filesByHost map[string][]agent.CronFile, cfg *config.Config, hosts []scheduler.Host, envName, releaseID, prefix string) error {
	placements, err := scheduler.PlaceServicesOnHosts(cfg, hosts)
	if err != nil {
		return err
	}
	placementByServiceReplica := map[string]scheduler.Placement{}
	for _, placement := range placements {
		placementByServiceReplica[schedulePlacementKey(placement.Service, placement.Replica)] = placement
	}
	for _, serviceName := range sortedServiceNames(cfg.Services) {
		svc := cfg.Services[serviceName]
		scheduleNames := make([]string, 0, len(svc.Schedules))
		for name := range svc.Schedules {
			scheduleNames = append(scheduleNames, name)
		}
		sort.Strings(scheduleNames)
		for _, scheduleName := range scheduleNames {
			schedule := svc.Schedules[scheduleName]
			replica := schedule.Replica
			if replica == 0 {
				replica = 1
			}
			placement, ok := placementByServiceReplica[schedulePlacementKey(serviceName, replica)]
			if !ok {
				return fmt.Errorf("schedule %s.%s references unplaced replica %d", serviceName, scheduleName, replica)
			}
			container := deployment.ContainerName(cfg.Project, envName, serviceName, replica, releaseID)
			fileName := prefix + safeCronName(serviceName) + "-" + safeCronName(scheduleName)
			content := renderCronFile(schedule, container, fileName)
			filesByHost[placement.Host.Name] = append(filesByHost[placement.Host.Name], agent.CronFile{Name: fileName, Content: content})
		}
	}
	return nil
}

func addAccessoryBackupScheduleFiles(filesByHost map[string][]agent.CronFile, cfg *config.Config, hosts []scheduler.Host, envName, prefix string, store state.Store) error {
	for _, name := range accessory.SortedNames(cfg, "") {
		acc := cfg.Accessories[name]
		if strings.TrimSpace(acc.Backup.Schedule.Cron) == "" {
			continue
		}
		if strings.TrimSpace(acc.Backup.Command) == "" {
			return fmt.Errorf("accessory %q backup.schedule requires backup.command", name)
		}
		placement, err := accessory.PlacementForHosts(cfg, hosts, envName, name, store)
		if err != nil {
			return err
		}
		if !placement.Persisted {
			return fmt.Errorf("accessory %q backup.schedule requires a saved placement; run accessory deploy first", name)
		}
		fileName := prefix + "accessory-" + safeCronName(name) + "-backup"
		content, err := renderAccessoryBackupCronFile(acc, envName, name, fileName)
		if err != nil {
			return err
		}
		filesByHost[placement.Host.Name] = append(filesByHost[placement.Host.Name], agent.CronFile{Name: fileName, Content: content})
	}
	return nil
}

func schedulePlacementKey(service string, replica int) string {
	return service + "\x00" + strconv.Itoa(replica)
}

func scheduleFilePrefix(project, envName string) string {
	return "ship-" + safeCronName(project) + "-" + safeCronName(envName) + "-"
}

func renderCronFile(schedule config.Schedule, container, fileName string) string {
	command := "docker exec " + shellQuote(container) + " sh -lc " + shellQuote(schedule.Command)
	if schedule.TimeoutSeconds > 0 {
		command = "timeout " + strconv.Itoa(schedule.TimeoutSeconds) + "s " + command
	}
	logPath := "/var/log/" + fileName + ".log"
	return strings.TrimSpace(schedule.Cron) + " root " + escapeCronCommand(command+" >> "+shellQuote(logPath)+" 2>&1") + "\n"
}

func renderAccessoryBackupCronFile(acc config.Accessory, envName, name, fileName string) (string, error) {
	backupCommand := strings.TrimSpace(acc.Backup.Command)
	if backupCommand == "" {
		return "", fmt.Errorf("backup.command is required")
	}
	exportCommand := strings.TrimSpace(acc.Backup.ExportCommand)
	dir := accessory.BackupArtifactDir(acc, envName, name)
	filePrefix := safeCronName(name) + "-"
	parts := []string{
		"artifact_dir=" + shellQuote(dir),
		"artifact=\"$artifact_dir/" + filePrefix + "$(date -u +%Y%m%dT%H%M%S.000000000Z).backup\"",
		"tmp=\"$artifact.tmp\"",
		"mkdir -p \"$artifact_dir\"",
		"( " + backupCommand + " ) > \"$tmp\"",
		"test -s \"$tmp\"",
		"mv \"$tmp\" \"$artifact\"",
	}
	if exportCommand != "" {
		parts = append(parts,
			"export_output=$(SHIP_BACKUP_ARTIFACT=\"$artifact\"; export SHIP_BACKUP_ARTIFACT; "+exportCommand+")",
			"if [ -n \"$export_output\" ]; then printf '%s\\n' \"$export_output\"; fi",
		)
	}
	parts = append(parts,
		"printf '%s\\n' \"$artifact\"",
	)
	command := strings.Join(parts, " && ")
	if acc.Backup.Schedule.TimeoutSeconds > 0 {
		command = "timeout " + strconv.Itoa(acc.Backup.Schedule.TimeoutSeconds) + "s " + command
	}
	logPath := "/var/log/" + fileName + ".log"
	return strings.TrimSpace(acc.Backup.Schedule.Cron) + " root " + escapeCronCommand(command+" >> "+shellQuote(logPath)+" 2>&1") + "\n", nil
}

func safeCronName(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		allowed := r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' ||
			r == '_' || r == '.' || r == '-'
		if allowed {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" {
		return "x"
	}
	return out
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func escapeCronCommand(value string) string {
	return strings.ReplaceAll(value, "%", `\%`)
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

func lockCmd(opts *options) *cobra.Command {
	var message string
	cmd := &cobra.Command{
		Use:   "lock ENV",
		Short: "Prevent deploys to an environment until unlocked",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			envName := args[0]
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			if _, err := cfg.Environment(envName); err != nil {
				return err
			}
			stateDir, err := localStateDirForConfig(opts.configPath)
			if err != nil {
				return err
			}
			store := state.NewStore(stateDir)
			lock := state.DeployLock{
				Environment: envName,
				Message:     message,
				CreatedAt:   deployNow(),
			}
			if err := store.SaveDeployLock(lock); err != nil {
				return err
			}
			recordEvent(store, state.Event{Environment: envName, Kind: "deploy_lock", Status: "locked", Message: strings.TrimSpace(message)})
			if strings.TrimSpace(message) != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "locked %s: %s\n", envName, strings.TrimSpace(message))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "locked %s\n", envName)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&message, "message", "", "reason shown when deploys are blocked")
	return cmd
}

func unlockCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "unlock ENV",
		Short: "Allow deploys to a locked environment",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			envName := args[0]
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			if _, err := cfg.Environment(envName); err != nil {
				return err
			}
			stateDir, err := localStateDirForConfig(opts.configPath)
			if err != nil {
				return err
			}
			store := state.NewStore(stateDir)
			if err := store.DeleteDeployLock(envName); err != nil {
				return err
			}
			recordEvent(store, state.Event{Environment: envName, Kind: "deploy_lock", Status: "unlocked"})
			fmt.Fprintf(cmd.OutOrStdout(), "unlocked %s\n", envName)
			return nil
		},
	}
}

func deployLockMessage(lock state.DeployLock) string {
	message := strings.TrimSpace(lock.Message)
	if message == "" {
		return fmt.Sprintf("deploys are locked for %s", lock.Environment)
	}
	return fmt.Sprintf("deploys are locked for %s: %s", lock.Environment, message)
}

func scaleCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "scale ENV SERVICE=N [SERVICE=N...]",
		Short: "Preview deterministic manual scaling placement",
		Args:  ui.MinimumArgs(2, ui.Env, ui.ScaleAssignments),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			envName := args[0]
			resolved, _, err := cfg.ResolveEnvironment(envName)
			if err != nil {
				return err
			}
			cfg = resolved
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

func pruneCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "prune ENV",
		Short: "Prune unused Ship-managed Docker images on environment hosts",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			envName := args[0]
			_, env, store, err := environmentContext(opts, envName)
			if err != nil {
				return err
			}
			hosts, err := resolvedHostsForEnvironment(store, envName, env)
			if err != nil {
				return err
			}
			if opts.dryRun {
				for _, host := range hosts {
					fmt.Fprintf(cmd.OutOrStdout(), "would prune unused Ship images on %s\n", host.Name)
				}
				recordEvent(store, state.Event{Environment: envName, Kind: "prune_images", Status: "planned", Message: fmt.Sprintf("hosts=%d", len(hosts))})
				return nil
			}
			operationLock, err := store.AcquireOperationLock(envName, "prune_images")
			if err != nil {
				recordEvent(store, state.Event{Environment: envName, Kind: "prune_images", Status: "blocked", Message: err.Error()})
				return err
			}
			defer operationLock.Unlock()
			recordEvent(store, state.Event{Environment: envName, Kind: "prune_images", Status: "started", Message: fmt.Sprintf("hosts=%d", len(hosts))})
			var failures []string
			for _, host := range hosts {
				if err := newDeployAgent(host).Call(cmd.Context(), "prune_images", map[string]any{}, nil); err != nil {
					failures = append(failures, fmt.Sprintf("%s: %v", host.Name, err))
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "pruned unused Ship images on %s\n", host.Name)
				recordEvent(store, state.Event{Environment: envName, Kind: "prune_images", Status: "succeeded", Host: host.Name})
			}
			if len(failures) > 0 {
				err := fmt.Errorf("prune images failed on %d/%d hosts: %s", len(failures), len(hosts), strings.Join(failures, "; "))
				recordEvent(store, state.Event{Environment: envName, Kind: "prune_images", Status: "failed", Message: err.Error()})
				return err
			}
			recordEvent(store, state.Event{Environment: envName, Kind: "prune_images", Status: "succeeded", Message: fmt.Sprintf("hosts=%d", len(hosts))})
			return nil
		},
	}
}

func statusCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "status ENV",
		Short: "Show desired placements and release state",
		Args:  ui.ExactArgs(ui.Env),
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

type psView struct {
	Environment string                       `json:"environment"`
	Current     string                       `json:"current_release,omitempty"`
	Containers  []deployment.ContainerStatus `json:"containers"`
}

func psCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	var all bool
	var service string
	cmd := &cobra.Command{
		Use:   "ps ENV",
		Short: "List observed Ship-managed containers",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, env, store, err := environmentContext(opts, args[0])
			if err != nil {
				return err
			}
			status, _, err := buildStatusView(cmd.Context(), cfg, env, args[0], store)
			if err != nil {
				return err
			}
			view := buildPSView(status, all, service)
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), view)
			}
			renderPSText(cmd.OutOrStdout(), view)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print containers as JSON")
	cmd.Flags().BoolVar(&all, "all", false, "include extra managed containers not in the desired placement")
	cmd.Flags().StringVar(&service, "service", "", "show one service only")
	return cmd
}

func buildPSView(status statusView, includeExtra bool, service string) psView {
	current := ""
	if status.CurrentRelease != nil {
		current = status.CurrentRelease.ID
	}
	seen := map[string]struct{}{}
	view := psView{Environment: status.Environment, Current: current}
	add := func(container deployment.ContainerStatus) {
		if service != "" && container.Service != service {
			return
		}
		key := container.Host + "\x00" + container.Name
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		view.Containers = append(view.Containers, container)
	}
	for _, desired := range status.Desired {
		for _, observed := range desired.Observed {
			if desired.Host != "" && observed.Host != desired.Host {
				continue
			}
			if desired.DesiredName != "" && observed.Name != desired.DesiredName {
				continue
			}
			add(observed)
		}
	}
	for _, observed := range status.Observed {
		if observed.Kind == "ingress" || observed.Kind == "accessory" {
			add(observed)
		}
	}
	if includeExtra {
		for _, observed := range status.ExtraObserved {
			add(observed)
		}
	}
	return view
}

func renderPSText(w io.Writer, view psView) {
	style := ui.NewStyle(w)
	fmt.Fprint(w, style.Teal("environment "))
	fmt.Fprint(w, style.White(view.Environment))
	if view.Current != "" {
		fmt.Fprint(w, style.Gray("  current "))
		fmt.Fprintln(w, style.Teal(view.Current))
	} else {
		fmt.Fprintln(w)
	}
	if len(view.Containers) == 0 {
		fmt.Fprintln(w, style.Gray("no containers"))
		return
	}

	table := ui.NewTable(w)
	table.SetHeaders("HOST", "NAME", "KIND", "SERVICE", "RELEASE", "STATUS")
	for _, container := range view.Containers {
		service := ""
		if container.Service != "" {
			service = fmt.Sprintf("%s.%d", container.Service, container.Replica)
		} else if container.Accessory != "" {
			service = container.Accessory
		}
		status := container.Status
		if status == "" {
			status = "-"
		}
		table.AddRow(
			container.Host,
			container.Name,
			container.Kind,
			service,
			container.Release,
			status,
		)
	}
	ui.RenderTable(w, table)
}

func healthCmd(opts *options) *cobra.Command {
	var replica int
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "health ENV [SERVICE]",
		Short: "Run configured health checks against the current release",
		Args:  ui.RangeArgs(1, 2, ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			envName := args[0]
			serviceName := ""
			if len(args) == 2 {
				serviceName = args[1]
			}
			if replica < 0 {
				return fmt.Errorf("--replica cannot be negative")
			}
			cfg, env, store, err := environmentContext(opts, envName)
			if err != nil {
				return err
			}
			if serviceName != "" {
				resolvedService, parsedReplica, err := resolveServiceReplica(cfg, envName, serviceName)
				if err != nil {
					return err
				}
				replica, err = mergeResolvedReplica(cmd, replica, parsedReplica, serviceName)
				if err != nil {
					return err
				}
				serviceName = resolvedService
			} else if replica > 0 {
				return fmt.Errorf("--replica requires SERVICE")
			}
			hosts, err := resolvedHostsForEnvironment(store, envName, env)
			if err != nil {
				return err
			}
			release, err := store.CurrentRelease(envName)
			if err != nil {
				return err
			}
			targets, err := restartTargets(cfg, hosts, serviceName, replica)
			if err != nil {
				return err
			}
			view := runHealthChecks(cmd.Context(), opts.dryRun, cfg, envName, release, targets)
			if jsonOutput {
				if err := writeJSON(cmd.OutOrStdout(), view); err != nil {
					return err
				}
			} else {
				renderHealthText(cmd.OutOrStdout(), view)
			}
			if !view.OK {
				return fmt.Errorf("health checks failed")
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&replica, "replica", 0, "check only one replica of SERVICE")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print health results as JSON")
	return cmd
}

func runHealthChecks(ctx context.Context, dryRun bool, cfg *config.Config, envName string, release state.Release, targets []scheduler.Placement) healthView {
	view := healthView{Environment: envName, Current: release.ID, OK: true}
	for _, target := range targets {
		svc := cfg.Services[target.Service]
		containerName := deployment.ContainerName(cfg.Project, envName, target.Service, target.Replica, release.ID)
		entry := healthEntry{
			Host:      target.Host.Name,
			Service:   target.Service,
			Replica:   target.Replica,
			Container: containerName,
			OK:        true,
		}
		health, ok, err := deployment.HealthCheck(svc, containerName)
		if err != nil {
			entry.Status = "invalid"
			entry.OK = false
			entry.Error = err.Error()
			view.OK = false
			view.Checks = append(view.Checks, entry)
			continue
		}
		entry.URL = health.URL
		entry.Command = health.Command
		if !ok {
			entry.Status = "skipped"
			view.Checks = append(view.Checks, entry)
			continue
		}
		if dryRun {
			entry.Status = "planned"
			view.Checks = append(view.Checks, entry)
			continue
		}
		entry.Checked = true
		var result agent.HealthCheckResult
		if err := newDeployAgent(target.Host).Call(ctx, "health_check", health, &result); err != nil {
			entry.Status = "failed"
			entry.OK = false
			entry.Error = err.Error()
			view.OK = false
			view.Checks = append(view.Checks, entry)
			continue
		}
		entry.StatusCode = result.StatusCode
		entry.Output = result.Output
		entry.DurationMS = result.DurationMS
		if !result.OK {
			entry.Status = "failed"
			entry.OK = false
			view.OK = false
			view.Checks = append(view.Checks, entry)
			continue
		}
		entry.Status = "ok"
		view.Checks = append(view.Checks, entry)
	}
	return view
}

func renderHealthText(w io.Writer, view healthView) {
	fields := []ui.HeaderField{{Label: "ok", Value: fmt.Sprintf("%t", view.OK)}}
	if view.Current != "" {
		fields = append([]ui.HeaderField{{Label: "release", Value: view.Current, Accent: true}}, fields...)
	}
	ui.PrintHeader(w, view.Environment, fields...)
	if len(view.Checks) == 0 {
		ui.PrintNotice(w, "no checks")
		return
	}
	table := ui.NewTable(w)
	table.SetHeaders("HOST", "SERVICE", "CONTAINER", "STATUS", "CODE", "MS", "DETAIL")
	for _, check := range view.Checks {
		code := "-"
		if check.StatusCode > 0 {
			code = strconv.Itoa(check.StatusCode)
		}
		ms := "-"
		if check.DurationMS > 0 {
			ms = strconv.FormatInt(check.DurationMS, 10)
		}
		detail := check.Error
		if detail == "" {
			detail = check.Output
		}
		if detail == "" && check.URL != "" {
			detail = check.URL
		}
		if detail == "" && check.Command != "" {
			detail = check.Command
		}
		table.AddRow(
			check.Host,
			fmt.Sprintf("%s.%d", check.Service, check.Replica),
			check.Container,
			check.Status,
			code,
			ms,
			ui.Dash(detail),
		)
	}
	ui.RenderTable(w, table)
}

func maintenanceCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{Use: "maintenance", Short: "Serve or clear a maintenance page at ingress"}
	var message string
	enable := &cobra.Command{
		Use:   "enable ENV",
		Short: "Serve a 503 maintenance page for all ingress domains",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMaintenanceEnable(cmd.Context(), cmd.OutOrStdout(), opts, args[0], message)
		},
	}
	enable.Flags().StringVar(&message, "message", "", "maintenance response body")
	cmd.AddCommand(enable)

	cmd.AddCommand(&cobra.Command{
		Use:   "disable ENV",
		Short: "Restore normal ingress routing",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMaintenanceDisable(cmd.Context(), cmd.OutOrStdout(), opts, args[0])
		},
	})

	var jsonOutput bool
	status := &cobra.Command{
		Use:   "status ENV",
		Short: "Show maintenance mode state",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			stateDir, err := localStateDirForConfig(opts.configPath)
			if err != nil {
				return err
			}
			view, err := readMaintenanceView(stateDir, args[0])
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), view)
			}
			renderMaintenanceStatus(cmd.OutOrStdout(), view)
			return nil
		},
	}
	status.Flags().BoolVar(&jsonOutput, "json", false, "print maintenance state as JSON")
	cmd.AddCommand(status)
	return cmd
}

func runMaintenanceEnable(ctx context.Context, w io.Writer, opts *options, envName, message string) error {
	cfg, env, store, err := environmentContext(opts, envName)
	if err != nil {
		return err
	}
	stateDir, err := localStateDirForConfig(opts.configPath)
	if err != nil {
		return err
	}
	placements, err := scheduler.PlaceServices(cfg, env)
	if err != nil {
		return err
	}
	hosts := ingress.HostsForEnvironment(cfg, env, placements)
	caddyfile := ingress.GenerateMaintenanceCaddyfile(cfg, message)
	if strings.TrimSpace(caddyfile) == "" {
		return fmt.Errorf("no ingress domains configured for %s", envName)
	}
	if len(hosts) == 0 {
		return fmt.Errorf("no ingress hosts available for %s", envName)
	}
	if opts.dryRun {
		fmt.Fprintf(w, "would enable maintenance for %s\n", envName)
		for _, host := range hosts {
			fmt.Fprintf(w, "- reload maintenance ingress on %s\n", host.Name)
		}
		return nil
	}
	operationLock, err := store.AcquireOperationLock(envName, "maintenance")
	if err != nil {
		recordEvent(store, state.Event{Environment: envName, Kind: "maintenance", Status: "blocked", Message: err.Error()})
		return err
	}
	defer operationLock.Unlock()
	action := maintenanceIngressAction(cfg, envName, stateDir, caddyfile, hosts)
	recordEvent(store, state.Event{Environment: envName, Kind: "maintenance", Status: "started", Message: "enable"})
	if err := deployment.ExecuteActions(ctx, []deployment.Action{action}, deploymentAgentFactory(), nil); err != nil {
		recordEvent(store, state.Event{Environment: envName, Kind: "maintenance", Status: "failed", Message: err.Error()})
		return err
	}
	view := maintenanceView{
		Environment: envName,
		Enabled:     true,
		Message:     maintenanceMessage(message),
		UpdatedAt:   deployNow().UTC(),
		Hosts:       hostNames(hosts),
	}
	if err := writeMaintenanceView(stateDir, view); err != nil {
		recordEvent(store, state.Event{Environment: envName, Kind: "maintenance", Status: "failed", Message: err.Error()})
		return err
	}
	recordEvent(store, state.Event{Environment: envName, Kind: "maintenance", Status: "succeeded", Message: "enabled"})
	fmt.Fprintf(w, "enabled maintenance for %s on %s\n", envName, strings.Join(view.Hosts, ","))
	return nil
}

func runMaintenanceDisable(ctx context.Context, w io.Writer, opts *options, envName string) error {
	cfg, env, store, err := environmentContext(opts, envName)
	if err != nil {
		return err
	}
	stateDir, err := localStateDirForConfig(opts.configPath)
	if err != nil {
		return err
	}
	view, err := readMaintenanceView(stateDir, envName)
	if err != nil {
		return err
	}
	if !view.Enabled {
		fmt.Fprintf(w, "maintenance disabled for %s\n", envName)
		return nil
	}
	hosts, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return err
	}
	action, err := normalIngressAction(cfg, env, envName, stateDir, preferredMaintenanceHosts(hosts, view.Hosts))
	if err != nil {
		return err
	}
	if opts.dryRun {
		fmt.Fprintf(w, "would disable maintenance for %s\n", envName)
		for _, host := range action.IngressHosts {
			fmt.Fprintf(w, "- reload normal ingress on %s\n", host.Name)
		}
		return nil
	}
	operationLock, err := store.AcquireOperationLock(envName, "maintenance")
	if err != nil {
		recordEvent(store, state.Event{Environment: envName, Kind: "maintenance", Status: "blocked", Message: err.Error()})
		return err
	}
	defer operationLock.Unlock()
	recordEvent(store, state.Event{Environment: envName, Kind: "maintenance", Status: "started", Message: "disable"})
	if err := deployment.ExecuteActions(ctx, []deployment.Action{action}, deploymentAgentFactory(), nil); err != nil {
		recordEvent(store, state.Event{Environment: envName, Kind: "maintenance", Status: "failed", Message: err.Error()})
		return err
	}
	if err := clearMaintenanceView(stateDir, envName); err != nil {
		recordEvent(store, state.Event{Environment: envName, Kind: "maintenance", Status: "failed", Message: err.Error()})
		return err
	}
	recordEvent(store, state.Event{Environment: envName, Kind: "maintenance", Status: "succeeded", Message: "disabled"})
	fmt.Fprintf(w, "disabled maintenance for %s\n", envName)
	return nil
}

func maintenanceMessage(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return "Service temporarily unavailable for maintenance."
	}
	return message
}

func maintenanceIngressAction(cfg *config.Config, envName, stateDir, caddyfile string, hosts []scheduler.Host) deployment.Action {
	return deployment.Action{
		Kind:              deployment.ActionIngress,
		IngressPath:       filepath.Join(stateDir, "ingress", envName+".Caddyfile"),
		IngressConfig:     caddyfile,
		IngressHosts:      hosts,
		CaddyImage:        resolvedCaddyImage(cfg),
		CaddyName:         deployment.CaddyContainerName(cfg.Project, envName),
		CaddyDataVolume:   deployment.CaddyDataVolume(cfg, envName),
		CaddyConfigVolume: deployment.CaddyConfigVolume(cfg, envName),
		CaddyLabels:       deployment.CaddyLabels(cfg.Project, envName),
		Network:           deployment.DockerNetworkName(cfg, envName),
		NetworkDriver:     deployment.DockerNetworkDriver(cfg),
	}
}

func normalIngressAction(cfg *config.Config, env config.Environment, envName, stateDir string, fallbackHosts []scheduler.Host) (deployment.Action, error) {
	placements, err := scheduler.PlaceServices(cfg, env)
	if err != nil {
		return deployment.Action{}, err
	}
	hosts := ingress.HostsForEnvironment(cfg, env, placements)
	caddyfile := ingress.GenerateCaddyfile(cfg, scheduler.HostsForEnvironment(env), placements)
	if strings.TrimSpace(caddyfile) == "" && len(fallbackHosts) > 0 {
		hosts = fallbackHosts
	}
	if len(hosts) == 0 {
		hosts = fallbackHosts
	}
	if len(hosts) == 0 {
		return deployment.Action{}, fmt.Errorf("no ingress hosts available for %s", envName)
	}
	return maintenanceIngressAction(cfg, envName, stateDir, caddyfile, hosts), nil
}

func resolvedCaddyImage(cfg *config.Config) string {
	if cfg != nil && strings.TrimSpace(cfg.Ingress.Caddy.Image) != "" {
		return cfg.Ingress.Caddy.Image
	}
	return config.DefaultCaddyImage
}

func maintenanceStatePath(stateDir, envName string) string {
	return filepath.Join(stateDir, "maintenance", envName+".json")
}

func readMaintenanceView(stateDir, envName string) (maintenanceView, error) {
	path := maintenanceStatePath(stateDir, envName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return maintenanceView{Environment: envName, Enabled: false}, nil
	}
	if err != nil {
		return maintenanceView{}, err
	}
	var view maintenanceView
	if err := json.Unmarshal(data, &view); err != nil {
		return maintenanceView{}, err
	}
	if view.Environment == "" {
		view.Environment = envName
	}
	view.Enabled = true
	return view, nil
}

func writeMaintenanceView(stateDir string, view maintenanceView) error {
	path := maintenanceStatePath(stateDir, view.Environment)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func clearMaintenanceView(stateDir, envName string) error {
	err := os.Remove(maintenanceStatePath(stateDir, envName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func renderMaintenanceStatus(w io.Writer, view maintenanceView) {
	fmt.Fprintf(w, "maintenance %s enabled=%t", view.Environment, view.Enabled)
	if view.Enabled {
		if !view.UpdatedAt.IsZero() {
			fmt.Fprintf(w, " updated=%s", view.UpdatedAt.Format(time.RFC3339))
		}
		if view.Message != "" {
			fmt.Fprintf(w, " message=%q", view.Message)
		}
		if len(view.Hosts) > 0 {
			fmt.Fprintf(w, " hosts=%s", strings.Join(view.Hosts, ","))
		}
	}
	fmt.Fprintln(w)
}

func hostNames(hosts []scheduler.Host) []string {
	names := make([]string, 0, len(hosts))
	for _, host := range hosts {
		names = append(names, host.Name)
	}
	sort.Strings(names)
	return names
}

func preferredMaintenanceHosts(hosts []scheduler.Host, names []string) []scheduler.Host {
	if len(names) == 0 {
		return nil
	}
	byName := map[string]scheduler.Host{}
	for _, host := range hosts {
		byName[host.Name] = host
	}
	var selected []scheduler.Host
	for _, name := range names {
		if host, ok := byName[name]; ok {
			selected = append(selected, host)
		}
	}
	return selected
}

func preserveMaintenanceIngress(ctx context.Context, cfg *config.Config, envName, stateDir string, hosts []scheduler.Host, store state.Store) error {
	view, err := readMaintenanceView(stateDir, envName)
	if err != nil {
		return err
	}
	if !view.Enabled {
		return nil
	}
	targets := preferredMaintenanceHosts(hosts, view.Hosts)
	if len(targets) == 0 {
		targets = hosts
	}
	caddyfile := ingress.GenerateMaintenanceCaddyfile(cfg, view.Message)
	if strings.TrimSpace(caddyfile) == "" {
		return nil
	}
	action := maintenanceIngressAction(cfg, envName, stateDir, caddyfile, targets)
	if err := deployment.ExecuteActions(ctx, []deployment.Action{action}, deploymentAgentFactory(), nil); err != nil {
		return err
	}
	recordEvent(store, state.Event{Environment: envName, Kind: "maintenance", Status: "preserved", Message: "deploy kept maintenance ingress enabled"})
	return nil
}

func logsCmd(opts *options) *cobra.Command {
	var lines int
	var replica int
	var follow bool
	var jsonOutput bool
	var requestedRelease string
	var failed bool
	cmd := &cobra.Command{
		Use:   "logs ENV SERVICE",
		Short: "Fetch service logs from placed hosts",
		Args:  ui.ExactArgs(ui.Env, ui.Service),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			resolved, env, err := cfg.ResolveEnvironment(args[0])
			if err != nil {
				return err
			}
			cfg = resolved
			serviceArg := args[1]
			resolvedService, parsedReplica, err := resolveServiceReplica(cfg, args[0], serviceArg)
			if err != nil {
				return err
			}
			args[1] = resolvedService
			if lines <= 0 {
				return fmt.Errorf("--lines must be greater than zero")
			}
			if replica < 0 {
				return fmt.Errorf("--replica cannot be negative")
			}
			replica, err = mergeResolvedReplica(cmd, replica, parsedReplica, serviceArg)
			if err != nil {
				return err
			}
			if failed && strings.TrimSpace(requestedRelease) != "" {
				return fmt.Errorf("--failed and --release are mutually exclusive")
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
			if selected, err := selectLogsRelease(store, args[0], args[1], requestedRelease, failed); err != nil {
				return err
			} else {
				releaseID = selected
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
				Release:     releaseID,
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
						Release:   releaseID,
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
	cmd.Flags().StringVar(&requestedRelease, "release", "", "fetch logs for a specific release id")
	cmd.Flags().BoolVar(&failed, "failed", false, "fetch logs for the newest failed release")
	return cmd
}

func selectLogsRelease(store state.Store, envName, serviceName, requested string, failed bool) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		release, err := store.ReadRelease(requested)
		if err != nil {
			return "", err
		}
		if release.Environment != envName {
			return "", fmt.Errorf("release %s belongs to environment %q", requested, release.Environment)
		}
		if strings.TrimSpace(release.Images[serviceName]) == "" {
			return "", fmt.Errorf("release %s has no image for service %q", requested, serviceName)
		}
		return release.ID, nil
	}
	if failed {
		releases, err := store.Releases(envName)
		if err != nil {
			return "", err
		}
		for i := len(releases) - 1; i >= 0; i-- {
			release := releases[i]
			if release.Status != state.ReleaseStatusFailed {
				continue
			}
			if strings.TrimSpace(release.Images[serviceName]) == "" {
				continue
			}
			return release.ID, nil
		}
		return "", fmt.Errorf("no failed release with service %q for %q", serviceName, envName)
	}
	if release, err := store.CurrentRelease(envName); err == nil {
		return release.ID, nil
	}
	return "", nil
}

func restartCmd(opts *options) *cobra.Command {
	var replica int
	cmd := &cobra.Command{
		Use:   "restart ENV [SERVICE]",
		Short: "Recreate current release service containers",
		Args:  ui.RangeArgs(1, 2, ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			envName := args[0]
			serviceName := ""
			if len(args) == 2 {
				serviceName = args[1]
			}
			if replica < 0 {
				return fmt.Errorf("--replica cannot be negative")
			}
			cfg, env, store, err := environmentContext(opts, envName)
			if err != nil {
				return err
			}
			if serviceName != "" {
				resolvedService, parsedReplica, err := resolveServiceReplica(cfg, envName, serviceName)
				if err != nil {
					return err
				}
				replica, err = mergeResolvedReplica(cmd, replica, parsedReplica, serviceName)
				if err != nil {
					return err
				}
				serviceName = resolvedService
			} else if replica > 0 {
				return fmt.Errorf("--replica requires SERVICE")
			}
			hosts, err := resolvedHostsForEnvironment(store, envName, env)
			if err != nil {
				return err
			}
			release, err := currentReleaseForServiceMutation(cmd.Context(), cfg, envName, store, hosts)
			if err != nil {
				return err
			}
			targets, err := restartTargets(cfg, hosts, serviceName, replica)
			if err != nil {
				return err
			}
			if opts.dryRun {
				for _, target := range targets {
					name := deployment.ContainerName(cfg.Project, envName, target.Service, target.Replica, release.ID)
					fmt.Fprintf(cmd.OutOrStdout(), "would restart %s on %s\n", name, target.Host.Name)
				}
				recordEvent(store, state.Event{Environment: envName, Kind: "restart", Status: "planned", Release: release.ID, Service: serviceName, Message: fmt.Sprintf("containers=%d", len(targets))})
				return nil
			}
			operationLock, err := store.AcquireOperationLock(envName, "restart")
			if err != nil {
				recordEvent(store, state.Event{Environment: envName, Kind: "restart", Status: "blocked", Release: release.ID, Service: serviceName, Message: err.Error()})
				return err
			}
			defer operationLock.Unlock()
			secretEnvFiles, err := deployedServiceSecretEnvFiles(cfg, envName)
			if err != nil {
				return err
			}
			actions, err := restartActions(cfg, envName, release, targets, secretEnvFiles)
			if err != nil {
				return err
			}
			recordEvent(store, state.Event{Environment: envName, Kind: "restart", Status: "started", Release: release.ID, Service: serviceName, Message: fmt.Sprintf("containers=%d", len(targets))})
			if err := deployment.ExecuteActions(cmd.Context(), actions, deploymentAgentFactory(), nil); err != nil {
				recordEvent(store, state.Event{Environment: envName, Kind: "restart", Status: "failed", Release: release.ID, Service: serviceName, Message: err.Error()})
				return err
			}
			for _, target := range targets {
				name := deployment.ContainerName(cfg.Project, envName, target.Service, target.Replica, release.ID)
				fmt.Fprintf(cmd.OutOrStdout(), "restarted %s on %s\n", name, target.Host.Name)
				recordEvent(store, state.Event{Environment: envName, Kind: "restart", Status: "succeeded", Release: release.ID, Service: target.Service, Host: target.Host.Name})
			}
			recordEvent(store, state.Event{Environment: envName, Kind: "restart", Status: "succeeded", Release: release.ID, Service: serviceName, Message: fmt.Sprintf("containers=%d", len(targets))})
			return nil
		},
	}
	cmd.Flags().IntVar(&replica, "replica", 0, "restart only one replica of SERVICE")
	return cmd
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

func countChangedAccessories(results []accessoryEnsureResult) int {
	var changed int
	for _, result := range results {
		if result.Changed {
			changed++
		}
	}
	return changed
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

func execServiceCmd(opts *options) *cobra.Command {
	var replica int
	var all bool
	var timeoutSeconds int
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "exec ENV SERVICE -- COMMAND",
		Short: "Run a command inside deployed service containers",
		Args:  ui.MinimumArgs(3, ui.Env, ui.Service, ui.ArgNamed("COMMAND", "command to run inside the container")),
		RunE: func(cmd *cobra.Command, args []string) error {
			envName := args[0]
			serviceName := args[1]
			command := strings.TrimSpace(strings.Join(args[2:], " "))
			if command == "" {
				return fmt.Errorf("command is required")
			}
			if replica < 0 {
				return fmt.Errorf("--replica cannot be negative")
			}
			if timeoutSeconds < 0 {
				return fmt.Errorf("--timeout cannot be negative")
			}
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			resolved, env, err := cfg.ResolveEnvironment(envName)
			if err != nil {
				return err
			}
			cfg = resolved
			resolvedService, parsedReplica, err := resolveServiceReplica(cfg, envName, serviceName)
			if err != nil {
				return err
			}
			serviceName = resolvedService
			replica, err = mergeResolvedReplica(cmd, replica, parsedReplica, args[1])
			if err != nil {
				return err
			}
			if all && replica > 0 {
				return fmt.Errorf("--all and --replica cannot be used together")
			}
			stateDir, err := localStateDirForConfig(opts.configPath)
			if err != nil {
				return err
			}
			store := state.NewStore(stateDir)
			release, err := store.CurrentRelease(envName)
			if err != nil {
				return fmt.Errorf("current release for %s is required before exec: %w", envName, err)
			}
			hosts, err := resolvedHostsForEnvironment(store, envName, env)
			if err != nil {
				return err
			}
			placements, err := scheduler.PlaceServicesOnHosts(cfg, hosts)
			if err != nil {
				return err
			}
			targetReplica := replica
			if !all && targetReplica == 0 {
				targetReplica = 1
			}
			var targets []scheduler.Placement
			for _, placement := range placements {
				if placement.Service != serviceName {
					continue
				}
				if targetReplica > 0 && placement.Replica != targetReplica {
					continue
				}
				targets = append(targets, placement)
			}
			if len(targets) == 0 {
				if targetReplica > 0 {
					return fmt.Errorf("service %q has no replica %d", serviceName, targetReplica)
				}
				return fmt.Errorf("service %q has no placed replicas", serviceName)
			}
			view := execView{
				Environment: envName,
				Service:     serviceName,
				Command:     command,
				All:         all,
				Replica:     targetReplica,
			}
			for _, placement := range targets {
				name := deployment.ContainerName(cfg.Project, envName, placement.Service, placement.Replica, release.ID)
				var result agent.CommandResult
				params := agent.ExecContainerParams{
					Name:           name,
					Command:        command,
					TimeoutSeconds: timeoutSeconds,
				}
				if err := newDeployAgent(placement.Host).Call(cmd.Context(), "exec_container", params, &result); err != nil {
					return fmt.Errorf("exec %s on %s: %w", name, placement.Host.Name, err)
				}
				entry := execEntry{
					Host:      placement.Host.Name,
					Service:   placement.Service,
					Replica:   placement.Replica,
					Container: name,
					Output:    result.Output,
				}
				view.Entries = append(view.Entries, entry)
				if !jsonOutput {
					fmt.Fprintf(cmd.OutOrStdout(), "==> %s/%s <==\n%s\n", placement.Host.Name, name, result.Output)
				}
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), view)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "run on all placed replicas")
	cmd.Flags().IntVar(&replica, "replica", 0, "run on one replica number (defaults to 1)")
	cmd.Flags().IntVar(&timeoutSeconds, "timeout", 0, "command timeout in seconds")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print exec results as JSON")
	return cmd
}

func inspectCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "inspect ENV",
		Short: "Show structured environment release, placement, observed state, and events",
		Args:  ui.ExactArgs(ui.Env),
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
				CurrentConfig:  status.CurrentConfig,
				DeployedConfig: status.DeployedConfig,
				ConfigDrift:    status.ConfigDrift,
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

type supportError struct {
	Section string `json:"section"`
	Error   string `json:"error"`
}

type supportBundle struct {
	Environment string                 `json:"environment"`
	GeneratedAt time.Time              `json:"generated_at"`
	ConfigPath  string                 `json:"config_path,omitempty"`
	Config      map[string]any         `json:"resolved_config,omitempty"`
	Hosts       *hostsView             `json:"hosts,omitempty"`
	Doctor      doctor.Report          `json:"doctor"`
	Status      *statusView            `json:"status,omitempty"`
	Releases    *releaseHistoryView    `json:"releases,omitempty"`
	Accessories []state.AccessoryState `json:"accessories,omitempty"`
	Events      []state.Event          `json:"events,omitempty"`
	Errors      []supportError         `json:"errors,omitempty"`
}

func supportCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	var eventsLimit int
	var releasesLimit int
	cmd := &cobra.Command{
		Use:   "support ENV",
		Short: "Collect a redacted support bundle for an environment",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			if eventsLimit <= 0 {
				return fmt.Errorf("--events-limit must be greater than zero")
			}
			if releasesLimit <= 0 {
				return fmt.Errorf("--releases-limit must be greater than zero")
			}
			cfg, env, store, err := environmentContext(opts, args[0])
			if err != nil {
				return err
			}
			bundle := buildSupportBundle(cmd.Context(), opts, cfg, env, args[0], store, eventsLimit, releasesLimit)
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), bundle)
			}
			renderSupportText(cmd.OutOrStdout(), bundle)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print the full support bundle as JSON")
	cmd.Flags().IntVar(&eventsLimit, "events-limit", 50, "maximum recent events to include")
	cmd.Flags().IntVar(&releasesLimit, "releases-limit", 20, "maximum recent releases to include")
	return cmd
}

func buildSupportBundle(ctx context.Context, opts *options, cfg *config.Config, env config.Environment, envName string, store state.Store, eventsLimit, releasesLimit int) supportBundle {
	bundle := supportBundle{
		Environment: envName,
		GeneratedAt: deployNow().UTC(),
		ConfigPath:  opts.configPath,
		Doctor:      doctor.Run(ctx, cfg, doctor.Options{ConfigPath: opts.configPath}),
	}
	if value, err := resolvedConfigValue(cfg, envName); err != nil {
		bundle.Errors = append(bundle.Errors, supportError{Section: "config", Error: err.Error()})
	} else {
		bundle.Config = value
	}
	if hosts, err := buildHostsView(store, envName, env); err != nil {
		bundle.Errors = append(bundle.Errors, supportError{Section: "hosts", Error: err.Error()})
	} else {
		bundle.Hosts = &hosts
	}
	if status, _, err := buildStatusView(ctx, cfg, env, envName, store); err != nil {
		bundle.Errors = append(bundle.Errors, supportError{Section: "status", Error: err.Error()})
	} else {
		bundle.Status = &status
	}
	if releases, err := buildReleaseHistoryView(envName, store, releasesLimit); err != nil {
		bundle.Errors = append(bundle.Errors, supportError{Section: "releases", Error: err.Error()})
	} else {
		bundle.Releases = &releases
	}
	if accessories, err := store.AccessoryStates(envName); err != nil {
		bundle.Errors = append(bundle.Errors, supportError{Section: "accessories", Error: err.Error()})
	} else {
		bundle.Accessories = accessories
	}
	if events, err := store.Events(envName); err != nil {
		bundle.Errors = append(bundle.Errors, supportError{Section: "events", Error: err.Error()})
	} else {
		bundle.Events = newestEvents(events, eventsLimit)
	}
	return bundle
}

func newestEvents(events []state.Event, limit int) []state.Event {
	if len(events) <= limit {
		return events
	}
	return append([]state.Event(nil), events[len(events)-limit:]...)
}

func renderSupportText(w io.Writer, bundle supportBundle) {
	ui.PrintHeader(w, bundle.Environment,
		ui.HeaderField{Label: "generated", Value: bundle.GeneratedAt.Format(time.RFC3339)},
	)
	table := ui.NewTable(w)
	table.SetHeaders("SECTION", "SUMMARY")
	table.AddRow("doctor", fmt.Sprintf("passed=%d warnings=%d failed=%d", bundle.Doctor.Summary.Passed, bundle.Doctor.Summary.Warnings, bundle.Doctor.Summary.Failed))
	if bundle.Hosts != nil {
		table.AddRow("hosts", fmt.Sprintf("count=%d source=%s", len(bundle.Hosts.Hosts), bundle.Hosts.Source))
	}
	if bundle.Status != nil {
		table.AddRow("status", fmt.Sprintf("desired=%d observed=%d extra=%d drift=%t config_drift=%t",
			len(bundle.Status.Desired), len(bundle.Status.Observed), len(bundle.Status.ExtraObserved), bundle.Status.Summary.Drift, bundle.Status.ConfigDrift))
	}
	if bundle.Releases != nil {
		table.AddRow("releases", fmt.Sprintf("count=%d", len(bundle.Releases.Releases)))
	}
	table.AddRow("accessories", fmt.Sprintf("count=%d", len(bundle.Accessories)))
	table.AddRow("events", fmt.Sprintf("count=%d", len(bundle.Events)))
	ui.RenderTable(w, table)
	if len(bundle.Errors) > 0 {
		ui.PrintSection(w, "Collection errors")
		for _, err := range bundle.Errors {
			ui.PrintErrorLine(w, err.Section+": "+err.Error)
		}
	}
	ui.PrintNotice(w, "use --json for the complete redacted bundle")
}

func eventsCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "events ENV",
		Short: "Show local Ship event timeline",
		Args:  ui.ExactArgs(ui.Env),
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
			ui.PrintHeader(cmd.OutOrStdout(), args[0])
			renderEventsText(cmd.OutOrStdout(), events)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print events as JSON")
	return cmd
}

type releaseHistoryEntry struct {
	Release        state.Release `json:"release"`
	Current        bool          `json:"current"`
	RollbackTarget bool          `json:"rollback_target"`
}

type releaseHistoryView struct {
	Environment string                `json:"environment"`
	Releases    []releaseHistoryEntry `json:"releases"`
}

func releasesCmd(opts *options) *cobra.Command {
	var jsonOutput bool
	var limit int
	cmd := &cobra.Command{
		Use:   "releases ENV",
		Short: "Show local release history for an environment",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _, store, err := environmentContext(opts, args[0])
			if err != nil {
				return err
			}
			view, err := buildReleaseHistoryView(args[0], store, limit)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), view)
			}
			renderReleaseHistoryText(cmd.OutOrStdout(), view)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print releases as JSON")
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum releases to show")
	cmd.AddCommand(releaseDiffCmd(opts))
	return cmd
}

type releaseDiffView struct {
	Environment string             `json:"environment"`
	From        state.Release      `json:"from"`
	To          state.Release      `json:"to"`
	Config      configDiffEntry    `json:"config"`
	Images      mapDiffView        `json:"images"`
	Secrets     secrets.DigestDiff `json:"secrets"`
	Changed     bool               `json:"changed"`
}

type configDiffEntry struct {
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Changed bool   `json:"changed"`
}

type mapDiffView struct {
	Added   []string          `json:"added,omitempty"`
	Removed []string          `json:"removed,omitempty"`
	Changed []mapChangedEntry `json:"changed,omitempty"`
}

type mapChangedEntry struct {
	Name string `json:"name"`
	From string `json:"from"`
	To   string `json:"to"`
}

func releaseDiffCmd(opts *options) *cobra.Command {
	var fromID string
	var toID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "diff ENV",
		Short: "Compare two release records",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(fromID) == "" {
				return fmt.Errorf("--from release id is required")
			}
			if strings.TrimSpace(toID) == "" {
				return fmt.Errorf("--to release id is required")
			}
			_, _, store, err := environmentContext(opts, args[0])
			if err != nil {
				return err
			}
			view, err := buildReleaseDiffView(store, args[0], fromID, toID)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), view)
			}
			renderReleaseDiffText(cmd.OutOrStdout(), view)
			if view.Changed {
				return fmt.Errorf("release diff detected")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&fromID, "from", "", "base release id")
	cmd.Flags().StringVar(&toID, "to", "", "target release id")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print release diff as JSON")
	return cmd
}

func buildReleaseDiffView(store state.Store, envName, fromID, toID string) (releaseDiffView, error) {
	from, err := store.ReadRelease(fromID)
	if err != nil {
		return releaseDiffView{}, err
	}
	to, err := store.ReadRelease(toID)
	if err != nil {
		return releaseDiffView{}, err
	}
	if from.Environment != envName {
		return releaseDiffView{}, fmt.Errorf("release %s belongs to environment %q", from.ID, from.Environment)
	}
	if to.Environment != envName {
		return releaseDiffView{}, fmt.Errorf("release %s belongs to environment %q", to.ID, to.Environment)
	}
	view := releaseDiffView{
		Environment: envName,
		From:        from,
		To:          to,
		Config: configDiffEntry{
			From:    from.ConfigHash,
			To:      to.ConfigHash,
			Changed: from.ConfigHash != to.ConfigHash,
		},
		Images:  diffStringMap(from.Images, to.Images),
		Secrets: secrets.Diff(to.SecretDigests, from.SecretDigests),
	}
	view.Changed = view.Config.Changed || !mapDiffEmpty(view.Images) || !view.Secrets.Empty()
	return view, nil
}

func diffStringMap(from, to map[string]string) mapDiffView {
	var diff mapDiffView
	for _, name := range sortedMapKeys(to) {
		toValue := to[name]
		fromValue, ok := from[name]
		switch {
		case !ok:
			diff.Added = append(diff.Added, name)
		case fromValue != toValue:
			diff.Changed = append(diff.Changed, mapChangedEntry{Name: name, From: fromValue, To: toValue})
		}
	}
	for _, name := range sortedMapKeys(from) {
		if _, ok := to[name]; !ok {
			diff.Removed = append(diff.Removed, name)
		}
	}
	return diff
}

func mapDiffEmpty(diff mapDiffView) bool {
	return len(diff.Added) == 0 && len(diff.Removed) == 0 && len(diff.Changed) == 0
}

func renderReleaseDiffText(w io.Writer, view releaseDiffView) {
	ui.PrintHeader(w, view.Environment,
		ui.HeaderField{Label: "from", Value: view.From.ID, Accent: true},
		ui.HeaderField{Label: "to", Value: view.To.ID, Accent: true},
	)
	if !view.Changed {
		ui.PrintNotice(w, "no changes")
		return
	}
	table := ui.NewTable(w)
	table.SetHeaders("KIND", "NAME", "FROM", "TO")
	if view.Config.Changed {
		table.AddRow("config", "ship.yml", emptyAsNone(view.Config.From), emptyAsNone(view.Config.To))
	}
	for _, name := range view.Images.Added {
		table.AddRow("image", name, "-", view.To.Images[name])
	}
	for _, entry := range view.Images.Changed {
		table.AddRow("image", entry.Name, entry.From, entry.To)
	}
	for _, name := range view.Images.Removed {
		table.AddRow("image", name, view.From.Images[name], "-")
	}
	for _, name := range view.Secrets.Missing {
		table.AddRow("secret", name, "-", "added")
	}
	for _, name := range view.Secrets.Changed {
		table.AddRow("secret", name, "changed", "changed")
	}
	for _, name := range view.Secrets.Extra {
		table.AddRow("secret", name, "present", "-")
	}
	ui.RenderTable(w, table)
}

func buildReleaseHistoryView(envName string, store state.Store, limit int) (releaseHistoryView, error) {
	if limit <= 0 {
		return releaseHistoryView{}, fmt.Errorf("--limit must be greater than zero")
	}
	releases, err := store.Releases(envName)
	if err != nil {
		return releaseHistoryView{}, err
	}
	for i, j := 0, len(releases)-1; i < j; i, j = i+1, j-1 {
		releases[i], releases[j] = releases[j], releases[i]
	}
	if len(releases) > limit {
		releases = releases[:limit]
	}
	currentID := ""
	if current, err := store.CurrentRelease(envName); err == nil {
		currentID = current.ID
	}
	rollbackID := ""
	if target, err := store.RollbackTarget(envName); err == nil {
		rollbackID = target.ID
	}
	view := releaseHistoryView{Environment: envName}
	for _, release := range releases {
		view.Releases = append(view.Releases, releaseHistoryEntry{
			Release:        release,
			Current:        release.ID == currentID,
			RollbackTarget: release.ID == rollbackID,
		})
	}
	return view, nil
}

func renderReleaseHistoryText(w io.Writer, view releaseHistoryView) {
	ui.PrintHeader(w, view.Environment)
	if len(view.Releases) == 0 {
		ui.PrintNotice(w, "no releases")
		return
	}
	table := ui.NewTable(w)
	table.SetHeaders("ID", "STATUS", "HEALTHY", "CREATED", "MARKERS")
	for _, entry := range view.Releases {
		release := entry.Release
		table.AddRow(
			release.ID,
			release.Status,
			fmt.Sprintf("%t", release.Healthy),
			release.CreatedAt.Format(time.RFC3339),
			ui.Dash(releaseHistoryMarkers(entry)),
		)
	}
	ui.RenderTable(w, table)
	style := ui.NewStyle(w)
	for _, entry := range view.Releases {
		release := entry.Release
		var details []string
		if release.CompletedAt != nil {
			details = append(details, "completed="+release.CompletedAt.Format(time.RFC3339))
		}
		if release.FailedAt != nil {
			details = append(details, "failed="+release.FailedAt.Format(time.RFC3339))
		}
		if release.Error != "" {
			details = append(details, fmt.Sprintf("error=%q", release.Error))
		}
		if release.ConfigHash != "" {
			details = append(details, "config="+release.ConfigHash)
		}
		if release.GitRevision != "" {
			details = append(details, "git="+release.GitRevision)
		}
		for _, service := range sortedMapKeys(release.Images) {
			details = append(details, fmt.Sprintf("image %s=%s", service, release.Images[service]))
		}
		if len(details) == 0 {
			continue
		}
		fmt.Fprintf(w, "%s\n", style.Gray("  "+release.ID+": "+strings.Join(details, "  ")))
	}
}

func releaseHistoryMarkers(entry releaseHistoryEntry) string {
	var markers []string
	if entry.Current {
		markers = append(markers, "current")
	}
	if entry.RollbackTarget {
		markers = append(markers, "rollback-target")
	}
	if len(markers) == 0 {
		return ""
	}
	return "[" + strings.Join(markers, ",") + "]"
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func rollbackCmd(opts *options) *cobra.Command {
	var toRelease string
	var allowDataRollback bool
	var allowSecretDrift bool
	cmd := &cobra.Command{
		Use:   "rollback ENV",
		Short: "Apply the previous healthy release",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			ctx := cmd.Context()
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			resolved, env, err := cfg.ResolveEnvironment(args[0])
			if err != nil {
				return err
			}
			cfg = resolved
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
			defer func() {
				if runErr != nil {
					runNotifications(ctx, store, cfg, args[0], "rollback", "failed", release.ID, runErr.Error(), release.Images)
				}
			}()
			var secretFile secrets.ScopedRenderedEnvFiles
			if !opts.dryRun {
				secretOpts, err := secretSourceOptions(opts, args[0])
				if err != nil {
					return err
				}
				secretFile, err = secrets.RenderScopedForEnv(cfg, secretOpts)
				if err != nil {
					return err
				}
				if diff := rollbackSecretDigestDiff(secretFile.Digests, release.SecretDigests); !diff.Empty() && !allowSecretDrift {
					message := rollbackSecretDriftError(diff)
					recordEvent(store, state.Event{Environment: args[0], Kind: "rollback", Status: "blocked", Release: release.ID, Message: message})
					return fmt.Errorf("%s", message)
				}
			}
			if !opts.dryRun {
				operationLock, err := store.AcquireOperationLock(args[0], "rollback")
				if err != nil {
					recordEvent(store, state.Event{Environment: args[0], Kind: "rollback", Status: "blocked", Release: release.ID, Message: err.Error()})
					return err
				}
				defer operationLock.Unlock()
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
			secretEnvFiles, secretWrites, err := serviceSecretEnvFiles(cfg, hosts, args[0], secretFile)
			if err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback_attempt", Status: "failed", Release: release.ID, Message: rollbackAttemptFailureMessage(currentReleaseID, err)})
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback", Status: "failed", Release: release.ID, Message: err.Error()})
				return err
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "rollback_secret_write", Status: "started", Release: release.ID, Message: fmt.Sprintf("writes=%d", len(secretWrites))})
			if err := writeRemoteSecretFiles(ctx, secretWrites); err != nil {
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
			runNotifications(ctx, store, cfg, args[0], "rollback", "succeeded", release.ID, rollbackAttemptMessage(currentReleaseID), release.Images)
			return nil
		},
	}
	cmd.Flags().StringVar(&toRelease, "to", "", "specific healthy release id to apply")
	cmd.Flags().BoolVar(&allowDataRollback, "allow-data-rollback", false, "confirm rollback risk for configured stateful accessories")
	cmd.Flags().BoolVar(&allowSecretDrift, "allow-secret-drift", false, "use currently rendered secrets even when they differ from the target release digests")
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
		case acc.IsPrimary() && acc.Backup.BackupRequired():
			blockers = append(blockers, rollbackBlocker{Accessory: name, Reason: "primary backup-required accessory"})
		case acc.IsPrimary():
			blockers = append(blockers, rollbackBlocker{Accessory: name, Reason: "primary accessory"})
		case acc.Backup.BackupRequired():
			blockers = append(blockers, rollbackBlocker{Accessory: name, Reason: "backup-required accessory"})
		}
	}
	return blockers
}

func rollbackBlockerError(blockers []rollbackBlocker) string {
	messages := rollbackBlockerMessages(blockers)
	return "rollback may be unsafe for stateful data: " + strings.Join(messages, ", ") + "; rerun with --allow-data-rollback after confirming app/data compatibility"
}

func rollbackSecretDigestDiff(local, release map[string]string) secrets.DigestDiff {
	return secrets.Diff(local, releaseSecretDigestsForCurrentScopes(local, release))
}

func releaseSecretDigestsForCurrentScopes(local, release map[string]string) map[string]string {
	if len(release) == 0 {
		return nil
	}
	scoped := false
	for name := range release {
		if strings.Contains(name, ":") {
			scoped = true
			break
		}
	}
	if scoped {
		return release
	}
	expanded := map[string]string{}
	for releaseName, digest := range release {
		matched := false
		for localName := range local {
			if scopedSecretName(localName) == releaseName {
				expanded[localName] = digest
				matched = true
			}
		}
		if !matched {
			expanded[releaseName] = digest
		}
	}
	return expanded
}

func scopedSecretName(scopeName string) string {
	if _, name, ok := strings.Cut(scopeName, ":"); ok {
		return name
	}
	return scopeName
}

func rollbackSecretDriftError(diff secrets.DigestDiff) string {
	messages := secretDiffMessages(diff)
	return "rollback secret drift detected: current secrets do not match target release digests: " + strings.Join(messages, ", ") + "; rerun with --allow-secret-drift to use currently rendered secrets"
}

func secretDiffMessages(diff secrets.DigestDiff) []string {
	var messages []string
	for _, name := range diff.Missing {
		messages = append(messages, "missing "+name)
	}
	for _, name := range diff.Changed {
		messages = append(messages, "changed "+name)
	}
	for _, name := range diff.Extra {
		messages = append(messages, "extra "+name)
	}
	return messages
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
		Args:    ui.ExactArgs(ui.Env),
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
	var fields []ui.HeaderField
	if view.CurrentRelease == nil {
		fields = append(fields, ui.HeaderField{Label: "release", Value: "none"})
	} else {
		fields = append(fields, ui.HeaderField{
			Label:  "release",
			Value:  fmt.Sprintf("%s (%s, healthy=%t)", view.CurrentRelease.ID, view.CurrentRelease.Status, view.CurrentRelease.Healthy),
			Accent: true,
		})
	}
	ui.PrintHeader(w, view.Environment, fields...)
	if len(view.FailedReleases) > 0 {
		ui.PrintSection(w, "Failed releases")
		table := ui.NewTable(w)
		table.SetHeaders("ID", "ERROR")
		for _, release := range view.FailedReleases {
			table.AddRow(release.ID, ui.Dash(release.Error))
		}
		ui.RenderTable(w, table)
	} else {
		ui.PrintNotice(w, "no failed releases")
	}
	if view.RollbackTarget != nil {
		ui.PrintLine(w, "rollback target:", view.RollbackTarget.ID)
	} else if view.RollbackError != "" {
		ui.PrintWarn(w, "rollback target unavailable: "+view.RollbackError)
	}
	if len(view.RollbackBlockers) > 0 {
		ui.PrintSection(w, "Rollback blockers")
		for _, blocker := range view.RollbackBlockers {
			ui.PrintWarn(w, blocker)
		}
	}
	if view.SuggestedCommand != "" {
		ui.PrintLine(w, "suggested rollback:", view.SuggestedCommand)
	}
	if len(view.Events) > 0 {
		ui.PrintSection(w, "Recent failure events")
		renderEventsText(w, view.Events)
	}
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

func printIngressDryRun(w io.Writer, cfg *config.Config, env config.Environment, envName, stateDir string) error {
	placements, err := scheduler.PlaceServices(cfg, env)
	if err != nil {
		return err
	}
	caddyfile := ingress.GenerateCaddyfile(cfg, scheduler.HostsForEnvironment(env), placements)
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
		Args:  ui.RangeArgs(1, 2, ui.Env),
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
		Args:  ui.RangeArgs(1, 2, ui.Env),
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
		Args:  ui.RangeArgs(1, 2, ui.Env),
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
	var logsLines int
	var logsFollow bool
	var logsJSONOutput bool
	logs := &cobra.Command{
		Use:   "logs ENV NAME",
		Short: "Fetch logs from a deployed accessory container",
		Args:  ui.ExactArgs(ui.Env, ui.Accessory),
		RunE: func(cmd *cobra.Command, args []string) error {
			if logsLines <= 0 {
				return fmt.Errorf("--lines must be greater than zero")
			}
			cfg, env, store, err := accessoryContext(opts, args[0])
			if err != nil {
				return err
			}
			if _, ok := cfg.Accessories[args[1]]; !ok {
				return fmt.Errorf("unknown accessory %q", args[1])
			}
			return runAccessoryLogs(cmd.Context(), cmd.OutOrStdout(), opts, cfg, env, args[0], store, args[1], logsLines, logsFollow, logsJSONOutput)
		},
	}
	logs.Flags().IntVar(&logsLines, "lines", 100, "number of log lines to fetch")
	logs.Flags().BoolVar(&logsFollow, "follow", false, "poll logs repeatedly in a short V1 follow loop")
	logs.Flags().BoolVar(&logsJSONOutput, "json", false, "print logs as JSON")
	cmd.AddCommand(logs)
	var execTimeoutSeconds int
	var execJSONOutput bool
	execCmd := &cobra.Command{
		Use:   "exec ENV NAME -- COMMAND",
		Short: "Run a command inside a deployed accessory container",
		Args:  ui.MinimumArgs(3, ui.Env, ui.Accessory, ui.ArgNamed("COMMAND", "command to run inside the container")),
		RunE: func(cmd *cobra.Command, args []string) error {
			if execTimeoutSeconds < 0 {
				return fmt.Errorf("--timeout cannot be negative")
			}
			cfg, env, store, err := accessoryContext(opts, args[0])
			if err != nil {
				return err
			}
			if _, ok := cfg.Accessories[args[1]]; !ok {
				return fmt.Errorf("unknown accessory %q", args[1])
			}
			command := strings.TrimSpace(strings.Join(args[2:], " "))
			if command == "" {
				return fmt.Errorf("command is required")
			}
			return runAccessoryExec(cmd.Context(), cmd.OutOrStdout(), opts, cfg, env, args[0], store, args[1], command, execTimeoutSeconds, execJSONOutput)
		},
	}
	execCmd.Flags().IntVar(&execTimeoutSeconds, "timeout", 0, "command timeout in seconds")
	execCmd.Flags().BoolVar(&execJSONOutput, "json", false, "print exec results as JSON")
	cmd.AddCommand(execCmd)
	var restoreArtifact string
	var restoreYes bool
	restore := &cobra.Command{
		Use:   "restore ENV NAME",
		Short: "Restore one accessory from an explicit backup artifact",
		Args:  ui.ExactArgs(ui.Env, ui.Accessory),
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
		Args:  ui.ExactArgs(ui.Env, ui.Accessory),
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
	results, err := ensureAccessories(ctx, w, opts, cfg, env, envName, store, names, accessoryForceDeploy)
	if err != nil || opts.dryRun || countChangedAccessories(results) == 0 {
		return err
	}
	hosts, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return err
	}
	return restartCurrentServicesAfterAccessoryChange(ctx, w, cfg, envName, store, hosts)
}

func ensureAccessories(ctx context.Context, w io.Writer, opts *options, cfg *config.Config, env config.Environment, envName string, store state.Store, names []string, mode accessoryEnsureMode) ([]accessoryEnsureResult, error) {
	var secretFile secrets.ScopedRenderedEnvFiles
	var err error
	if !opts.dryRun {
		secretOpts, err := secretSourceOptions(opts, envName)
		if err != nil {
			return nil, err
		}
		scopes := make([]string, 0, len(names))
		for _, name := range names {
			scopes = append(scopes, "accessory-"+name)
		}
		secretFile, err = secrets.RenderScopedForEnv(cfg, secretOpts, scopes...)
		if err != nil {
			return nil, err
		}
	}
	hosts, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return nil, err
	}
	observed := map[string][]accessoryObservation{}
	if !opts.dryRun && len(names) > 0 {
		observed, err = collectAccessoryObservations(ctx, cfg, hosts, envName, names)
		if err != nil {
			return nil, err
		}
	}
	results := make([]accessoryEnsureResult, 0, len(names))
	for _, name := range names {
		acc := cfg.Accessories[name]
		if err := accessory.ValidateDeploy(acc); err != nil {
			return nil, fmt.Errorf("accessory %q: %w", name, err)
		}
		placement, err := accessory.PlacementForHosts(cfg, hosts, envName, name, store)
		if err != nil {
			return nil, err
		}
		if opts.dryRun {
			if mode == accessoryEnsureOnly {
				fmt.Fprintf(w, "would ensure accessory %s on %s image=%s\n", name, placement.Host.Name, acc.Image)
			} else {
				fmt.Fprintf(w, "would deploy accessory %s on %s image=%s\n", name, placement.Host.Name, acc.Image)
			}
			results = append(results, accessoryEnsureResult{Name: name, Host: placement.Host, Changed: true})
			continue
		}
		containerName := accessory.ContainerName(cfg.Project, envName, name)
		if err := validateSingleAccessory(name, placement, containerName, observed[name]); err != nil {
			return nil, err
		}
		if mode == accessoryEnsureOnly && accessoryObservationRunning(containerName, observed[name]) {
			if !placement.Persisted {
				placement, err = accessory.EnsurePlacementForHosts(cfg, hosts, envName, name, store, deployNow())
				if err != nil {
					return nil, err
				}
			}
			fmt.Fprintf(w, "accessory %s already running on %s image=%s\n", name, placement.Host.Name, acc.Image)
			results = append(results, accessoryEnsureResult{Name: name, Host: placement.Host})
			continue
		}
		placement, err = accessory.EnsurePlacementForHosts(cfg, hosts, envName, name, store, deployNow())
		if err != nil {
			return nil, err
		}
		client := newDeployAgent(placement.Host)
		secretEnvFile, secretContent := accessorySecretEnvFile(envName, name, secretFile)
		if err := writeRemoteSecretFile(ctx, placement.Host, secretEnvFile, secretContent); err != nil {
			return nil, err
		}
		if err := syncRemoteRegistryAuth(ctx, newDeployDocker(), []scheduler.Host{placement.Host}, []string{acc.Image}); err != nil {
			return nil, fmt.Errorf("write registry auth for accessory %s on %s: %w", name, placement.Host.Name, err)
		}
		if err := client.Call(ctx, "pull", map[string]string{"image": acc.Image}, nil); err != nil {
			return nil, fmt.Errorf("pull accessory %s on %s: %w", name, placement.Host.Name, err)
		}
		networkName := deployment.DockerNetworkName(cfg, envName)
		if err := ensureManagedDockerNetwork(ctx, client, cfg, envName); err != nil {
			return nil, fmt.Errorf("ensure network %s for accessory %s on %s: %w", networkName, name, placement.Host.Name, err)
		}
		for _, volume := range accessory.NamedVolumes(acc) {
			params := agent.EnsureVolumeParams{Name: volume, Owner: acc.VolumeOwner}
			if err := client.Call(ctx, "ensure_volume", params, nil); err != nil {
				return nil, fmt.Errorf("ensure volume %s for accessory %s on %s: %w", volume, name, placement.Host.Name, err)
			}
		}
		params := agent.RunContainerParams{
			Name:           containerName,
			Image:          acc.Image,
			Command:        acc.Command,
			Args:           accessory.DockerArgs(acc, secretEnvFile),
			Labels:         accessory.ContainerLabels(cfg.Project, envName, name, acc.Labels),
			Network:        networkName,
			NetworkAliases: accessory.NetworkAliases(name, acc),
		}
		if err := client.Call(ctx, "run_container", params, nil); err != nil {
			return nil, fmt.Errorf("deploy accessory %s on %s: %w", name, placement.Host.Name, err)
		}
		if mode == accessoryEnsureOnly {
			fmt.Fprintf(w, "ensured accessory %s on %s image=%s\n", name, placement.Host.Name, acc.Image)
		} else {
			fmt.Fprintf(w, "deployed accessory %s on %s image=%s\n", name, placement.Host.Name, acc.Image)
		}
		results = append(results, accessoryEnsureResult{Name: name, Host: placement.Host, Changed: true})
	}
	return results, nil
}

func accessoryObservationRunning(containerName string, observed []accessoryObservation) bool {
	for _, item := range observed {
		if item.Container.Names == containerName && strings.HasPrefix(item.Container.Status, "Up ") {
			return true
		}
	}
	return false
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
	ui.PrintHeader(w, envName)
	table := ui.NewTable(w)
	table.SetHeaders("ACCESSORY", "PLACEMENT", "HOST", "IMAGE", "STATUS")
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
			table.AddRow(name, placement, "-", "-", "missing")
		case 1:
			item := items[0]
			table.AddRow(name, placement, item.Host.Name, ui.Dash(item.Container.Image), item.Container.Status)
		default:
			table.AddRow(name, placement, observationHosts(items), "-", "replicated")
		}
	}
	ui.RenderTable(w, table)
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
		exportedArtifact := ""
		exportOutput := ""
		exportCommand, err := accessory.BackupExportCommand(acc, artifact)
		if err != nil {
			return fail(err)
		}
		if exportCommand != "" {
			recordEvent(store, state.Event{Environment: envName, Kind: "accessory_backup_export", Status: "started", Accessory: name, Host: host.Name, Message: artifact})
			var exportResult agent.CommandResult
			if err := newDeployAgent(host).Call(ctx, "accessory_backup", agent.AccessoryCommandParams{
				Name:           name,
				Command:        exportCommand,
				TimeoutSeconds: accessory.BackupExportTimeoutSeconds(acc),
			}, &exportResult); err != nil {
				recordEvent(store, state.Event{Environment: envName, Kind: "accessory_backup_export", Status: "failed", Accessory: name, Host: host.Name, Message: err.Error()})
				return fail(fmt.Errorf("export backup accessory %s on %s: %w", name, host.Name, err))
			}
			exportOutput = exportResult.Output
			exportedArtifact = firstNonEmptyLine(exportResult.Output)
			recordEvent(store, state.Event{Environment: envName, Kind: "accessory_backup_export", Status: "succeeded", Accessory: name, Host: host.Name, Message: exportedArtifact})
		}
		if _, err := store.RecordAccessoryBackup(envName, name, state.AccessoryBackup{
			Artifact:         artifact,
			ExportedArtifact: exportedArtifact,
			Host:             host.Name,
			Output:           result.Output,
			ExportOutput:     exportOutput,
			CreatedAt:        deployNow().UTC(),
		}); err != nil {
			return fail(err)
		}
		recordEvent(store, state.Event{Environment: envName, Kind: "accessory_backup", Status: "succeeded", Accessory: name, Host: host.Name, Message: artifact})
		if exportedArtifact != "" {
			fmt.Fprintf(w, "backed up accessory %s on %s artifact=%s exported=%s\n", name, host.Name, artifact, exportedArtifact)
		} else {
			fmt.Fprintf(w, "backed up accessory %s on %s artifact=%s\n", name, host.Name, artifact)
		}
	}
	return nil
}

func runAccessoryLogs(ctx context.Context, w io.Writer, opts *options, cfg *config.Config, env config.Environment, envName string, store state.Store, name string, lines int, follow bool, jsonOutput bool) error {
	hosts, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return err
	}
	placement, err := accessory.PlacementForHosts(cfg, hosts, envName, name, store)
	if err != nil {
		return err
	}
	if !placement.Persisted {
		return fmt.Errorf("accessory %q is not deployed; run accessory deploy first", name)
	}
	containerName := accessory.ContainerName(cfg.Project, envName, name)
	view := logsView{
		Environment: envName,
		Accessory:   name,
		Lines:       lines,
		Follow:      follow,
	}
	if opts.dryRun {
		entry := logsEntry{
			Iteration: 1,
			Host:      placement.Host.Name,
			Accessory: name,
			Container: containerName,
			Logs:      "dry-run: logs would be fetched over SSH",
		}
		view.Entries = append(view.Entries, entry)
		if jsonOutput {
			return writeJSON(w, view)
		}
		fmt.Fprintf(w, "would fetch logs for accessory %s on %s container=%s lines=%d\n", name, placement.Host.Name, containerName, lines)
		return nil
	}
	observed, err := collectAccessoryObservations(ctx, cfg, hosts, envName, []string{name})
	if err != nil {
		return err
	}
	if err := validatePlacedAccessory(name, placement, containerName, observed[name]); err != nil {
		return err
	}
	polls := 1
	if follow {
		polls = logsFollowPolls
	}
	for iteration := 1; iteration <= polls; iteration++ {
		if iteration > 1 {
			timer := time.NewTimer(logsFollowInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				if jsonOutput {
					return writeJSON(w, view)
				}
				return ctx.Err()
			case <-timer.C:
			}
		}
		var out map[string]string
		if err := newDeployAgent(placement.Host).Call(ctx, "logs", agent.LogsParams{Name: containerName, Lines: lines}, &out); err != nil {
			return fmt.Errorf("logs accessory %s on %s: %w", name, placement.Host.Name, err)
		}
		entry := logsEntry{
			Iteration: iteration,
			Host:      placement.Host.Name,
			Accessory: name,
			Container: containerName,
			Logs:      out["logs"],
		}
		view.Entries = append(view.Entries, entry)
		if !jsonOutput {
			fmt.Fprintf(w, "==> %s/%s <==\n%s\n", placement.Host.Name, containerName, entry.Logs)
		}
	}
	if jsonOutput {
		return writeJSON(w, view)
	}
	return nil
}

func runAccessoryExec(ctx context.Context, w io.Writer, opts *options, cfg *config.Config, env config.Environment, envName string, store state.Store, name, command string, timeoutSeconds int, jsonOutput bool) error {
	hosts, err := resolvedHostsForEnvironment(store, envName, env)
	if err != nil {
		return err
	}
	placement, err := accessory.PlacementForHosts(cfg, hosts, envName, name, store)
	if err != nil {
		return err
	}
	if !placement.Persisted {
		return fmt.Errorf("accessory %q is not deployed; run accessory deploy first", name)
	}
	containerName := accessory.ContainerName(cfg.Project, envName, name)
	view := execView{
		Environment: envName,
		Accessory:   name,
		Command:     command,
	}
	if opts.dryRun {
		entry := execEntry{
			Host:      placement.Host.Name,
			Accessory: name,
			Container: containerName,
			Output:    "dry-run",
		}
		view.Entries = append(view.Entries, entry)
		recordEvent(store, state.Event{Environment: envName, Kind: "accessory_exec", Status: "planned", Accessory: name, Host: placement.Host.Name, Message: command})
		if jsonOutput {
			return writeJSON(w, view)
		}
		fmt.Fprintf(w, "would exec accessory %s on %s container=%s command=%q\n", name, placement.Host.Name, containerName, command)
		return nil
	}
	observed, err := collectAccessoryObservations(ctx, cfg, hosts, envName, []string{name})
	if err != nil {
		return err
	}
	if err := validatePlacedAccessory(name, placement, containerName, observed[name]); err != nil {
		return err
	}
	var result agent.CommandResult
	params := agent.ExecContainerParams{
		Name:           containerName,
		Command:        command,
		TimeoutSeconds: timeoutSeconds,
	}
	recordEvent(store, state.Event{Environment: envName, Kind: "accessory_exec", Status: "started", Accessory: name, Host: placement.Host.Name, Message: command})
	if err := newDeployAgent(placement.Host).Call(ctx, "exec_container", params, &result); err != nil {
		recordEvent(store, state.Event{Environment: envName, Kind: "accessory_exec", Status: "failed", Accessory: name, Host: placement.Host.Name, Message: err.Error()})
		return fmt.Errorf("exec accessory %s on %s: %w", name, placement.Host.Name, err)
	}
	entry := execEntry{
		Host:      placement.Host.Name,
		Accessory: name,
		Container: containerName,
		Output:    result.Output,
	}
	view.Entries = append(view.Entries, entry)
	recordEvent(store, state.Event{Environment: envName, Kind: "accessory_exec", Status: "succeeded", Accessory: name, Host: placement.Host.Name})
	if jsonOutput {
		return writeJSON(w, view)
	}
	fmt.Fprintf(w, "==> %s/%s <==\n%s\n", placement.Host.Name, containerName, result.Output)
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

	result, err := startAccessoryWithRestore(ctx, opts, cfg, envName, name, target, artifact)
	if err != nil {
		return fail(err)
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

// startAccessoryWithRestore starts an accessory container on the target host
// and restores it from the given backup artifact, which must already exist on
// the target. Shared by accessory failover and host migration.
func startAccessoryWithRestore(ctx context.Context, opts *options, cfg *config.Config, envName, name string, target scheduler.Host, artifact string) (agent.CommandResult, error) {
	acc := cfg.Accessories[name]
	containerName := accessory.ContainerName(cfg.Project, envName, name)
	secretOpts, err := secretSourceOptions(opts, envName)
	if err != nil {
		return agent.CommandResult{}, err
	}
	secretFile, err := secrets.RenderScopedForEnv(cfg, secretOpts)
	if err != nil {
		return agent.CommandResult{}, err
	}
	targetClient := newDeployAgent(target)
	secretEnvFile, secretContent := accessorySecretEnvFile(envName, name, secretFile)
	if err := writeRemoteSecretFile(ctx, target, secretEnvFile, secretContent); err != nil {
		return agent.CommandResult{}, err
	}
	if err := syncRemoteRegistryAuth(ctx, newDeployDocker(), []scheduler.Host{target}, []string{acc.Image}); err != nil {
		return agent.CommandResult{}, fmt.Errorf("write registry auth for accessory %s on %s: %w", name, target.Name, err)
	}
	if err := targetClient.Call(ctx, "pull", map[string]string{"image": acc.Image}, nil); err != nil {
		return agent.CommandResult{}, fmt.Errorf("pull accessory %s on %s: %w", name, target.Name, err)
	}
	networkName := deployment.DockerNetworkName(cfg, envName)
	if err := ensureManagedDockerNetwork(ctx, targetClient, cfg, envName); err != nil {
		return agent.CommandResult{}, fmt.Errorf("ensure network %s for accessory %s on %s: %w", networkName, name, target.Name, err)
	}
	for _, volume := range accessory.NamedVolumes(acc) {
		params := agent.EnsureVolumeParams{Name: volume, Owner: acc.VolumeOwner}
		if err := targetClient.Call(ctx, "ensure_volume", params, nil); err != nil {
			return agent.CommandResult{}, fmt.Errorf("ensure volume %s for accessory %s on %s: %w", volume, name, target.Name, err)
		}
	}
	params := agent.RunContainerParams{
		Name:           containerName,
		Image:          acc.Image,
		Command:        acc.Command,
		Args:           accessory.DockerArgs(acc, secretEnvFile),
		Labels:         accessory.ContainerLabels(cfg.Project, envName, name, acc.Labels),
		Network:        networkName,
		NetworkAliases: accessory.NetworkAliases(name, acc),
	}
	if err := targetClient.Call(ctx, "run_container", params, nil); err != nil {
		return agent.CommandResult{}, fmt.Errorf("start accessory %s on %s: %w", name, target.Name, err)
	}
	checkCommand, err := accessory.RestoreCheckCommand(acc, envName, name, artifact)
	if err != nil {
		return agent.CommandResult{}, err
	}
	var check agent.HealthCheckResult
	if err := targetClient.Call(ctx, "health_check", agent.HealthCheckParams{Command: checkCommand, TimeoutSeconds: 30}, &check); err != nil {
		return agent.CommandResult{}, fmt.Errorf("verify backup artifact for accessory %s on %s: %w", name, target.Name, err)
	}
	if !check.OK {
		return agent.CommandResult{}, fmt.Errorf("verify backup artifact for accessory %s on %s failed", name, target.Name)
	}
	restoreCommand, err := accessory.RestoreCommand(acc, artifact)
	if err != nil {
		return agent.CommandResult{}, err
	}
	var result agent.CommandResult
	if err := targetClient.Call(ctx, "accessory_restore", agent.AccessoryCommandParams{
		Name:           name,
		Command:        restoreCommand,
		TimeoutSeconds: accessory.BackupTimeoutSeconds(acc),
	}, &result); err != nil {
		return agent.CommandResult{}, fmt.Errorf("restore accessory %s on %s: %w", name, target.Name, err)
	}
	return result, nil
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
	cmd := &cobra.Command{Use: "secrets", Short: "Manage and verify Ship secrets"}
	var initRecipient string
	initCmd := &cobra.Command{
		Use:   "init ENV",
		Short: "Create an encrypted secret store for an environment",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			secretOpts, err := secretSourceOptions(opts, args[0])
			if err != nil {
				return err
			}
			if err := secrets.InitStore(secretOpts, initRecipient); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created %s\n", secrets.StorePath(secretOpts))
			return nil
		},
	}
	initCmd.Flags().StringVar(&initRecipient, "recipient", "", "age recipient for encrypting this environment's secrets")
	_ = initCmd.MarkFlagRequired("recipient")
	cmd.AddCommand(initCmd)

	var setValue string
	setCmd := &cobra.Command{
		Use:   "set ENV NAME",
		Short: "Set a secret in the encrypted store",
		Args:  ui.ExactArgs(ui.Env, ui.Secret),
		RunE: func(cmd *cobra.Command, args []string) error {
			secretOpts, err := secretSourceOptions(opts, args[0])
			if err != nil {
				return err
			}
			value := setValue
			if value == "" {
				var ok bool
				value, ok = os.LookupEnv(args[1])
				if !ok {
					return fmt.Errorf("missing --value and environment variable %s", args[1])
				}
			}
			if err := secrets.SetStoredSecret(secretOpts, "", args[1], value); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "set %s in %s\n", args[1], secrets.StorePath(secretOpts))
			return nil
		},
	}
	setCmd.Flags().StringVar(&setValue, "value", "", "secret value; defaults to environment variable NAME")
	cmd.AddCommand(setCmd)

	unsetCmd := &cobra.Command{
		Use:   "unset ENV NAME",
		Short: "Remove a secret from the encrypted store",
		Args:  ui.ExactArgs(ui.Env, ui.Secret),
		RunE: func(cmd *cobra.Command, args []string) error {
			secretOpts, err := secretSourceOptions(opts, args[0])
			if err != nil {
				return err
			}
			if err := secrets.UnsetStoredSecret(secretOpts, "", args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "unset %s in %s\n", args[1], secrets.StorePath(secretOpts))
			return nil
		},
	}
	cmd.AddCommand(unsetCmd)

	cmd.AddCommand(&cobra.Command{
		Use:   "list ENV",
		Short: "List encrypted store secret names",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			secretOpts, err := secretSourceOptions(opts, args[0])
			if err != nil {
				return err
			}
			values, err := secrets.ReadStore(secretOpts)
			if err != nil {
				return err
			}
			names := make([]string, 0, len(values))
			for name := range values {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				fmt.Fprintln(cmd.OutOrStdout(), name)
			}
			return nil
		},
	})

	var exportRedacted bool
	exportCmd := &cobra.Command{
		Use:   "export ENV",
		Short: "Export encrypted store secrets",
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			secretOpts, err := secretSourceOptions(opts, args[0])
			if err != nil {
				return err
			}
			values, err := secrets.ReadStore(secretOpts)
			if err != nil {
				return err
			}
			names := make([]string, 0, len(values))
			for name := range values {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				value := values[name]
				if exportRedacted {
					value = "<redacted:" + secrets.Digest(value) + ">"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", name, value)
			}
			return nil
		},
	}
	exportCmd.Flags().BoolVar(&exportRedacted, "redacted", false, "redact values and show digests")
	cmd.AddCommand(exportCmd)

	cmd.AddCommand(&cobra.Command{
		Use:   "verify [ENV]",
		Short: "Check required secrets exist",
		Args:  cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			var checks []secrets.Check
			if len(args) > 0 {
				resolved, _, err := cfg.ResolveEnvironment(args[0])
				if err != nil {
					return err
				}
				secretOpts, err := secretSourceOptions(opts, args[0])
				if err != nil {
					return err
				}
				checks, err = secrets.VerifyForEnv(resolved, secretOpts)
			} else {
				checks, err = secrets.Verify(cfg)
			}
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
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			resolved, _, err := cfg.ResolveEnvironment(args[0])
			if err != nil {
				return err
			}
			secretOpts, err := secretSourceOptions(opts, args[0])
			if err != nil {
				return err
			}
			rendered, err := secrets.RenderScopedForEnv(resolved, secretOpts)
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
		Args:  ui.ExactArgs(ui.Env),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !renderDryRun && !opts.dryRun {
				return fmt.Errorf("secrets render only supports --dry-run in V1")
			}
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			resolved, env, err := cfg.ResolveEnvironment(args[0])
			if err != nil {
				return err
			}
			secretOpts, err := secretSourceOptions(opts, args[0])
			if err != nil {
				return err
			}
			rendered, err := secrets.RenderScopedForEnv(resolved, secretOpts)
			if err != nil {
				return err
			}
			if len(rendered.Scopes) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no required secrets")
				return nil
			}
			for _, scope := range secretRenderScopes(resolved, env, args[0]) {
				file, ok := rendered.Scopes[strings.TrimSuffix(filepath.Base(scope), ".env")]
				if !ok {
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "# %s\n%s\n", scope, file.Redacted)
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

// systemdUnit is an install marker, not a daemon: Ship's RPC is served by
// execing `ship agent rpc` over each SSH session, so there is no long-running
// process to supervise. `ship agent run` prints a notice and exits 0, which
// under Type=oneshot + RemainAfterExit leaves the unit "active (exited)" —
// a Restart=always unit here would loop forever.
func systemdUnit() string {
	return `[Unit]
Description=Ship node agent (RPC served on demand via SSH; no daemon)
Documentation=https://github.com/watzon/ship
After=docker.service
Requires=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=` + config.RemoteBinaryPath + ` agent run

[Install]
WantedBy=multi-user.target`
}
