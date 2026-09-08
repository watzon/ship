package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/watzon/ship/internal/docker"
)

type proxyInspectDocker struct {
	DockerOps
	container         proxyContainerInspect
	networkID, driver string
	calls             []string
	fail              bool
}

func proxyFixture() (*proxyInspectDocker, ProxyTargetParams) {
	var c proxyContainerInspect
	_ = json.Unmarshal([]byte(`{"Id":"`+strings.Repeat("a", 64)+`","Name":"/ship-demo-web-1","RestartCount":0,"Config":{"Labels":{"managed-by":"ship","project":"demo","environment":"production","service":"web","replica":"1","release":"old","custom.secret":"hidden"}},"State":{"Running":true,"StartedAt":"2026-09-08T01:02:03Z"},"NetworkSettings":{"SandboxID":"sandbox-one","Networks":{"ship-demo-production":{"NetworkID":"network-one","EndpointID":"endpoint-one","IPAddress":"172.18.0.2"}},"Ports":{"8080/tcp":[{"HostIP":"0.0.0.0","HostPort":"18080"},{"HostIP":"::","HostPort":"18080"}],"9090/tcp":[{"HostIP":"0.0.0.0","HostPort":"19090"}]}}}`), &c)
	return &proxyInspectDocker{container: c, networkID: "network-one", driver: "bridge"}, ProxyTargetParams{Name: "aaaaaaaaaaaa", Network: "ship-demo-production", Port: 8080, Labels: map[string]string{docker.LabelManagedBy: "ship", docker.LabelProject: "demo", docker.LabelEnvironment: "production", docker.LabelService: "web", docker.LabelReplica: "1"}}
}

func (f *proxyInspectDocker) Inspect(ctx context.Context, name string) (json.RawMessage, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("missing inspection deadline")
	}
	f.calls = append(f.calls, name)
	if f.fail {
		return nil, errors.New("sensitive Docker diagnostic")
	}
	if name == "ship-demo-production" {
		return json.Marshal([]map[string]string{{"Id": f.networkID, "Name": name, "Driver": f.driver}})
	}
	return json.Marshal([]proxyContainerInspect{f.container})
}

