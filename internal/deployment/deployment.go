package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/fsatomic"
	"github.com/watzon/ship/internal/ingress"
	"github.com/watzon/ship/internal/scheduler"
)

const (
	ActionPull            ActionKind = "pull"
	ActionStart           ActionKind = "start"
	ActionHealth          ActionKind = "health"
	ActionIngress         ActionKind = "ingress"
	ActionDrain           ActionKind = "drain"
	ActionCanary          ActionKind = "canary_pause"
	ActionStop            ActionKind = "stop"
	ActionPreserveStop    ActionKind = "preserve_stop"
	ActionRemovePreserved ActionKind = "remove_preserved"
)

const defaultHealthTimeoutSeconds = 30
const defaultHealthRetryIntervalSeconds = 2

type ActionKind string

type Agent interface {
	Call(ctx context.Context, method string, params any, out any) error
}

type AgentFactory func(scheduler.Host) Agent

type ObservedContainer struct {
	Host      scheduler.Host
	Container docker.ContainerSummary
}

type Action struct {
	Kind              ActionKind
	Host              scheduler.Host
	Service           string
	Replica           int
	Release           string
	ContainerName     string
	Image             string
	Command           string
	Args              []string
	Labels            map[string]string
	Network           string
	NetworkDriver     string
	NetworkAliases    []string
	Health            agent.HealthCheckParams
	HealthRetries     int
	HealthInterval    time.Duration
	IngressPath       string
	IngressConfig     string
	IngressHosts      []scheduler.Host
	CaddyImage        string
	CaddyName         string
	CaddyDataVolume   string
	CaddyConfigVolume string
	CaddyLabels       map[string]string
	DrainTimeout      time.Duration
	PauseDuration     time.Duration
}

type PlanInput struct {
	Config         *config.Config
	Environment    config.Environment
	Hosts          []scheduler.Host
	EnvName        string
	ReleaseID      string
	Images         map[string]string
	Observed       []ObservedContainer
	StateDir       string
	SecretEnvFiles map[string]string
}

type FixedPortRollbackConflict struct {
	Host                scheduler.Host
	Service             string
	Replica             int
	Ports               []int
	ContainerName       string
	ContainerRelease    string
	TargetContainerName string
}

type RolloutOptions struct {
	Config         *config.Config
	Environment    config.Environment
	Hosts          []scheduler.Host
	EnvName        string
	ReleaseID      string
	Images         map[string]string
	StateDir       string
	SecretEnvFiles map[string]string
	AgentFor       AgentFactory
	Sleep          func(time.Duration)
}

// CleanupWarning reports a preserved container that could not be removed after commit.
type CleanupWarning struct {
	Host          scheduler.Host
	ContainerName string
	Err           error
}

func (w CleanupWarning) Error() string {
	return fmt.Sprintf("remove preserved container %s on %s: %v", w.ContainerName, w.Host.Name, w.Err)
}

// ExecutionResult reports non-fatal work left after a committed action stream.
type ExecutionResult struct {
	CleanupWarnings []CleanupWarning
}

// RolloutResult contains the planned actions and any post-commit cleanup warnings.
type RolloutResult struct {
	Actions         []Action
	CleanupWarnings []CleanupWarning
}

func inputHosts(env config.Environment, hosts []scheduler.Host) []scheduler.Host {
	if len(hosts) > 0 {
		return append([]scheduler.Host(nil), hosts...)
	}
	return scheduler.HostsForEnvironment(env)
}

func placementsForInput(input PlanInput) ([]scheduler.Placement, error) {
	hosts := inputHosts(input.Environment, input.Hosts)
	return scheduler.PlaceServicesOnHosts(input.Config, hosts)
}

// RolloutWithResult executes a rollout and preserves post-commit cleanup warnings.
func RolloutWithResult(ctx context.Context, opts RolloutOptions) (RolloutResult, error) {
	hosts := inputHosts(opts.Environment, opts.Hosts)
	observed, err := InspectObservedOnHosts(ctx, hosts, opts.AgentFor)
	if err != nil {
		return RolloutResult{}, err
	}
	actions, err := BuildActions(PlanInput{
		Config:         opts.Config,
		Environment:    opts.Environment,
		Hosts:          hosts,
		EnvName:        opts.EnvName,
		ReleaseID:      opts.ReleaseID,
		Images:         opts.Images,
		Observed:       observed,
		StateDir:       opts.StateDir,
		SecretEnvFiles: opts.SecretEnvFiles,
	})
	if err != nil {
		return RolloutResult{}, err
	}
	execution, err := ExecuteActionsWithResult(ctx, actions, opts.AgentFor, opts.Sleep)
	result := RolloutResult{Actions: actions, CleanupWarnings: execution.CleanupWarnings}
	if err != nil {
		return result, err
	}
	return result, nil
}

func InspectObserved(ctx context.Context, env config.Environment, agentFor AgentFactory) ([]ObservedContainer, error) {
	return InspectObservedOnHosts(ctx, scheduler.HostsForEnvironment(env), agentFor)
}

func InspectObservedOnHosts(ctx context.Context, hosts []scheduler.Host, agentFor AgentFactory) ([]ObservedContainer, error) {
	if agentFor == nil {
		return nil, errors.New("agent factory is required")
	}
	var observed []ObservedContainer
	for _, host := range hosts {
		var containers []docker.ContainerSummary
		if err := agentFor(host).Call(ctx, "list_ship_containers", map[string]any{}, &containers); err != nil {
			return nil, fmt.Errorf("inspect observed containers on %s: %w", host.Name, err)
		}
		for _, container := range containers {
			observed = append(observed, ObservedContainer{Host: host, Container: container})
		}
	}
	return observed, nil
}

