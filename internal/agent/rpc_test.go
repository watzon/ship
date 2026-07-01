package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/state"
)

func TestServeUnknownMethodReturnsStructuredErrorAndRequestID(t *testing.T) {
	var out bytes.Buffer
	err := Serve(context.Background(), strings.NewReader(`{"id":"abc","method":"nope"}`+"\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != "abc" {
		t.Fatalf("id = %q", resp.ID)
	}
	if resp.OK || resp.ErrorCode != ErrorUnknownMethod || !strings.Contains(resp.Error, "unknown method") {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestServeInvalidJSONReturnsStructuredError(t *testing.T) {
	var out bytes.Buffer
	err := Serve(context.Background(), strings.NewReader("{not json}\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.ErrorCode != ErrorInvalidJSON {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestServeProcessesNewlineDelimitedRequests(t *testing.T) {
	var out bytes.Buffer
	input := strings.NewReader(`{"id":"one","method":"status"}` + "\n" + `{"id":"two","method":"unknown"}` + "\n")
	if err := Serve(context.Background(), input, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, output = %q", len(lines), out.String())
	}
	var first, second Response
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if first.ID != "one" || !first.OK {
		t.Fatalf("first response = %+v", first)
	}
	if second.ID != "two" || second.OK || second.ErrorCode != ErrorUnknownMethod {
		t.Fatalf("second response = %+v", second)
	}
}

func TestServeHandlesLargeSingleLineRequest(t *testing.T) {
	params, err := json.Marshal(map[string]string{"padding": strings.Repeat("x", 128*1024)})
	if err != nil {
		t.Fatal(err)
	}
	req := Request{ID: "large", Method: "status", Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := Serve(context.Background(), strings.NewReader(string(data)+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.ID != "large" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestNegotiateRejectsIncompatibleProtocol(t *testing.T) {
	server := testServer(t)
	resp := server.Handle(context.Background(), request(t, "req-1", "negotiate", NegotiateParams{
		MinProtocolVersion: AgentProtocol + 1,
		MaxProtocolVersion: AgentProtocol + 1,
	}))
	if resp.OK || resp.ID != "req-1" || resp.ErrorCode != ErrorIncompatibleProtocol {
		t.Fatalf("response = %+v", resp)
	}
}

func TestDockerMethodsUseInjectedDocker(t *testing.T) {
	server := testServer(t)

	inspectResp := server.Handle(context.Background(), request(t, "inspect-1", "docker_inspect", DockerInspectParams{Name: "ship_web_1"}))
	if !inspectResp.OK || inspectResp.ID != "inspect-1" {
		t.Fatalf("inspect response = %+v", inspectResp)
	}
	var inspect DockerInspectResult
	decodeResult(t, inspectResp, &inspect)
	if !json.Valid(inspect.Inspect) || !strings.Contains(string(inspect.Inspect), "ship_web_1") {
		t.Fatalf("inspect = %s", inspect.Inspect)
	}

	listResp := server.Handle(context.Background(), request(t, "list-1", "list_ship_containers", nil))
	if !listResp.OK || listResp.ID != "list-1" {
		t.Fatalf("list response = %+v", listResp)
	}
	var containers []docker.ContainerSummary
	decodeResult(t, listResp, &containers)
	if len(containers) != 1 || containers[0].Labels[docker.LabelManagedBy] != docker.LabelManagedByValue {
		t.Fatalf("containers = %+v", containers)
	}
}

func TestRunContainerReplacesExistingContainerUnderLock(t *testing.T) {
	fake := &fakeDocker{inspectImage: "example/web:old"}
	server := testServer(t)
	server.Docker = fake

	resp := server.Handle(context.Background(), request(t, "run-1", "run_container", RunContainerParams{
		Name:   "ship_web_1",
		Image:  "example/web:1",
		Args:   []string{"-p", "3000:3000"},
		Labels: map[string]string{"project": "demo", "environment": "production"},
	}))
	if !resp.OK {
		t.Fatalf("response = %+v", resp)
	}
	want := []string{"inspect:ship_web_1", "stop_remove:ship_web_1", "run:ship_web_1:example/web:1"}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("calls = %#v, want %#v", fake.calls, want)
	}
	if _, err := os.Stat(filepath.Join(server.StateDir, "locks", "host.lock")); err != nil {
		t.Fatalf("host lock was not created: %v", err)
	}
	joinedArgs := strings.Join(fake.runArgs, " ")
	if !strings.Contains(joinedArgs, "--label environment=production") || !strings.Contains(joinedArgs, "--label project=demo") {
		t.Fatalf("run args missing labels: %#v", fake.runArgs)
	}
}

func TestRunContainerReplacesExistingContainerWithSameImage(t *testing.T) {
	fake := &fakeDocker{inspectImage: "example/web:1"}
	server := testServer(t)
	server.Docker = fake

	resp := server.Handle(context.Background(), request(t, "run-1", "run_container", RunContainerParams{
		Name:  "ship_web_1",
		Image: "example/web:1",
	}))
	if !resp.OK {
		t.Fatalf("response = %+v", resp)
	}
	want := []string{"inspect:ship_web_1", "stop_remove:ship_web_1", "run:ship_web_1:example/web:1"}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("calls = %#v, want %#v", fake.calls, want)
	}
}

func TestPruneImagesUsesDockerUnderLock(t *testing.T) {
	fake := &fakeDocker{}
	server := testServer(t)
	server.Docker = fake

	resp := server.Handle(context.Background(), request(t, "prune-1", "prune_images", map[string]any{}))
	if !resp.OK {
		t.Fatalf("response = %+v", resp)
	}
	want := []string{"prune_images"}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("calls = %#v, want %#v", fake.calls, want)
	}
}

func TestHealthChecksUseCommandAndHTTP(t *testing.T) {
	var commands []string
	server := testServer(t)
	server.CommandRunner = func(ctx context.Context, name string, args ...string) (string, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return "healthy\n", nil
	}
	commandResp := server.Handle(context.Background(), request(t, "health-command", "health_check", HealthCheckParams{Command: "curl -f localhost/up"}))
	if !commandResp.OK {
		t.Fatalf("command response = %+v", commandResp)
	}
	if len(commands) != 1 || !strings.Contains(commands[0], "curl -f localhost/up") {
		t.Fatalf("commands = %#v", commands)
	}

	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "ok")
	}))
	t.Cleanup(httpServer.Close)

	httpResp := server.Handle(context.Background(), request(t, "health-http", "health_check", HealthCheckParams{URL: httpServer.URL}))
	if !httpResp.OK {
		t.Fatalf("http response = %+v", httpResp)
	}
	var result HealthCheckResult
	decodeResult(t, httpResp, &result)
	if !result.OK || result.StatusCode != http.StatusOK || result.Output != "ok" {
		t.Fatalf("health result = %+v", result)
	}
}

func TestWriteFileInstallBinaryAndStateMigration(t *testing.T) {
	server := testServer(t)
	target := filepath.Join(t.TempDir(), "nested", "config.txt")
	content := base64.StdEncoding.EncodeToString([]byte("hello"))
	resp := server.Handle(context.Background(), request(t, "write-1", "write_file", WriteFileParams{
		Path:     target,
		Content:  content,
		Encoding: "base64",
		Mode:     0o600,
	}))
	if !resp.OK {
		t.Fatalf("write response = %+v", resp)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("file data = %q", data)
	}

	binaryPath := filepath.Join(t.TempDir(), "ship")
	binaryContent := base64.StdEncoding.EncodeToString([]byte("binary"))
	installResp := server.Handle(context.Background(), request(t, "install-1", "install_binary", InstallBinaryParams{
		Path:          binaryPath,
		ContentBase64: binaryContent,
	}))
	if !installResp.OK {
		t.Fatalf("install response = %+v", installResp)
	}
	var install InstallBinaryResult
	decodeResult(t, installResp, &install)
	if !install.Installed || install.SHA256 == "" {
		t.Fatalf("install result = %+v", install)
	}
	secondInstallResp := server.Handle(context.Background(), request(t, "install-2", "install_binary", InstallBinaryParams{
		Path:          binaryPath,
		ContentBase64: binaryContent,
	}))
	decodeResult(t, secondInstallResp, &install)
	if install.Installed {
		t.Fatalf("second install should be idempotent: %+v", install)
	}

	from := t.TempDir()
	to := t.TempDir()
	if err := os.WriteFile(filepath.Join(from, "legacy.txt"), []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	migrateResp := server.Handle(context.Background(), request(t, "migrate-1", "migrate_state_dir", MigrateStateDirParams{From: from, To: to}))
	if !migrateResp.OK {
		t.Fatalf("migrate response = %+v", migrateResp)
	}
	if _, err := os.Stat(filepath.Join(to, "legacy.txt")); err != nil {
		t.Fatalf("migrated file missing: %v", err)
	}
}

func TestReleaseStateMethods(t *testing.T) {
	server := testServer(t)
	release := state.Release{
		ID:          "20260630",
		Environment: "production",
		Images:      map[string]string{"web": "example/web:1"},
		CreatedAt:   time.Unix(100, 0),
		Healthy:     true,
	}
	writeResp := server.Handle(context.Background(), request(t, "release-write", "write_release_state", WriteReleaseStateParams{Release: release}))
	if !writeResp.OK {
		t.Fatalf("write response = %+v", writeResp)
	}

	readResp := server.Handle(context.Background(), request(t, "release-read", "read_release_state", ReadReleaseStateParams{Environment: "production"}))
	if !readResp.OK {
		t.Fatalf("read response = %+v", readResp)
	}
	var got state.Release
	decodeResult(t, readResp, &got)
	if got.ID != release.ID || got.Images["web"] != release.Images["web"] {
		t.Fatalf("release = %+v", got)
	}
}

func TestWriteReleaseStatePreservesPendingStatus(t *testing.T) {
	server := testServer(t)
	current := state.Release{
		ID:          "current",
		Environment: "production",
		Images:      map[string]string{"web": "example/web:current"},
		CreatedAt:   time.Unix(100, 0),
		Healthy:     true,
	}
	writeCurrent := server.Handle(context.Background(), request(t, "release-current", "write_release_state", WriteReleaseStateParams{Release: current}))
	if !writeCurrent.OK {
		t.Fatalf("write current response = %+v", writeCurrent)
	}
	pending := state.Release{
		ID:          "pending",
		Environment: "production",
		Images:      map[string]string{"web": "example/web:pending"},
		CreatedAt:   time.Unix(200, 0),
		Status:      state.ReleaseStatusPending,
	}
	writePending := server.Handle(context.Background(), request(t, "release-pending", "write_release_state", WriteReleaseStateParams{Release: pending}))
	if !writePending.OK {
		t.Fatalf("write pending response = %+v", writePending)
	}
	readPending := server.Handle(context.Background(), request(t, "release-read-pending", "read_release_state", ReadReleaseStateParams{ID: "pending"}))
	if !readPending.OK {
		t.Fatalf("read pending response = %+v", readPending)
	}
	var gotPending state.Release
	decodeResult(t, readPending, &gotPending)
	if gotPending.Status != state.ReleaseStatusPending || gotPending.Healthy {
		t.Fatalf("pending release = %+v", gotPending)
	}
	readCurrent := server.Handle(context.Background(), request(t, "release-read-current", "read_release_state", ReadReleaseStateParams{Environment: "production"}))
	if !readCurrent.OK {
		t.Fatalf("read current response = %+v", readCurrent)
	}
	var gotCurrent state.Release
	decodeResult(t, readCurrent, &gotCurrent)
	if gotCurrent.ID != "current" {
		t.Fatalf("current release = %+v", gotCurrent)
	}
}

func TestCaddyReloadAndAccessoryCommandsUseInjectedRunner(t *testing.T) {
	var commands []string
	server := testServer(t)
	server.CommandRunner = func(ctx context.Context, name string, args ...string) (string, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return "done\n", nil
	}
	caddyPath := filepath.Join(t.TempDir(), "Caddyfile")
	caddyResp := server.Handle(context.Background(), request(t, "caddy-1", "caddy_reload", CaddyReloadParams{
		Path:     caddyPath,
		Config:   "example.com { respond \"ok\" }\n",
		Validate: true,
	}))
	if !caddyResp.OK {
		t.Fatalf("caddy response = %+v", caddyResp)
	}
	backupResp := server.Handle(context.Background(), request(t, "backup-1", "accessory_backup", AccessoryCommandParams{
		Name:    "postgres",
		Command: "pg_dump app",
	}))
	if !backupResp.OK {
		t.Fatalf("backup response = %+v", backupResp)
	}
	restoreResp := server.Handle(context.Background(), request(t, "restore-1", "accessory_restore", AccessoryCommandParams{
		Name:    "postgres",
		Command: "psql app",
	}))
	if !restoreResp.OK {
		t.Fatalf("restore response = %+v", restoreResp)
	}
	joined := strings.Join(commands, "\n")
	for _, want := range []string{"caddy validate", "caddy reload", "pg_dump app", "psql app"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("commands %q missing %q", joined, want)
		}
	}
}

func TestEnsureVolumeCreatesVolumeAndAppliesOwner(t *testing.T) {
	var commands [][]string
	server := testServer(t)
	server.CommandRunner = func(ctx context.Context, name string, args ...string) (string, error) {
		commands = append(commands, append([]string{name}, args...))
		return "created\n", nil
	}
	resp := server.Handle(context.Background(), request(t, "volume-1", "ensure_volume", EnsureVolumeParams{
		Name:  "postgres-data",
		Owner: "999:999",
	}))
	if !resp.OK {
		t.Fatalf("response = %+v", resp)
	}
	want := []string{
		"docker volume create postgres-data",
		"docker run --rm -v postgres-data:/ship-volume busybox:1.36 chown -R 999:999 /ship-volume",
	}
	if len(commands) != len(want) {
		t.Fatalf("commands = %#v", commands)
	}
	for i, command := range commands {
		if strings.Join(command, " ") != want[i] {
			t.Fatalf("command %d = %#v, want %q", i, command, want[i])
		}
	}
}

func TestCaddyReloadGeneratedConfigUsesCaddyfileAdapter(t *testing.T) {
	var commands [][]string
	server := testServer(t)
	server.CommandRunner = func(ctx context.Context, name string, args ...string) (string, error) {
		commands = append(commands, append([]string{name}, args...))
		return "done\n", nil
	}
	caddyPath := filepath.Join(t.TempDir(), "Caddyfile")
	resp := server.Handle(context.Background(), request(t, "caddy-adapter", "caddy_reload", CaddyReloadParams{
		Path:     caddyPath,
		Config:   "example.com {\n  respond \"ok\"\n}\n",
		Validate: true,
	}))
	if !resp.OK {
		t.Fatalf("response = %+v", resp)
	}
	if len(commands) != 2 {
		t.Fatalf("commands = %#v, want validate and reload", commands)
	}
	for _, command := range commands {
		joined := strings.Join(command, " ")
		if !strings.Contains(joined, "--adapter caddyfile") {
			t.Fatalf("command missing caddyfile adapter: %#v", command)
		}
	}
	if commands[0][1] != "validate" || !strings.Contains(filepath.Base(commands[0][3]), ".ship-caddy-") {
		t.Fatalf("validate command did not use staged generated config: %#v", commands[0])
	}
	if commands[1][1] != "reload" || commands[1][3] != caddyPath {
		t.Fatalf("reload command did not use final config path: %#v", commands[1])
	}
}

func TestCaddyReloadValidationFailureKeepsPreviousConfig(t *testing.T) {
	server := testServer(t)
	caddyPath := filepath.Join(t.TempDir(), "Caddyfile")
	previous := "example.com { respond \"old\" }\n"
	if err := os.WriteFile(caddyPath, []byte(previous), 0o644); err != nil {
		t.Fatal(err)
	}
	var commands []string
	server.CommandRunner = func(ctx context.Context, name string, args ...string) (string, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		if name == "caddy" && len(args) > 0 && args[0] == "validate" {
			return "", fmt.Errorf("invalid caddyfile")
		}
		return "done\n", nil
	}
	resp := server.Handle(context.Background(), request(t, "caddy-invalid", "caddy_reload", CaddyReloadParams{
		Path:     caddyPath,
		Config:   "example.com { invalid }\n",
		Validate: true,
	}))
	if resp.OK || resp.ErrorCode != ErrorCommandFailed {
		t.Fatalf("response = %+v", resp)
	}
	data, err := os.ReadFile(caddyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != previous {
		t.Fatalf("caddyfile changed after validation failure: %q", data)
	}
	if strings.Contains(strings.Join(commands, "\n"), "reload") {
		t.Fatalf("reload ran after validation failure: %#v", commands)
	}
}

func TestCaddyReloadFailureRestoresPreviousConfig(t *testing.T) {
	server := testServer(t)
	caddyPath := filepath.Join(t.TempDir(), "Caddyfile")
	previous := "example.com { respond \"old\" }\n"
	if err := os.WriteFile(caddyPath, []byte(previous), 0o644); err != nil {
		t.Fatal(err)
	}
	reloads := 0
	server.CommandRunner = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "caddy" && len(args) > 0 && args[0] == "reload" {
			reloads++
			if reloads == 1 {
				return "", fmt.Errorf("reload failed")
			}
		}
		return "done\n", nil
	}
	resp := server.Handle(context.Background(), request(t, "caddy-reload-fails", "caddy_reload", CaddyReloadParams{
		Path:     caddyPath,
		Config:   "example.com { respond \"new\" }\n",
		Validate: true,
	}))
	if resp.OK || resp.ErrorCode != ErrorCommandFailed {
		t.Fatalf("response = %+v", resp)
	}
	data, err := os.ReadFile(caddyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != previous {
		t.Fatalf("caddyfile was not restored: %q", data)
	}
	if reloads != 2 {
		t.Fatalf("reloads = %d, want failed reload plus rollback reload", reloads)
	}
}

func TestShipAgentRPCSubprocess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess integration test in short mode")
	}
	cmd := exec.Command("go", "run", "../../cmd/ship", "agent", "rpc")
	cmd.Stdin = strings.NewReader(`{"id":"subprocess-1","method":"status"}` + "\n")
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go run failed: %v\nstderr:\n%s", err, stderr.String())
	}
	var resp Response
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("decode response %q: %v", out.String(), err)
	}
	if !resp.OK || resp.ID != "subprocess-1" {
		t.Fatalf("response = %+v", resp)
	}
}

