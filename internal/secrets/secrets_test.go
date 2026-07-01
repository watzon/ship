package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/watzon/ship/internal/config"
)

func TestVerifyReportsMissingSecrets(t *testing.T) {
	restoreEnv(t, "SHIP_TEST_SECRET_MISSING")
	if err := os.Unsetenv("SHIP_TEST_SECRET_MISSING"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Secrets: []string{"SHIP_TEST_SECRET_MISSING"}}
	checks, err := Verify(cfg)
	if err == nil {
		t.Fatal("expected error")
	}
	if len(checks) != 1 || checks[0].Present {
		t.Fatalf("unexpected checks: %+v", checks)
	}
}

func TestVerifyReportsDigestWithoutValue(t *testing.T) {
	t.Setenv("SHIP_TEST_SECRET_PRESENT", "super-secret-value")
	cfg := &config.Config{Secrets: []string{"SHIP_TEST_SECRET_PRESENT"}}

	checks, err := Verify(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 || !checks[0].Present {
		t.Fatalf("unexpected checks: %+v", checks)
	}
	sum := sha256.Sum256([]byte("super-secret-value"))
	want := hex.EncodeToString(sum[:])[:12]
	if checks[0].Digest != want {
		t.Fatalf("digest = %q, want %q", checks[0].Digest, want)
	}
	if checks[0].Digest == "super-secret-value" {
		t.Fatal("digest leaked the secret value")
	}
}

func TestRenderEnvFileSortsRedactsAndDoesNotLeakValue(t *testing.T) {
	t.Setenv("SHIP_TEST_RENDER_B", "value with spaces # and = signs")
	t.Setenv("SHIP_TEST_RENDER_A", "simple")
	cfg := &config.Config{Secrets: []string{"SHIP_TEST_RENDER_B", "SHIP_TEST_RENDER_A", "SHIP_TEST_RENDER_A"}}

	rendered, err := RenderEnvFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	wantContent := "SHIP_TEST_RENDER_A=simple\nSHIP_TEST_RENDER_B=value with spaces # and = signs\n"
	if rendered.Content != wantContent {
		t.Fatalf("content = %q, want %q", rendered.Content, wantContent)
	}
	if strings.Contains(rendered.Redacted, "simple") || strings.Contains(rendered.Redacted, "value with spaces") {
		t.Fatalf("redacted output leaked value: %q", rendered.Redacted)
	}
	for _, needle := range []string{"SHIP_TEST_RENDER_A=<redacted:", "SHIP_TEST_RENDER_B=<redacted:"} {
		if !strings.Contains(rendered.Redacted, needle) {
			t.Fatalf("redacted output missing %q: %q", needle, rendered.Redacted)
		}
	}
}

func TestRenderEnvFileRejectsMultilineSecrets(t *testing.T) {
	t.Setenv("SHIP_TEST_MULTILINE", "line one\nline two")
	cfg := &config.Config{Secrets: []string{"SHIP_TEST_MULTILINE"}}

	_, err := RenderEnvFile(cfg)
	if err == nil || !strings.Contains(err.Error(), "contains a newline") {
		t.Fatalf("expected newline error, got %v", err)
	}
}

func TestDiffClassifiesMissingChangedAndExtraDigests(t *testing.T) {
	diff := Diff(
		map[string]string{
			"CHANGED": "local",
			"NEW":     "local",
			"SAME":    "same",
		},
		map[string]string{
			"CHANGED": "release",
			"OLD":     "release",
			"SAME":    "same",
		},
	)
	if strings.Join(diff.Missing, ",") != "NEW" {
		t.Fatalf("missing = %#v", diff.Missing)
	}
	if strings.Join(diff.Changed, ",") != "CHANGED" {
		t.Fatalf("changed = %#v", diff.Changed)
	}
	if strings.Join(diff.Extra, ",") != "OLD" {
		t.Fatalf("extra = %#v", diff.Extra)
	}
}

func restoreEnv(t *testing.T, name string) {
	t.Helper()
	value, ok := os.LookupEnv(name)
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(name, value)
			return
		}
		_ = os.Unsetenv(name)
	})
}