func BuildActions(input PlanInput) ([]Action, error) {
	if input.Config == nil {
		return nil, errors.New("config is required")
	}
	placements, err := placementsForInput(input)
	if err != nil {
		return nil, err
	}
	desiredNames := map[string]struct{}{}
	preStopped := map[string]struct{}{}
	pendingStops := map[string][]ObservedContainer{}
	surgeCounts := map[string]int{}
	startedCounts := map[string]int{}
	desiredCounts := desiredServiceCounts(placements)
	var actions []Action
	networkName := DockerNetworkName(input.Config, input.EnvName)
	networkDriver := DockerNetworkDriver(input.Config)
	for _, placement := range placements {
		svc := input.Config.Services[placement.Service]
		image := input.Images[placement.Service]
		if strings.TrimSpace(image) == "" {
			return nil, fmt.Errorf("missing image for service %q", placement.Service)
		}
		name := ContainerName(input.Config.Project, input.EnvName, placement.Service, placement.Replica, input.ReleaseID)
		desiredNames[name] = struct{}{}
		labels := ContainerLabels(input.Config.Project, input.EnvName, placement.Service, placement.Replica, input.ReleaseID, svc.Labels)
		args := DockerArgs(svc, input.SecretEnvFiles[placement.Service])
		networkAliases := ServiceNetworkAliases(placement.Service, svc)
		actions = append(actions, Action{
			Kind:          ActionPull,
			Host:          placement.Host,
			Service:       placement.Service,
			Replica:       placement.Replica,
			Release:       input.ReleaseID,
			ContainerName: name,
			Image:         image,
		})
		if usesFixedHostPorts(svc) {
			for _, old := range replacementStopCandidates(input.Config, input.EnvName, placement, name, input.Observed) {
				actions = appendPreserveStopActions(actions, old, svc)
				preStopped[observedKey(old)] = struct{}{}
			}
		} else {
			replacements := replacementStopCandidates(input.Config, input.EnvName, placement, name, input.Observed)
			if len(replacements) > 0 && rollingMaxSurge(svc) == 0 {
				if rollingMaxUnavailable(svc) == 0 {
					return nil, fmt.Errorf("service %q rolling strategy cannot have max_surge=0 and max_unavailable=0", placement.Service)
				}
				for _, old := range replacements {
					actions = appendPreserveStopActions(actions, old, svc)
					preStopped[observedKey(old)] = struct{}{}
				}
			} else {
				pendingStops[placement.Service] = append(pendingStops[placement.Service], replacements...)
			}
		}
		actions = append(actions, Action{
			Kind:           ActionStart,
			Host:           placement.Host,
			Service:        placement.Service,
			Replica:        placement.Replica,
			Release:        input.ReleaseID,
			ContainerName:  name,
			Image:          image,
			Command:        svc.Command,
			Args:           args,
			Labels:         labels,
			Network:        networkName,
			NetworkDriver:  networkDriver,
			NetworkAliases: networkAliases,
		})
		health, ok, err := HealthCheck(svc, name)
		if err != nil {
			return nil, fmt.Errorf("service %q health check: %w", placement.Service, err)
		}
		if ok {
			actions = append(actions, Action{
				Kind:           ActionHealth,
				Host:           placement.Host,
				Service:        placement.Service,
				Replica:        placement.Replica,
				Release:        input.ReleaseID,
				ContainerName:  name,
				Health:         health,
				HealthRetries:  svc.Rolling.HealthRetries,
				HealthInterval: HealthRetryInterval(svc),
			})
		}
		startedCounts[placement.Service]++
		actions = appendCanaryPauseAction(actions, placement, input.ReleaseID, svc, startedCounts[placement.Service], desiredCounts[placement.Service])
		if len(pendingStops[placement.Service]) > 0 {
			surgeCounts[placement.Service]++
			if surgeCounts[placement.Service] >= rollingMaxSurge(svc) {
				actions = flushPendingStops(actions, pendingStops, surgeCounts, placement.Service, svc, preStopped)
			}
		}
	}
	serviceNames := make([]string, 0, len(pendingStops))
	for serviceName := range pendingStops {
		serviceNames = append(serviceNames, serviceName)
	}
	sort.Strings(serviceNames)
	for _, serviceName := range serviceNames {
		actions = flushPendingStops(actions, pendingStops, surgeCounts, serviceName, input.Config.Services[serviceName], preStopped)
	}
	stateDir := strings.TrimSpace(input.StateDir)
	if stateDir == "" {
		stateDir = config.LocalStateDir
	}
	ingressPath := filepath.Join(stateDir, "ingress", input.EnvName+".Caddyfile")
	caddyfile := ingress.GenerateCaddyfile(input.Config, inputHosts(input.Environment, input.Hosts), placements)
	if strings.TrimSpace(caddyfile) != "" {
		actions = append(actions, Action{
			Kind:              ActionIngress,
			IngressPath:       ingressPath,
			IngressConfig:     caddyfile,
			IngressHosts:      ingress.HostsFor(input.Config, inputHosts(input.Environment, input.Hosts), placements),
			CaddyImage:        caddyImage(input.Config),
			CaddyName:         CaddyContainerName(input.Config.Project, input.EnvName),
			CaddyDataVolume:   caddyDataVolume(input.Config, input.EnvName),
			CaddyConfigVolume: caddyConfigVolume(input.Config, input.EnvName),
			CaddyLabels:       CaddyLabels(input.Config.Project, input.EnvName),
			Network:           networkName,
			NetworkDriver:     networkDriver,
		})
	} else if hasObservedCaddyContainer(input.Config, input.EnvName, input.Observed) {
		actions = append(actions, Action{
			Kind:              ActionIngress,
			IngressPath:       ingressPath,
			IngressHosts:      clearIngressHosts(input),
			CaddyImage:        caddyImage(input.Config),
			CaddyName:         CaddyContainerName(input.Config.Project, input.EnvName),
			CaddyDataVolume:   caddyDataVolume(input.Config, input.EnvName),
			CaddyConfigVolume: caddyConfigVolume(input.Config, input.EnvName),
			CaddyLabels:       CaddyLabels(input.Config.Project, input.EnvName),
			Network:           networkName,
			NetworkDriver:     networkDriver,
		})
	}
	for _, old := range stopCandidates(input.Config, input.EnvName, input.Observed, desiredNames) {
		if _, ok := preStopped[observedKey(old)]; ok {
			continue
		}
		serviceName := old.Container.Labels[docker.LabelService]
		actions = appendStopActions(actions, old, input.Config.Services[serviceName])
	}
	preCommitCount := len(actions)
	for i := 0; i < preCommitCount; i++ {
		if actions[i].Kind != ActionPreserveStop {
			continue
		}
		cleanup := actions[i]
		cleanup.Kind = ActionRemovePreserved
		actions = append(actions, cleanup)
	}
	return actions, nil
}

