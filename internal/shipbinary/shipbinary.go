package shipbinary

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/binfmt"
)

const (
	modulePath     = "github.com/watzon/ship"
	releaseBaseURL = "https://github.com/watzon/ship/releases/download"

	// maxAssetBytes caps a downloaded release asset; maxBinaryBytes caps a
	// single extracted binary regardless of what the tar header claims.
	maxAssetBytes  = 256 << 20
	maxBinaryBytes = 256 << 20
)

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
		return "", fmt.Errorf("unsupported remote OS %q (ship supports linux and darwin hosts)", sysname)
	}
}

func normalizeGOARCH(machine string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(machine)) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported remote architecture %q (ship supports amd64 and arm64 hosts)", machine)
	}
}

// PlatformOfExecutable reports the GOOS/GOARCH a binary file was built for by
// inspecting its object format (ELF or Mach-O). No toolchain is required.
func PlatformOfExecutable(path string) (Platform, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Platform{}, false, nil
	}
	platform, ok := detectPlatform(data)
	return platform, ok, nil
}

func detectPlatform(data []byte) (Platform, bool) {
	goos, goarch, ok := binfmt.Detect(data)
	return Platform{GOOS: goos, GOARCH: goarch}, ok
}

func verifyBinaryPlatform(data []byte, target Platform) error {
	if platform, ok := detectPlatform(data); ok {
		if platform.matches(target) {
			return nil
		}
		return fmt.Errorf("%w: binary is %s, host needs %s", errWrongPlatform, platform, target)
	}
	if target.GOOS == "darwin" && binfmt.HasDarwinSlice(data, target.GOARCH) {
		return nil
	}
	return fmt.Errorf("%w: not a recognizable %s executable", errWrongPlatform, target)
}

// Options controls where Resolve may obtain the agent binary. When either
// override is set, only that source is consulted: an explicit override that
// does not work must fail loudly, never fall through to a network download.
type Options struct {
	// BinaryPath points at a prebuilt ship binary (or release .tar.gz) for
	// the target platform.
	BinaryPath string
	// ReleaseDir points at a local mirror of release assets:
	// ship_{version}_{goos}_{goarch}.tar.gz files plus checksums.txt.
	ReleaseDir string
}

// Sentinel error classes for resolution failures. resolveError renders them
// into per-class remediation guidance.
var (
	errNoModuleRoot  = errors.New("ship module root not found")
	errAssetNotFound = errors.New("release asset not found")
	errBadChecksum   = errors.New("release asset failed integrity verification")
	errWrongPlatform = errors.New("binary platform mismatch")
	errDevVersion    = errors.New("development build has no release assets")
)

type attempt struct {
	strategy string
	outcome  string // "skipped" or "FAILED"
	detail   string
	err      error
}

// resolveError reports every strategy Resolve attempted and why each did not
// produce a binary, ending with one actionable remediation line.
type resolveError struct {
	target   Platform
	version  string
	attempts []attempt
}

func (e *resolveError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "could not obtain a %s agent binary\n\nTried:\n", e.target)
	for _, a := range e.attempts {
		fmt.Fprintf(&b, "  %-14s %-8s %s\n", a.strategy, a.outcome, a.detail)
	}
	fmt.Fprintf(&b, "\n%s", e.fix())
	return b.String()
}

func (e *resolveError) fix() string {
	for _, a := range e.attempts {
		if a.err == nil {
			continue
		}
		switch {
		case errors.Is(a.err, errBadChecksum):
			return "Fix: integrity verification failed — do not retry blindly. Verify the release at https://github.com/watzon/ship/releases or provide a trusted binary with --agent-binary."
		case errors.Is(a.err, errWrongPlatform):
			return "Fix: the resolved binary is not a " + e.target.String() + " executable — verify the release assets at https://github.com/watzon/ship/releases."
		case errors.Is(a.err, errDevVersion):
			return "Fix: go install github.com/watzon/ship/cmd/ship@latest  (this development build has no release assets; alternatively run ship from the " + modulePath + " checkout to cross-compile)"
		case errors.Is(a.err, errAssetNotFound):
			return "Fix: ship release check v" + e.version + "  (then install a version whose assets exist: go install github.com/watzon/ship/cmd/ship@vX.Y.Z)"
		}
	}
	return "Fix: check network access to github.com and retry; for hosts without egress, mirror the release assets and pass --agent-release-dir (see docs/airgap.md)."
}

