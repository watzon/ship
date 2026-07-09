package ship_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var stableVersionRE = regexp.MustCompile(`\bv?([0-9]+)\.([0-9]+)\.([0-9]+)\b`)

func TestPinnedReleaseDocsMatchLatestStableTag(t *testing.T) {
	tag := docsReleaseTag(t)
	wantVersion := strings.TrimPrefix(tag, "v")
	files := []string{
		"README.md",
		"docs/quickstart.md",
		"docs/airgap.md",
		"skills/ship/SKILL.md",
	}

	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Clean(file))
			if err != nil {
				t.Fatal(err)
			}
			matches := stableVersionRE.FindAll(body, -1)
			if len(matches) == 0 {
				t.Fatalf("no pinned stable release version found; expected %s", tag)
			}
			for _, match := range matches {
				got := string(match)
				if strings.HasPrefix(got, "v") {
					if got != tag {
						t.Errorf("pinned release %s should be %s", got, tag)
					}
					continue
				}
				if got != wantVersion {
					t.Errorf("pinned release asset version %s should be %s", got, wantVersion)
				}
			}
		})
	}
}

func docsReleaseTag(t *testing.T) string {
	t.Helper()
	if tag := os.Getenv("SHIP_DOCS_VERSION_TAG"); tag != "" {
		if !isStableTag(tag) {
			t.Fatalf("SHIP_DOCS_VERSION_TAG=%q is not an exact vX.Y.Z stable tag", tag)
		}
		return tag
	}

	cmd := exec.Command("git", "tag", "--list", "v*")
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("cannot list Git tags and SHIP_DOCS_VERSION_TAG is unset: %v", err)
	}
	var latest string
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		tag := string(bytes.TrimSpace(line))
		if !isStableTag(tag) {
			continue
		}
		if latest == "" || compareStableTags(tag, latest) > 0 {
			latest = tag
		}
	}
	if latest == "" {
		t.Skip("no local stable Git tags and SHIP_DOCS_VERSION_TAG is unset")
	}
	return latest
}

func isStableTag(tag string) bool {
	if !strings.HasPrefix(tag, "v") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(tag, "v"), ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return true
}

func compareStableTags(a, b string) int {
	ap := stableTagParts(a)
	bp := stableTagParts(b)
	for i := range ap {
		if ap[i] < bp[i] {
			return -1
		}
		if ap[i] > bp[i] {
			return 1
		}
	}
	return 0
}

func stableTagParts(tag string) [3]int {
	parts := strings.Split(strings.TrimPrefix(tag, "v"), ".")
	var nums [3]int
	for i, part := range parts {
		nums[i], _ = strconv.Atoi(part)
	}
	return nums
}