func desiredServiceCounts(placements []scheduler.Placement) map[string]int {
	counts := map[string]int{}
	for _, placement := range placements {
		counts[placement.Service]++
	}
	return counts
}

func appendCanaryPauseAction(actions []Action, placement scheduler.Placement, releaseID string, svc config.Service, started, desired int) []Action {
	pause := canaryPauseDuration(svc)
	if pause <= 0 || desired <= 1 {
		return actions
	}
	target := canaryReplicaTarget(svc)
	if target <= 0 || target >= desired || started != target {
		return actions
	}
	return append(actions, Action{
		Kind:          ActionCanary,
		Host:          placement.Host,
		Service:       placement.Service,
		Replica:       placement.Replica,
		Release:       releaseID,
		PauseDuration: pause,
	})
}

func canaryReplicaTarget(svc config.Service) int {
	if svc.Rolling.CanaryReplicas > 0 {
		return svc.Rolling.CanaryReplicas
	}
	if svc.Rolling.CanaryPauseSeconds > 0 {
		return 1
	}
	return 0
}

func canaryPauseDuration(svc config.Service) time.Duration {
	if svc.Rolling.CanaryPauseSeconds <= 0 {
		return 0
	}
	return time.Duration(svc.Rolling.CanaryPauseSeconds) * time.Second
}

func flushPendingStops(actions []Action, pendingStops map[string][]ObservedContainer, surgeCounts map[string]int, serviceName string, svc config.Service, preStopped map[string]struct{}) []Action {
	for _, old := range pendingStops[serviceName] {
		if _, ok := preStopped[observedKey(old)]; ok {
			continue
		}
		actions = appendPreserveStopActions(actions, old, svc)
		preStopped[observedKey(old)] = struct{}{}
	}
	pendingStops[serviceName] = nil
	surgeCounts[serviceName] = 0
	return actions
}

func rollingMaxSurge(svc config.Service) int {
	if svc.Rolling.MaxSurge > 0 {
		return svc.Rolling.MaxSurge
	}
	if svc.Rolling.MaxUnavailable > 0 {
		return 0
	}
	return 1
}

func rollingMaxUnavailable(svc config.Service) int {
	if svc.Rolling.MaxUnavailable > 0 {
		return svc.Rolling.MaxUnavailable
	}
	if svc.Rolling.MaxSurge > 0 {
		return 0
	}
	return 1
}

func FixedPortRollbackConflicts(input PlanInput) ([]FixedPortRollbackConflict, error) {
	if input.Config == nil {
		return nil, errors.New("config is required")
	}
	placements, err := placementsForInput(input)
	if err != nil {
		return nil, err
	}
	var conflicts []FixedPortRollbackConflict
	for _, placement := range placements {
		svc := input.Config.Services[placement.Service]
		if !usesFixedHostPorts(svc) {
			continue
		}
		targetName := ContainerName(input.Config.Project, input.EnvName, placement.Service, placement.Replica, input.ReleaseID)
		for _, old := range replacementStopCandidates(input.Config, input.EnvName, placement, targetName, input.Observed) {
			release := old.Container.Labels[docker.LabelRelease]
			if release == input.ReleaseID {
				continue
			}
			conflicts = append(conflicts, FixedPortRollbackConflict{
				Host:                old.Host,
				Service:             placement.Service,
				Replica:             placement.Replica,
				Ports:               append([]int(nil), svc.Ports...),
				ContainerName:       old.Container.Names,
				ContainerRelease:    release,
				TargetContainerName: targetName,
			})
		}
	}
	sort.Slice(conflicts, func(i, j int) bool {
		if conflicts[i].Host.Name != conflicts[j].Host.Name {
			return conflicts[i].Host.Name < conflicts[j].Host.Name
		}
		if conflicts[i].Service != conflicts[j].Service {
			return conflicts[i].Service < conflicts[j].Service
		}
		if conflicts[i].Replica != conflicts[j].Replica {
			return conflicts[i].Replica < conflicts[j].Replica
		}
		return conflicts[i].ContainerName < conflicts[j].ContainerName
	})
	return conflicts, nil
}

func ExecuteActions(ctx context.Context, actions []Action, agentFor AgentFactory, sleep func(time.Duration)) error {
	_, err := ExecuteActionsWithResult(ctx, actions, agentFor, sleep)
	return err
}

