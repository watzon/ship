package deployment

import (
	"context"
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
	"github.com/watzon/ship/internal/ingress"
	"github.com/watzon/ship/internal/scheduler"
)

const (
	ActionPull    ActionKind = "pull"
	ActionStart   ActionKind = "start"
	ActionHealth  ActionKind = "health"
	ActionIngress ActionKind = "ingress"
	ActionDrain   ActionKind = "drain"
	ActionStop    ActionKind = "stop"
)

const defaultHealthTimeoutSeconds = 30

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
	Kind          ActionKind
	Host          scheduler.Host
	Service       string
	Replica       int
	Release       string
	ContainerName string
	Image         string
	Command       string
	Args          []string
	Labels        map[string]string
	Health        agent.HealthCheckParams
	IngressPath   string
	IngressConfig string
	IngressHosts  []scheduler.Host
	CaddyImage    string
	CaddyName     string
	CaddyLabels   map[string]string
	DrainTimeout  time.Duration
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

func Rollout(ctx context.Context, opts RolloutOptions) ([]Action, error) {
	hosts := inputHosts(opts.Environment, opts.Hosts)
	observed, err := InspectObservedOnHosts(ctx, hosts, opts.AgentFor)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	if err := ExecuteActions(ctx, actions, opts.AgentFor, opts.Sleep); err != nil {
		return actions, err
	}
	return actions, nil
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
	var actions []Action
	for _, placement := range placements {
		svc := input.Config.Services[placement.Service]
		image := input.Images[placement.Service]
		if strings.TrimSpace(image) == "" {
			return nil, fmt.Errorf("missing image for service %q", placement.Service)
		}
		name := ContainerName(input.Config.Project, input.EnvName, placement.Service, placement.Replica, input.ReleaseID)
		desiredNames[name] = struct{}{}
		labels := ContainerLabels(input.Config.Project, input.EnvName, placement.Service, placement.Replica, input.ReleaseID)
		args := DockerArgs(svc, input.SecretEnvFiles[placement.Service])
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
				actions = appendStopActions(actions, old, svc)
				preStopped[observedKey(old)] = struct{}{}
			}
		} else {
			replacements := replacementStopCandidates(input.Config, input.EnvName, placement, name, input.Observed)
			if len(replacements) > 0 && rollingMaxSurge(svc) == 0 {
				if rollingMaxUnavailable(svc) == 0 {
					return nil, fmt.Errorf("service %q rolling strategy cannot have max_surge=0 and max_unavailable=0", placement.Service)
				}
				for _, old := range replacements {
					actions = appendStopActions(actions, old, svc)
					preStopped[observedKey(old)] = struct{}{}
				}
			} else {
				pendingStops[placement.Service] = append(pendingStops[placement.Service], replacements...)
			}
		}
		actions = append(actions, Action{
			Kind:          ActionStart,
			Host:          placement.Host,
			Service:       placement.Service,
			Replica:       placement.Replica,
			Release:       input.ReleaseID,
			ContainerName: name,
			Image:         image,
			Command:       svc.Command,
			Args:          args,
			Labels:        labels,
		})
		health, ok, err := HealthCheck(svc, name)
		if err != nil {
			return nil, fmt.Errorf("service %q health check: %w", placement.Service, err)
		}
		if ok {
			actions = append(actions, Action{
				Kind:          ActionHealth,
				Host:          placement.Host,
				Service:       placement.Service,
				Replica:       placement.Replica,
				Release:       input.ReleaseID,
				ContainerName: name,
				Health:        health,
			})
		}
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
	caddyfile := ingress.GenerateCaddyfile(input.Config, placements)
	if strings.TrimSpace(caddyfile) != "" {
		actions = append(actions, Action{
			Kind:          ActionIngress,
			IngressPath:   ingressPath,
			IngressConfig: caddyfile,
			IngressHosts:  ingress.HostsFor(input.Config, inputHosts(input.Environment, input.Hosts), placements),
			CaddyImage:    caddyImage(input.Config),
			CaddyName:     CaddyContainerName(input.Config.Project, input.EnvName),
			CaddyLabels:   CaddyLabels(input.Config.Project, input.EnvName),
		})
	} else {
		shouldClear, err := shouldClearIngress(ingressPath)
		if err != nil {
			return nil, fmt.Errorf("read previous ingress state: %w", err)
		}
		if shouldClear {
			actions = append(actions, Action{
				Kind:         ActionIngress,
				IngressPath:  ingressPath,
				IngressHosts: clearIngressHosts(input),
				CaddyImage:   caddyImage(input.Config),
				CaddyName:    CaddyContainerName(input.Config.Project, input.EnvName),
				CaddyLabels:  CaddyLabels(input.Config.Project, input.EnvName),
			})
		}
	}
	for _, old := range stopCandidates(input.Config, input.EnvName, input.Observed, desiredNames) {
		if _, ok := preStopped[observedKey(old)]; ok {
			continue
		}
		serviceName := old.Container.Labels[docker.LabelService]
		actions = appendStopActions(actions, old, input.Config.Services[serviceName])
	}
	return actions, nil
}

