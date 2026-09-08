package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/watzon/ship/internal/accessory"
	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/deployment"
	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/state"
)

type proxyTestAgent struct {
	callFn  func(context.Context, string, any, any) error
	proxyFn func(context.Context, agent.ProxyTarget, net.Conn) error
}

func (a proxyTestAgent) Call(ctx context.Context, method string, params, out any) error {
	return a.callFn(ctx, method, params, out)
}

func (a proxyTestAgent) Proxy(ctx context.Context, target agent.ProxyTarget, local net.Conn) error {
	return a.proxyFn(ctx, target, local)
}

type proxyTestFleet struct {
	config      *config.Config
	hosts       []scheduler.Host
	store       state.Store
	observed    map[string][]docker.ContainerSummary
	running     map[string]bool
	paused      map[string]bool
	unreachable string
	oldAgent    bool
	version     int
	probed      []agent.ProxyTargetParams
}

func newProxyTestFleet(t *testing.T) *proxyTestFleet {
	t.Helper()
	f := &proxyTestFleet{
		config: &config.Config{
			Project:     "demo",
			Services:    map[string]config.Service{"web": {Pool: "app"}},
			Accessories: map[string]config.Accessory{"postgres": {Pool: "data"}},
		},
		hosts: []scheduler.Host{
			{Name: "app-a", Pool: "app", Contact: "app-a"},
			{Name: "app-b", Pool: "app", Contact: "app-b"},
			{Name: "data-a", Pool: "data", Contact: "data-a"},
			{Name: "data-b", Pool: "data", Contact: "data-b"},
		},
		store: state.NewStore(t.TempDir()), observed: map[string][]docker.ContainerSummary{},
		running: map[string]bool{}, paused: map[string]bool{}, version: agent.ProxyStreamVersion,
	}
	original := newProxyAgent
	newProxyAgent = func(host scheduler.Host) proxyAgent {
		return proxyTestAgent{callFn: func(_ context.Context, method string, params, out any) error {
			if host.Name == f.unreachable {
				return errors.New("SSH host is unavailable")
			}
			switch method {
			case "list_ship_containers":
				*out.(*[]docker.ContainerSummary) = f.observed[host.Name]
			case "docker_inspect":
				name := params.(agent.DockerInspectParams).Name
				raw, _ := json.Marshal([]any{map[string]any{"State": map[string]bool{"Running": f.running[name], "Paused": f.paused[name]}}})
				out.(*agent.DockerInspectResult).Inspect = raw
			case "status":
				if !f.oldAgent {
					out.(*agent.Status).SupportedMethods = []string{"proxy_target"}
				}
			case "proxy_target":
				p := params.(agent.ProxyTargetParams)
				f.probed = append(f.probed, p)
				for _, container := range f.observed[host.Name] {
					if container.ID == p.Name {
						*out.(*agent.ProxyTargetResult) = agent.ProxyTargetResult{Version: f.version, Target: agent.ProxyTarget{ID: container.ID, Name: container.Names, Labels: container.Labels, Port: p.Port}}
						return nil
					}
				}
				return errors.New("missing test container")
			default:
				return fmt.Errorf("unexpected RPC %s", method)
			}
			return nil
		}}
	}
	t.Cleanup(func() { newProxyAgent = original })
	return f
}

func (f *proxyTestFleet) add(host, service string, replica int, release string, running bool) docker.ContainerSummary {
	name := deployment.ContainerName("demo", "production", service, replica, release)
	labels := deployment.ContainerLabels("demo", "production", service, replica, release)
	if service == "postgres" {
		name = accessory.ContainerName("demo", "production", service)
		labels = accessory.ContainerLabels("demo", "production", service)
	}
	labels[docker.LabelManagedBy] = docker.LabelManagedByValue
	digest := sha256.Sum256([]byte(host + name))
	container := docker.ContainerSummary{ID: hex.EncodeToString(digest[:]), Names: name, Labels: labels, Status: "Status is deliberately not authoritative"}
	f.observed[host] = append(f.observed[host], container)
	f.running[container.ID] = running
	return container
}