// ExecuteActionsWithResult executes actions transactionally through the ingress commit point.
func ExecuteActionsWithResult(ctx context.Context, actions []Action, agentFor AgentFactory, sleep func(time.Duration)) (ExecutionResult, error) {
	if agentFor == nil {
		return ExecutionResult{}, errors.New("agent factory is required")
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	var started []Action
	var preserved []Action
	var cleanup []Action
	fail := func(primary error) (ExecutionResult, error) {
		return ExecutionResult{}, compensateActions(ctx, primary, started, preserved, agentFor)
	}
	for _, action := range actions {
		switch action.Kind {
		case ActionPull:
			client := agentFor(action.Host)
			if err := client.Call(ctx, "pull", map[string]string{"image": action.Image}, nil); err != nil {
				return fail(fmt.Errorf("pull %s on %s: %w", action.Image, action.Host.Name, err))
			}
		case ActionStart:
			client := agentFor(action.Host)
			if err := ensureNetwork(ctx, client, action); err != nil {
				return fail(fmt.Errorf("ensure network %s on %s: %w", action.Network, action.Host.Name, err))
			}
			params := agent.RunContainerParams{
				Name:           action.ContainerName,
				Image:          action.Image,
				Command:        action.Command,
				Args:           action.Args,
				Labels:         action.Labels,
				Network:        action.Network,
				NetworkAliases: action.NetworkAliases,
			}
			if err := client.Call(ctx, "run_container", params, nil); err != nil {
				return fail(fmt.Errorf("start %s on %s: %w", action.ContainerName, action.Host.Name, err))
			}
			started = append(started, action)
		case ActionHealth:
			if err := executeHealthAction(ctx, action, agentFor, sleep); err != nil {
				return fail(err)
			}
		case ActionIngress:
			if err := executeIngressAction(ctx, action, agentFor); err != nil {
				return fail(err)
			}
		case ActionDrain:
			sleep(action.DrainTimeout)
		case ActionCanary:
			sleep(action.PauseDuration)
		case ActionStop:
			client := agentFor(action.Host)
			if err := client.Call(ctx, "stop_container", map[string]string{"name": action.ContainerName}, nil); err != nil {
				return fail(fmt.Errorf("stop %s on %s: %w", action.ContainerName, action.Host.Name, err))
			}
		case ActionPreserveStop:
			client := agentFor(action.Host)
			if err := client.Call(ctx, "stop_container_keep", map[string]string{"name": action.ContainerName}, nil); err != nil {
				return fail(fmt.Errorf("preserve %s on %s: %w", action.ContainerName, action.Host.Name, err))
			}
			preserved = append(preserved, action)
		case ActionRemovePreserved:
			cleanup = append(cleanup, action)
		default:
			return fail(fmt.Errorf("unknown deployment action %q", action.Kind))
		}
	}
	result := ExecutionResult{}
	for _, action := range cleanup {
		client := agentFor(action.Host)
		if err := client.Call(ctx, "remove_container", map[string]string{"name": action.ContainerName}, nil); err != nil {
			result.CleanupWarnings = append(result.CleanupWarnings, CleanupWarning{
				Host:          action.Host,
				ContainerName: action.ContainerName,
				Err:           err,
			})
		}
	}
	return result, nil
}

func compensateActions(ctx context.Context, primary error, started, preserved []Action, agentFor AgentFactory) error {
	var failures []string
	for i := len(started) - 1; i >= 0; i-- {
		action := started[i]
		if err := agentFor(action.Host).Call(ctx, "remove_container", map[string]string{"name": action.ContainerName}, nil); err != nil {
			failures = append(failures, fmt.Sprintf("remove %s on %s: %v", action.ContainerName, action.Host.Name, err))
		}
	}
	for i := len(preserved) - 1; i >= 0; i-- {
		action := preserved[i]
		if err := agentFor(action.Host).Call(ctx, "start_container", map[string]string{"name": action.ContainerName}, nil); err != nil {
			failures = append(failures, fmt.Sprintf("restart %s on %s: %v", action.ContainerName, action.Host.Name, err))
		}
	}
	if len(failures) == 0 {
		return primary
	}
	return fmt.Errorf("%w; additionally failed to compensate: %s", primary, strings.Join(failures, "; "))
}

func executeHealthAction(ctx context.Context, action Action, agentFor AgentFactory, sleep func(time.Duration)) error {
	attempts := action.HealthRetries + 1
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		client := agentFor(action.Host)
		var result agent.HealthCheckResult
		err := client.Call(ctx, "health_check", action.Health, &result)
		if err == nil && result.OK {
			return nil
		}
		if err != nil {
			lastErr = fmt.Errorf("health %s on %s: %w", action.ContainerName, action.Host.Name, err)
		} else {
			lastErr = fmt.Errorf("health %s on %s failed", action.ContainerName, action.Host.Name)
		}
		if attempt < attempts && action.HealthInterval > 0 {
			sleep(action.HealthInterval)
		}
	}
	return lastErr
}

// executeIngressAction applies the desired Caddyfile to every ingress host,
// snapshotting each host's on-disk config immediately beforehand so a
// mid-rollout failure can restore exactly what that host was actually
// running. The snapshot is read live from the host, not from local .ship/
// state: a CI runner's checkout never carries state from prior deploys, so
// sourcing "previous" locally silently turned rollback into a no-op there.
func executeIngressAction(ctx context.Context, action Action, agentFor AgentFactory) error {
	remotePath := remoteCaddyfilePath(action.IngressPath)
	previous := map[string]ingressSnapshot{}
	for _, host := range action.IngressHosts {
		previous[host.Name] = readRemoteIngressSnapshot(ctx, host, remotePath, agentFor)
	}

	var reloaded []scheduler.Host
	for _, host := range action.IngressHosts {
		if err := applyCaddyContainer(ctx, host, action, agentFor); err != nil {
			if rollbackErr := rollbackIngressHosts(ctx, reloaded, previous, action, agentFor); rollbackErr != nil {
				return fmt.Errorf("reload ingress on %s: %w; additionally failed to roll back ingress: %v", host.Name, err, rollbackErr)
			}
			return fmt.Errorf("reload ingress on %s: %w", host.Name, err)
		}
		reloaded = append(reloaded, host)
	}

	if err := os.MkdirAll(filepath.Dir(action.IngressPath), 0o755); err != nil {
		if rollbackErr := rollbackIngressHosts(ctx, reloaded, previous, action, agentFor); rollbackErr != nil {
			return fmt.Errorf("prepare ingress state: %w; additionally failed to roll back ingress: %v", err, rollbackErr)
		}
		return fmt.Errorf("prepare ingress state: %w", err)
	}
	if err := fsatomic.WriteFile(action.IngressPath, []byte(action.IngressConfig), 0o644); err != nil {
		if rollbackErr := rollbackIngressHosts(ctx, reloaded, previous, action, agentFor); rollbackErr != nil {
			return fmt.Errorf("write ingress state: %w; additionally failed to roll back ingress: %v", err, rollbackErr)
		}
		return fmt.Errorf("write ingress state: %w", err)
	}
	return nil
}

type ingressSnapshot struct {
	content string
	exists  bool
}

