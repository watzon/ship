package shipbinary

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/watzon/ship/internal/agent"
)

func TestParseUname(t *testing.T) {
	platform, err := ParseUname("Linux", "x86_64")
	if err != nil {
		t.Fatal(err)
	}
	if platform.GOOS != "linux" || platform.GOARCH != "amd64" {
		t.Fatalf("platform = %+v", platform)
	}
}

func TestNormalizeGOARCHRejectsUnknown(t *testing.T) {
	if _, err := normalizeGOARCH("riscv64"); err == nil {
		t.Fatal("expected error")
	}
}

// fakeELF builds a minimal but parseable ELF64 header for the given machine.
func fakeELF(machine elf.Machine) []byte {
	var b bytes.Buffer
	ident := [16]byte{0x7f, 'E', 'L', 'F', 2 /* 64-bit */, 1 /* little endian */, 1 /* version */}
	b.Write(ident[:])
	le := binary.LittleEndian
	writeU16 := func(v uint16) { _ = binary.Write(&b, le, v) }
	writeU32 := func(v uint32) { _ = binary.Write(&b, le, v) }
	writeU64 := func(v uint64) { _ = binary.Write(&b, le, v) }
	writeU16(2)               // e_type ET_EXEC
	writeU16(uint16(machine)) // e_machine
	writeU32(1)               // e_version
	writeU64(0)               // e_entry
	writeU64(0)               // e_phoff
	writeU64(0)               // e_shoff
	writeU32(0)               // e_flags
	writeU16(64)              // e_ehsize
	writeU16(0)               // e_phentsize
	writeU16(0)               // e_phnum
	writeU16(0)               // e_shentsize
	writeU16(0)               // e_shnum
	writeU16(0)               // e_shstrndx
	b.WriteString("fake ship binary payload")
	return b.Bytes()
}

func TestDetectPlatformELF(t *testing.T) {
	platform, ok := detectPlatform(fakeELF(elf.EM_X86_64))
	if !ok || platform.GOOS != "linux" || platform.GOARCH != "amd64" {
		t.Fatalf("platform = %+v ok = %v", platform, ok)
	}
	platform, ok = detectPlatform(fakeELF(elf.EM_AARCH64))
	if !ok || platform.GOARCH != "arm64" {
		t.Fatalf("platform = %+v ok = %v", platform, ok)
	}
	if _, ok := detectPlatform([]byte("not a binary")); ok {
		t.Fatal("expected detection failure")
	}
}

func TestDetectPlatformOwnExecutable(t *testing.T) {
	path, err := os.Executable()
	if err != nil {
		t.Skip("cannot locate test executable")
	}
	platform, ok, err := PlatformOfExecutable(path)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !platform.matches(LocalPlatform()) {
		t.Fatalf("test binary detected as %s, runtime is %s", platform, LocalPlatform())
	}
}

func TestVerifyBinaryPlatform(t *testing.T) {
	linuxAMD64 := Platform{GOOS: "linux", GOARCH: "amd64"}
	if err := verifyBinaryPlatform(fakeELF(elf.EM_X86_64), linuxAMD64); err != nil {
		t.Fatal(err)
	}
	err := verifyBinaryPlatform(fakeELF(elf.EM_AARCH64), linuxAMD64)
	if !errors.Is(err, errWrongPlatform) {
		t.Fatalf("err = %v", err)
	}
	err = verifyBinaryPlatform([]byte("garbage"), linuxAMD64)
	if !errors.Is(err, errWrongPlatform) {
		t.Fatalf("err = %v", err)
	}
}

func setAgentVersion(t *testing.T, version string) {
	t.Helper()
	orig := agent.AgentVersion
	agent.AgentVersion = version
	t.Cleanup(func() { agent.AgentVersion = orig })
}

func disableRetryDelays(t *testing.T) {
	t.Helper()
	orig := downloadRetryDelays
	downloadRetryDelays = nil
	t.Cleanup(func() { downloadRetryDelays = orig })
}

func stubModuleRoot(t *testing.T, fn func() (string, error)) {
	t.Helper()
	orig := moduleRootFunc
	moduleRootFunc = fn
	t.Cleanup(func() { moduleRootFunc = orig })
}

func stubExecutable(t *testing.T, path string) {
	t.Helper()
	orig := osExecutable
	osExecutable = func() (string, error) { return path, nil }
	t.Cleanup(func() { osExecutable = orig })
}

