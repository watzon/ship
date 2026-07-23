package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/watzon/ship/internal/accessory"
	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/cli/ui"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/deployment"
	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/secrets"
	"github.com/watzon/ship/internal/state"
)

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

func countChangedAccessories(results []accessoryEnsureResult) int {
	var changed int
	for _, result := range results {
		if result.Changed {
			changed++
		}
	}
	return changed
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