// readRemoteIngressSnapshot is best-effort: an agent that predates the
// read_file RPC, or any other read failure, degrades to "no known previous
// state" rather than blocking the deploy, matching prior behavior for hosts
// we genuinely know nothing about.
func readRemoteIngressSnapshot(ctx context.Context, host scheduler.Host, remotePath string, agentFor AgentFactory) ingressSnapshot {
	var result agent.ReadFileResult
	if err := agentFor(host).Call(ctx, "read_file", agent.ReadFileParams{Path: remotePath}, &result); err != nil {
		return ingressSnapshot{}
	}
	return ingressSnapshot{content: result.Content, exists: result.Exists}
}

func hasObservedCaddyContainer(cfg *config.Config, envName string, observed []ObservedContainer) bool {
	for _, item := range observed {
		if isManagedCaddyContainer(cfg, envName, item.Container) {
			return true
		}
	}
	return false
}

func clearIngressHosts(input PlanInput) []scheduler.Host {
	allHosts := inputHosts(input.Environment, input.Hosts)
	var dedicated []scheduler.Host
	for _, host := range allHosts {
		if host.Pool == "ingress" {
			dedicated = append(dedicated, host)
		}
	}
	if len(dedicated) > 0 {
		return dedicated
	}

	ingressedByName := map[string]scheduler.Host{}
	shipByName := map[string]scheduler.Host{}
	for _, item := range input.Observed {
		if !matchesShipScope(input.Config, input.EnvName, item.Container.Labels) {
			continue
		}
		shipByName[item.Host.Name] = item.Host
		serviceName := item.Container.Labels[docker.LabelService]
		if svc, ok := input.Config.Services[serviceName]; ok && svc.Ingress != nil && len(svc.Ingress.Domains) > 0 {
			ingressedByName[item.Host.Name] = item.Host
		}
	}
	if len(ingressedByName) > 0 {
		return hostsByName(ingressedByName)
	}
	if len(shipByName) > 0 {
		return hostsByName(shipByName)
	}
	return allHosts
}

func hostsByName(byName map[string]scheduler.Host) []scheduler.Host {
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	hosts := make([]scheduler.Host, 0, len(names))
	for _, name := range names {
		hosts = append(hosts, byName[name])
	}
	return hosts
}

func rollbackIngressHosts(ctx context.Context, hosts []scheduler.Host, previous map[string]ingressSnapshot, base Action, agentFor AgentFactory) error {
	var errs []string
	for _, host := range hosts {
		snap, ok := previous[host.Name]
		if !ok || !snap.exists {
			continue
		}
		action := base
		action.IngressConfig = snap.content
		if err := applyCaddyContainer(ctx, host, action, agentFor); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", host.Name, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func applyCaddyContainer(ctx context.Context, host scheduler.Host, action Action, agentFor AgentFactory) error {
	client := agentFor(host)
	if strings.TrimSpace(action.IngressConfig) == "" {
		if action.CaddyName != "" {
			if err := client.Call(ctx, "stop_container", map[string]string{"name": action.CaddyName}, nil); err != nil {
				return err
			}
		}
		return nil
	}
	remotePath := remoteCaddyfilePath(action.IngressPath)
	if err := client.Call(ctx, "write_file", agent.WriteFileParams{
		Path:    remotePath,
		Content: action.IngressConfig,
		Mode:    0o644,
	}, nil); err != nil {
		return err
	}
	image := action.CaddyImage
	if image == "" {
		image = config.DefaultCaddyImage
	}
	name := action.CaddyName
	if name == "" {
		name = "ship_caddy"
	}
	dataVolume := strings.TrimSpace(action.CaddyDataVolume)
	if dataVolume == "" {
		dataVolume = name + "_data"
	}
	configVolume := strings.TrimSpace(action.CaddyConfigVolume)
	if configVolume == "" {
		configVolume = name + "_config"
	}
	if err := ensureNetwork(ctx, client, action); err != nil {
		return err
	}
	if err := validateCaddyfile(ctx, client, remotePath, image); err != nil {
		return err
	}
	args := []string{
		"--restart", config.DefaultRestartPolicy,
		"-p", "80:80",
		"-p", "443:443",
		"-p", "443:443/udp",
		"-v", remotePath + ":/etc/caddy/Caddyfile:ro",
		"-v", dataVolume + ":/data",
		"-v", configVolume + ":/config",
	}
	if err := client.Call(ctx, "run_container", agent.RunContainerParams{
		Name:    name,
		Image:   image,
		Command: "caddy run --config /etc/caddy/Caddyfile --adapter caddyfile",
		Args:    args,
		Labels:  action.CaddyLabels,
		Network: action.Network,
	}, nil); err != nil {
		return err
	}
	return waitForCaddyContainer(ctx, client, name)
}

const caddyValidateTimeoutSeconds = 30
const caddyStartupGraceSeconds = 5

func validateCaddyfile(ctx context.Context, client Agent, remotePath, image string) error {
	validateName := "ship_caddy_validate_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	return client.Call(ctx, "run_oneoff_container", agent.RunOneOffContainerParams{
		Name:           validateName,
		Image:          image,
		Command:        "caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile",
		Args:           []string{"-v", remotePath + ":/etc/caddy/Caddyfile:ro"},
		TimeoutSeconds: caddyValidateTimeoutSeconds,
	}, nil)
}