func TestProxyPortMapping(t *testing.T) {
	for _, tc := range []struct {
		input  string
		local  int
		remote int
	}{
		{"15432:5432", 15432, 5432}, {"1:65535", 1, 65535}, {"00080:080", 80, 80},
		{"0:5432", 0, 0}, {"80:65536", 0, 0}, {"80:0", 0, 0}, {"80", 0, 0},
		{"127.0.0.1:80:80", 0, 0}, {"80:80/udp", 0, 0}, {"80-81:80", 0, 0},
		{"+80:80", 0, 0}, {" 80:80", 0, 0}, {"80:80 ", 0, 0}, {"999999999999999999999999:80", 0, 0},
	} {
		t.Run(tc.input, func(t *testing.T) {
			local, remote, err := parseProxyPorts(tc.input)
			if tc.local == 0 {
				if err == nil {
					t.Fatal("invalid mapping accepted")
				}
			} else if err != nil || local != tc.local || remote != tc.remote {
				t.Fatalf("mapping = %d:%d, %v", local, remote, err)
			}
		})
	}
}

func FuzzProxyPortMapping(f *testing.F) {
	for _, seed := range []string{"15432:5432", "", "80:80/udp", "0:0", "65536:80"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		local, remote, err := parseProxyPorts(value)
		if err == nil && (local < 1 || local > 65535 || remote < 1 || remote > 65535 || strings.Count(value, ":") != 1) {
			t.Fatalf("accepted invalid mapping %q", value)
		}
	})
}

func TestProxySelectsObservedReplicaWithoutReleaseState(t *testing.T) {
	f := newProxyTestFleet(t)
	f.add("app-a", "web", 1, "old", true)
	expected := f.add("app-b", "web", 2, "current", true)
	f.add("app-a", "web", 2, "previous", false)
	result, err := selectProxyTarget(context.Background(), f.config, f.store, f.hosts, "production", proxySelection{name: "web", replica: 2, remotePort: 8080})
	if err != nil {
		t.Fatal(err)
	}
	if result.host.Name != "app-b" || result.target.ID != expected.ID {
		t.Fatalf("selected = %+v", result)
	}
	if len(f.probed) != 1 || !f.probed[0].Probe || f.probed[0].Port != 8080 || f.probed[0].Network != "ship-demo-production" {
		t.Fatalf("probe = %+v", f.probed)
	}
	if entries, err := os.ReadDir(f.store.Dir); err != nil || len(entries) != 0 {
		t.Fatalf("selection changed local state: entries=%v error=%v", entries, err)
	}
}