func TestProxyTargetFiltersAndPins(t *testing.T) {
	f, p := proxyFixture()
	s := Server{Docker: f}
	target, err := s.ResolveProxyTarget(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if target.ID != strings.Repeat("a", 64) || target.Name != "ship-demo-web-1" || target.Labels["custom.secret"] != "" || len(target.Labels) != 6 {
		t.Fatalf("unexpected target: %+v", target)
	}
	if strings.Join(target.Published, ",") != "0.0.0.0:18080->8080/tcp,[::]:18080->8080/tcp" {
		t.Fatalf("bindings: %v", target.Published)
	}
	_, err = s.ResolveProxyTarget(context.Background(), ProxyTargetParams{Expected: &target})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls[2] != target.ID {
		t.Fatalf("expected full ID lookup, got %v", f.calls)
	}
	f.container.State.StartedAt = "2026-09-08T01:03:00Z"
	_, err = s.ResolveProxyTarget(context.Background(), ProxyTargetParams{Expected: &target})
	if !IsProxyFatal(err) {
		t.Fatalf("old target accepted: %v", err)
	}
}

func TestProxyTargetRejectsInvalidMetadata(t *testing.T) {
	tests := map[string]func(*proxyInspectDocker){
		"short ID":              func(f *proxyInspectDocker) { f.container.ID = "aaaa" },
		"missing start":         func(f *proxyInspectDocker) { f.container.State.StartedAt = "" },
		"missing restart count": func(f *proxyInspectDocker) { f.container.RestartCount = nil },
		"paused":                func(f *proxyInspectDocker) { f.container.State.Paused = true },
		"restarting":            func(f *proxyInspectDocker) { f.container.State.Restarting = true },
		"stopped":               func(f *proxyInspectDocker) { f.container.State.Running = false },
		"missing sandbox":       func(f *proxyInspectDocker) { f.container.NetworkSettings.SandboxID = "" },
		"wrong driver":          func(f *proxyInspectDocker) { f.driver = "overlay" },
		"wrong network":         func(f *proxyInspectDocker) { f.networkID = "another" },
		"wrong labels":          func(f *proxyInspectDocker) { f.container.Config.Labels["project"] = "other" },
		"both kinds":            func(f *proxyInspectDocker) { f.container.Config.Labels["accessory"] = "redis" },
		"missing endpoint": func(f *proxyInspectDocker) {
			e := f.container.NetworkSettings.Networks["ship-demo-production"]
			e.EndpointID = ""
			f.container.NetworkSettings.Networks["ship-demo-production"] = e
		},
	}
	for _, address := range []string{"localhost", "127.0.0.1", "0.0.0.0", "224.0.0.1", "::1", "::", "ff02::1", "fe80::1%eth0"} {
		tests[address] = func(f *proxyInspectDocker) {
			e := f.container.NetworkSettings.Networks["ship-demo-production"]
			e.IPAddress = address
			f.container.NetworkSettings.Networks["ship-demo-production"] = e
		}
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			f, p := proxyFixture()
			mutate(f)
			_, err := (Server{Docker: f}).ResolveProxyTarget(context.Background(), p)
			if err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
}

func TestProxyTargetIPv6Fallback(t *testing.T) {
	f, p := proxyFixture()
	e := f.container.NetworkSettings.Networks[p.Network]
	e.IPAddress = ""
	e.GlobalIPv6Address = "fd00::2"
	f.container.NetworkSettings.Networks[p.Network] = e
	target, err := (Server{Docker: f}).ResolveProxyTarget(context.Background(), p)
	if err != nil || target.Address != "fd00::2" {
		t.Fatalf("target %v: %v", target, err)
	}
}

func TestProxyDialRechecksIdentityWithoutApplicationBytes(t *testing.T) {
	changes := map[string]func(*proxyInspectDocker){
		"replacement":     func(f *proxyInspectDocker) { f.container.ID = strings.Repeat("b", 64) },
		"same ID restart": func(f *proxyInspectDocker) { f.container.State.StartedAt = "2026-09-08T01:03:00Z" },
		"restart count":   func(f *proxyInspectDocker) { *f.container.RestartCount++ },
		"sandbox":         func(f *proxyInspectDocker) { f.container.NetworkSettings.SandboxID = "sandbox-two" },
		"endpoint": func(f *proxyInspectDocker) {
			e := f.container.NetworkSettings.Networks["ship-demo-production"]
			e.EndpointID = "endpoint-two"
			f.container.NetworkSettings.Networks["ship-demo-production"] = e
		},
		"IP reuse":         func(f *proxyInspectDocker) { f.container.ID = strings.Repeat("b", 64) },
		"inspection fails": func(f *proxyInspectDocker) { f.fail = true },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f, p := proxyFixture()
			s := Server{Docker: f}
			target, err := s.ResolveProxyTarget(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			local, peer := net.Pipe()
			defer peer.Close()
			s.ProxyDial = func(ctx context.Context, network, address string) (net.Conn, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("missing dial deadline")
				}
				if network != "tcp" || address != "172.18.0.2:8080" {
					t.Fatalf("unchecked address %s %s", network, address)
				}
				change(f)
				return local, nil
			}
			conn, err := s.DialProxyTarget(context.Background(), target)
			if conn != nil || !IsProxyFatal(err) {
				t.Fatalf("changed target returned conn %v error %v", conn, err)
			}
			_ = peer.SetReadDeadline(time.Now().Add(time.Second))
			buffer := make([]byte, 1)
			n, err := peer.Read(buffer)
			if n != 0 || err != io.EOF {
				t.Fatalf("connection not closed without bytes: n=%d err=%v", n, err)
			}
		})
	}
}

