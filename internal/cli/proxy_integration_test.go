package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/watzon/ship/internal/deployment"
)

// TestProxyIntegration uses a disposable privileged Docker host. The default
// test suite does not build images, access Docker, or open SSH connections.
func TestProxyIntegration(t *testing.T) {
	if os.Getenv("SHIP_PROXY_INTEGRATION") != "1" {
		t.Skip("set SHIP_PROXY_INTEGRATION=1 to test real SSH and Docker")
	}
	for _, name := range []string{"docker", "go", "ssh", "ssh-keygen"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatalf("proxy integration requires %s on PATH: %v", name, err)
		}
	}
	f := newProxyIntegrationFixture(t)
	t.Run("unpublished ports and bridge isolation", func(t *testing.T) {
		for _, name := range []string{f.database, f.web1, f.web2, f.halfclose} {
			output := f.docker(t, "inspect", "--format", "{{json .NetworkSettings.Ports}}", name)
			var ports map[string][]any
			if err := json.Unmarshal([]byte(output), &ports); err != nil {
				t.Fatal(err)
			}
			for port, bindings := range ports {
				if len(bindings) != 0 {
					t.Fatalf("%s publishes %s: %v", name, port, bindings)
				}
			}
		}
		f.docker(t, "network", "create", "outside")
		for _, target := range []struct{ name, port string }{{f.database, "5432"}, {f.web1, "3000"}} {
			address := strings.TrimSpace(f.docker(t, "inspect", "--format", "{{(index .NetworkSettings.Networks \"ship-proxy-integration-test\").IPAddress}}", target.name))
			probeArgs := func(network string) []string {
				return []string{"run", "--rm", "--network", network, "-v", "/fixture/probe:/probe:ro", "alpine:3.22", "/probe", net.JoinHostPort(address, target.port)}
			}
			output, err := f.tryDocker(probeArgs("ship-proxy-integration-test")...)
			if err != nil || strings.TrimSpace(output) != "connected" {
				t.Fatalf("same-network probe could not reach %s:%s: %v, %s", address, target.port, err, output)
			}
			output, err = f.tryDocker(probeArgs("outside")...)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 || !strings.HasPrefix(output, "connect_failed:") {
				t.Fatalf("outside probe must report TCP failure for %s:%s, got %v, %s", address, target.port, err, output)
			}
		}
	})
	t.Run("Postgres through accessory command", func(t *testing.T) {
		p := f.startProxy(t, false, "postgres", 5432)
		started := time.Now()
		value, err := proxyIntegrationSQL(p.address)
		if err != nil || value != "42" {
			t.Fatalf("SELECT 42 returned %q, %v; %s", value, err, p.logs())
		}
		t.Logf("Postgres connection plus query: %s", time.Since(started))
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				v, err := proxyIntegrationSQL(p.address)
				if err != nil || v != "42" {
					t.Errorf("concurrent SELECT 42: %q %v", v, err)
				}
			}()
		}
		wg.Wait()
		p.interrupt(t)
	})
	t.Run("service replica selection", func(t *testing.T) {
		first := f.startProxy(t, true, "web", 3000)
		if body := proxyIntegrationHTTP(t, first); !strings.Contains(body, "accessory=not-configured") {
			t.Fatalf("wrong replica: %s", body)
		}
		first.interrupt(t)
		second := f.startProxy(t, true, "web.2", 3000)
		if body := proxyIntegrationHTTP(t, second); !strings.Contains(body, "accessory=configured") {
			t.Fatalf("wrong replica: %s", body)
		}
		second.interrupt(t)
		flagged := f.startProxy(t, true, "web", 3000, "--replica", "2")
		if body := proxyIntegrationHTTP(t, flagged); !strings.Contains(body, "accessory=configured") {
			t.Fatalf("wrong --replica 2 target: %s", body)
		}
		flagged.interrupt(t)
		exact := f.startProxy(t, true, f.web2, 3000)
		if body := proxyIntegrationHTTP(t, exact); !strings.Contains(body, "accessory=configured") {
			t.Fatalf("wrong exact container: %s", body)
		}
		exact.interrupt(t)
	})
	t.Run("client sends FIN first", func(t *testing.T) {
		p := f.startProxy(t, true, "halfclose", 4000)
		conn, err := net.DialTimeout("tcp", p.address, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
		payload := bytes.Repeat([]byte{0, 1, 2, 255}, 32768)
		if _, err := conn.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(conn)
		if err != nil || !bytes.Equal(body, append([]byte("reply:"), payload...)) {
			t.Fatalf("half close: %d bytes, %v; %s", len(body), err, p.logs())
		}
		p.interrupt(t)
	})
	t.Run("server sends FIN first", func(t *testing.T) {
		p := f.startProxy(t, true, "halfclose", 4001)
		conn, err := net.DialTimeout("tcp", p.address, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
		greeting, err := io.ReadAll(conn)
		if err != nil || string(greeting) != "ready\n" {
			t.Fatalf("server FIN or greeting: %q %v", greeting, err)
		}
		payload := bytes.Repeat([]byte{255, 0, 2, 1}, 32768)
		if _, err := conn.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatal(err)
		}
		expected := fmt.Sprintf("server-first received %d bytes sha256=%x", len(payload), sha256.Sum256(payload))
		f.until(t, 10*time.Second, func() bool {
			return strings.Contains(f.docker(t, "logs", f.halfclose), expected)
		}, "server receiving all client bytes after its FIN")
		p.interrupt(t)
	})
	t.Run("Ctrl-C closes active streams", func(t *testing.T) {
		p := f.startProxy(t, true, "halfclose", 4000)
		conn, err := net.DialTimeout("tcp", p.address, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte("pending request")); err != nil {
			t.Fatal(err)
		}
		f.until(t, 10*time.Second, func() bool {
			out := f.run(t, nil, "docker", "exec", f.name, "ps", "-o", "args")
			return strings.Contains(out, "/usr/local/bin/ship agent proxy")
		}, "active remote proxy stream")
		p.interrupt(t)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		b := make([]byte, 1)
		if n, err := conn.Read(b); n != 0 || err == nil {
			t.Fatalf("active stream survived Ctrl-C: %d %v", n, err)
		}
		f.until(t, 5*time.Second, func() bool {
			out := f.run(t, nil, "docker", "exec", f.name, "ps", "-o", "args")
			return !strings.Contains(out, "/usr/local/bin/ship agent proxy")
		}, "remote proxy cleanup")
	})
	t.Run("same container restart stops listener", func(t *testing.T) {
		p := f.startProxy(t, true, "web", 3000)
		proxyIntegrationHTTP(t, p)
		f.docker(t, "restart", f.web1)
		p.waitStopped(t, false)
	})
	t.Run("container replacement stops listener", func(t *testing.T) {
		p := f.startProxy(t, true, "web.2", 3000)
		proxyIntegrationHTTP(t, p)
		f.docker(t, "rm", "-f", f.web2)
		f.runWeb(t, 2)
		p.waitStopped(t, false)
	})
	t.Run("SSH forwarding hop", func(t *testing.T) {
		old := f.config
		configData, err := os.ReadFile(old)
		if err != nil {
			t.Fatal(err)
		}
		// The inner SSH target is reachable only through this SSH forwarding hop.
		// An explicit ProxyCommand also keeps all keys and host records in the fixture.
		hop := fmt.Sprintf("ssh -F /dev/null -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=%s -p %s -W %%h:%%p root@127.0.0.1", f.key, f.knownHosts, f.sshPort)
		changed := strings.Replace(string(configData), "port: "+f.sshPort, "port: 22", 1)
		changed = strings.Replace(changed, "    IdentitiesOnly: yes", "    IdentitiesOnly: yes\n    ProxyCommand: "+strconv.Quote(hop), 1)
		f.config = filepath.Join(f.dir, "hop.yml")
		defer func() { f.config = old }()
		if err := os.WriteFile(f.config, []byte(changed), 0600); err != nil {
			t.Fatal(err)
		}
		p := f.startProxy(t, true, "web", 3000)
		proxyIntegrationHTTP(t, p)
		p.interrupt(t)
	})
}

