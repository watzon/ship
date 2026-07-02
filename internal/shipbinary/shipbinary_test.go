package shipbinary

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
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

func TestResolveUsesLocalExecutableWhenPlatformsMatch(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not installed")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "ship")
	cmd := exec.Command("go", "build", "-o", binary, filepath.Join("..", "..", "cmd", "ship"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build local ship: %v\n%s", err, out)
	}
	orig := osExecutable
	osExecutable = func() (string, error) { return binary, nil }
	t.Cleanup(func() { osExecutable = orig })

	data, err := Resolve(context.Background(), LocalPlatform())
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("expected binary bytes")
	}
}

func TestResolveCrossCompilesForRemoteLinuxAMD64(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not installed")
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		t.Skip("local platform already matches target")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "ship")
	cmd := exec.Command("go", "build", "-o", binary, filepath.Join("..", "..", "cmd", "ship"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build local ship: %v\n%s", err, out)
	}
	orig := osExecutable
	osExecutable = func() (string, error) { return binary, nil }
	t.Cleanup(func() { osExecutable = orig })

	data, err := Resolve(context.Background(), Platform{GOOS: "linux", GOARCH: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	platform, ok, err := PlatformOfExecutable(writeTempBinary(t, data))
	if err != nil || !ok {
		t.Fatalf("platform parse failed: ok=%v err=%v", ok, err)
	}
	if platform.GOOS != "linux" || platform.GOARCH != "amd64" {
		t.Fatalf("unexpected cross-compiled platform: %+v", platform)
	}
}

func TestResolveDownloadsReleaseWhenCrossCompileUnavailable(t *testing.T) {
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		t.Skip("local platform already matches target")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "ship")
	if err := os.WriteFile(binary, []byte("local"), 0o755); err != nil {
		t.Fatal(err)
	}
	archive, err := fakeReleaseArchive([]byte("remote-ship-binary"))
	if err != nil {
		t.Fatal(err)
	}
	origExecutable := osExecutable
	origModuleRoot := moduleRootFunc
	origHTTPGet := httpGet
	osExecutable = func() (string, error) { return binary, nil }
	moduleRootFunc = func() (string, error) { return "", errNoModuleRoot }
	httpGet = func(req httpRequest) ([]byte, error) { return archive, nil }
	t.Cleanup(func() {
		osExecutable = origExecutable
		moduleRootFunc = origModuleRoot
		httpGet = origHTTPGet
	})

	data, err := Resolve(context.Background(), Platform{GOOS: "linux", GOARCH: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "remote-ship-binary" {
		t.Fatalf("binary = %q", string(data))
	}
}

func writeTempBinary(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ship")
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
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

func TestNormalizeGOARCHRejectsUnknown(t *testing.T) {
	if _, err := normalizeGOARCH("riscv64"); err == nil {
		t.Fatal("expected error")
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