func flushPendingStops(actions []Action, pendingStops map[string][]ObservedContainer, surgeCounts map[string]int, serviceName string, svc config.Service, preStopped map[string]struct{}) []Action {
	for _, old := range pendingStops[serviceName] {
		if _, ok := preStopped[observedKey(old)]; ok {
			continue
		}
		actions = appendStopActions(actions, old, svc)
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
	if agentFor == nil {
		return errors.New("agent factory is required")
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	for _, action := range actions {
		switch action.Kind {
		case ActionPull:
			client := agentFor(action.Host)
			if err := client.Call(ctx, "pull", map[string]string{"image": action.Image}, nil); err != nil {
				return fmt.Errorf("pull %s on %s: %w", action.Image, action.Host.Name, err)
			}
		case ActionStart:
			client := agentFor(action.Host)
			params := agent.RunContainerParams{
				Name:    action.ContainerName,
				Image:   action.Image,
				Command: action.Command,
				Args:    action.Args,
				Labels:  action.Labels,
			}
			if err := client.Call(ctx, "run_container", params, nil); err != nil {
				return fmt.Errorf("start %s on %s: %w", action.ContainerName, action.Host.Name, err)
			}
		case ActionHealth:
			client := agentFor(action.Host)
			var result agent.HealthCheckResult
			if err := client.Call(ctx, "health_check", action.Health, &result); err != nil {
				return fmt.Errorf("health %s on %s: %w", action.ContainerName, action.Host.Name, err)
			}
			if !result.OK {
				return fmt.Errorf("health %s on %s failed", action.ContainerName, action.Host.Name)
			}
		case ActionIngress:
			if err := executeIngressAction(ctx, action, agentFor); err != nil {
				return err
			}
		case ActionDrain:
			sleep(action.DrainTimeout)
		case ActionStop:
			client := agentFor(action.Host)
			if err := client.Call(ctx, "stop_container", map[string]string{"name": action.ContainerName}, nil); err != nil {
				return fmt.Errorf("stop %s on %s: %w", action.ContainerName, action.Host.Name, err)
			}
		default:
			return fmt.Errorf("unknown deployment action %q", action.Kind)
		}
	}
	return nil
}

func executeIngressAction(ctx context.Context, action Action, agentFor AgentFactory) error {
	previous, hadPrevious, err := readPreviousIngressConfig(action.IngressPath)
	if err != nil {
		return fmt.Errorf("read previous ingress state: %w", err)
	}

	var reloaded []scheduler.Host
	for _, host := range action.IngressHosts {
		if err := applyCaddyContainer(ctx, host, action, agentFor); err != nil {
			if rollbackErr := rollbackIngressHosts(ctx, reloaded, previous, hadPrevious, action, agentFor); rollbackErr != nil {
				return fmt.Errorf("reload ingress on %s: %w; additionally failed to roll back ingress: %v", host.Name, err, rollbackErr)
			}
			return fmt.Errorf("reload ingress on %s: %w", host.Name, err)
		}
		reloaded = append(reloaded, host)
	}

	if err := os.MkdirAll(filepath.Dir(action.IngressPath), 0o755); err != nil {
		if rollbackErr := rollbackIngressHosts(ctx, reloaded, previous, hadPrevious, action, agentFor); rollbackErr != nil {
			return fmt.Errorf("prepare ingress state: %w; additionally failed to roll back ingress: %v", err, rollbackErr)
		}
		return fmt.Errorf("prepare ingress state: %w", err)
	}
	if err := os.WriteFile(action.IngressPath, []byte(action.IngressConfig), 0o644); err != nil {
		if rollbackErr := rollbackIngressHosts(ctx, reloaded, previous, hadPrevious, action, agentFor); rollbackErr != nil {
			return fmt.Errorf("write ingress state: %w; additionally failed to roll back ingress: %v", err, rollbackErr)
		}
		return fmt.Errorf("write ingress state: %w", err)
	}
	return nil
}

func readPreviousIngressConfig(path string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(data), true, nil
}

func shouldClearIngress(path string) (bool, error) {
	previous, hadPrevious, err := readPreviousIngressConfig(path)
	if err != nil {
		return false, err
	}
	return hadPrevious && strings.TrimSpace(previous) != "", nil
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

func rollbackIngressHosts(ctx context.Context, hosts []scheduler.Host, previous string, hadPrevious bool, base Action, agentFor AgentFactory) error {
	if !hadPrevious {
		return nil
	}
	var errs []string
	for _, host := range hosts {
		action := base
		action.IngressConfig = previous
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
	args := []string{
		"-p", "80:80",
		"-p", "443:443",
		"-v", remotePath + ":/etc/caddy/Caddyfile:ro",
	}
	return client.Call(ctx, "run_container", agent.RunContainerParams{
		Name:    name,
		Image:   image,
		Command: "caddy run --config /etc/caddy/Caddyfile --adapter caddyfile",
		Args:    args,
		Labels:  action.CaddyLabels,
	}, nil)
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

func ContainerLabels(project, envName, service string, replica int, release string) map[string]string {
	return map[string]string{
		docker.LabelProject:     safeLabelValue(project),
		docker.LabelEnvironment: safeLabelValue(envName),
		docker.LabelService:     safeLabelValue(service),
		docker.LabelReplica:     strconv.Itoa(replica),
		docker.LabelRelease:     safeLabelValue(release),
	}
}

func DockerArgs(svc config.Service, envFiles ...string) []string {
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
	for _, port := range svc.Ports {
		args = append(args, "-p", fmt.Sprintf("%d:%d", port, port))
	}
	return args
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

func stopCandidates(cfg *config.Config, envName string, observed []ObservedContainer, desiredNames map[string]struct{}) []ObservedContainer {
	var candidates []ObservedContainer
	for _, item := range observed {
		labels := item.Container.Labels
		if !matchesShipScope(cfg, envName, labels) {
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
	serviceName := old.Container.Labels[docker.LabelService]
	if svc.Rolling.DrainTimeoutSeconds > 0 {
		actions = append(actions, Action{
			Kind:          ActionDrain,
			Host:          old.Host,
			Service:       serviceName,
			Release:       old.Container.Labels[docker.LabelRelease],
			ContainerName: old.Container.Names,
			DrainTimeout:  time.Duration(svc.Rolling.DrainTimeoutSeconds) * time.Second,
		})
	}
	return append(actions, Action{
		Kind:          ActionStop,
		Host:          old.Host,
		Service:       serviceName,
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
