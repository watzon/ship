package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/watzon/ship/internal/binfmt"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/docker"
	"github.com/watzon/ship/internal/state"
)

// AgentVersion is set by release builds via
// -ldflags "-X github.com/watzon/ship/internal/agent.AgentVersion=...".
// Every other build derives its version at runtime; see Version. Keeping the
// source default empty means a build can never claim a release it is not:
// a hand-bumped constant once baked "0.4.0" into the immutable v0.1.0 tag,
// pointing its binaries at release assets that never existed.
var AgentVersion = ""

// devVersion is reported by plain `go build` checkouts, where neither a
// release stamp nor a module version is available. Bump after each release.
const devVersion = "0.6.0-dev"

// Version reports the ship CLI/agent version: the release-stamped value when
// present, else the true module version recorded by `go install ...@vX.Y.Z`,
// else the development sentinel (which is never used to derive release-asset
// URLs).
func Version() string {
	if v := strings.TrimSpace(AgentVersion); v != "" {
		return v
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := strings.TrimSpace(bi.Main.Version); isReleaseTag(v) {
			return strings.TrimPrefix(v, "v")
		}
	}
	return devVersion
}

// isReleaseTag accepts exact release tags (v0.4.6) and rejects "(devel)",
// pseudo-versions from @main installs, and anything else that has no
// corresponding release assets.
func isReleaseTag(v string) bool {
	return releaseTagPattern.MatchString(v)
}

var releaseTagPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

const (
	AgentMinProtocol     = 1
	AgentProtocol        = 2
	defaultCaddyfilePath = "/etc/caddy/Caddyfile"
)

const (
	ErrorInvalidJSON          = "invalid_json"
	ErrorInvalidParams        = "invalid_params"
	ErrorUnknownMethod        = "unknown_method"
	ErrorInternal             = "internal_error"
	ErrorDocker               = "docker_error"
	ErrorCommandFailed        = "command_failed"
	ErrorFileOperation        = "file_operation_failed"
	ErrorHealthCheckFailed    = "health_check_failed"
	ErrorLock                 = "lock_failed"
	ErrorReleaseState         = "release_state_error"
	ErrorIncompatibleProtocol = "incompatible_protocol"
)

type Request struct {
	ID              string          `json:"id,omitempty"`
	Method          string          `json:"method"`
	Params          json.RawMessage `json:"params,omitempty"`
	ProtocolVersion int             `json:"protocol_version,omitempty"`
}

type Response struct {
	ID        string          `json:"id,omitempty"`
	OK        bool            `json:"ok"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	ErrorCode string          `json:"error_code,omitempty"`
}

type Status struct {
	Hostname         string   `json:"hostname"`
	StateDir         string   `json:"state_dir"`
	DockerOK         bool     `json:"docker_ok"`
	AgentVersion     string   `json:"agent_version"`
	ProtocolVersion  int      `json:"protocol_version"`
	SupportedMethods []string `json:"supported_methods,omitempty"`
}

type NegotiateParams struct {
	ClientVersion      string `json:"client_version,omitempty"`
	MinProtocolVersion int    `json:"min_protocol_version,omitempty"`
	MaxProtocolVersion int    `json:"max_protocol_version,omitempty"`
}

type NegotiateResult struct {
	AgentVersion     string   `json:"agent_version"`
	ProtocolVersion  int      `json:"protocol_version"`
	SupportedMethods []string `json:"supported_methods"`
}

type RunContainerParams struct {
	Name           string            `json:"name"`
	Image          string            `json:"image"`
	Command        string            `json:"command"`
	Args           []string          `json:"args"`
	Labels         map[string]string `json:"labels,omitempty"`
	Network        string            `json:"network,omitempty"`
	NetworkAliases []string          `json:"network_aliases,omitempty"`
}

type LogsParams struct {
	Name  string `json:"name"`
	Lines int    `json:"lines"`
}

type DockerInspectParams struct {
	Name string `json:"name"`
}

type DockerInspectResult struct {
	Inspect json.RawMessage `json:"inspect"`
}

type HealthCheckParams struct {
	Command        string `json:"command,omitempty"`
	URL            string `json:"url,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type HealthCheckResult struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code,omitempty"`
	Output     string `json:"output,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

type ExecContainerParams struct {
	Name           string `json:"name"`
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type RunOneOffContainerParams struct {
	Name           string            `json:"name"`
	Image          string            `json:"image"`
	Command        string            `json:"command"`
	Args           []string          `json:"args,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Network        string            `json:"network,omitempty"`
	NetworkAliases []string          `json:"network_aliases,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

type WriteFileParams struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding,omitempty"`
	Mode     uint32 `json:"mode,omitempty"`
}

type WriteFileResult struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
}

type ReadFileParams struct {
	Path string `json:"path"`
}

type ReadFileResult struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Exists  bool   `json:"exists"`
}

type WriteRegistryAuthParams struct {
	Server string          `json:"server"`
	Auth   json.RawMessage `json:"auth"`
}

type WriteRegistryAuthResult struct {
	Path   string `json:"path"`
	Server string `json:"server"`
}

type SyncCronFilesParams struct {
	Prefix string     `json:"prefix"`
	Files  []CronFile `json:"files"`
}

type CronFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type SyncCronFilesResult struct {
	Dir     string   `json:"dir"`
	Written []string `json:"written,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

type ReadReleaseStateParams struct {
	Environment string `json:"environment,omitempty"`
	ID          string `json:"id,omitempty"`
}

type WriteReleaseStateParams struct {
	Release state.Release `json:"release"`
}

type CaddyReloadParams struct {
	Path     string `json:"path,omitempty"`
	Config   string `json:"config,omitempty"`
	Validate bool   `json:"validate,omitempty"`
	Clear    bool   `json:"clear,omitempty"`
}

type CommandResult struct {
	Output string `json:"output,omitempty"`
}

type AccessoryCommandParams struct {
	Name           string `json:"name"`
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type EnsureVolumeParams struct {
	Name  string `json:"name"`
	Owner string `json:"owner,omitempty"`
}

type EnsureNetworkParams struct {
	Name   string `json:"name"`
	Driver string `json:"driver,omitempty"`
}

type InstallBinaryParams struct {
	Path          string `json:"path,omitempty"`
	ContentBase64 string `json:"content_base64"`
	SHA256        string `json:"sha256,omitempty"`
	Mode          uint32 `json:"mode,omitempty"`
}

type InstallBinaryResult struct {
	Path      string `json:"path"`
	Installed bool   `json:"installed"`
	SHA256    string `json:"sha256"`
}

type MigrateStateDirParams struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

type MigrateStateDirResult struct {
	From     string `json:"from,omitempty"`
	To       string `json:"to"`
	Migrated bool   `json:"migrated"`
}

type DockerOps interface {
	Available(ctx context.Context) error
	Pull(ctx context.Context, image string) error
	PruneShipImages(ctx context.Context) error
	Run(ctx context.Context, name, image, command string, args ...string) error
	StopRemove(ctx context.Context, name string) error
	Logs(ctx context.Context, name string, lines int) (string, error)
	Inspect(ctx context.Context, name string) (json.RawMessage, error)
	ListShipContainers(ctx context.Context) ([]docker.ContainerSummary, error)
}

type CommandRunner func(ctx context.Context, name string, args ...string) (string, error)

type Server struct {
	Docker          DockerOps
	CommandRunner   CommandRunner
	HTTPClient      *http.Client
	StateDir        string
	DockerConfigDir string
	CronDir         string
	Hostname        func() (string, error)
}

type rpcError struct {
	code    string
	message string
	err     error
}

func (e rpcError) Error() string {
	if e.message != "" {
		return e.message
	}
	if e.err != nil {
		return e.err.Error()
	}
	return e.code
}

func (e rpcError) Unwrap() error {
	return e.err
}

func Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	server := NewServer()
	reader := bufio.NewReader(in)
	encoder := json.NewEncoder(out)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			if err := serveLine(ctx, server, encoder, line); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func serveLine(ctx context.Context, server Server, encoder *json.Encoder, line string) error {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	var req Request
	var resp Response
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		resp = failure("", ErrorInvalidJSON, fmt.Errorf("invalid JSON request: %w", err))
	} else {
		resp = server.Handle(ctx, req)
	}
	return encoder.Encode(resp)
}

func NewServer() Server {
	return Server{
		Docker:        docker.Client{},
		CommandRunner: defaultCommandRunner,
		HTTPClient:    http.DefaultClient,
		StateDir:      config.RemoteStateDir,
		Hostname:      os.Hostname,
	}
}

type rpcHandler func(Server, context.Context, Request) Response

func lockedRPCHandler(operation string, handler rpcHandler) rpcHandler {
	return func(s Server, ctx context.Context, req Request) Response {
		return s.withHostLock(req, operation, func() Response {
			return handler(s, ctx, req)
		})
	}
}

func rpcHandlerWithoutContext(handler func(Server, Request) Response) rpcHandler {
	return func(s Server, _ context.Context, req Request) Response {
		return handler(s, req)
	}
}

var rpcHandlers map[string]rpcHandler

func init() {
	rpcHandlers = map[string]rpcHandler{
		"accessory_backup":     lockedRPCHandler("accessory_backup", Server.accessoryCommand),
		"accessory_restore":    lockedRPCHandler("accessory_restore", Server.accessoryCommand),
		"caddy_reload":         lockedRPCHandler("caddy_reload", Server.caddyReload),
		"docker_inspect":       Server.handleDockerInspect,
		"ensure_network":       lockedRPCHandler("ensure_network", Server.ensureNetwork),
		"ensure_volume":        lockedRPCHandler("ensure_volume", Server.ensureVolume),
		"exec_container":       lockedRPCHandler("exec_container", Server.execContainer),
		"health_check":         Server.healthCheck,
		"install_binary":       lockedRPCHandler("install_binary", rpcHandlerWithoutContext(Server.installBinary)),
		"list_ship_containers": Server.handleListShipContainers,
		"logs":                 Server.handleLogs,
		"migrate_state_dir":    lockedRPCHandler("migrate_state_dir", rpcHandlerWithoutContext(Server.migrateStateDir)),
		"negotiate":            rpcHandlerWithoutContext(Server.negotiate),
		"prune_images":         Server.handlePruneImages,
		"pull":                 Server.handlePull,
		"read_file":            rpcHandlerWithoutContext(Server.readFile),
		"read_release_state":   rpcHandlerWithoutContext(Server.readReleaseState),
		"run_container":        Server.handleRunContainer,
		"run_oneoff_container": lockedRPCHandler("run_oneoff_container", Server.runOneOffContainer),
		"status":               Server.status,
		"stop_container":       Server.handleStopContainer,
		"sync_cron_files":      lockedRPCHandler("sync_cron_files", rpcHandlerWithoutContext(Server.syncCronFiles)),
		"write_file":           lockedRPCHandler("write_file", rpcHandlerWithoutContext(Server.writeFile)),
		"write_registry_auth":  lockedRPCHandler("write_registry_auth", rpcHandlerWithoutContext(Server.writeRegistryAuth)),
		"write_release_state":  lockedRPCHandler("write_release_state", rpcHandlerWithoutContext(Server.writeReleaseState)),
	}
}

func (s Server) Handle(ctx context.Context, req Request) Response {
	handler, ok := rpcHandlers[req.Method]
	if !ok {
		return failure(req.ID, ErrorUnknownMethod, fmt.Errorf("unknown method %q", req.Method))
	}
	return handler(s, ctx, req)
}

func (s Server) handlePull(ctx context.Context, req Request) Response {
	return s.withHostLock(req, "pull", func() Response {
		var p struct {
			Image string `json:"image"`
		}
		if err := decode(req.Params, &p); err != nil {
			return failure(req.ID, ErrorInvalidParams, err)
		}
		if strings.TrimSpace(p.Image) == "" {
			return failure(req.ID, ErrorInvalidParams, errors.New("image is required"))
		}
		return empty(req.ID, ErrorDocker, s.docker().Pull(ctx, p.Image))
	})
}

func (s Server) handlePruneImages(ctx context.Context, req Request) Response {
	return s.withHostLock(req, "prune_images", func() Response {
		return empty(req.ID, ErrorDocker, s.docker().PruneShipImages(ctx))
	})
}

func (s Server) handleRunContainer(ctx context.Context, req Request) Response {
	return s.withHostLock(req, "run_container", func() Response {
		var p RunContainerParams
		if err := decode(req.Params, &p); err != nil {
			return failure(req.ID, ErrorInvalidParams, err)
		}
		if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Image) == "" {
			return failure(req.ID, ErrorInvalidParams, errors.New("name and image are required"))
		}
		if err := validateNetworkAliases(p.NetworkAliases); err != nil {
			return failure(req.ID, ErrorInvalidParams, err)
		}
		if _, err := s.docker().Inspect(ctx, p.Name); err == nil {
			if err := s.docker().StopRemove(ctx, p.Name); err != nil {
				return failure(req.ID, ErrorDocker, fmt.Errorf("replace container %q: %w", p.Name, err))
			}
		}
		args := append(labelArgs(p.Labels), networkArgs(p.Network, p.NetworkAliases)...)
		args = append(args, p.Args...)
		return empty(req.ID, ErrorDocker, s.enrichPortConflict(ctx, s.docker().Run(ctx, p.Name, p.Image, p.Command, args...)))
	})
}

func (s Server) handleStopContainer(ctx context.Context, req Request) Response {
	return s.withHostLock(req, "stop_container", func() Response {
		var p struct {
			Name string `json:"name"`
		}
		if err := decode(req.Params, &p); err != nil {
			return failure(req.ID, ErrorInvalidParams, err)
		}
		if strings.TrimSpace(p.Name) == "" {
			return failure(req.ID, ErrorInvalidParams, errors.New("name is required"))
		}
		return empty(req.ID, ErrorDocker, s.docker().StopRemove(ctx, p.Name))
	})
}

func (s Server) handleLogs(ctx context.Context, req Request) Response {
	var p LogsParams
	if err := decode(req.Params, &p); err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	logs, err := s.docker().Logs(ctx, p.Name, p.Lines)
	if err != nil {
		return failure(req.ID, ErrorDocker, err)
	}
	return result(req.ID, map[string]string{"logs": logs})
}

func (s Server) handleDockerInspect(ctx context.Context, req Request) Response {
	var p DockerInspectParams
	if err := decode(req.Params, &p); err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	inspect, err := s.docker().Inspect(ctx, p.Name)
	if err != nil {
		return failure(req.ID, ErrorDocker, err)
	}
	return result(req.ID, DockerInspectResult{Inspect: inspect})
}

func (s Server) handleListShipContainers(ctx context.Context, req Request) Response {
	containers, err := s.docker().ListShipContainers(ctx)
	if err != nil {
		return failure(req.ID, ErrorDocker, err)
	}
	return result(req.ID, containers)
}

func ServeStdio(ctx context.Context) error {
	return Serve(ctx, os.Stdin, os.Stdout)
}

func (s Server) negotiate(req Request) Response {
	var p NegotiateParams
	if err := decode(req.Params, &p); err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	minVersion := p.MinProtocolVersion
	maxVersion := p.MaxProtocolVersion
	if minVersion == 0 {
		minVersion = AgentMinProtocol
	}
	if maxVersion == 0 {
		maxVersion = AgentProtocol
	}
	selected := AgentProtocol
	if selected > maxVersion {
		selected = maxVersion
	}
	if selected < minVersion || selected < AgentMinProtocol {
		return failure(req.ID, ErrorIncompatibleProtocol, fmt.Errorf("agent supports protocol %d-%d, client requested %d-%d", AgentMinProtocol, AgentProtocol, minVersion, maxVersion))
	}
	return result(req.ID, NegotiateResult{
		AgentVersion:     Version(),
		ProtocolVersion:  selected,
		SupportedMethods: supportedMethods(),
	})
}

func (s Server) status(ctx context.Context, req Request) Response {
	hostname, err := s.hostname()
	if err != nil {
		hostname = ""
	}
	dockerErr := s.docker().Available(ctx)
	return result(req.ID, Status{
		Hostname:         hostname,
		StateDir:         s.stateDir(),
		DockerOK:         dockerErr == nil,
		AgentVersion:     Version(),
		ProtocolVersion:  AgentProtocol,
		SupportedMethods: supportedMethods(),
	})
}

func (s Server) healthCheck(ctx context.Context, req Request) Response {
	var p HealthCheckParams
	if err := decode(req.Params, &p); err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	if strings.TrimSpace(p.Command) == "" && strings.TrimSpace(p.URL) == "" {
		return failure(req.ID, ErrorInvalidParams, errors.New("command or url is required"))
	}
	timeout := time.Duration(p.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	if p.Command != "" {
		out, err := s.command()(checkCtx, "sh", "-lc", p.Command)
		if err != nil {
			return failure(req.ID, ErrorHealthCheckFailed, fmt.Errorf("health command failed: %w", err))
		}
		return result(req.ID, HealthCheckResult{OK: true, Output: trimOutput(out), DurationMS: time.Since(start).Milliseconds()})
	}
	httpClient := s.httpClient()
	httpClient.Timeout = timeout
	res, err := httpClient.Get(p.URL)
	if err != nil {
		return failure(req.ID, ErrorHealthCheckFailed, fmt.Errorf("health request failed: %w", err))
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode < 200 || res.StatusCode >= 400 {
		return failure(req.ID, ErrorHealthCheckFailed, fmt.Errorf("health request returned HTTP %d", res.StatusCode))
	}
	return result(req.ID, HealthCheckResult{OK: true, StatusCode: res.StatusCode, Output: trimOutput(string(body)), DurationMS: time.Since(start).Milliseconds()})
}

func (s Server) execContainer(ctx context.Context, req Request) Response {
	var p ExecContainerParams
	if err := decode(req.Params, &p); err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	if strings.TrimSpace(p.Name) == "" {
		return failure(req.ID, ErrorInvalidParams, errors.New("name is required"))
	}
	if strings.TrimSpace(p.Command) == "" {
		return failure(req.ID, ErrorInvalidParams, errors.New("command is required"))
	}
	commandCtx := ctx
	cancel := func() {}
	if p.TimeoutSeconds > 0 {
		commandCtx, cancel = context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	}
	defer cancel()
	out, err := s.command()(commandCtx, "docker", "exec", p.Name, "sh", "-lc", p.Command)
	if err != nil {
		return failure(req.ID, ErrorCommandFailed, fmt.Errorf("exec %q failed: %w", p.Name, err))
	}
	return result(req.ID, CommandResult{Output: trimOutput(out)})
}

func (s Server) runOneOffContainer(ctx context.Context, req Request) Response {
	var p RunOneOffContainerParams
	if err := decode(req.Params, &p); err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Image) == "" {
		return failure(req.ID, ErrorInvalidParams, errors.New("name and image are required"))
	}
	if strings.TrimSpace(p.Command) == "" {
		return failure(req.ID, ErrorInvalidParams, errors.New("command is required"))
	}
	if err := validateNetworkAliases(p.NetworkAliases); err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	// Exec-form, not sh -lc: see docker.SplitCommand for why wrapping every
	// one-off container's CMD in a login shell broke images (postgres,
	// mysql, ...) whose entrypoint scripts branch on argv[0] and whose PATH
	// comes from image ENV rather than /etc/profile.
	tokens, err := docker.SplitCommand(p.Command)
	if err != nil {
		return failure(req.ID, ErrorInvalidParams, fmt.Errorf("command for %q: %w", p.Name, err))
	}
	commandCtx := ctx
	cancel := func() {}
	if p.TimeoutSeconds > 0 {
		commandCtx, cancel = context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	}
	defer cancel()
	args := []string{"run", "--rm", "--name", p.Name}
	args = append(args, labelArgs(p.Labels)...)
	args = append(args, networkArgs(p.Network, p.NetworkAliases)...)
	args = append(args, p.Args...)
	args = append(args, p.Image)
	args = append(args, tokens...)
	out, err := s.command()(commandCtx, "docker", args...)
	if err != nil {
		return failure(req.ID, ErrorDocker, fmt.Errorf("one-off container %q failed: %w", p.Name, err))
	}
	return result(req.ID, CommandResult{Output: trimOutput(out)})
}

func (s Server) writeFile(req Request) Response {
	var p WriteFileParams
	if err := decode(req.Params, &p); err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	data, err := contentBytes(p.Content, p.Encoding)
	if err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	mode := os.FileMode(0o644)
	if p.Mode != 0 {
		mode = os.FileMode(p.Mode)
	}
	if err := atomicWriteFile(p.Path, data, mode); err != nil {
		return failure(req.ID, ErrorFileOperation, err)
	}
	return result(req.ID, WriteFileResult{Path: p.Path, Bytes: len(data)})
}

func (s Server) readFile(req Request) Response {
	var p ReadFileParams
	if err := decode(req.Params, &p); err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	if strings.TrimSpace(p.Path) == "" {
		return failure(req.ID, ErrorInvalidParams, errors.New("path is required"))
	}
	if !filepath.IsAbs(p.Path) {
		return failure(req.ID, ErrorInvalidParams, fmt.Errorf("path %q must be absolute", p.Path))
	}
	data, _, exists, err := fileSnapshot(p.Path)
	if err != nil {
		return failure(req.ID, ErrorFileOperation, err)
	}
	return result(req.ID, ReadFileResult{Path: p.Path, Content: string(data), Exists: exists})
}

func (s Server) writeRegistryAuth(req Request) Response {
	var p WriteRegistryAuthParams
	if err := decode(req.Params, &p); err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	server := strings.TrimSpace(p.Server)
	if server == "" {
		return failure(req.ID, ErrorInvalidParams, errors.New("server is required"))
	}
	if !json.Valid(p.Auth) {
		return failure(req.ID, ErrorInvalidParams, errors.New("auth must be valid JSON"))
	}
	var authObject map[string]json.RawMessage
	if err := json.Unmarshal(p.Auth, &authObject); err != nil || len(authObject) == 0 {
		return failure(req.ID, ErrorInvalidParams, errors.New("auth must be a JSON object"))
	}
	configDir, err := s.dockerConfigDir()
	if err != nil {
		return failure(req.ID, ErrorFileOperation, err)
	}
	path := filepath.Join(configDir, "config.json")
	merged, err := mergeDockerAuthConfig(path, server, p.Auth)
	if err != nil {
		return failure(req.ID, ErrorFileOperation, err)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return failure(req.ID, ErrorFileOperation, err)
	}
	if err := atomicWriteFile(path, merged, 0o600); err != nil {
		return failure(req.ID, ErrorFileOperation, err)
	}
	return result(req.ID, WriteRegistryAuthResult{Path: path, Server: server})
}

func (s Server) syncCronFiles(req Request) Response {
	var p SyncCronFilesParams
	if err := decode(req.Params, &p); err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	prefix := strings.TrimSpace(p.Prefix)
	if !validCronFileName(prefix) {
		return failure(req.ID, ErrorInvalidParams, errors.New("prefix must be a safe cron filename prefix"))
	}
	cronDir := s.cronDir()
	if err := os.MkdirAll(cronDir, 0o755); err != nil {
		return failure(req.ID, ErrorFileOperation, err)
	}
	desired := map[string]string{}
	for _, file := range p.Files {
		name := strings.TrimSpace(file.Name)
		if !strings.HasPrefix(name, prefix) || !validCronFileName(name) {
			return failure(req.ID, ErrorInvalidParams, fmt.Errorf("cron file %q must use the safe prefix %q", name, prefix))
		}
		desired[name] = file.Content
	}

	var removed []string
	entries, err := os.ReadDir(cronDir)
	if err != nil {
		return failure(req.ID, ErrorFileOperation, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if _, ok := desired[name]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(cronDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return failure(req.ID, ErrorFileOperation, err)
		}
		removed = append(removed, name)
	}
	sort.Strings(removed)

	names := make([]string, 0, len(desired))
	for name := range desired {
		names = append(names, name)
	}
	sort.Strings(names)
	var written []string
	for _, name := range names {
		content := desired[name]
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if err := atomicWriteFile(filepath.Join(cronDir, name), []byte(content), 0o644); err != nil {
			return failure(req.ID, ErrorFileOperation, err)
		}
		written = append(written, name)
	}
	return result(req.ID, SyncCronFilesResult{Dir: cronDir, Written: written, Removed: removed})
}

func (s Server) readReleaseState(req Request) Response {
	var p ReadReleaseStateParams
	if err := decode(req.Params, &p); err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	store := state.NewStore(s.stateDir())
	var (
		release state.Release
		err     error
	)
	if strings.TrimSpace(p.ID) != "" {
		release, err = store.ReadRelease(p.ID)
	} else {
		release, err = store.CurrentRelease(p.Environment)
	}
	if err != nil {
		return failure(req.ID, ErrorReleaseState, err)
	}
	return result(req.ID, release)
}

func (s Server) writeReleaseState(req Request) Response {
	var p WriteReleaseStateParams
	if err := decode(req.Params, &p); err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	store := state.NewStore(s.stateDir())
	if err := store.SaveReleaseRecord(p.Release); err != nil {
		return failure(req.ID, ErrorReleaseState, err)
	}
	release, err := store.ReadRelease(p.Release.ID)
	if err != nil {
		return failure(req.ID, ErrorReleaseState, err)
	}
	if release.Status == state.ReleaseStatusHealthy && release.Healthy {
		if err := store.PromoteRelease(release.ID); err != nil {
			return failure(req.ID, ErrorReleaseState, err)
		}
	}
	return result(req.ID, release)
}

func (s Server) caddyReload(ctx context.Context, req Request) Response {
	var p CaddyReloadParams
	if err := decode(req.Params, &p); err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	path := p.Path
	if path == "" {
		path = defaultCaddyfilePath
	}
	if p.Config != "" || p.Clear {
		return s.writeValidateReloadCaddy(ctx, req.ID, path, []byte(p.Config), p.Validate)
	}
	if p.Validate {
		if _, err := s.command()(ctx, "caddy", "validate", "--config", path); err != nil {
			return failure(req.ID, ErrorCommandFailed, fmt.Errorf("caddy validate failed: %w", err))
		}
	}
	out, err := s.command()(ctx, "caddy", "reload", "--config", path)
	if err != nil {
		return failure(req.ID, ErrorCommandFailed, fmt.Errorf("caddy reload failed: %w", err))
	}
	return result(req.ID, CommandResult{Output: trimOutput(out)})
}

func (s Server) writeValidateReloadCaddy(ctx context.Context, id, path string, data []byte, validate bool) Response {
	if strings.TrimSpace(path) == "" {
		return failure(id, ErrorFileOperation, errors.New("path is required"))
	}
	if !filepath.IsAbs(path) {
		return failure(id, ErrorFileOperation, fmt.Errorf("path %q must be absolute", path))
	}
	previous, previousMode, hadPrevious, err := fileSnapshot(path)
	if err != nil {
		return failure(id, ErrorFileOperation, err)
	}
	stagePath, err := writeTempFile(filepath.Dir(path), data, 0o644)
	if err != nil {
		return failure(id, ErrorFileOperation, err)
	}
	defer os.Remove(stagePath)
	if validate {
		if _, err := s.command()(ctx, "caddy", caddyfileCommandArgs("validate", stagePath)...); err != nil {
			return failure(id, ErrorCommandFailed, fmt.Errorf("caddy validate failed: %w", err))
		}
	}
	if err := atomicWriteFile(path, data, 0o644); err != nil {
		return failure(id, ErrorFileOperation, err)
	}
	out, err := s.command()(ctx, "caddy", caddyfileCommandArgs("reload", path)...)
	if err != nil {
		rollbackErr := restoreFileSnapshot(path, previous, previousMode, hadPrevious)
		if rollbackErr == nil && hadPrevious {
			_, _ = s.command()(ctx, "caddy", caddyfileCommandArgs("reload", path)...)
		}
		if rollbackErr != nil {
			return failure(id, ErrorCommandFailed, fmt.Errorf("caddy reload failed: %w; rollback failed: %v", err, rollbackErr))
		}
		return failure(id, ErrorCommandFailed, fmt.Errorf("caddy reload failed: %w", err))
	}
	return result(id, CommandResult{Output: trimOutput(out)})
}

func caddyfileCommandArgs(command, path string) []string {
	return []string{command, "--config", path, "--adapter", "caddyfile"}
}

func (s Server) accessoryCommand(ctx context.Context, req Request) Response {
	var p AccessoryCommandParams
	if err := decode(req.Params, &p); err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	if strings.TrimSpace(p.Command) == "" {
		return failure(req.ID, ErrorInvalidParams, errors.New("command is required"))
	}
	timeout := time.Duration(p.TimeoutSeconds) * time.Second
	commandCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		commandCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	out, err := s.command()(commandCtx, "sh", "-lc", p.Command)
	if err != nil {
		return failure(req.ID, ErrorCommandFailed, fmt.Errorf("accessory %q command failed: %w", p.Name, err))
	}
	return result(req.ID, CommandResult{Output: trimOutput(out)})
}

func (s Server) ensureVolume(ctx context.Context, req Request) Response {
	var p EnsureVolumeParams
	if err := decode(req.Params, &p); err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	if !validDockerVolumeName(p.Name) {
		return failure(req.ID, ErrorInvalidParams, fmt.Errorf("invalid volume name %q", p.Name))
	}
	out, err := s.command()(ctx, "docker", "volume", "create", p.Name)
	if err != nil {
		return failure(req.ID, ErrorDocker, fmt.Errorf("create volume %q: %w", p.Name, err))
	}
	if strings.TrimSpace(p.Owner) != "" {
		if !validVolumeOwner(p.Owner) {
			return failure(req.ID, ErrorInvalidParams, fmt.Errorf("invalid volume owner %q", p.Owner))
		}
		if _, err := s.command()(ctx, "docker", "run", "--rm", "-v", p.Name+":/ship-volume", "busybox:1.36", "chown", "-R", p.Owner, "/ship-volume"); err != nil {
			return failure(req.ID, ErrorDocker, fmt.Errorf("set owner on volume %q: %w", p.Name, err))
		}
	}
	return result(req.ID, CommandResult{Output: trimOutput(out)})
}

func (s Server) ensureNetwork(ctx context.Context, req Request) Response {
	var p EnsureNetworkParams
	if err := decode(req.Params, &p); err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	if !validDockerVolumeName(p.Name) {
		return failure(req.ID, ErrorInvalidParams, fmt.Errorf("invalid network name %q", p.Name))
	}
	if reservedDockerNetworkName(p.Name) {
		return failure(req.ID, ErrorInvalidParams, fmt.Errorf("network name %q is reserved", p.Name))
	}
	driver := strings.TrimSpace(p.Driver)
	if driver == "" {
		driver = "bridge"
	}
	if !validDockerVolumeName(driver) {
		return failure(req.ID, ErrorInvalidParams, fmt.Errorf("invalid network driver %q", p.Driver))
	}
	if _, err := s.command()(ctx, "docker", "network", "inspect", p.Name); err == nil {
		return result(req.ID, CommandResult{})
	}
	out, err := s.command()(ctx, "docker", "network", "create", "--driver", driver, p.Name)
	if err != nil {
		return failure(req.ID, ErrorDocker, fmt.Errorf("create network %q: %w", p.Name, err))
	}
	return result(req.ID, CommandResult{Output: trimOutput(out)})
}

var bindConflictPattern = regexp.MustCompile(`Bind for (?:\S*:)?(\d+) failed`)

// enrichPortConflict names the container publishing a host port when docker
// refuses to bind it. Half-migrated hosts hit this constantly — kamal-proxy
// still holding 80/443, or a Kamal-managed accessory on its old port — and
// the bare docker error gives no next step.
func (s Server) enrichPortConflict(ctx context.Context, err error) error {
	if err == nil || !strings.Contains(err.Error(), "port is already allocated") {
		return err
	}
	match := bindConflictPattern.FindStringSubmatch(err.Error())
	if match == nil {
		return err
	}
	port := match[1]
	out, psErr := s.command()(ctx, "docker", "ps", "--format", "{{.Names}}\t{{.Ports}}")
	if psErr != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name, ports, ok := strings.Cut(line, "\t")
		if !ok || !publishesHostPort(ports, port) {
			continue
		}
		return fmt.Errorf("%w; host port %s is published by container %q — stop it before deploying (docker stop %s); during a Kamal migration this is typically kamal-proxy or a Kamal accessory that is still running", err, port, name, name)
	}
	return err
}

func publishesHostPort(ports, port string) bool {
	return strings.Contains(ports, ":"+port+"->")
}

func (s Server) installBinary(req Request) Response {
	var p InstallBinaryParams
	if err := decode(req.Params, &p); err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	path := p.Path
	if path == "" {
		path = config.RemoteBinaryPath
	}
	data, err := base64.StdEncoding.DecodeString(p.ContentBase64)
	if err != nil {
		return failure(req.ID, ErrorInvalidParams, fmt.Errorf("decode binary content: %w", err))
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if p.SHA256 != "" && !strings.EqualFold(p.SHA256, digest) {
		return failure(req.ID, ErrorInvalidParams, fmt.Errorf("binary checksum mismatch"))
	}
	// The client supplies both bytes and hash, so the checksum alone proves
	// nothing about what is being installed; refuse anything that is not an
	// executable for this host before it can replace the agent.
	goos, goarch, ok := binfmt.Detect(data)
	if !ok && runtime.GOOS == "darwin" && binfmt.HasDarwinSlice(data, runtime.GOARCH) {
		// Universal Mach-O with a slice for this host; mirrors the CLI-side
		// verifyBinaryPlatform acceptance.
		goos, goarch, ok = runtime.GOOS, runtime.GOARCH, true
	}
	if !ok {
		return failure(req.ID, ErrorInvalidParams, fmt.Errorf("binary is not a recognizable executable"))
	}
	if goos != runtime.GOOS || goarch != runtime.GOARCH {
		return failure(req.ID, ErrorInvalidParams, fmt.Errorf("binary targets %s/%s but this host is %s/%s", goos, goarch, runtime.GOOS, runtime.GOARCH))
	}
	mode := os.FileMode(0o755)
	if p.Mode != 0 {
		mode = os.FileMode(p.Mode)
	}
	if current, err := os.ReadFile(path); err == nil {
		currentSum := sha256.Sum256(current)
		if hex.EncodeToString(currentSum[:]) == digest {
			if err := os.Chmod(path, mode); err != nil {
				return failure(req.ID, ErrorFileOperation, err)
			}
			return result(req.ID, InstallBinaryResult{Path: path, Installed: false, SHA256: digest})
		}
	}
	if err := atomicWriteFile(path, data, mode); err != nil {
		return failure(req.ID, ErrorFileOperation, err)
	}
	return result(req.ID, InstallBinaryResult{Path: path, Installed: true, SHA256: digest})
}

func (s Server) migrateStateDir(req Request) Response {
	var p MigrateStateDirParams
	if err := decode(req.Params, &p); err != nil {
		return failure(req.ID, ErrorInvalidParams, err)
	}
	to := p.To
	if to == "" {
		to = s.stateDir()
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return failure(req.ID, ErrorFileOperation, err)
	}
	migrated := false
	if p.From != "" && filepath.Clean(p.From) != filepath.Clean(to) {
		if err := copyDirIfExists(p.From, to); err != nil {
			return failure(req.ID, ErrorFileOperation, err)
		}
		migrated = true
	}
	return result(req.ID, MigrateStateDirResult{From: p.From, To: to, Migrated: migrated})
}

func (s Server) withHostLock(req Request, operation string, fn func() Response) Response {
	unlock, err := acquireHostLock(s.stateDir())
	if err != nil {
		return failure(req.ID, ErrorLock, fmt.Errorf("%s lock: %w", operation, err))
	}
	defer unlock()
	return fn()
}

func (s Server) docker() DockerOps {
	if s.Docker != nil {
		return s.Docker
	}
	return docker.Client{}
}

func (s Server) command() CommandRunner {
	if s.CommandRunner != nil {
		return s.CommandRunner
	}
	return defaultCommandRunner
}

func (s Server) httpClient() *http.Client {
	base := s.HTTPClient
	if base == nil {
		base = http.DefaultClient
	}
	copy := *base
	return &copy
}

func (s Server) stateDir() string {
	if strings.TrimSpace(s.StateDir) != "" {
		return s.StateDir
	}
	return config.RemoteStateDir
}

func (s Server) dockerConfigDir() (string, error) {
	if strings.TrimSpace(s.DockerConfigDir) != "" {
		return s.DockerConfigDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".docker"), nil
}

func (s Server) cronDir() string {
	if strings.TrimSpace(s.CronDir) != "" {
		return s.CronDir
	}
	return "/etc/cron.d"
}

func (s Server) hostname() (string, error) {
	if s.Hostname != nil {
		return s.Hostname()
	}
	return os.Hostname()
}

func defaultCommandRunner(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s failed: %s", name, msg)
	}
	return string(out), nil
}

func acquireHostLock(stateDir string) (func(), error) {
	lockDir := filepath.Join(stateDir, "locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(lockDir, "host.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func decode(data json.RawMessage, out any) error {
	if len(data) == 0 {
		data = []byte("{}")
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode params: %w", err)
	}
	return nil
}

func result(id string, value any) Response {
	data, err := json.Marshal(value)
	if err != nil {
		return failure(id, ErrorInternal, err)
	}
	return Response{ID: id, OK: true, Result: data}
}

func empty(id string, code string, err error) Response {
	if err != nil {
		return failure(id, code, err)
	}
	return Response{ID: id, OK: true}
}

func failure(id string, code string, err error) Response {
	if err == nil {
		err = errors.New("unknown error")
	}
	var coded rpcError
	if errors.As(err, &coded) && coded.code != "" {
		code = coded.code
	}
	if code == "" {
		code = ErrorInternal
	}
	return Response{ID: id, OK: false, Error: err.Error(), ErrorCode: code}
}

func supportedMethods() []string {
	methods := make([]string, 0, len(rpcHandlers))
	for method := range rpcHandlers {
		methods = append(methods, method)
	}
	sort.Strings(methods)
	return methods
}

func mergeDockerAuthConfig(path, server string, auth json.RawMessage) ([]byte, error) {
	config := map[string]json.RawMessage{}
	if data, err := os.ReadFile(path); err == nil {
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, &config); err != nil {
				return nil, fmt.Errorf("parse docker config: %w", err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	auths := map[string]json.RawMessage{}
	if raw := config["auths"]; len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &auths); err != nil {
			return nil, fmt.Errorf("parse docker config auths: %w", err)
		}
	}
	auths[server] = append(json.RawMessage(nil), auth...)
	rawAuths, err := json.Marshal(auths)
	if err != nil {
		return nil, err
	}
	config["auths"] = rawAuths
	return json.MarshalIndent(config, "", "  ")
}

func validDockerVolumeName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		allowed := r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' ||
			r == '_' || r == '.' || r == '-'
		if !allowed {
			return false
		}
	}
	return true
}

func validVolumeOwner(owner string) bool {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return false
	}
	for _, r := range owner {
		allowed := r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' ||
			r == '_' || r == '-' || r == ':' || r == '.'
		if !allowed {
			return false
		}
	}
	return true
}

func validCronFileName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		allowed := r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' ||
			r == '_' || r == '.' || r == '-'
		if !allowed {
			return false
		}
	}
	return true
}

func labelArgs(labels map[string]string) []string {
	if len(labels) == 0 {
		return nil
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	args := make([]string, 0, len(keys)*2)
	for _, key := range keys {
		value := labels[key]
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			continue
		}
		args = append(args, "--label", key+"="+value)
	}
	return args
}

func networkArgs(network string, aliases []string) []string {
	network = strings.TrimSpace(network)
	if network == "" {
		return nil
	}
	args := []string{"--network", network}
	for _, alias := range normalizedNetworkAliases(aliases) {
		args = append(args, "--network-alias", alias)
	}
	return args
}

func normalizedNetworkAliases(aliases []string) []string {
	if len(aliases) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		if _, ok := seen[alias]; ok {
			continue
		}
		seen[alias] = struct{}{}
		out = append(out, alias)
	}
	sort.Strings(out)
	return out
}

func validateNetworkAliases(aliases []string) error {
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		if !validDockerVolumeName(alias) {
			return fmt.Errorf("invalid network alias %q", alias)
		}
	}
	return nil
}

func reservedDockerNetworkName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "bridge", "host", "none":
		return true
	default:
		return false
	}
}

func contentBytes(content, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "plain", "text":
		return []byte(content), nil
	case "base64":
		data, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return nil, fmt.Errorf("decode base64 content: %w", err)
		}
		return data, nil
	default:
		return nil, fmt.Errorf("unsupported content encoding %q", encoding)
	}
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("path is required")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("path %q must be absolute", path)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ship-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func writeTempFile(dir string, data []byte, mode os.FileMode) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".ship-caddy-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	return tmpName, nil
}

func fileSnapshot(path string) ([]byte, os.FileMode, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0o644, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, false, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, false, err
	}
	return data, info.Mode(), true, nil
}

func restoreFileSnapshot(path string, data []byte, mode os.FileMode, exists bool) error {
	if exists {
		return atomicWriteFile(path, data, mode)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func copyDirIfExists(from, to string) error {
	info, err := os.Stat(from)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", from)
	}
	return filepath.WalkDir(from, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(to, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if _, err := os.Stat(target); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return atomicWriteFile(target, data, info.Mode())
	})
}

func trimOutput(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 4096 {
		return value
	}
	return value[:4096]
}