type proxyIntegrationFixture struct {
	t                                                                *testing.T
	ctx                                                              context.Context
	dir, root, name, image, binary, config, key, knownHosts, sshPort string
	database, web1, web2, halfclose                                  string
}

func newProxyIntegrationFixture(t *testing.T) *proxyIntegrationFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	f := &proxyIntegrationFixture{t: t, ctx: ctx, root: root, dir: dir, name: "ship-proxy-test-" + suffix, image: "ship-proxy-test:" + suffix, binary: filepath.Join(dir, "ship-local"), key: filepath.Join(dir, "id_ed25519"), knownHosts: filepath.Join(dir, "known_hosts"), config: filepath.Join(dir, "ship.yml"), database: "ship-proxy-postgres", web1: deployment.ContainerName("proxy-integration", "test", "web", 1, "fixture"), web2: deployment.ContainerName("proxy-integration", "test", "web", 2, "fixture"), halfclose: deployment.ContainerName("proxy-integration", "test", "halfclose", 1, "fixture")}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if t.Failed() {
			out, _ := exec.CommandContext(cleanup, "docker", "logs", f.name).CombinedOutput()
			t.Logf("fixture logs: %s", out)
			out, _ = exec.CommandContext(cleanup, "docker", "exec", f.name, "tail", "-100", "/tmp/dockerd.log").CombinedOutput()
			t.Logf("Docker logs: %s", out)
		}
		if out, err := exec.CommandContext(cleanup, "docker", "rm", "-fv", f.name).CombinedOutput(); err != nil {
			t.Logf("fixture cleanup: %s %v", out, err)
		}
		if out, err := exec.CommandContext(cleanup, "docker", "image", "rm", f.image).CombinedOutput(); err != nil {
			t.Logf("fixture image cleanup: %s %v", out, err)
		}
	})
	arch := strings.TrimSpace(f.run(t, nil, "docker", "info", "--format", "{{.Architecture}}"))
	switch arch {
	case "aarch64":
		arch = "arm64"
	case "x86_64":
		arch = "amd64"
	}
	if arch != "arm64" && arch != "amd64" {
		t.Fatalf("unsupported fixture architecture %s", arch)
	}
	f.run(t, nil, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", f.key)
	buildDir := filepath.Join(dir, "build")
	if err := os.Mkdir(buildDir, 0700); err != nil {
		t.Fatal(err)
	}
	copyFile := func(from, to string) {
		data, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"Dockerfile", "entrypoint.sh"} {
		copyFile(filepath.Join(root, "testdata/proxy", name), filepath.Join(buildDir, name))
	}
	copyFile(f.key+".pub", filepath.Join(buildDir, "authorized_keys"))
	env := append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	f.run(t, env, "go", "build", "-o", filepath.Join(buildDir, "ship"), "./cmd/ship")
	f.run(t, env, "go", "build", "-o", filepath.Join(buildDir, "sample-app"), "./testdata/sample-app/main.go")
	f.run(t, env, "go", "build", "-o", filepath.Join(buildDir, "halfclose"), "./testdata/proxy/halfclose.go")
	f.run(t, env, "go", "build", "-o", filepath.Join(buildDir, "probe"), "./testdata/proxy/probe")
	f.run(t, append(os.Environ(), "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH), "go", "build", "-o", f.binary, "./cmd/ship")
	t.Log("building disposable Linux SSH/Docker fixture")
	f.run(t, nil, "docker", "build", "-t", f.image, buildDir)
	f.run(t, nil, "docker", "run", "--detach", "--privileged", "--name", f.name, "-p", "127.0.0.1::22", f.image)
	var published map[string][]struct{ HostIP, HostPort string }
	output := f.run(t, nil, "docker", "inspect", "--format", "{{json .NetworkSettings.Ports}}", f.name)
	if err := json.Unmarshal([]byte(output), &published); err != nil {
		t.Fatal(err)
	}
	f.sshPort = published["22/tcp"][0].HostPort
	f.until(t, 90*time.Second, func() bool { _, err := f.tryDocker("info"); return err == nil }, "nested Docker daemon")
	hostKey := strings.TrimSpace(f.run(t, nil, "docker", "exec", f.name, "cat", "/etc/ssh/ssh_host_ed25519_key.pub"))
	fields := strings.Fields(hostKey)
	if len(fields) < 2 {
		t.Fatal("missing SSH host key")
	}
	known := fmt.Sprintf("[127.0.0.1]:%s,127.0.0.1 %s %s\n", f.sshPort, fields[0], fields[1])
	if err := os.WriteFile(f.knownHosts, []byte(known), 0600); err != nil {
		t.Fatal(err)
	}
	f.docker(t, "network", "create", "ship-proxy-integration-test")
	t.Log("pulling disposable PostgreSQL and Alpine images")
	f.docker(t, "pull", "postgres:17-alpine")
	f.docker(t, "pull", "alpine:3.22")
	f.docker(t, "run", "-d", "--name", f.database, "--network", "ship-proxy-integration-test", "--label", "managed-by=ship", "--label", "project=proxy-integration", "--label", "environment=test", "--label", "accessory=postgres", "-e", "POSTGRES_PASSWORD=fixture-only", "-e", "POSTGRES_HOST_AUTH_METHOD=trust", "postgres:17-alpine")
	f.until(t, 60*time.Second, func() bool {
		_, err := f.tryDocker("exec", f.database, "pg_isready", "-U", "postgres")
		return err == nil
	}, "PostgreSQL readiness")
	f.runWeb(t, 1)
	f.runWeb(t, 2)
	f.docker(t, "run", "-d", "--name", f.halfclose, "--network", "ship-proxy-integration-test", "--label", "managed-by=ship", "--label", "project=proxy-integration", "--label", "environment=test", "--label", "service=halfclose", "--label", "replica=1", "--label", "release=fixture", "-v", "/fixture/halfclose:/halfclose:ro", "alpine:3.22", "/halfclose")
	configuration := fmt.Sprintf(`project: proxy-integration
registry: registry.invalid/fixture
ssh:
  port: %s
  identity_file: %q
  known_hosts_file: %q
  options:
    StrictHostKeyChecking: yes
    IdentitiesOnly: yes
    LogLevel: ERROR
docker:
  network:
    name: ship-proxy-integration-test
    driver: bridge
environments:
  test:
    provider:
      manual: {}
    hosts:
      pools:
        app:
          user: root
          hosts: [127.0.0.1]
services:
  web:
    image: {build: .}
    pool: app
    scale: 2
  halfclose:
    image: {build: .}
    pool: app
    scale: 1
accessories:
  postgres:
    image: postgres:17-alpine
    pool: app
`, f.sshPort, f.key, f.knownHosts)
	if err := os.WriteFile(f.config, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *proxyIntegrationFixture) run(t *testing.T, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(f.ctx, name, args...)
	cmd.Dir = f.root
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
	}
	return string(out)
}
func (f *proxyIntegrationFixture) tryDocker(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", append([]string{"exec", f.name, "docker"}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
func (f *proxyIntegrationFixture) docker(t *testing.T, args ...string) string {
	t.Helper()
	return f.run(t, nil, "docker", append([]string{"exec", f.name, "docker"}, args...)...)
}
func (f *proxyIntegrationFixture) until(t *testing.T, limit time.Duration, check func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		select {
		case <-f.ctx.Done():
			t.Fatal(f.ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for %s", description)
}
func (f *proxyIntegrationFixture) runWeb(t *testing.T, replica int) {
	t.Helper()
	name := f.web1
	if replica == 2 {
		name = f.web2
	}
	args := []string{"run", "-d", "--name", name, "--network", "ship-proxy-integration-test", "--label", "managed-by=ship", "--label", "project=proxy-integration", "--label", "environment=test", "--label", "service=web", "--label", "replica=" + strconv.Itoa(replica), "--label", "release=fixture", "-v", "/fixture/sample-app:/sample-app:ro"}
	if replica == 2 {
		args = append(args, "-e", "DATABASE_URL=fixture-only")
	}
	args = append(args, "alpine:3.22", "/sample-app")
	f.docker(t, args...)
}

type proxyIntegrationProcess struct {
	cmd     *exec.Cmd
	address string
	done    chan struct{}
	err     error
	output  bytes.Buffer
	mu      sync.Mutex
}

func (p *proxyIntegrationProcess) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.output.Write(data)
}
func (p *proxyIntegrationProcess) logs() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.output.String()
}
func (f *proxyIntegrationFixture) startProxy(t *testing.T, service bool, name string, remote int, flags ...string) *proxyIntegrationProcess {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	args := []string{"--config", f.config}
	if !service {
		args = append(args, "accessory")
	}
	args = append(args, "proxy", "test", name, fmt.Sprintf("%d:%d", port, remote))
	args = append(args, flags...)
	p := &proxyIntegrationProcess{cmd: exec.CommandContext(f.ctx, f.binary, args...), address: address, done: make(chan struct{})}
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p.cmd.Dir = f.dir
	p.cmd.Stdout = p
	p.cmd.Stderr = p
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		select {
		case <-p.done:
			return
		default:
		}
		_ = p.cmd.Process.Signal(os.Interrupt)
		select {
		case <-p.done:
		case <-time.After(8 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
			t.Errorf("proxy did not stop during cleanup: %s", p.logs())
		}
	})
	started := time.Now()
	f.until(t, 40*time.Second, func() bool {
		select {
		case <-p.done:
			t.Fatalf("proxy exited before ready: %v\n%s", p.err, p.logs())
		default:
		}
		return strings.Contains(p.logs(), "127.0.0.1:"+strconv.Itoa(port)) && strings.Contains(p.logs(), "Ctrl-C")
	}, "proxy ready message")
	t.Logf("%s preflight and listener setup: %s", name, time.Since(started))
	return p
}
func (p *proxyIntegrationProcess) interrupt(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	p.waitStopped(t, true)
}
func (p *proxyIntegrationProcess) waitStopped(t *testing.T, success bool) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		t.Fatalf("proxy did not stop: %s", p.logs())
	}
	if success && p.err != nil {
		t.Fatalf("Ctrl-C failed: %v\n%s", p.err, p.logs())
	}
	if !success && p.err == nil {
		t.Fatalf("target change exited successfully: %s", p.logs())
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, 0); err != syscall.ESRCH {
		t.Errorf("proxy child process group survived shutdown: %v", err)
	}
	conn, err := net.DialTimeout("tcp", p.address, time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatal("proxy listener survived shutdown")
	}
}
func proxyIntegrationHTTP(t *testing.T, p *proxyIntegrationProcess) string {
	t.Helper()
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	response, err := client.Get("http://" + p.address + "/")
	if err != nil {
		t.Fatalf("HTTP request failed: %v\n%s", err, p.logs())
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("HTTP response %d: %v", response.StatusCode, err)
	}
	return string(body)
}

