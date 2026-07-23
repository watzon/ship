package cli

import (
	"context"
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
	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/doctor"
	"github.com/watzon/ship/internal/ingress"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/secrets"
	"github.com/watzon/ship/internal/state"
)

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