func TestProxySelectionRejectsAmbiguityAndUnavailableTargets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*proxyTestFleet)
		want   string
	}{
		{"multiple releases", func(f *proxyTestFleet) { f.add("app-b", "web", 1, "new", true) }, "exact container name"},
		{"paused", func(f *proxyTestFleet) { f.paused[f.observed["app-a"][0].ID] = true }, "no running container"},
		{"stopped", func(f *proxyTestFleet) { f.running[f.observed["app-a"][0].ID] = false }, "no running container"},
		{"wrong project", func(f *proxyTestFleet) { f.observed["app-a"][0].Labels[docker.LabelProject] = "other" }, "no running container"},
		{"incomplete discovery", func(f *proxyTestFleet) { f.unreachable = "data-b" }, "data-b"},
		{"old agent", func(f *proxyTestFleet) { f.oldAgent = true }, "ship agent upgrade production"},
		{"stream version", func(f *proxyTestFleet) { f.version = 99 }, "unsupported proxy stream version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProxyTestFleet(t)
			f.add("app-a", "web", 1, "old", true)
			tc.change(f)
			_, err := selectProxyTarget(context.Background(), f.config, f.store, f.hosts, "production", proxySelection{name: "web", replica: 1, remotePort: 8080})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestProxyExactContainerSelectsOneRelease(t *testing.T) {
	f := newProxyTestFleet(t)
	f.add("app-a", "web", 1, "old", true)
	expected := f.add("app-a", "web", 1, "release.17", true)
	result, err := selectProxyTarget(context.Background(), f.config, f.store, f.hosts, "production", proxySelection{name: "web", replica: 1, containerName: expected.Names, remotePort: 8080})
	if err != nil || result.target.ID != expected.ID {
		t.Fatalf("target=%+v error=%v", result, err)
	}
}

func TestProxyAccessoryPlacement(t *testing.T) {
	for _, tc := range []struct{ name, savedHost, extraHost, want string }{
		{name: "no local placement"}, {name: "saved placement", savedHost: "data-a"},
		{name: "wrong placement", savedHost: "data-b", want: "saved placement"},
		{name: "duplicate", extraHost: "data-b", want: "multiple managed containers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProxyTestFleet(t)
			expected := f.add("data-a", "postgres", 0, "", true)
			if tc.extraHost != "" {
				f.add(tc.extraHost, "postgres", 0, "", false)
			}
			if tc.savedHost != "" {
				if err := f.store.SaveAccessoryState(state.AccessoryState{Environment: "production", Name: "postgres", Host: state.HostFact{Name: tc.savedHost, Pool: "data"}}); err != nil {
					t.Fatal(err)
				}
			}
			result, err := selectProxyTarget(context.Background(), f.config, f.store, f.hosts, "production", proxySelection{name: "postgres", accessory: true, remotePort: 5432})
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error=%v, want %q", err, tc.want)
				}
				return
			}
			if err != nil || result.target.ID != expected.ID {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if tc.savedHost == "" {
				if _, err := f.store.ReadAccessoryState("production", "postgres"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("selection wrote placement: %v", err)
				}
			}
		})
	}
}

func TestProxyDryRunAndReplicaArguments(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{"default replica", []string{"production", "web", "18080:8080"}, "web.1", false},
		{"replica flag", []string{"production", "web", "18080:8080", "--replica", "2"}, "web.2", false},
		{"replica shorthand", []string{"production", "web.2", "18080:8080"}, "web.2", false},
		{"exact container with dotted release", []string{"production", deployment.ContainerName("demo", "production", "web", 1, "release.17"), "18080:8080"}, "requested container:", false},
		{"conflict", []string{"production", "web.2", "18080:8080", "--replica", "1"}, "conflicts", true},
		{"zero replica", []string{"production", "web", "18080:8080", "--replica", "0"}, "greater than zero", true},
		{"unknown service", []string{"production", "missing", "18080:8080"}, "unknown service", true},
		{"unknown environment", []string{"missing", "web", "18080:8080"}, "environment", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "ship.yml")
			if err := os.WriteFile(path, []byte(singleHostConfig()), 0o600); err != nil {
				t.Fatal(err)
			}
			original := newProxyAgent
			newProxyAgent = func(scheduler.Host) proxyAgent { t.Fatal("dry-run made an agent call"); return nil }
			defer func() { newProxyAgent = original }()
			var out bytes.Buffer
			cmd := proxyCmd(&options{configPath: path, dryRun: true})
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error=%v want=%q", err, tc.want)
				}
			} else if err != nil || !strings.Contains(out.String(), tc.want) || !strings.Contains(out.String(), "checks are pending") {
				t.Fatalf("output=%s error=%v", out.String(), err)
			}
			if _, err := os.Stat(filepath.Join(dir, config.LocalStateDir)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("dry-run created state: %v", err)
			}
		})
	}
}

func proxySessionTestListener(t *testing.T, client proxyAgent, interval time.Duration) (net.Listener, context.CancelFunc, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveProxyListener(ctx, io.Discard, listener, client, agent.ProxyTarget{}, interval) }()
	t.Cleanup(func() { cancel(); listener.Close() })
	return listener, cancel, done
}