func waitForCaddyContainer(ctx context.Context, client Agent, name string) error {
	deadline := time.Now().Add(caddyStartupGraceSeconds * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		var result agent.DockerInspectResult
		err := client.Call(ctx, "docker_inspect", agent.DockerInspectParams{Name: name}, &result)
		if err != nil {
			lastErr = err
		} else if running, err := containerRunning(result.Inspect); err != nil {
			lastErr = err
		} else if running {
			return nil
		} else {
			lastErr = fmt.Errorf("container %q is not running", name)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for caddy container %q: %w", name, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("container %q did not start", name)
	}
	if logs := containerLogTail(ctx, client, name); strings.TrimSpace(logs) != "" {
		return fmt.Errorf("caddy container failed to stay running: %w; recent logs: %s", lastErr, logs)
	}
	return fmt.Errorf("caddy container failed to stay running: %w", lastErr)
}

func containerLogTail(ctx context.Context, client Agent, name string) string {
	var result map[string]string
	if err := client.Call(ctx, "logs", agent.LogsParams{Name: name, Lines: 20}, &result); err != nil {
		return ""
	}
	return strings.TrimSpace(result["logs"])
}

func containerRunning(inspect json.RawMessage) (bool, error) {
	var items []struct {
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
	}
	if len(inspect) == 0 {
		return false, errors.New("inspect payload is empty")
	}
	if err := json.Unmarshal(inspect, &items); err != nil {
		return false, err
	}
	if len(items) == 0 {
		return false, errors.New("inspect payload is empty")
	}
	return items[0].State.Running, nil
}

func ensureNetwork(ctx context.Context, client Agent, action Action) error {
	if strings.TrimSpace(action.Network) == "" {
		return nil
	}
	return client.Call(ctx, "ensure_network", agent.EnsureNetworkParams{Name: action.Network, Driver: action.NetworkDriver}, nil)
}

func remoteCaddyfilePath(localPath string) string {
	name := filepath.Base(localPath)
	if name == "." || name == string(filepath.Separator) || strings.TrimSpace(name) == "" {
		name = "Caddyfile"
	}
	return filepath.Join(config.RemoteStateDir, "ingress", name)
}

func caddyImage(cfg *config.Config) string {
	if cfg != nil && strings.TrimSpace(cfg.Ingress.Caddy.Image) != "" {
		return cfg.Ingress.Caddy.Image
	}
	return config.DefaultCaddyImage
}

func CaddyContainerName(project, envName string) string {
	return strings.Join([]string{"ship", safeNamePart(project), safeNamePart(envName), "caddy"}, "_")
}

func CaddyDataVolumeName(project, envName string) string {
	return CaddyContainerName(project, envName) + "_data"
}

func CaddyConfigVolumeName(project, envName string) string {
	return CaddyContainerName(project, envName) + "_config"
}

func CaddyDataVolume(cfg *config.Config, envName string) string {
	return caddyDataVolume(cfg, envName)
}

func CaddyConfigVolume(cfg *config.Config, envName string) string {
	return caddyConfigVolume(cfg, envName)
}

func caddyDataVolume(cfg *config.Config, envName string) string {
	if cfg != nil {
		if volume := strings.TrimSpace(cfg.Ingress.Caddy.DataVolume); volume != "" {
			return volume
		}
		return CaddyDataVolumeName(cfg.Project, envName)
	}
	return ""
}

func caddyConfigVolume(cfg *config.Config, envName string) string {
	if cfg != nil {
		if volume := strings.TrimSpace(cfg.Ingress.Caddy.ConfigVolume); volume != "" {
			return volume
		}
		return CaddyConfigVolumeName(cfg.Project, envName)
	}
	return ""
}

func DockerNetworkName(cfg *config.Config, envName string) string {
	if cfg == nil || !cfg.Docker.Network.EnabledValue(true) {
		return ""
	}
	if name := strings.TrimSpace(cfg.Docker.Network.Name); name != "" {
		return name
	}
	return "ship-" + safeNamePart(cfg.Project) + "-" + safeNamePart(envName)
}

func DockerNetworkDriver(cfg *config.Config) string {
	if cfg != nil {
		if driver := strings.TrimSpace(cfg.Docker.Network.Driver); driver != "" {
			return driver
		}
	}
	return "bridge"
}

func ServiceNetworkAliases(service string, svc config.Service) []string {
	return normalizedAliases(append([]string{service}, svc.NetworkAliases...))
}

func normalizedAliases(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func CaddyLabels(project, envName string) map[string]string {
	return map[string]string{
		docker.LabelProject:     safeLabelValue(project),
		docker.LabelEnvironment: safeLabelValue(envName),
		docker.LabelService:     "caddy",
	}
}

func ContainerName(project, envName, service string, replica int, release string) string {
	parts := []string{"ship", safeNamePart(project), safeNamePart(envName), safeNamePart(service), strconv.Itoa(replica), safeNamePart(release)}
	return strings.Join(parts, "_")
}

// ParseContainerName recovers the service name and replica number from a
// full container name produced by ContainerName (e.g. as pasted from `ship
// ps` output). Since project, environment, and service names may themselves
// contain underscores, it disambiguates by checking the known service names
// rather than blindly splitting on "_".
func ParseContainerName(project, envName string, services map[string]config.Service, name string) (service string, replica int, ok bool) {
	prefix := strings.Join([]string{"ship", safeNamePart(project), safeNamePart(envName), ""}, "_")
	if !strings.HasPrefix(name, prefix) {
		return "", 0, false
	}
	rest := strings.TrimPrefix(name, prefix)
	for svc := range services {
		svcPrefix := safeNamePart(svc) + "_"
		after, found := strings.CutPrefix(rest, svcPrefix)
		if !found {
			continue
		}
		replicaPart := after
		if idx := strings.Index(after, "_"); idx >= 0 {
			replicaPart = after[:idx]
		}
		n, err := strconv.Atoi(replicaPart)
		if err != nil || n <= 0 {
			continue
		}
		return svc, n, true
	}
	return "", 0, false
}

func ContainerLabels(project, envName, service string, replica int, release string, custom ...map[string]string) map[string]string {
	labels := mergeLabels(custom...)
	for key, value := range map[string]string{
		docker.LabelProject:     safeLabelValue(project),
		docker.LabelEnvironment: safeLabelValue(envName),
		docker.LabelService:     safeLabelValue(service),
		docker.LabelReplica:     strconv.Itoa(replica),
		docker.LabelRelease:     safeLabelValue(release),
	} {
		labels[key] = value
	}
	return labels
}

func mergeLabels(inputs ...map[string]string) map[string]string {
	labels := map[string]string{}
	for _, input := range inputs {
		for key, value := range input {
			if strings.TrimSpace(key) != "" {
				labels[key] = value
			}
		}
	}
	return labels
}

func DockerArgs(svc config.Service, envFiles ...string) []string {
	return dockerArgs(svc, true, true, envFiles...)
}

func DockerOneOffArgs(svc config.Service, envFiles ...string) []string {
	return dockerArgs(svc, false, false, envFiles...)
}

func dockerArgs(svc config.Service, includeRestart, includePorts bool, envFiles ...string) []string {
	args := []string{}
	for _, env := range svc.Env {
		if strings.TrimSpace(env) != "" {
			args = append(args, "-e", env)
		}
	}
	for _, envFile := range envFiles {
		if strings.TrimSpace(envFile) != "" {
			args = append(args, "--env-file", envFile)
		}
	}
	if includeRestart {
		args = append(args, RestartPolicyArgs(svc.RestartPolicy)...)
	}
	for _, volume := range svc.Volumes {
		if strings.TrimSpace(volume) != "" {
			args = append(args, "-v", volume)
		}
	}
	args = append(args, ResourceArgs(svc.Resources)...)
	args = append(args, RuntimeArgs(svc.Runtime)...)
	args = append(args, LoggingArgs(svc.Logging)...)
	if includePorts {
		for _, port := range svc.Ports {
			args = append(args, "-p", fmt.Sprintf("%d:%d", port, port))
		}
		for _, spec := range sortedNonEmpty(svc.Publish) {
			args = append(args, "-p", spec)
		}
	}
	return args
}

func RestartPolicyArgs(policy string) []string {
	policy = strings.TrimSpace(policy)
	if policy == "" {
		policy = config.DefaultRestartPolicy
	}
	return []string{"--restart", policy}
}

func LoggingArgs(logging config.LoggingConfig) []string {
	var args []string
	if driver := strings.TrimSpace(logging.Driver); driver != "" {
		args = append(args, "--log-driver", driver)
	}
	keys := make([]string, 0, len(logging.Options))
	for key := range logging.Options {
		if strings.TrimSpace(key) != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--log-opt", key+"="+logging.Options[key])
	}
	return args
}

func ResourceArgs(resources config.ResourceConfig) []string {
	var args []string
	if value := strings.TrimSpace(resources.CPUs); value != "" {
		args = append(args, "--cpus", value)
	}
	if value := strings.TrimSpace(resources.Memory); value != "" {
		args = append(args, "--memory", value)
	}
	if value := strings.TrimSpace(resources.MemoryReservation); value != "" {
		args = append(args, "--memory-reservation", value)
	}
	if value := strings.TrimSpace(resources.MemorySwap); value != "" {
		args = append(args, "--memory-swap", value)
	}
	if resources.CPUShares > 0 {
		args = append(args, "--cpu-shares", strconv.Itoa(resources.CPUShares))
	}
	if value := strings.TrimSpace(resources.CPUSetCPUs); value != "" {
		args = append(args, "--cpuset-cpus", value)
	}
	if resources.PIDsLimit > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(resources.PIDsLimit))
	}
	return args
}