var osExecutable = os.Executable

// Resolve returns a ship binary built for target. Strategies are tried in
// order — explicit override, local binary, cross-compile from a verified ship
// checkout, checksum-verified release download — and no strategy's failure
// short-circuits the ones after it; if all fail, the returned error lists
// every attempt and why it failed.
func Resolve(ctx context.Context, target Platform, opts Options) ([]byte, error) {
	if opts.BinaryPath != "" && opts.ReleaseDir != "" {
		return nil, errors.New("both an agent binary and an agent release dir are set (--agent-binary/SHIP_AGENT_BINARY and --agent-release-dir/SHIP_AGENT_RELEASE_DIR); set exactly one")
	}
	if opts.BinaryPath != "" || opts.ReleaseDir != "" {
		return resolveOverride(target, opts)
	}

	var attempts []attempt

	if local := LocalPlatform(); local.matches(target) {
		data, err := readLocalBinary()
		if err == nil {
			return data, nil
		}
		attempts = append(attempts, attempt{"local binary", "FAILED", err.Error(), err})
	} else {
		attempts = append(attempts, attempt{
			"local binary", "skipped",
			fmt.Sprintf("this CLI is %s; host needs %s", local, target), nil,
		})
	}

	if built, err := crossCompile(ctx, target); err == nil {
		return built, nil
	} else if errors.Is(err, errNoModuleRoot) {
		attempts = append(attempts, attempt{"cross-compile", "skipped", "not inside the " + modulePath + " source tree", nil})
	} else {
		attempts = append(attempts, attempt{"cross-compile", "FAILED", compactError(err), err})
	}

	version := agent.Version()
	if downloaded, err := downloadRelease(ctx, target); err == nil {
		return downloaded, nil
	} else {
		attempts = append(attempts, attempt{"release v" + version, "FAILED", compactError(err), err})
	}

	return nil, &resolveError{target: target, version: version, attempts: attempts}
}

func readLocalBinary() ([]byte, error) {
	localPath, err := osExecutable()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(localPath)
}

func compactError(err error) string {
	msg := strings.TrimSpace(err.Error())
	if idx := strings.IndexByte(msg, '\n'); idx >= 0 {
		msg = msg[:idx] + " …"
	}
	return msg
}

func resolveOverride(target Platform, opts Options) ([]byte, error) {
	if opts.BinaryPath != "" {
		data, err := os.ReadFile(opts.BinaryPath)
		if err != nil {
			return nil, fmt.Errorf("agent binary override: %w", err)
		}
		if strings.HasSuffix(opts.BinaryPath, ".tar.gz") || strings.HasSuffix(opts.BinaryPath, ".tgz") {
			data, err = extractShipBinary(data)
			if err != nil {
				return nil, fmt.Errorf("agent binary override %s: %w", opts.BinaryPath, err)
			}
		}
		if err := verifyBinaryPlatform(data, target); err != nil {
			return nil, fmt.Errorf("agent binary override %s: %w", opts.BinaryPath, err)
		}
		return data, nil
	}
	return resolveFromReleaseDir(target, opts.ReleaseDir)
}

func resolveFromReleaseDir(target Platform, dir string) ([]byte, error) {
	assetName, err := releaseDirAsset(target, dir)
	if err != nil {
		return nil, err
	}
	tarball, err := os.ReadFile(filepath.Join(dir, assetName))
	if err != nil {
		return nil, fmt.Errorf("agent release dir %s: %w", dir, err)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "checksums.txt"))
	if err != nil {
		return nil, fmt.Errorf("agent release dir %s: checksums.txt is required to verify mirrored assets: %w", dir, err)
	}
	if err := verifyChecksumAgainstManifest(manifest, assetName, tarball); err != nil {
		return nil, fmt.Errorf("agent release dir %s: %w", dir, err)
	}
	binary, err := extractShipBinary(tarball)
	if err != nil {
		return nil, fmt.Errorf("agent release dir %s: %s: %w", dir, assetName, err)
	}
	if err := verifyBinaryPlatform(binary, target); err != nil {
		return nil, fmt.Errorf("agent release dir %s: %s: %w", dir, assetName, err)
	}
	return binary, nil
}