func proxySessionWait(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("proxy session did not stop")
		return nil
	}
}

func TestProxyListenerConcurrentConnectionsAndCancellation(t *testing.T) {
	client := proxyTestAgent{proxyFn: func(_ context.Context, _ agent.ProxyTarget, conn net.Conn) error {
		_, err := io.Copy(conn, conn)
		return err
	}}
	listener, cancel, done := proxySessionTestListener(t, client, time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(3 * time.Second))
			want := []byte{0, 1, 255, 4}
			if _, err := conn.Write(want); err != nil {
				t.Error(err)
				return
			}
			got := make([]byte, len(want))
			if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(want, got) {
				t.Errorf("round trip=%v error=%v", got, err)
			}
		}()
	}
	wg.Wait()
	cancel()
	if err := proxySessionWait(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if conn, err := net.DialTimeout("tcp", listener.Addr().String(), 100*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("listener survived cancellation")
	}
}

func TestProxyListenerErrorScope(t *testing.T) {
	for _, fatal := range []bool{false, true} {
		t.Run(fmt.Sprint(fatal), func(t *testing.T) {
			var calls atomic.Int32
			client := proxyTestAgent{proxyFn: func(_ context.Context, _ agent.ProxyTarget, conn net.Conn) error {
				if calls.Add(1) == 1 {
					code := agent.ProxyErrorConnect
					if fatal {
						code = agent.ProxyErrorTargetChanged
					}
					return &agent.ProxyError{Code: code, Message: "test failure"}
				}
				_, err := io.Copy(conn, conn)
				return err
			}}
			listener, cancel, done := proxySessionTestListener(t, client, time.Hour)
			conn, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			conn.SetReadDeadline(time.Now().Add(time.Second))
			_, _ = conn.Read(make([]byte, 1))
			conn.Close()
			if fatal {
				if err := proxySessionWait(t, done); err == nil || !strings.Contains(err.Error(), "proxy stopped") {
					t.Fatalf("error=%v", err)
				}
			} else {
				conn, err := net.Dial("tcp", listener.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				conn.SetDeadline(time.Now().Add(time.Second))
				_, _ = conn.Write([]byte("x"))
				if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
					t.Fatal(err)
				}
				conn.Close()
				cancel()
				proxySessionWait(t, done)
			}
		})
	}
}

func TestProxyListenerWatcherStopsIdleSession(t *testing.T) {
	client := proxyTestAgent{callFn: func(_ context.Context, method string, params, _ any) error {
		if method != "proxy_target" || params.(agent.ProxyTargetParams).Expected == nil {
			t.Error("watch did not validate pinned target")
		}
		return errors.New("container changed")
	}}
	_, _, done := proxySessionTestListener(t, client, 5*time.Millisecond)
	if err := proxySessionWait(t, done); err == nil || !strings.Contains(err.Error(), "run the proxy command again") {
		t.Fatalf("error=%v", err)
	}
}

func TestProxyListenerConnectionLimit(t *testing.T) {
	started := make(chan struct{}, proxyConnectionLimit)
	client := proxyTestAgent{proxyFn: func(ctx context.Context, _ agent.ProxyTarget, _ net.Conn) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}}
	listener, cancel, done := proxySessionTestListener(t, client, time.Hour)
	var connections []net.Conn
	defer func() {
		for _, conn := range connections {
			conn.Close()
		}
	}()
	for i := 0; i < proxyConnectionLimit; i++ {
		conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
	}
	for i := 0; i < proxyConnectionLimit; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("connections did not start")
		}
	}
	excess, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	excess.SetReadDeadline(time.Now().Add(time.Second))
	_, err = excess.Read(make([]byte, 1))
	excess.Close()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("excess connection error=%v", err)
	}
	cancel()
	proxySessionWait(t, done)
}