func RuntimeArgs(runtime config.RuntimeConfig) []string {
	var args []string
	if runtime.Privileged {
		args = append(args, "--privileged")
	}
	if runtime.ReadOnly {
		args = append(args, "--read-only")
	}
	if runtime.Init {
		args = append(args, "--init")
	}
	if value := strings.TrimSpace(runtime.User); value != "" {
		args = append(args, "--user", value)
	}
	if value := strings.TrimSpace(runtime.Workdir); value != "" {
		args = append(args, "--workdir", value)
	}
	if value := strings.TrimSpace(runtime.Hostname); value != "" {
		args = append(args, "--hostname", value)
	}
	if value := strings.TrimSpace(runtime.Entrypoint); value != "" {
		args = append(args, "--entrypoint", value)
	}
	if value := strings.TrimSpace(runtime.IPC); value != "" {
		args = append(args, "--ipc", value)
	}
	if value := strings.TrimSpace(runtime.PID); value != "" {
		args = append(args, "--pid", value)
	}
	if value := strings.TrimSpace(runtime.CgroupNS); value != "" {
		args = append(args, "--cgroupns", value)
	}
	if value := strings.TrimSpace(runtime.StopSignal); value != "" {
		args = append(args, "--stop-signal", value)
	}
	if runtime.StopTimeoutSeconds > 0 {
		args = append(args, "--stop-timeout", strconv.Itoa(runtime.StopTimeoutSeconds))
	}
	if value := strings.TrimSpace(runtime.ShmSize); value != "" {
		args = append(args, "--shm-size", value)
	}
	if value := strings.TrimSpace(runtime.GPUs); value != "" {
		args = append(args, "--gpus", value)
	}
	if runtime.NoHealthcheck {
		args = append(args, "--no-healthcheck")
	}
	if value := strings.TrimSpace(runtime.HealthCMD); value != "" {
		args = append(args, "--health-cmd", value)
	}
	if value := strings.TrimSpace(runtime.HealthInterval); value != "" {
		args = append(args, "--health-interval", value)
	}
	if value := strings.TrimSpace(runtime.HealthTimeout); value != "" {
		args = append(args, "--health-timeout", value)
	}
	if value := strings.TrimSpace(runtime.HealthStartPeriod); value != "" {
		args = append(args, "--health-start-period", value)
	}
	if runtime.HealthRetries > 0 {
		args = append(args, "--health-retries", strconv.Itoa(runtime.HealthRetries))
	}
	for _, value := range sortedNonEmpty(runtime.CapAdd) {
		args = append(args, "--cap-add", value)
	}
	for _, value := range sortedNonEmpty(runtime.CapDrop) {
		args = append(args, "--cap-drop", value)
	}
	for _, value := range sortedNonEmpty(runtime.GroupAdd) {
		args = append(args, "--group-add", value)
	}
	for _, value := range sortedNonEmpty(runtime.SecurityOpt) {
		args = append(args, "--security-opt", value)
	}
	for _, value := range sortedNonEmpty(runtime.Ulimits) {
		args = append(args, "--ulimit", value)
	}
	for _, value := range sortedNonEmpty(runtime.Mounts) {
		args = append(args, "--mount", value)
	}
	for _, value := range sortedNonEmpty(runtime.AddHosts) {
		args = append(args, "--add-host", value)
	}
	for _, value := range sortedNonEmpty(runtime.DNS) {
		args = append(args, "--dns", value)
	}
	for _, value := range sortedNonEmpty(runtime.DNSSearch) {
		args = append(args, "--dns-search", value)
	}
	for _, value := range sortedNonEmpty(runtime.DNSOptions) {
		args = append(args, "--dns-option", value)
	}
	for _, value := range sortedNonEmpty(runtime.Devices) {
		args = append(args, "--device", value)
	}
	for _, value := range sortedNonEmpty(runtime.DeviceCgroupRules) {
		args = append(args, "--device-cgroup-rule", value)
	}
	for _, value := range sortedNonEmpty(runtime.Tmpfs) {
		args = append(args, "--tmpfs", value)
	}
	keys := make([]string, 0, len(runtime.Sysctls))
	for key := range runtime.Sysctls {
		if strings.TrimSpace(key) != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if value := strings.TrimSpace(runtime.Sysctls[key]); value != "" {
			args = append(args, "--sysctl", key+"="+value)
		}
	}
	return args
}

func sortedNonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func usesFixedHostPorts(svc config.Service) bool {
	return len(svc.Ports) > 0
}

func HealthCheck(svc config.Service, containerName string) (agent.HealthCheckParams, bool, error) {
	timeout := svc.Rolling.HealthTimeoutSeconds
	if timeout <= 0 {
		timeout = defaultHealthTimeoutSeconds
	}
	if command := strings.TrimSpace(svc.Health.Command); command != "" {
		return agent.HealthCheckParams{
			Command:        "docker exec " + shellQuote(containerName) + " sh -lc " + shellQuote(command),
			TimeoutSeconds: timeout,
		}, true, nil
	}
	if httpPath := strings.TrimSpace(svc.Health.HTTP); httpPath != "" {
		if strings.HasPrefix(httpPath, "http://") || strings.HasPrefix(httpPath, "https://") {
			return agent.HealthCheckParams{URL: httpPath, TimeoutSeconds: timeout}, true, nil
		}
		if len(svc.Ports) == 0 {
			return agent.HealthCheckParams{}, false, errors.New("http health check requires at least one published port")
		}
		if !strings.HasPrefix(httpPath, "/") {
			httpPath = "/" + httpPath
		}
		return agent.HealthCheckParams{
			URL:            fmt.Sprintf("http://127.0.0.1:%d%s", svc.Ports[0], httpPath),
			TimeoutSeconds: timeout,
		}, true, nil
	}
	return agent.HealthCheckParams{}, false, nil
}

func HealthRetryInterval(svc config.Service) time.Duration {
	if svc.Rolling.HealthRetries <= 0 {
		return 0
	}
	seconds := svc.Rolling.HealthIntervalSeconds
	if seconds <= 0 {
		seconds = defaultHealthRetryIntervalSeconds
	}
	return time.Duration(seconds) * time.Second
}

func stopCandidates(cfg *config.Config, envName string, observed []ObservedContainer, desiredNames map[string]struct{}) []ObservedContainer {
	var candidates []ObservedContainer
	for _, item := range observed {
		labels := item.Container.Labels
		if !matchesShipScope(cfg, envName, labels) {
			continue
		}
		if labels[docker.LabelService] == "" || labels[docker.LabelAccessory] != "" {
			continue
		}
		if isManagedCaddyContainer(cfg, envName, item.Container) {
			continue
		}
		if _, desired := desiredNames[item.Container.Names]; desired {
			continue
		}
		candidates = append(candidates, item)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Host.Name != candidates[j].Host.Name {
			return candidates[i].Host.Name < candidates[j].Host.Name
		}
		return candidates[i].Container.Names < candidates[j].Container.Names
	})
	return candidates
}

func replacementStopCandidates(cfg *config.Config, envName string, placement scheduler.Placement, desiredName string, observed []ObservedContainer) []ObservedContainer {
	var candidates []ObservedContainer
	for _, item := range observed {
		labels := item.Container.Labels
		if !matchesShipScope(cfg, envName, labels) {
			continue
		}
		if item.Host.Name != placement.Host.Name {
			continue
		}
		if labels[docker.LabelService] != placement.Service || labels[docker.LabelReplica] != strconv.Itoa(placement.Replica) {
			continue
		}
		if item.Container.Names == desiredName {
			continue
		}
		candidates = append(candidates, item)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Container.Names < candidates[j].Container.Names
	})
	return candidates
}

func appendStopActions(actions []Action, old ObservedContainer, svc config.Service) []Action {
	return appendContainerStopActions(actions, old, svc, ActionStop)
}

func appendPreserveStopActions(actions []Action, old ObservedContainer, svc config.Service) []Action {
	return appendContainerStopActions(actions, old, svc, ActionPreserveStop)
}

func appendContainerStopActions(actions []Action, old ObservedContainer, svc config.Service, kind ActionKind) []Action {
	serviceName := old.Container.Labels[docker.LabelService]
	replica, _ := strconv.Atoi(old.Container.Labels[docker.LabelReplica])
	if svc.Rolling.DrainTimeoutSeconds > 0 {
		actions = append(actions, Action{
			Kind:          ActionDrain,
			Host:          old.Host,
			Service:       serviceName,
			Replica:       replica,
			Release:       old.Container.Labels[docker.LabelRelease],
			ContainerName: old.Container.Names,
			DrainTimeout:  time.Duration(svc.Rolling.DrainTimeoutSeconds) * time.Second,
		})
	}
	return append(actions, Action{
		Kind:          kind,
		Host:          old.Host,
		Service:       serviceName,
		Replica:       replica,
		Release:       old.Container.Labels[docker.LabelRelease],
		ContainerName: old.Container.Names,
	})
}

func matchesShipScope(cfg *config.Config, envName string, labels map[string]string) bool {
	return labels[docker.LabelManagedBy] == docker.LabelManagedByValue &&
		labels[docker.LabelProject] == safeLabelValue(cfg.Project) &&
		labels[docker.LabelEnvironment] == safeLabelValue(envName)
}

func observedKey(item ObservedContainer) string {
	return item.Host.Name + "\x00" + item.Container.Names
}

func safeNamePart(value string) string {
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

func safeLabelValue(value string) string {
	return safeNamePart(value)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