func releaseDirAsset(target Platform, dir string) (string, error) {
	if version := agent.Version(); releaseVersion(version) {
		exact := releaseAssetName(version, target)
		if _, err := os.Stat(filepath.Join(dir, exact)); err == nil {
			return exact, nil
		}
	}
	pattern := fmt.Sprintf("ship_*_%s_%s.tar.gz", target.GOOS, target.GOARCH)
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return "", fmt.Errorf("agent release dir %s: %w", dir, err)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("agent release dir %s has no %s asset (expected %s)", dir, target, pattern)
	case 1:
		return filepath.Base(matches[0]), nil
	default:
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = filepath.Base(m)
		}
		return "", fmt.Errorf("agent release dir %s has multiple %s assets (%s) and none matches this CLI's version %s; keep exactly one, or install the matching CLI version", dir, target, strings.Join(names, ", "), agent.Version())
	}
}

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

	ldflags := fmt.Sprintf("-s -w -X %s/internal/agent.AgentVersion=%s", modulePath, agent.Version())
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

// moduleRoot locates the github.com/watzon/ship module root. Being inside
// some other Go module (deploying a Go application, say) must not count:
// cross-compiling ./cmd/ship only makes sense in ship's own source tree.
// Under a go.work workspace `go list -m` prints one line per member module,
// so scan for ship's line rather than assuming a single module.
func moduleRoot() (string, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Path}}::{{.Dir}}").Output()
	if err != nil {
		return "", errNoModuleRoot
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, dir, ok := strings.Cut(strings.TrimSpace(line), "::")
		if ok && path == modulePath && dir != "" {
			return dir, nil
		}
	}
	return "", errNoModuleRoot
}

var releaseVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func releaseVersion(version string) bool {
	return releaseVersionPattern.MatchString(version)
}

func releaseAssetName(version string, target Platform) string {
	return fmt.Sprintf("ship_%s_%s_%s.tar.gz", version, target.GOOS, target.GOARCH)
}

func releaseAssetURL(version, assetName string) string {
	return fmt.Sprintf("%s/v%s/%s", releaseBaseURL, version, assetName)
}

func downloadRelease(ctx context.Context, target Platform) ([]byte, error) {
	version := agent.Version()
	if !releaseVersion(version) {
		return nil, fmt.Errorf("%w: this build reports version %q", errDevVersion, version)
	}
	assetName := releaseAssetName(version, target)
	tarball, err := fetchAsset(ctx, releaseAssetURL(version, assetName))
	if err != nil {
		return nil, err
	}
	if err := verifyChecksum(ctx, version, assetName, tarball); err != nil {
		return nil, err
	}
	binary, err := extractShipBinary(tarball)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", assetName, err)
	}
	if err := verifyBinaryPlatform(binary, target); err != nil {
		return nil, fmt.Errorf("%s: %w", assetName, err)
	}
	return binary, nil
}

// verifyChecksum checks the downloaded tarball against the release's
// checksums.txt. This is transfer integrity (corruption, truncated uploads,
// wrong-asset redirects) — the manifest shares the tarball's channel and
// trust root, so it is not supply-chain provenance. Fail closed either way:
// these bytes are installed as the root-privileged agent.
func verifyChecksum(ctx context.Context, version, assetName string, tarball []byte) error {
	manifest, err := fetchAsset(ctx, releaseAssetURL(version, "checksums.txt"))
	if err != nil {
		return fmt.Errorf("%w: cannot verify %s: %v", errBadChecksum, assetName, err)
	}
	return verifyChecksumAgainstManifest(manifest, assetName, tarball)
}

func verifyChecksumAgainstManifest(manifest []byte, assetName string, tarball []byte) error {
	want, err := checksumFor(manifest, assetName)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(tarball)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("%w: %s is sha256 %s but checksums.txt says %s", errBadChecksum, assetName, got, want)
	}
	return nil
}

