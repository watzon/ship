package shipbinary

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/watzon/ship/internal/agent"
)

const modulePath = "github.com/watzon/ship"

// Platform identifies a GOOS/GOARCH target for the ship CLI and agent binary.
type Platform struct {
	GOOS   string
	GOARCH string
}

func (p Platform) String() string {
	return p.GOOS + "/" + p.GOARCH
}

func LocalPlatform() Platform {
	return Platform{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
}

func (p Platform) matches(other Platform) bool {
	return strings.EqualFold(p.GOOS, other.GOOS) && strings.EqualFold(p.GOARCH, other.GOARCH)
}

// ParseUname converts uname output into a ship binary platform.
func ParseUname(sysname, machine string) (Platform, error) {
	goos, err := normalizeGOOS(sysname)
	if err != nil {
		return Platform{}, err
	}
	goarch, err := normalizeGOARCH(machine)
	if err != nil {
		return Platform{}, err
	}
	return Platform{GOOS: goos, GOARCH: goarch}, nil
}

func normalizeGOOS(sysname string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(sysname)) {
	case "linux":
		return "linux", nil
	case "darwin":
		return "darwin", nil
	default:
		return "", fmt.Errorf("unsupported remote OS %q", sysname)
	}
}

func normalizeGOARCH(machine string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(machine)) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported remote architecture %q", machine)
	}
}

// PlatformOfExecutable reports the GOOS/GOARCH a ship binary was built for.
func PlatformOfExecutable(path string) (Platform, bool, error) {
	out, err := exec.Command("go", "version", "-m", path).CombinedOutput()
	if err != nil {
		return Platform{}, false, nil
	}
	var goos, goarch string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "build\tGOOS=") {
			goos = strings.TrimPrefix(line, "build\tGOOS=")
		}
		if strings.HasPrefix(line, "build\tGOARCH=") {
			goarch = strings.TrimPrefix(line, "build\tGOARCH=")
		}
	}
	if goos == "" || goarch == "" {
		return Platform{}, false, nil
	}
	return Platform{GOOS: goos, GOARCH: goarch}, true, nil
}

// Resolve returns a ship binary built for target, cross-compiling or downloading
// a release asset when the local executable targets a different platform.
var osExecutable = os.Executable

func Resolve(ctx context.Context, target Platform) ([]byte, error) {
	localPath, err := osExecutable()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		return nil, err
	}
	if platform, ok, err := PlatformOfExecutable(localPath); err != nil {
		return nil, err
	} else if ok && platform.matches(target) {
		return data, nil
	}
	if local := LocalPlatform(); local.matches(target) {
		return data, nil
	}
	if built, err := crossCompile(ctx, target); err == nil {
		return built, nil
	} else if !errors.Is(err, errNoModuleRoot) {
		return nil, fmt.Errorf("cross-compile ship for %s: %w", target, err)
	}
	if downloaded, err := downloadRelease(ctx, target); err == nil {
		return downloaded, nil
	}
	return nil, fmt.Errorf(
		"local ship binary is %s but remote host needs %s; install a matching release binary or run ship from a checkout with Go installed",
		LocalPlatform(),
		target,
	)
}

var errNoModuleRoot = errors.New("ship module root not found")

var moduleRootFunc = moduleRoot

func crossCompile(ctx context.Context, target Platform) ([]byte, error) {
	modRoot, err := moduleRootFunc()
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp("", "ship-build-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	ldflags := fmt.Sprintf("-s -w -X %s/internal/agent.AgentVersion=%s", modulePath, agent.AgentVersion)
	cmd := exec.CommandContext(
		ctx,
		"go",
		"build",
		"-trimpath",
		"-ldflags",
		ldflags,
		"-o",
		tmpPath,
		"./cmd/ship",
	)
	cmd.Dir = modRoot
	cmd.Env = append(os.Environ(),
		"GOOS="+target.GOOS,
		"GOARCH="+target.GOARCH,
		"CGO_ENABLED=0",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return os.ReadFile(tmpPath)
}

func moduleRoot() (string, error) {
	cmd := exec.Command("go", "env", "GOMOD")
	out, err := cmd.Output()
	if err != nil {
		return "", errNoModuleRoot
	}
	modPath := strings.TrimSpace(string(out))
	if modPath == "" || modPath == "/dev/null" {
		return "", errNoModuleRoot
	}
	return filepath.Dir(modPath), nil
}

func downloadRelease(ctx context.Context, target Platform) ([]byte, error) {
	version := strings.TrimSpace(agent.AgentVersion)
	if version == "" || version == "dev" {
		return nil, fmt.Errorf("no release version configured")
	}
	url := fmt.Sprintf(
		"https://github.com/watzon/ship/releases/download/v%s/ship_%s_%s_%s.tar.gz",
		version,
		version,
		target.GOOS,
		target.GOARCH,
	)
	req, err := httpNewRequestWithContext(ctx, url)
	if err != nil {
		return nil, err
	}
	body, err := httpGet(req)
	if err != nil {
		return nil, err
	}
	return extractShipBinary(body)
}

var (
	httpNewRequestWithContext = defaultHTTPNewRequestWithContext
	httpGet                   = defaultHTTPGet
)

func defaultHTTPNewRequestWithContext(ctx context.Context, url string) (httpRequest, error) {
	return httpRequest{ctx: ctx, url: url}, nil
}

func defaultHTTPGet(req httpRequest) ([]byte, error) {
	command := exec.CommandContext(req.ctx, "curl", "-fsSL", req.url)
	return command.Output()
}

type httpRequest struct {
	ctx context.Context
	url string
}

func extractShipBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg || header.Name != "ship" {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, header.Size))
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	return nil, errors.New("ship binary not found in release archive")
}
