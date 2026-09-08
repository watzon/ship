package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/watzon/ship/internal/accessory"
	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/cli/ui"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/deployment"
	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/state"
)

const (
	proxyPreflightTimeout = 30 * time.Second
	proxyWatchInterval    = 5 * time.Second
	proxyWatchTimeout     = 5 * time.Second
	proxyConnectionLimit  = 64
)

type proxyAgent interface {
	Call(context.Context, string, any, any) error
	Proxy(context.Context, agent.ProxyTarget, net.Conn) error
}

var newProxyAgent = func(host scheduler.Host) proxyAgent {
	return agent.Client{SSH: sshForHost(host, false)}
}

type proxySelection struct {
	name          string
	accessory     bool
	replica       int
	containerName string
	localPort     int
	remotePort    int
}

type placedProxyTarget struct {
	host   scheduler.Host
	target agent.ProxyTarget
}

func proxyCmd(opts *options) *cobra.Command {
	return newProxyCommand(opts, false)
}

func accessoryProxyCmd(opts *options) *cobra.Command {
	return newProxyCommand(opts, true)
}

func newProxyCommand(opts *options, isAccessory bool) *cobra.Command {
	var replica int
	nameArg := ui.Service
	use := "proxy ENV SERVICE LOCAL:REMOTE"
	usage := "ship " + use
	short := "Forward a local TCP port to one running service replica"
	if isAccessory {
		nameArg = ui.Accessory
		use = "proxy ENV NAME LOCAL:REMOTE"
		usage = "ship accessory " + use
		short = "Forward a local TCP port to a running accessory"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long: short + ".\n\nUsage: " + usage + "\n\nThe listener binds to 127.0.0.1. Press Ctrl-C to close it. " +
			"The remote port is the container port; it does not need a ports or publish entry. " +
			"The proxy stops when its container restarts or changes. The host agent must support proxy streams.",
		Args: ui.ExactArgs(ui.Env, nameArg, ui.ArgNamed("LOCAL:REMOTE", "local TCP port and container TCP port, for example 15432:5432")),
		RunE: func(cmd *cobra.Command, args []string) error {
			localPort, remotePort, err := parseProxyPorts(args[2])
			if err != nil {
				return err
			}
			cfg, env, store, err := environmentContext(opts, args[0])
			if err != nil {
				return err
			}
			selection := proxySelection{name: args[1], accessory: isAccessory, localPort: localPort, remotePort: remotePort}
			if isAccessory {
				if _, ok := cfg.Accessories[selection.name]; !ok {
					return fmt.Errorf("unknown accessory %q", selection.name)
				}
			} else {
				if cmd.Flags().Changed("replica") && replica <= 0 {
					return fmt.Errorf("--replica must be greater than zero")
				}
				serviceName, parsedReplica, err := resolveServiceReplica(cfg, args[0], args[1])
				if err != nil {
					return err
				}
				selectedReplica, err := mergeResolvedReplica(cmd, replica, parsedReplica, args[1])
				if err != nil {
					return err
				}
				if selectedReplica == 0 {
					selectedReplica = 1
				}
				selection.name = serviceName
				selection.replica = selectedReplica
				if _, bare := cfg.Services[args[1]]; !bare {
					shorthandName, _, shorthand := splitServiceReplica(args[1])
					if !shorthand || shorthandName != serviceName {
						selection.containerName = args[1]
					}
				}
			}
			if deployment.DockerNetworkName(cfg, args[0]) == "" || deployment.DockerNetworkDriver(cfg) != "bridge" {
				return fmt.Errorf("proxy requires an enabled Docker bridge network; check docker.network in ship.yml")
			}
			hosts, err := resolvedHostsForEnvironment(store, args[0], env)
			if err != nil {
				return err
			}
			if opts.dryRun {
				return previewProxy(cmd.OutOrStdout(), cfg, store, hosts, args[0], selection)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			err = runProxyCommand(ctx, cmd.ErrOrStderr(), cfg, store, hosts, args[0], selection)
			if ctx.Err() != nil && cmd.Context().Err() == nil {
				return nil
			}
			return err
		},
	}
	if !isAccessory {
		cmd.Flags().IntVar(&replica, "replica", 1, "select a service replica")
	}
	return cmd
}

func parseProxyPorts(mapping string) (int, int, error) {
	left, right, ok := strings.Cut(mapping, ":")
	parse := func(value string) (int, bool) {
		if value == "" {
			return 0, false
		}
		for _, char := range value {
			if char < '0' || char > '9' {
				return 0, false
			}
		}
		port, err := strconv.Atoi(value)
		return port, err == nil && port >= 1 && port <= 65535
	}
	local, localOK := parse(left)
	remote, remoteOK := parse(right)
	if !ok || !localOK || !remoteOK {
		return 0, 0, fmt.Errorf("invalid port mapping %q: use LOCAL:REMOTE with TCP ports from 1 through 65535", mapping)
	}
	return local, remote, nil
}

func previewProxy(w io.Writer, cfg *config.Config, store state.Store, hosts []scheduler.Host, envName string, selection proxySelection) error {
	kind := "service"
	name := fmt.Sprintf("%s.%d", selection.name, selection.replica)
	if selection.accessory {
		kind, name = "accessory", selection.name
		if saved, err := store.ReadAccessoryState(envName, selection.name); err == nil {
			fmt.Fprintf(w, "saved accessory placement: %s\n", saved.Host.Name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	names := make([]string, 0, len(hosts))
	for _, host := range hosts {
		names = append(names, host.Name)
	}
	sort.Strings(names)
	fmt.Fprintf(w, "would proxy %s %s in %s: 127.0.0.1:%d -> container TCP port %d\n", kind, name, envName, selection.localPort, selection.remotePort)
	if selection.containerName != "" {
		fmt.Fprintf(w, "requested container: %s\n", selection.containerName)
	}
	fmt.Fprintf(w, "candidate hosts: %s; network: %s\n", strings.Join(names, ", "), deployment.DockerNetworkName(cfg, envName))
	fmt.Fprintln(w, "live container identity, agent compatibility, and reachability checks are pending")
	return nil
}

func proxyExpectedLabels(cfg *config.Config, envName string, selection proxySelection) map[string]string {
	var labels map[string]string
	if selection.accessory {
		labels = accessory.ContainerLabels(cfg.Project, envName, selection.name)
	} else {
		labels = deployment.ContainerLabels(cfg.Project, envName, selection.name, selection.replica, "")
		delete(labels, docker.LabelRelease)
	}
	labels[docker.LabelManagedBy] = docker.LabelManagedByValue
	return labels
}

func proxyLabelsMatch(actual, expected map[string]string) bool {
	for key, value := range expected {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func selectProxyTarget(ctx context.Context, cfg *config.Config, store state.Store, hosts []scheduler.Host, envName string, selection proxySelection) (placedProxyTarget, error) {
	observed, err := deployment.InspectObservedOnHosts(ctx, hosts, func(host scheduler.Host) deployment.Agent {
		return newProxyAgent(host)
	})
	if err != nil {
		return placedProxyTarget{}, err
	}
	labels := proxyExpectedLabels(cfg, envName, selection)
	var matches []deployment.ObservedContainer
	for _, item := range observed {
		if !proxyLabelsMatch(item.Container.Labels, labels) {
			continue
		}
		if selection.containerName != "" && item.Container.Names != selection.containerName {
			continue
		}
		matches = append(matches, item)
	}
	if selection.accessory {
		var accessories []accessoryObservation
		for _, match := range matches {
			accessories = append(accessories, accessoryObservation{Host: match.Host, Container: match.Container})
		}
		if _, err := store.ReadAccessoryState(envName, selection.name); err == nil {
			placement, err := accessory.PlacementForHosts(cfg, hosts, envName, selection.name, store)
			if err != nil {
				return placedProxyTarget{}, err
			}
			if err := validatePlacedAccessory(selection.name, placement, accessory.ContainerName(cfg.Project, envName, selection.name), accessories); err != nil {
				return placedProxyTarget{}, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return placedProxyTarget{}, err
		} else if len(matches) > 1 {
			return placedProxyTarget{}, fmt.Errorf("accessory %q has multiple managed containers on hosts %s; inspect accessory status before proxying", selection.name, observationHosts(accessories))
		}
		for _, match := range matches {
			if match.Host.Pool != cfg.Accessories[selection.name].Pool {
				return placedProxyTarget{}, fmt.Errorf("accessory %q is on host %s outside its configured pool; inspect accessory status before proxying", selection.name, match.Host.Name)
			}
		}
	}
	var running []deployment.ObservedContainer
	for _, match := range matches {
		var result agent.DockerInspectResult
		if err := newProxyAgent(match.Host).Call(ctx, "docker_inspect", agent.DockerInspectParams{Name: match.Container.ID}, &result); err != nil {
			return placedProxyTarget{}, fmt.Errorf("inspect proxy target %s on %s: %w", match.Container.Names, match.Host.Name, err)
		}
		ok, err := proxyContainerRunning(result.Inspect)
		if err != nil {
			return placedProxyTarget{}, fmt.Errorf("inspect proxy target %s on %s: %w", match.Container.Names, match.Host.Name, err)
		}
		if ok {
			running = append(running, match)
		}
	}
	if len(running) == 0 {
		return placedProxyTarget{}, fmt.Errorf("no running container matches %q in %s; check ship ps or ship accessory status", selection.name, envName)
	}
	if len(running) > 1 {
		names := make([]string, 0, len(running))
		for _, item := range running {
			names = append(names, item.Container.Names+" on "+item.Host.Name)
		}
		sort.Strings(names)
		return placedProxyTarget{}, fmt.Errorf("multiple running containers match %s.%d: %s; use an exact container name from ship ps", selection.name, selection.replica, strings.Join(names, ", "))
	}
	selected := running[0]
	client := newProxyAgent(selected.Host)
	var status agent.Status
	if err := client.Call(ctx, "status", nil, &status); err != nil {
		return placedProxyTarget{}, fmt.Errorf("check proxy support on %s: %w", selected.Host.Name, err)
	}
	supported := false
	for _, method := range status.SupportedMethods {
		supported = supported || method == "proxy_target"
	}
	if !supported {
		return placedProxyTarget{}, fmt.Errorf("agent on %s does not support proxy streams; run ship agent upgrade %s", selected.Host.Name, envName)
	}
	var result agent.ProxyTargetResult
	if err := client.Call(ctx, "proxy_target", agent.ProxyTargetParams{
		Name: selected.Container.ID, Network: deployment.DockerNetworkName(cfg, envName),
		Port: selection.remotePort, Labels: labels, Probe: true,
	}, &result); err != nil {
		return placedProxyTarget{}, fmt.Errorf("prepare proxy to %s on %s: %w", selected.Container.Names, selected.Host.Name, err)
	}
	if result.Version != agent.ProxyStreamVersion {
		return placedProxyTarget{}, fmt.Errorf("agent on %s uses unsupported proxy stream version %d; update the CLI and run ship agent upgrade %s", selected.Host.Name, result.Version, envName)
	}
	if selection.containerName != "" && result.Target.Name != selection.containerName {
		return placedProxyTarget{}, fmt.Errorf("selected container changed during setup; run the proxy command again")
	}
	return placedProxyTarget{host: selected.Host, target: result.Target}, nil
}

func proxyContainerRunning(raw json.RawMessage) (bool, error) {
	var items []struct {
		State struct {
			Running    bool `json:"Running"`
			Paused     bool `json:"Paused"`
			Restarting bool `json:"Restarting"`
			Dead       bool `json:"Dead"`
		} `json:"State"`
	}
	if err := json.Unmarshal(raw, &items); err != nil || len(items) != 1 {
		return false, fmt.Errorf("agent returned invalid container state")
	}
	s := items[0].State
	return s.Running && !s.Paused && !s.Restarting && !s.Dead, nil
}

func runProxyCommand(ctx context.Context, w io.Writer, cfg *config.Config, store state.Store, hosts []scheduler.Host, envName string, selection proxySelection) error {
	setupCtx, cancel := context.WithTimeout(ctx, proxyPreflightTimeout)
	placed, err := selectProxyTarget(setupCtx, cfg, store, hosts, envName, selection)
	cancel()
	if err != nil {
		return err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(selection.localPort)))
	if err != nil {
		return fmt.Errorf("cannot listen on 127.0.0.1:%d: %w; choose an available local port", selection.localPort, err)
	}
	defer listener.Close()
	name := selection.name
	if !selection.accessory {
		name = fmt.Sprintf("%s.%d", name, selection.replica)
	}
	id := placed.target.ID
	if len(id) > 12 {
		id = id[:12]
	}
	fmt.Fprintf(w, "proxy %s/%s on %s container=%s", envName, name, placed.host.Name, id)
	if release := placed.target.Labels[docker.LabelRelease]; release != "" {
		fmt.Fprintf(w, " release=%s", release)
	}
	fmt.Fprintln(w)
	for _, binding := range placed.target.Published {
		fmt.Fprintf(w, "container port %d already has published binding %s; the proxy does not remove it\n", selection.remotePort, binding)
	}
	fmt.Fprintf(w, "listening on %s -> container TCP port %d; press Ctrl-C to stop\n", listener.Addr(), selection.remotePort)
	return serveProxyListener(ctx, w, listener, newProxyAgent(placed.host), placed.target, proxyWatchInterval)
}

func serveProxyListener(ctx context.Context, w io.Writer, listener net.Listener, client proxyAgent, target agent.ProxyTarget, watchInterval time.Duration) error {
	sessionCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var activeMu, outputMu sync.Mutex
	active := make(map[net.Conn]struct{})
	var workers sync.WaitGroup
	slots := make(chan struct{}, proxyConnectionLimit)
	logError := func(format string, args ...any) {
		outputMu.Lock()
		defer outputMu.Unlock()
		fmt.Fprintf(w, format, args...)
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-sessionCtx.Done()
		listener.Close()
		activeMu.Lock()
		defer activeMu.Unlock()
		for conn := range active {
			conn.Close()
		}
	}()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		ticker := time.NewTicker(watchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-sessionCtx.Done():
				return
			case <-ticker.C:
				checkCtx, checkCancel := context.WithTimeout(sessionCtx, proxyWatchTimeout)
				var result agent.ProxyTargetResult
				err := client.Call(checkCtx, "proxy_target", agent.ProxyTargetParams{Expected: &target}, &result)
				checkCancel()
				if err != nil {
					cancel(fmt.Errorf("proxy target changed or cannot be checked: %w; run the proxy command again", err))
					return
				}
				if result.Version != agent.ProxyStreamVersion {
					cancel(fmt.Errorf("proxy stream version changed; update the CLI and agent, then run the proxy command again"))
					return
				}
			}
		}
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if sessionCtx.Err() == nil {
				cancel(fmt.Errorf("accept proxy connection: %w", err))
			}
			break
		}
		select {
		case slots <- struct{}{}:
		default:
			conn.Close()
			logError("proxy connection limit of %d reached; close an existing connection and try again\n", proxyConnectionLimit)
			continue
		}
		activeMu.Lock()
		if sessionCtx.Err() != nil {
			activeMu.Unlock()
			conn.Close()
			<-slots
			break
		}
		active[conn] = struct{}{}
		activeMu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() {
				conn.Close()
				activeMu.Lock()
				delete(active, conn)
				activeMu.Unlock()
				<-slots
			}()
			if err := client.Proxy(sessionCtx, target, conn); err != nil && sessionCtx.Err() == nil {
				if agent.IsProxyFatal(err) {
					cancel(fmt.Errorf("proxy stopped: %w; run the proxy command again", err))
				} else {
					logError("proxy connection failed: %v\n", err)
				}
			}
		}()
	}
	<-shutdownDone
	workers.Wait()
	<-watchDone
	return context.Cause(sessionCtx)
}

func agentProxyCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "proxy",
		Short:  "Serve one private TCP connection over an SSH command stream",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return agent.NewServer().ServeProxy(cmd.Context(), os.Stdin, os.Stdout)
		},
	}
}