func checksumFor(manifest []byte, assetName string) (string, error) {
	for _, line := range strings.Split(string(manifest), "\n") {
		fields := strings.Fields(line)
		// Tolerate manifests with extra non-checksum lines (early releases
		// carried a bare-filename preamble) and sha256sum's binary-mode "*".
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == assetName {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("%w: checksums.txt has no entry for %s", errBadChecksum, assetName)
}

var downloadRetryDelays = []time.Duration{2 * time.Second, 6 * time.Second}

// fetchAsset downloads a release asset, retrying transient failures and 404s
// (a release can be visible minutes before its assets finish uploading).
func fetchAsset(ctx context.Context, url string) ([]byte, error) {
	var lastErr error
	for i := 0; ; i++ {
		status, body, err := httpGet(ctx, url)
		switch {
		case err != nil:
			lastErr = fmt.Errorf("download %s: %w", url, err)
		case status == http.StatusOK:
			return body, nil
		case status == http.StatusNotFound:
			lastErr = fmt.Errorf("%w: %s (HTTP 404)", errAssetNotFound, url)
		default:
			lastErr = fmt.Errorf("download %s: HTTP %d", url, status)
		}
		if i >= len(downloadRetryDelays) || ctx.Err() != nil {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, lastErr
		case <-time.After(downloadRetryDelays[i]):
		}
	}
}

var httpGet = defaultHTTPGet

var httpClient = &http.Client{
	// The deploy path threads a context with no deadline; this is the
	// backstop against a stalled transfer hanging a deploy indefinitely.
	Timeout: 5 * time.Minute,
}

func defaultHTTPGet(ctx context.Context, url string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAssetBytes+1))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if len(body) > maxAssetBytes {
		return resp.StatusCode, nil, fmt.Errorf("response body exceeds %d bytes", maxAssetBytes)
	}
	return resp.StatusCode, body, nil
}

// SupportedPlatforms is the release build matrix: every platform a published
// release ships binaries for.
var SupportedPlatforms = []Platform{
	{GOOS: "linux", GOARCH: "amd64"},
	{GOOS: "linux", GOARCH: "arm64"},
	{GOOS: "darwin", GOARCH: "amd64"},
	{GOOS: "darwin", GOARCH: "arm64"},
}

// AssetStatus reports whether one published release asset exists.
type AssetStatus struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// CheckRelease probes the direct download URLs (never the rate-limited
// GitHub API) for every asset a release must publish, so a version can be
// vetted before pinning it in CI or relying on agent installs.
func CheckRelease(ctx context.Context, version string) ([]AssetStatus, bool, error) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if !releaseVersion(version) {
		return nil, false, fmt.Errorf("%q is not a release version (expected X.Y.Z, e.g. 0.4.6)", version)
	}
	names := []string{"checksums.txt"}
	for _, platform := range SupportedPlatforms {
		names = append(names, releaseAssetName(version, platform))
	}
	statuses := make([]AssetStatus, 0, len(names))
	allOK := true
	for _, name := range names {
		status := AssetStatus{Name: name, URL: releaseAssetURL(version, name)}
		code, err := httpHead(ctx, status.URL)
		switch {
		case err != nil:
			status.Detail = err.Error()
		case code == http.StatusOK:
			status.OK = true
		case code == http.StatusNotFound:
			status.Detail = "not published (HTTP 404)"
		default:
			status.Detail = fmt.Sprintf("HTTP %d", code)
		}
		if !status.OK {
			allOK = false
		}
		statuses = append(statuses, status)
	}
	return statuses, allOK, nil
}

var httpHead = defaultHTTPHead

func defaultHTTPHead(ctx context.Context, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
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
		if header.Size < 0 || header.Size > maxBinaryBytes {
			return nil, fmt.Errorf("ship binary entry claims %d bytes, refusing to extract", header.Size)
		}
		data, err := io.ReadAll(io.LimitReader(tr, header.Size))
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	return nil, errors.New("ship binary not found in release archive")
}