// stubRelease serves a fake release (tarball + checksums.txt) through the
// httpGet seam and returns the binary the tarball contains.
func stubRelease(t *testing.T, version string, target Platform, extraManifestLines string) []byte {
	t.Helper()
	binary := fakeELF(elf.EM_X86_64)
	if target.GOARCH == "arm64" {
		binary = fakeELF(elf.EM_AARCH64)
	}
	archive, err := fakeReleaseArchive(binary)
	if err != nil {
		t.Fatal(err)
	}
	assetName := releaseAssetName(version, target)
	sum := sha256.Sum256(archive)
	manifest := extraManifestLines + hex.EncodeToString(sum[:]) + "  " + assetName + "\n"
	origHTTPGet := httpGet
	httpGet = func(ctx context.Context, url string) (int, []byte, error) {
		switch {
		case strings.HasSuffix(url, "/checksums.txt"):
			return http.StatusOK, []byte(manifest), nil
		case strings.HasSuffix(url, "/"+assetName):
			return http.StatusOK, archive, nil
		default:
			return http.StatusNotFound, nil, nil
		}
	}
	t.Cleanup(func() { httpGet = origHTTPGet })
	return binary
}

func TestResolveUsesLocalExecutableWhenPlatformsMatch(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "ship")
	if err := os.WriteFile(binary, []byte("local-ship"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubExecutable(t, binary)

	data, err := Resolve(context.Background(), LocalPlatform(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "local-ship" {
		t.Fatalf("data = %q", data)
	}
}

func TestResolveCrossCompilesForRemoteLinuxAMD64(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not installed")
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		t.Skip("local platform already matches target")
	}
	stubExecutable(t, filepath.Join(t.TempDir(), "missing"))

	data, err := Resolve(context.Background(), Platform{GOOS: "linux", GOARCH: "amd64"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	platform, ok := detectPlatform(data)
	if !ok || platform.GOOS != "linux" || platform.GOARCH != "amd64" {
		t.Fatalf("unexpected cross-compiled platform: %+v ok=%v", platform, ok)
	}
}

func TestModuleRootRejectsForeignModule(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/notship\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if _, err := moduleRoot(); !errors.Is(err, errNoModuleRoot) {
		t.Fatalf("err = %v, want errNoModuleRoot", err)
	}
}

func TestModuleRootFindsShipInWorkspace(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not installed")
	}
	// Under go.work, `go list -m` prints one line per member module; the ship
	// line is deliberately not first here.
	ws := t.TempDir()
	for _, m := range []struct{ dir, module string }{
		{"aaa", "example.com/aaa"},
		{"ship", "github.com/watzon/ship"},
	} {
		dir := filepath.Join(ws, m.dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module "+m.module+"\n\ngo 1.26\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, "go.work"), []byte("go 1.26\n\nuse (\n\t./aaa\n\t./ship\n)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(ws)

	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(ws, "ship"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("moduleRoot() = %q, want %q", got, want)
	}
}

func TestResolveDownloadsReleaseWhenCrossCompileUnavailable(t *testing.T) {
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		t.Skip("local platform already matches target")
	}
	target := Platform{GOOS: "linux", GOARCH: "amd64"}
	setAgentVersion(t, "9.9.9")
	stubExecutable(t, filepath.Join(t.TempDir(), "missing"))
	stubModuleRoot(t, func() (string, error) { return "", errNoModuleRoot })
	want := stubRelease(t, "9.9.9", target, "")

	data, err := Resolve(context.Background(), target, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("resolved bytes do not match the release binary")
	}
}

// The wrong-module trap: cross-compile fails for a reason other than "no
// module root" and resolution must still fall through to the download.
func TestResolveFallsThroughToDownloadWhenCrossCompileFails(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not installed")
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		t.Skip("local platform already matches target")
	}
	target := Platform{GOOS: "linux", GOARCH: "amd64"}
	foreign := t.TempDir()
	if err := os.WriteFile(filepath.Join(foreign, "go.mod"), []byte("module example.com/notship\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setAgentVersion(t, "9.9.9")
	stubExecutable(t, filepath.Join(t.TempDir(), "missing"))
	stubModuleRoot(t, func() (string, error) { return foreign, nil })
	want := stubRelease(t, "9.9.9", target, "")

	data, err := Resolve(context.Background(), target, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("resolved bytes do not match the release binary")
	}
}

func TestResolveReportsEveryAttemptOn404(t *testing.T) {
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		t.Skip("local platform already matches target")
	}
	target := Platform{GOOS: "linux", GOARCH: "amd64"}
	setAgentVersion(t, "9.9.9")
	disableRetryDelays(t)
	stubExecutable(t, filepath.Join(t.TempDir(), "missing"))
	stubModuleRoot(t, func() (string, error) { return "", errNoModuleRoot })
	origHTTPGet := httpGet
	httpGet = func(ctx context.Context, url string) (int, []byte, error) {
		return http.StatusNotFound, nil, nil
	}
	t.Cleanup(func() { httpGet = origHTTPGet })

	_, err := Resolve(context.Background(), target, Options{})
	if err == nil {
		t.Fatal("expected error")
	}
	var resErr *resolveError
	if !errors.As(err, &resErr) {
		t.Fatalf("err type = %T", err)
	}
	msg := err.Error()
	for _, want := range []string{
		"Tried:",
		"local binary",
		"cross-compile",
		"release v9.9.9",
		"HTTP 404",
		"Fix: ship release check v9.9.9",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error message missing %q:\n%s", want, msg)
		}
	}
}

func TestResolveReportsDevBuildFix(t *testing.T) {
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		t.Skip("local platform already matches target")
	}
	setAgentVersion(t, "") // Version() falls back to the dev sentinel under `go test`
	stubExecutable(t, filepath.Join(t.TempDir(), "missing"))
	stubModuleRoot(t, func() (string, error) { return "", errNoModuleRoot })
	origHTTPGet := httpGet
	httpGet = func(ctx context.Context, url string) (int, []byte, error) {
		t.Fatal("dev builds must not attempt release downloads")
		return 0, nil, nil
	}
	t.Cleanup(func() { httpGet = origHTTPGet })

	_, err := Resolve(context.Background(), Platform{GOOS: "linux", GOARCH: "amd64"}, Options{})
	if err == nil || !strings.Contains(err.Error(), "go install github.com/watzon/ship/cmd/ship@latest") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchAssetRetriesTransientFailures(t *testing.T) {
	origDelays := downloadRetryDelays
	downloadRetryDelays = []time.Duration{0, 0}
	t.Cleanup(func() { downloadRetryDelays = origDelays })
	calls := 0
	origHTTPGet := httpGet
	httpGet = func(ctx context.Context, url string) (int, []byte, error) {
		calls++
		if calls < 3 {
			return 0, nil, errors.New("connection reset")
		}
		return http.StatusOK, []byte("payload"), nil
	}
	t.Cleanup(func() { httpGet = origHTTPGet })

	body, err := fetchAsset(context.Background(), "https://example.invalid/asset")
	if err != nil || string(body) != "payload" {
		t.Fatalf("body = %q err = %v", body, err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestDownloadReleaseRejectsChecksumMismatch(t *testing.T) {
	target := Platform{GOOS: "linux", GOARCH: "amd64"}
	setAgentVersion(t, "9.9.9")
	disableRetryDelays(t)
	archive, err := fakeReleaseArchive(fakeELF(elf.EM_X86_64))
	if err != nil {
		t.Fatal(err)
	}
	assetName := releaseAssetName("9.9.9", target)
	manifest := strings.Repeat("0", 64) + "  " + assetName + "\n"
	origHTTPGet := httpGet
	httpGet = func(ctx context.Context, url string) (int, []byte, error) {
		if strings.HasSuffix(url, "/checksums.txt") {
			return http.StatusOK, []byte(manifest), nil
		}
		return http.StatusOK, archive, nil
	}
	t.Cleanup(func() { httpGet = origHTTPGet })

	_, err = downloadRelease(context.Background(), target)
	if !errors.Is(err, errBadChecksum) {
		t.Fatalf("err = %v, want errBadChecksum", err)
	}
}

func TestDownloadReleaseRejectsWrongPlatformBinary(t *testing.T) {
	target := Platform{GOOS: "linux", GOARCH: "amd64"}
	setAgentVersion(t, "9.9.9")
	disableRetryDelays(t)
	archive, err := fakeReleaseArchive(fakeELF(elf.EM_AARCH64)) // arm64 bytes behind an amd64 asset name
	if err != nil {
		t.Fatal(err)
	}
	assetName := releaseAssetName("9.9.9", target)
	sum := sha256.Sum256(archive)
	manifest := hex.EncodeToString(sum[:]) + "  " + assetName + "\n"
	origHTTPGet := httpGet
	httpGet = func(ctx context.Context, url string) (int, []byte, error) {
		if strings.HasSuffix(url, "/checksums.txt") {
			return http.StatusOK, []byte(manifest), nil
		}
		return http.StatusOK, archive, nil
	}
	t.Cleanup(func() { httpGet = origHTTPGet })

	_, err = downloadRelease(context.Background(), target)
	if !errors.Is(err, errWrongPlatform) {
		t.Fatalf("err = %v, want errWrongPlatform", err)
	}
}

func TestChecksumForToleratesManifestPreamble(t *testing.T) {
	target := Platform{GOOS: "linux", GOARCH: "amd64"}
	setAgentVersion(t, "9.9.9")
	stubExecutable(t, filepath.Join(t.TempDir(), "missing"))
	stubModuleRoot(t, func() (string, error) { return "", errNoModuleRoot })
	// Early releases published checksums.txt with bare-filename preamble lines.
	preamble := "ship_9.9.9_linux_amd64.tar.gz\nship_9.9.9_linux_arm64.tar.gz\n"
	want := stubRelease(t, "9.9.9", target, preamble)

	data, err := downloadRelease(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Fatal("resolved bytes do not match the release binary")
	}
}

func TestResolveBinaryPathOverride(t *testing.T) {
	target := Platform{GOOS: "linux", GOARCH: "amd64"}
	path := filepath.Join(t.TempDir(), "ship-linux-amd64")
	if err := os.WriteFile(path, fakeELF(elf.EM_X86_64), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := Resolve(context.Background(), target, Options{BinaryPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := detectPlatform(data); !ok {
		t.Fatal("override did not return the binary")
	}
}

func TestResolveBinaryPathOverrideFailsLoudOnWrongPlatform(t *testing.T) {
	target := Platform{GOOS: "linux", GOARCH: "amd64"}
	path := filepath.Join(t.TempDir(), "ship-linux-arm64")
	if err := os.WriteFile(path, fakeELF(elf.EM_AARCH64), 0o755); err != nil {
		t.Fatal(err)
	}
	origHTTPGet := httpGet
	httpGet = func(ctx context.Context, url string) (int, []byte, error) {
		t.Fatal("an explicit override must never fall through to a download")
		return 0, nil, nil
	}
	t.Cleanup(func() { httpGet = origHTTPGet })

	_, err := Resolve(context.Background(), target, Options{BinaryPath: path})
	if !errors.Is(err, errWrongPlatform) {
		t.Fatalf("err = %v, want errWrongPlatform", err)
	}
}

func TestResolveReleaseDirOverride(t *testing.T) {
	target := Platform{GOOS: "linux", GOARCH: "amd64"}
	setAgentVersion(t, "9.9.9")
	dir := t.TempDir()
	want := fakeELF(elf.EM_X86_64)
	archive, err := fakeReleaseArchive(want)
	if err != nil {
		t.Fatal(err)
	}
	assetName := releaseAssetName("9.9.9", target)
	if err := os.WriteFile(filepath.Join(dir, assetName), archive, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive)
	manifest := hex.EncodeToString(sum[:]) + "  " + assetName + "\n"
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	data, err := Resolve(context.Background(), target, Options{ReleaseDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Fatal("resolved bytes do not match the mirrored binary")
	}
}

func TestResolveReleaseDirRequiresManifest(t *testing.T) {
	target := Platform{GOOS: "linux", GOARCH: "amd64"}
	setAgentVersion(t, "9.9.9")
	dir := t.TempDir()
	archive, err := fakeReleaseArchive(fakeELF(elf.EM_X86_64))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, releaseAssetName("9.9.9", target)), archive, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = Resolve(context.Background(), target, Options{ReleaseDir: dir})
	if err == nil || !strings.Contains(err.Error(), "checksums.txt") {
		t.Fatalf("err = %v", err)
	}
}

func TestExtractShipBinaryMissingEntry(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "README", Mode: 0o644, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := extractShipBinary(buf.Bytes()); err == nil {
		t.Fatal("expected missing entry error")
	}
}

func TestExtractShipBinaryRefusesOversizedEntry(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "ship", Mode: 0o755, Size: maxBinaryBytes + 1}); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	if _, err := extractShipBinary(buf.Bytes()); err == nil || !strings.Contains(err.Error(), "refusing to extract") {
		t.Fatalf("err = %v", err)
	}
}

func fakeReleaseArchive(binary []byte) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "ship",
		Mode: 0o755,
		Size: int64(len(binary)),
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(binary); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