// proxyIntegrationSQL speaks only the PostgreSQL startup and simple query
// messages needed by this test. The isolated fixture uses test-only trust auth.
func proxyIntegrationSQL(address string) (string, error) {
	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	startup := append([]byte{0, 3, 0, 0}, []byte("user\x00postgres\x00database\x00postgres\x00\x00")...)
	packet := make([]byte, 4+len(startup))
	binary.BigEndian.PutUint32(packet, uint32(len(packet)))
	copy(packet[4:], startup)
	if _, err := conn.Write(packet); err != nil {
		return "", err
	}
	read := func() (byte, []byte, error) {
		header := make([]byte, 5)
		if _, err := io.ReadFull(conn, header); err != nil {
			return 0, nil, err
		}
		length := binary.BigEndian.Uint32(header[1:])
		if length < 4 || length > 1024*1024 {
			return 0, nil, fmt.Errorf("invalid Postgres message length %d", length)
		}
		body := make([]byte, int(length)-4)
		_, err := io.ReadFull(conn, body)
		return header[0], body, err
	}
	for {
		kind, body, err := read()
		if err != nil {
			return "", err
		}
		if kind == 'E' {
			return "", fmt.Errorf("Postgres startup: %q", body)
		}
		if kind == 'Z' {
			break
		}
	}
	query := []byte("SELECT 42\x00")
	packet = make([]byte, 5+len(query))
	packet[0] = 'Q'
	binary.BigEndian.PutUint32(packet[1:], uint32(4+len(query)))
	copy(packet[5:], query)
	if _, err := conn.Write(packet); err != nil {
		return "", err
	}
	value := ""
	for {
		kind, body, err := read()
		if err != nil {
			return "", err
		}
		if kind == 'E' {
			return "", fmt.Errorf("Postgres query: %q", body)
		}
		if kind == 'D' {
			if len(body) < 6 || binary.BigEndian.Uint16(body) != 1 || int(binary.BigEndian.Uint32(body[2:])) != len(body)-6 {
				return "", fmt.Errorf("invalid Postgres data row")
			}
			value = string(body[6:])
		}
		if kind == 'Z' {
			return value, nil
		}
	}
}
