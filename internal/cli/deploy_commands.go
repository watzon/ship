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
	"github.com/watzon/ship/internal/accessory"
	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/cli/ui"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/deployment"
	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/ingress"
	"github.com/watzon/ship/internal/planner"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/secrets"
	"github.com/watzon/ship/internal/state"
)

type deployDocker interface {
	BuildImage(ctx context.Context, opts docker.BuildOptions) error
	Push(ctx context.Context, image string) error
	ResolveDigest(ctx context.Context, image string) (string, error)
	RegistryAuth(ctx context.Context, registry string) (docker.RegistryAuth, bool, error)
}

var newDeployDocker = func() deployDocker {
	return docker.Client{}
}

var deployGitRevision = docker.GitShortSHA

var newReleaseID = docker.NewReleaseID

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
				rendered, err := secrets.RenderForEnv(cfg, secretOpts)
				if err != nil {
					return err
				}
				printProcessEnvStoreOverrideWarning(cmd.OutOrStdout(), rendered.ProcessEnvStoreOverrides)
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
			printProcessEnvStoreOverrideWarning(cmd.OutOrStdout(), secretFiles.ProcessEnvStoreOverrides)
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
			rollout, err := deployment.RolloutWithResult(ctx, deployment.RolloutOptions{
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
			actions := rollout.Actions
			recordEvent(store, state.Event{Environment: args[0], Kind: "deploy_rollout", Status: "succeeded", Release: releaseID})
			recordRolloutCleanupWarnings(cmd.OutOrStdout(), store, args[0], "deploy_cleanup", releaseID, rollout.CleanupWarnings)
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
			rollout, err := deployment.RolloutWithResult(ctx, deployment.RolloutOptions{
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
			actions := rollout.Actions
			recordEvent(store, state.Event{Environment: targetEnv, Kind: "promote_rollout", Status: "succeeded", Release: releaseID})
			recordRolloutCleanupWarnings(cmd.OutOrStdout(), store, targetEnv, "promote_cleanup", releaseID, rollout.CleanupWarnings)
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