func TestProxyTargetProbeClosesSocket(t *testing.T) {
	f, p := proxyFixture()
	local, peer := net.Pipe()
	defer peer.Close()
	p.Probe = true
	s := Server{Docker: f, ProxyDial: func(context.Context, string, string) (net.Conn, error) { return local, nil }}
	if _, err := s.ResolveProxyTarget(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 6 {
		t.Fatalf("probe omitted inspection: %v", f.calls)
	}
	b := make([]byte, 1)
	if n, err := peer.Read(b); n != 0 || err != io.EOF {
		t.Fatalf("probe sent bytes or leaked: %d %v", n, err)
	}
}

func TestProxyTargetRPCAndErrors(t *testing.T) {
	f, p := proxyFixture()
	raw, _ := json.Marshal(p)
	s := Server{Docker: f}
	response := s.handleProxyTarget(context.Background(), Request{ID: "test", Params: raw})
	var result ProxyTargetResult
	if !response.OK || json.Unmarshal(response.Result, &result) != nil || result.Version != ProxyStreamVersion {
		t.Fatalf("response: %+v", response)
	}
	f.fail = true
	response = s.handleProxyTarget(context.Background(), Request{Params: raw})
	if response.ErrorCode != ProxyErrorUnavailable || strings.Contains(response.Error, "sensitive") {
		t.Fatalf("response: %+v", response)
	}
	if IsProxyFatal(&ProxyError{Code: ProxyErrorConnect}) {
		t.Fatal("connect refusal must affect only one connection")
	}
}

func TestProxyTargetRejectsInvalidRequestsBeforeInspect(t *testing.T) {
	tests := map[string]func(*ProxyTargetParams){
		"zero port":           func(p *ProxyTargetParams) { p.Port = 0 },
		"high port":           func(p *ProxyTargetParams) { p.Port = 65536 },
		"missing ownership":   func(p *ProxyTargetParams) { delete(p.Labels, docker.LabelManagedBy) },
		"missing project":     func(p *ProxyTargetParams) { delete(p.Labels, docker.LabelProject) },
		"missing environment": func(p *ProxyTargetParams) { delete(p.Labels, docker.LabelEnvironment) },
		"missing kind":        func(p *ProxyTargetParams) { delete(p.Labels, docker.LabelService) },
		"both kinds":          func(p *ProxyTargetParams) { p.Labels[docker.LabelAccessory] = "redis" },
		"zero replica":        func(p *ProxyTargetParams) { p.Labels[docker.LabelReplica] = "0" },
		"invalid replica":     func(p *ProxyTargetParams) { p.Labels[docker.LabelReplica] = "1x" },
		"custom label":        func(p *ProxyTargetParams) { p.Labels["secret"] = "value" },
		"missing name":        func(p *ProxyTargetParams) { p.Name = "" },
		"name option":         func(p *ProxyTargetParams) { p.Name = "--help" },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			f, p := proxyFixture()
			change(&p)
			_, err := (Server{Docker: f}).ResolveProxyTarget(context.Background(), p)
			if err == nil {
				t.Fatal("invalid request accepted")
			}
			if len(f.calls) != 0 {
				t.Fatalf("invalid request inspected Docker: %v", f.calls)
			}
		})
	}
}

func TestProxyDialRejectsChangedTargetBeforeDial(t *testing.T) {
	f, p := proxyFixture()
	s := Server{Docker: f}
	target, err := s.ResolveProxyTarget(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	f.container.State.StartedAt = "2026-09-08T01:03:00Z"
	s.ProxyDial = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("dialed changed target")
		return nil, nil
	}
	if _, err := s.DialProxyTarget(context.Background(), target); !IsProxyFatal(err) {
		t.Fatalf("changed target accepted: %v", err)
	}
	target.ID = "aaaaaaaaaaaa"
	if _, err := s.DialProxyTarget(context.Background(), target); !IsProxyFatal(err) {
		t.Fatalf("short pinned ID accepted: %v", err)
	}
}
