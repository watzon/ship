package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/deployment"
	"github.com/watzon/ship/internal/scheduler"
)

func TestProxyAccessoryDryRunAndEarlyValidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		accessory bool
		network   string
		args      []string
		want      string
		wantErr   bool
	}{
		{name: "accessory dry run", accessory: true, args: []string{"production", "postgres", "15432:5432"}, want: "accessory postgres"},
		{name: "unknown accessory", accessory: true, args: []string{"production", "missing", "15432:5432"}, want: "unknown accessory", wantErr: true},
		{name: "disabled network", network: "    enabled: false\n", args: []string{"production", "web", "18080:8080"}, want: "enabled Docker bridge network", wantErr: true},
		{name: "custom nonbridge network", network: "    name: custom-network\n    driver: overlay\n", args: []string{"production", "web", "18080:8080"}, want: "enabled Docker bridge network", wantErr: true},
		{name: "exact name replica conflict", args: []string{"production", deployment.ContainerName("demo", "production", "web", 2, "release.17"), "18080:8080", "--replica", "1"}, want: "conflicts", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "ship.yml")
			text := singleHostConfig() + "\naccessories:\n  postgres:\n    image: postgres:17-alpine\n    pool: web\n"
			if tc.network != "" {
				text += "\ndocker:\n  network:\n" + tc.network
			}
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			original := newProxyAgent
			newProxyAgent = func(scheduler.Host) proxyAgent { t.Fatal("validation made an agent call"); return nil }
			t.Cleanup(func() { newProxyAgent = original })
			// Only the successful accessory command is a dry run. Invalid live commands
			// must reject their input before creating an agent or opening a listener.
			opts := &options{configPath: path, dryRun: !tc.wantErr}
			cmd := newProxyCommand(opts, tc.accessory)
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error=%v, want %q", err, tc.want)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(output.String(), tc.want) || !strings.Contains(output.String(), "checks are pending") {
					t.Fatalf("output=%q", output.String())
				}
			}
			if _, err := os.Stat(filepath.Join(dir, config.LocalStateDir)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("command wrote state: %v", err)
			}
		})
	}
}

func TestProxyMissingExactContainerDoesNotSelectAnotherRelease(t *testing.T) {
	f := newProxyTestFleet(t)
	f.add("app-a", "web", 1, "other-release", true)
	_, err := selectProxyTarget(context.Background(), f.config, f.store, f.hosts, "production", proxySelection{
		name: "web", replica: 1, containerName: deployment.ContainerName("demo", "production", "web", 1, "missing-release"), remotePort: 8080,
	})
	if err == nil || !strings.Contains(err.Error(), "no running container") {
		t.Fatalf("error=%v", err)
	}
	if len(f.probed) != 0 {
		t.Fatalf("probed another release: %+v", f.probed)
	}
}

func TestProxyOccupiedLocalPort(t *testing.T) {
	f := newProxyTestFleet(t)
	f.add("app-a", "web", 1, "release", true)
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port
	var output bytes.Buffer
	err = runProxyCommand(context.Background(), &output, f.config, f.store, f.hosts, "production", proxySelection{name: "web", replica: 1, localPort: port, remotePort: 8080})
	if err == nil || !strings.Contains(err.Error(), "cannot listen on 127.0.0.1:"+strconv.Itoa(port)) || !strings.Contains(err.Error(), "choose an available local port") {
		t.Fatalf("error=%v", err)
	}
	if strings.Contains(output.String(), "listening on") {
		t.Fatalf("printed readiness after failed bind: %q", output.String())
	}
}

func TestProxyMalformedContainerState(t *testing.T) {
	for _, raw := range []string{"{", "{}", "null", "[]", "[{},{}]", `[{"State":"running"}]`, `[{"State":{"Running":"true"}}]`} {
		t.Run(raw, func(t *testing.T) {
			running, err := proxyContainerRunning(json.RawMessage(raw))
			if err == nil || running {
				t.Fatalf("running=%v error=%v", running, err)
			}
		})
	}
}

func TestProxyCorruptSavedAccessoryState(t *testing.T) {
	f := newProxyTestFleet(t)
	f.add("data-a", "postgres", 0, "", true)
	path := filepath.Join(f.store.Dir, "accessories", "production", "postgres.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	selection := proxySelection{name: "postgres", accessory: true, localPort: 15432, remotePort: 5432}
	_, err := selectProxyTarget(context.Background(), f.config, f.store, f.hosts, "production", selection)
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		t.Fatalf("selection ignored corrupt state: %v", err)
	}
	if len(f.probed) != 0 {
		t.Fatal("selection probed with corrupt placement")
	}
	err = previewProxy(io.Discard, f.config, f.store, f.hosts, "production", selection)
	if !errors.As(err, &syntax) {
		t.Fatalf("preview ignored corrupt state: %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "{" {
		t.Fatalf("command changed corrupt state: %q, %v", contents, err)
	}
}

type proxyReadyOutput struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *proxyReadyOutput) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if bytes.Contains(p, []byte("listening on")) {
		w.cancel()
	}
	return n, err
}

func TestProxyReadinessAndPublishedBindingOutput(t *testing.T) {
	f := newProxyTestFleet(t)
	container := f.add("app-a", "web", 2, "release", true)
	factory := newProxyAgent
	newProxyAgent = func(host scheduler.Host) proxyAgent {
		delegate := factory(host)
		return proxyTestAgent{callFn: func(ctx context.Context, method string, params, out any) error {
			if err := delegate.Call(ctx, method, params, out); err != nil {
				return err
			}
			if method == "proxy_target" {
				out.(*agent.ProxyTargetResult).Target.Published = []string{"0.0.0.0:8080->8080/tcp"}
			}
			return nil
		}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := &proxyReadyOutput{cancel: cancel}
	// Port zero is available only through this internal helper. Public commands
	// reject it. It lets the operating system choose a free port for this test.
	err := runProxyCommand(ctx, output, f.config, f.store, f.hosts, "production", proxySelection{name: "web", replica: 2, remotePort: 8080})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	for _, want := range []string{
		"proxy production/web.2 on app-a container=" + container.ID[:12],
		" release=release",
		"container port 8080 already has published binding 0.0.0.0:8080->8080/tcp; the proxy does not remove it",
		"listening on 127.0.0.1:",
		"-> container TCP port 8080; press Ctrl-C to stop",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("missing %q in %q", want, output.String())
		}
	}
}
