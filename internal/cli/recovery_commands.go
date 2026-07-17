package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/watzon/ship/internal/accessory"
	"github.com/watzon/ship/internal/cli/ui"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/deployment"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/secrets"
	"github.com/watzon/ship/internal/state"
)

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
			var cleanupWarnings []deployment.CleanupWarning
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
					var execution deployment.ExecutionResult
					execution, err = deployment.ExecuteActionsWithResult(ctx, actions, agentFor, nil)
					cleanupWarnings = execution.CleanupWarnings
				}
			} else {
				var rollout deployment.RolloutResult
				rollout, err = deployment.RolloutWithResult(ctx, deployment.RolloutOptions{
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
				actions = rollout.Actions
				cleanupWarnings = rollout.CleanupWarnings
			}
			if err != nil {
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback_rollout", Status: "failed", Release: release.ID, Message: err.Error()})
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback_attempt", Status: "failed", Release: release.ID, Message: rollbackAttemptFailureMessage(currentReleaseID, err)})
				recordEvent(store, state.Event{Environment: args[0], Kind: "rollback", Status: "failed", Release: release.ID, Message: err.Error()})
				return err
			}
			recordEvent(store, state.Event{Environment: args[0], Kind: "rollback_rollout", Status: "succeeded", Release: release.ID})
			recordRolloutCleanupWarnings(cmd.OutOrStdout(), store, args[0], "rollback_cleanup", release.ID, cleanupWarnings)
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
