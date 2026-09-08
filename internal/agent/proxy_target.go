package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/watzon/ship/internal/docker"
)

// ProxyStreamVersion identifies the private TCP stream protocol.
const ProxyStreamVersion = 1

const (
	ProxyErrorTargetChanged = "target_changed"
	ProxyErrorProtocol      = "protocol_error"
	ProxyErrorUnavailable   = "target_unavailable"
	ProxyErrorConnect       = "connect_failed"
)

// ProxyError reports a bounded proxy failure without Docker inspect contents.
type ProxyError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ProxyError) Error() string { return e.Message }

// IsProxyFatal reports failures that require closing the complete proxy session.
func IsProxyFatal(err error) bool {
	var p *ProxyError
	return errors.As(err, &p) && (p.Code == ProxyErrorTargetChanged || p.Code == ProxyErrorProtocol || p.Code == ProxyErrorUnavailable)
}

// ProxyTargetParams selects a managed container or verifies its pinned identity.
type ProxyTargetParams struct {
	Name     string            `json:"name,omitempty"`
	Network  string            `json:"network,omitempty"`
	Port     int               `json:"port,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Expected *ProxyTarget      `json:"expected,omitempty"`
	Probe    bool              `json:"probe,omitempty"`
}

// ProxyTarget contains only the container identity needed for private TCP access.
type ProxyTarget struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	StartedAt    string            `json:"started_at"`
	RestartCount int               `json:"restart_count"`
	SandboxID    string            `json:"sandbox_id"`
	Network      string            `json:"network"`
	NetworkID    string            `json:"network_id"`
	EndpointID   string            `json:"endpoint_id"`
	Address      string            `json:"address"`
	Port         int               `json:"port"`
	Labels       map[string]string `json:"labels"`
	Published    []string          `json:"published,omitempty"`
}

// ProxyTargetResult returns the supported stream version and verified target.
type ProxyTargetResult struct {
	Version int         `json:"version"`
	Target  ProxyTarget `json:"target"`
}

var proxyContainerID = regexp.MustCompile(`^[a-f0-9]{64}$`)
var proxyLabelKeys = []string{docker.LabelManagedBy, docker.LabelProject, docker.LabelEnvironment, docker.LabelService, docker.LabelAccessory, docker.LabelReplica, docker.LabelRelease}

func proxyFailure(code, message string) error { return &ProxyError{Code: code, Message: message} }

func validateProxyLabels(labels map[string]string) error {
	if labels[docker.LabelManagedBy] != docker.LabelManagedByValue || strings.TrimSpace(labels[docker.LabelProject]) == "" || strings.TrimSpace(labels[docker.LabelEnvironment]) == "" {
		return proxyFailure(ProxyErrorProtocol, "Proxy target requires Ship ownership, project, and environment labels.")
	}
	service, accessory := labels[docker.LabelService], labels[docker.LabelAccessory]
	if (strings.TrimSpace(service) == "") == (strings.TrimSpace(accessory) == "") {
		return proxyFailure(ProxyErrorProtocol, "Proxy target requires exactly one service or accessory label.")
	}
	if service != "" {
		replica, err := strconv.Atoi(labels[docker.LabelReplica])
		if err != nil || replica < 1 || strconv.Itoa(replica) != labels[docker.LabelReplica] {
			return proxyFailure(ProxyErrorProtocol, "Proxy target requires a positive service replica.")
		}
	}
	for key, value := range labels {
		allowed := false
		for _, reserved := range proxyLabelKeys {
			if key == reserved {
				allowed = true
				break
			}
		}
		if !allowed || strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n\x00") {
			return proxyFailure(ProxyErrorProtocol, "Proxy target contains an invalid scope label.")
		}
	}
	return nil
}

func validateProxyTarget(t ProxyTarget) error {
	started, err := time.Parse(time.RFC3339Nano, t.StartedAt)
	address, addrErr := netip.ParseAddr(t.Address)
	if !proxyContainerID.MatchString(t.ID) || !validDockerVolumeName(t.Name) || t.Name != strings.TrimSpace(t.Name) || err != nil || started.IsZero() || t.RestartCount < 0 || strings.TrimSpace(t.SandboxID) == "" || !validDockerVolumeName(t.Network) || strings.HasPrefix(t.Network, "-") || t.Network != strings.TrimSpace(t.Network) || strings.TrimSpace(t.NetworkID) == "" || strings.TrimSpace(t.EndpointID) == "" || t.Port < 1 || t.Port > 65535 || addrErr != nil || !address.IsGlobalUnicast() || address.IsLoopback() || address.Zone() != "" {
		return proxyFailure(ProxyErrorProtocol, "Proxy target has an incomplete or invalid container identity. Run the proxy command again.")
	}
	return validateProxyLabels(t.Labels)
}

func sameProxyIdentity(a, b ProxyTarget) bool {
	return a.ID == b.ID && a.Name == b.Name && a.StartedAt == b.StartedAt && a.RestartCount == b.RestartCount && a.SandboxID == b.SandboxID && a.Network == b.Network && a.NetworkID == b.NetworkID && a.EndpointID == b.EndpointID && a.Address == b.Address && a.Port == b.Port && maps.Equal(a.Labels, b.Labels)
}

// ResolveProxyTarget inspects a managed container and its actual Docker bridge.
// A probe uses the same identity checks as each later stream connection.
func (s Server) ResolveProxyTarget(ctx context.Context, p ProxyTargetParams) (ProxyTarget, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if p.Expected != nil {
		if err := validateProxyTarget(*p.Expected); err != nil {
			return ProxyTarget{}, err
		}
		if (p.Name != "" && p.Name != p.Expected.ID) || (p.Network != "" && p.Network != p.Expected.Network) || (p.Port != 0 && p.Port != p.Expected.Port) {
			return ProxyTarget{}, proxyFailure(ProxyErrorProtocol, "Proxy request conflicts with the pinned target.")
		}
		if len(p.Labels) != 0 {
			for k, v := range p.Labels {
				if p.Expected.Labels[k] != v {
					return ProxyTarget{}, proxyFailure(ProxyErrorProtocol, "Proxy labels conflict with the pinned target.")
				}
			}
		}
		p.Name, p.Network, p.Port, p.Labels = p.Expected.ID, p.Expected.Network, p.Expected.Port, p.Expected.Labels
	}
	if !validDockerVolumeName(p.Name) || strings.HasPrefix(p.Name, "-") || p.Name != strings.TrimSpace(p.Name) || !validDockerVolumeName(p.Network) || strings.HasPrefix(p.Network, "-") || p.Network != strings.TrimSpace(p.Network) || p.Port < 1 || p.Port > 65535 {
		return ProxyTarget{}, proxyFailure(ProxyErrorProtocol, "Proxy request requires a container, network, and TCP port from 1 through 65535.")
	}
	if err := validateProxyLabels(p.Labels); err != nil {
		return ProxyTarget{}, err
	}
	t, err := s.inspectProxyTarget(ctx, p)
	if err != nil {
		return ProxyTarget{}, err
	}
	if p.Expected != nil && !sameProxyIdentity(t, *p.Expected) {
		return ProxyTarget{}, proxyFailure(ProxyErrorTargetChanged, "Proxy target changed. Run the proxy command again.")
	}
	if p.Probe {
		conn, err := s.DialProxyTarget(ctx, t)
		if err != nil {
			return ProxyTarget{}, err
		}
		_ = conn.Close()
	}
	return t, nil
}

type proxyContainerInspect struct {
	ID           string `json:"Id"`
	Name         string
	RestartCount *int
	Config       struct{ Labels map[string]string }
	State        struct {
		Running, Paused, Restarting, Dead bool
		StartedAt                         string
	}
	NetworkSettings struct {
		SandboxID string
		Networks  map[string]struct{ NetworkID, EndpointID, IPAddress, GlobalIPv6Address string }
		Ports     map[string][]struct{ HostIP, HostPort string }
	}
}

func (s Server) inspectProxyTarget(ctx context.Context, p ProxyTargetParams) (ProxyTarget, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := s.docker().Inspect(ctx, p.Name)
	if err != nil {
		return ProxyTarget{}, proxyFailure(ProxyErrorUnavailable, "Cannot inspect the proxy container. Run the proxy command again.")
	}
	var containers []proxyContainerInspect
	if json.Unmarshal(raw, &containers) != nil || len(containers) != 1 {
		return ProxyTarget{}, proxyFailure(ProxyErrorUnavailable, "Docker returned invalid proxy container metadata.")
	}
	c := containers[0]
	if !c.State.Running || c.State.Paused || c.State.Restarting || c.State.Dead {
		return ProxyTarget{}, proxyFailure(ProxyErrorTargetChanged, "Proxy container is not running. Run the proxy command again.")
	}
	for key, value := range p.Labels {
		if c.Config.Labels[key] != value {
			return ProxyTarget{}, proxyFailure(ProxyErrorTargetChanged, "Proxy container labels changed. Run the proxy command again.")
		}
	}
	endpoint, ok := c.NetworkSettings.Networks[p.Network]
	if !ok {
		return ProxyTarget{}, proxyFailure(ProxyErrorTargetChanged, "Proxy container is no longer attached to the expected network. Run the proxy command again.")
	}
	raw, err = s.docker().Inspect(ctx, p.Network)
	if err != nil {
		return ProxyTarget{}, proxyFailure(ProxyErrorUnavailable, "Cannot inspect the proxy network. Run the proxy command again.")
	}
	var networks []struct {
		ID           string `json:"Id"`
		Name, Driver string
	}
	if json.Unmarshal(raw, &networks) != nil || len(networks) != 1 || networks[0].Name != p.Network || networks[0].Driver != "bridge" || networks[0].ID != endpoint.NetworkID {
		return ProxyTarget{}, proxyFailure(ProxyErrorTargetChanged, "Proxy target requires the expected Docker bridge network. Run the proxy command again.")
	}
	labels := map[string]string{}
	for _, key := range proxyLabelKeys {
		if value := c.Config.Labels[key]; value != "" {
			labels[key] = value
		}
	}
	address := endpoint.IPAddress
	if address == "" {
		address = endpoint.GlobalIPv6Address
	}
	restartCount := -1
	if c.RestartCount != nil {
		restartCount = *c.RestartCount
	}
	t := ProxyTarget{ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), StartedAt: c.State.StartedAt, RestartCount: restartCount, SandboxID: c.NetworkSettings.SandboxID, Network: p.Network, NetworkID: endpoint.NetworkID, EndpointID: endpoint.EndpointID, Address: address, Port: p.Port, Labels: labels}
	if err := validateProxyTarget(t); err != nil {
		return ProxyTarget{}, proxyFailure(ProxyErrorUnavailable, "Docker returned an incomplete or invalid proxy target identity.")
	}
	for _, binding := range c.NetworkSettings.Ports[strconv.Itoa(p.Port)+"/tcp"] {
		port, err := strconv.Atoi(binding.HostPort)
		ip, ipErr := netip.ParseAddr(binding.HostIP)
		if err != nil || port < 1 || port > 65535 || ipErr != nil || ip.Zone() != "" {
			return ProxyTarget{}, proxyFailure(ProxyErrorUnavailable, "Docker returned an invalid published port binding.")
		}
		t.Published = append(t.Published, fmt.Sprintf("%s->%d/tcp", net.JoinHostPort(binding.HostIP, binding.HostPort), p.Port))
	}
	sort.Strings(t.Published)
	return t, nil
}

// DialProxyTarget checks the pinned identity before and after connecting.
// It sends and reads no application bytes until both checks succeed.
func (s Server) DialProxyTarget(ctx context.Context, target ProxyTarget) (net.Conn, error) {
	verified, err := s.ResolveProxyTarget(ctx, ProxyTargetParams{Expected: &target})
	if err != nil {
		return nil, err
	}
	dial := s.ProxyDial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, err := dial(dialCtx, "tcp", net.JoinHostPort(verified.Address, strconv.Itoa(verified.Port)))
	cancel()
	if err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, proxyFailure(ProxyErrorConnect, "Cannot connect to the proxy container TCP port.")
	}
	if conn == nil {
		return nil, proxyFailure(ProxyErrorConnect, "Cannot connect to the proxy container TCP port.")
	}
	if _, err := s.ResolveProxyTarget(ctx, ProxyTargetParams{Expected: &target}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if ctx.Err() != nil {
		_ = conn.Close()
		return nil, proxyFailure(ProxyErrorConnect, "Proxy connection setup was canceled.")
	}
	return conn, nil
}

func (s Server) handleProxyTarget(ctx context.Context, req Request) Response {
	var p ProxyTargetParams
	if err := decode(req.Params, &p); err != nil {
		return failure(req.ID, ProxyErrorProtocol, errors.New("Invalid proxy target request."))
	}
	target, err := s.ResolveProxyTarget(ctx, p)
	if err != nil {
		var proxyErr *ProxyError
		if errors.As(err, &proxyErr) {
			return failure(req.ID, proxyErr.Code, proxyErr)
		}
		return failure(req.ID, ProxyErrorUnavailable, errors.New("Cannot resolve the proxy target."))
	}
	return result(req.ID, ProxyTargetResult{Version: ProxyStreamVersion, Target: target})
}