func request(t *testing.T, id, method string, params any) Request {
	t.Helper()
	var raw json.RawMessage
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		raw = data
	}
	return Request{ID: id, Method: method, Params: raw, ProtocolVersion: AgentProtocol}
}

func decodeResult(t *testing.T, resp Response, out any) {
	t.Helper()
	if !resp.OK {
		t.Fatalf("response failed: %+v", resp)
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		t.Fatal(err)
	}
}

func testServer(t *testing.T) Server {
	t.Helper()
	return Server{
		Docker:   &fakeDocker{},
		StateDir: t.TempDir(),
		Hostname: func() (string, error) {
			return "host-a", nil
		},
		CommandRunner: func(ctx context.Context, name string, args ...string) (string, error) {
			return "", nil
		},
	}
}

type fakeDocker struct {
	calls        []string
	inspectImage string
	runArgs      []string
}

func (f *fakeDocker) Available(ctx context.Context) error {
	return nil
}

func (f *fakeDocker) Pull(ctx context.Context, image string) error {
	f.calls = append(f.calls, "pull:"+image)
	return nil
}

func (f *fakeDocker) PruneShipImages(ctx context.Context) error {
	f.calls = append(f.calls, "prune_images")
	return nil
}

func (f *fakeDocker) Run(ctx context.Context, name, image, command string, args ...string) error {
	f.calls = append(f.calls, "run:"+name+":"+image)
	f.runArgs = append([]string(nil), args...)
	return nil
}

func (f *fakeDocker) StopRemove(ctx context.Context, name string) error {
	f.calls = append(f.calls, "stop_remove:"+name)
	return nil
}

func (f *fakeDocker) Logs(ctx context.Context, name string, lines int) (string, error) {
	return "logs", nil
}

func (f *fakeDocker) Inspect(ctx context.Context, name string) (json.RawMessage, error) {
	f.calls = append(f.calls, "inspect:"+name)
	if f.inspectImage != "" {
		return json.RawMessage(fmt.Sprintf(`[{"Name":%q,"Config":{"Image":%q}}]`, name, f.inspectImage)), nil
	}
	return json.RawMessage(fmt.Sprintf(`[{"Name":%q}]`, name)), nil
}

func (f *fakeDocker) ListShipContainers(ctx context.Context) ([]docker.ContainerSummary, error) {
	return []docker.ContainerSummary{{
		ID:     "abc",
		Image:  "example/web:1",
		Names:  "ship_web_1",
		Status: "Up",
		Labels: map[string]string{docker.LabelManagedBy: docker.LabelManagedByValue},
	}}, nil
}
