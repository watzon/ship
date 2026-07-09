package shipbinary

import (
	"context"
	"os"
	"testing"
)

// TestLiveReleaseDownload exercises the real GitHub release download path —
// fetch, checksum verification against the published checksums.txt, and
// platform verification — for a version known to have assets. Gated because
// it needs network access and downloads a full release tarball.
//
//	SHIP_LIVE_DOWNLOAD_TEST=1 go test ./internal/shipbinary/ -run TestLiveReleaseDownload -v
func TestLiveReleaseDownload(t *testing.T) {
	if os.Getenv("SHIP_LIVE_DOWNLOAD_TEST") != "1" {
		t.Skip("set SHIP_LIVE_DOWNLOAD_TEST=1 to run the live release download test")
	}
	setAgentVersion(t, "0.4.6")
	target := Platform{GOOS: "linux", GOARCH: "amd64"}

	binary, err := downloadRelease(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	platform, ok := detectPlatform(binary)
	if !ok || !platform.matches(target) {
		t.Fatalf("downloaded binary platform = %+v ok=%v", platform, ok)
	}
	t.Logf("downloaded, checksum-verified, and platform-verified %d bytes", len(binary))
}
