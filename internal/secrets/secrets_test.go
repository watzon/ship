package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"filippo.io/age"
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

func TestRequiredScopesBuildsPerServiceAndAccessoryScopes(t *testing.T) {
	cfg := &config.Config{
		Secrets: []string{"ROOT_ONLY", "SHARED"},
		Services: map[string]config.Service{
			"web": {
				Secrets: []string{"SHARED", "WEB_KEY", "WEB_KEY", " "},
			},
			"worker": {},
		},
		Accessories: map[string]config.Accessory{
			"db": {
				Secrets: []string{"DB_PASS"},
			},
		},
	}

	scopes, err := RequiredScopes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"accessory-db": {"DB_PASS"},
		"service-web":  {"SHARED", "WEB_KEY"},
	}
	if !reflect.DeepEqual(scopes, want) {
		t.Fatalf("scopes = %#v, want %#v", scopes, want)
	}
	for scope, names := range scopes {
		for _, name := range names {
			if name == "ROOT_ONLY" {
				t.Fatalf("root-only secret leaked into %s scope: %#v", scope, names)
			}
		}
	}

	_, err = RequiredScopes(&config.Config{
		Services: map[string]config.Service{
			"web": {
				Secrets: []string{"1BAD"},
			},
		},
	})
	if err == nil {
		t.Fatal("expected invalid secret name error")
	}
}

func TestRenderForEnvUsesEncryptedStoreDotenvAndEnvPrecedence(t *testing.T) {
	dir := t.TempDir()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	identityFile := filepath.Join(dir, "identity.txt")
	if err := os.WriteFile(identityFile, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(dir, "override.env")
	if err := os.WriteFile(envFile, []byte("SHIP_TEST_DOTENV=from-dotenv\nSHIP_TEST_ENV=from-dotenv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := SourceOptions{
		EnvName:      "production",
		StateDir:     dir,
		IdentityFile: identityFile,
		EnvFiles:     []string{envFile},
	}
	if err := InitStore(opts, identity.Recipient().String()); err != nil {
		t.Fatal(err)
	}
	if err := SetStoredSecret(opts, "", "SHIP_TEST_STORE", "from-store"); err != nil {
		t.Fatal(err)
	}
	if err := SetStoredSecret(opts, "", "SHIP_TEST_DOTENV", "from-store"); err != nil {
		t.Fatal(err)
	}
	if err := SetStoredSecret(opts, "", "SHIP_TEST_ENV", "from-store"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHIP_TEST_ENV", "from-env")
	cfg := &config.Config{Secrets: []string{"SHIP_TEST_STORE", "SHIP_TEST_DOTENV", "SHIP_TEST_ENV"}}
	rendered, err := RenderForEnv(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		"SHIP_TEST_STORE=from-store",
		"SHIP_TEST_DOTENV=from-dotenv",
		"SHIP_TEST_ENV=from-env",
	} {
		if !strings.Contains(rendered.Content, needle) {
			t.Fatalf("rendered content missing %q:\n%s", needle, rendered.Content)
		}
	}
	if !reflect.DeepEqual(rendered.ProcessEnvStoreOverrides, []string{"SHIP_TEST_ENV"}) {
		t.Fatalf("process env store overrides = %#v", rendered.ProcessEnvStoreOverrides)
	}

	opts.SkipProcessEnv = true
	rendered, err = RenderForEnv(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered.Content, "SHIP_TEST_ENV=from-dotenv") {
		t.Fatalf("rendered content did not retain explicit dotenv value:\n%s", rendered.Content)
	}
	if len(rendered.ProcessEnvStoreOverrides) != 0 {
		t.Fatalf("disabled process env reported overrides: %#v", rendered.ProcessEnvStoreOverrides)
	}
}

func TestRenderScopedForEnvRendersAndFiltersScopes(t *testing.T) {
	dir := t.TempDir()
	identity, identityFile := newTestIdentityFile(t, dir, "identity.txt")
	opts := SourceOptions{
		EnvName:      "production",
		StateDir:     dir,
		IdentityFile: identityFile,
	}
	if err := InitStore(opts, identity.Recipient().String()); err != nil {
		t.Fatal(err)
	}
	if err := SetStoredSecret(opts, "", "WEB_KEY", "web-value"); err != nil {
		t.Fatal(err)
	}
	if err := SetStoredSecret(opts, "", "DB_PASS", "db-value"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Services: map[string]config.Service{
			"web": {
				Secrets: []string{"WEB_KEY"},
			},
		},
		Accessories: map[string]config.Accessory{
			"db": {
				Secrets: []string{"DB_PASS"},
			},
		},
	}

	rendered, err := RenderScopedForEnv(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rendered.Scopes["service-web"].Content, "WEB_KEY=web-value\n"; got != want {
		t.Fatalf("service-web content = %q, want %q", got, want)
	}
	if strings.Contains(rendered.Scopes["accessory-db"].Content, "WEB_KEY") {
		t.Fatalf("accessory-db content leaked WEB_KEY: %q", rendered.Scopes["accessory-db"].Content)
	}
	wantDigests := map[string]string{
		"accessory-db:DB_PASS": Digest("db-value"),
		"service-web:WEB_KEY":  Digest("web-value"),
	}
	if !reflect.DeepEqual(rendered.Digests, wantDigests) {
		t.Fatalf("digests = %#v, want %#v", rendered.Digests, wantDigests)
	}
	assertChecksPresent(t, rendered.Checks, []string{"DB_PASS", "WEB_KEY"})
	if strings.Contains(rendered.Scopes["service-web"].Redacted, "web-value") {
		t.Fatalf("redacted service-web output leaked value: %q", rendered.Scopes["service-web"].Redacted)
	}

	filtered, err := RenderScopedForEnv(cfg, opts, "service-web")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Scopes) != 1 {
		t.Fatalf("filtered scopes = %#v, want only service-web", filtered.Scopes)
	}
	if _, ok := filtered.Scopes["service-web"]; !ok {
		t.Fatalf("filtered scopes missing service-web: %#v", filtered.Scopes)
	}
}

func TestUnsetStoredSecretRemovesValue(t *testing.T) {
	dir := t.TempDir()
	identity, identityFile := newTestIdentityFile(t, dir, "identity.txt")
	opts := SourceOptions{
		EnvName:      "production",
		StateDir:     dir,
		IdentityFile: identityFile,
	}
	if err := InitStore(opts, identity.Recipient().String()); err != nil {
		t.Fatal(err)
	}
	if err := SetStoredSecret(opts, "", "SHIP_TEST_NAME", "stored-value"); err != nil {
		t.Fatal(err)
	}
	if err := UnsetStoredSecret(opts, "", "SHIP_TEST_NAME"); err != nil {
		t.Fatal(err)
	}
	values, err := ReadStore(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := values["SHIP_TEST_NAME"]; ok {
		t.Fatalf("SHIP_TEST_NAME still present after unset: %#v", values)
	}
	if err := UnsetStoredSecret(opts, "", "SHIP_TEST_MISSING"); err != nil {
		t.Fatalf("unset missing secret returned error: %v", err)
	}
}

func TestWriteStoreSupportsMultipleRecipients(t *testing.T) {
	dir := t.TempDir()
	identity1, identityFile1 := newTestIdentityFile(t, dir, "identity-1.txt")
	identity2, identityFile2 := newTestIdentityFile(t, dir, "identity-2.txt")
	opts := SourceOptions{
		EnvName:      "production",
		StateDir:     dir,
		IdentityFile: identityFile1,
	}
	want := map[string]string{
		"SHIP_TEST_ALPHA": "one",
		"SHIP_TEST_BETA":  "two",
	}
	if err := WriteStoreWithRecipients(opts, want, []age.Recipient{identity1.Recipient(), identity2.Recipient()}); err != nil {
		t.Fatal(err)
	}

	got1, err := ReadStore(opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.IdentityFile = identityFile2
	got2, err := ReadStore(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got1, want) {
		t.Fatalf("identity 1 values = %#v, want %#v", got1, want)
	}
	if !reflect.DeepEqual(got2, want) {
		t.Fatalf("identity 2 values = %#v, want %#v", got2, want)
	}
}

func newTestIdentityFile(t *testing.T, dir, name string) (*age.X25519Identity, string) {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	identityFile := filepath.Join(dir, name)
	if err := os.WriteFile(identityFile, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return identity, identityFile
}

func assertChecksPresent(t *testing.T, checks []Check, names []string) {
	t.Helper()
	got := map[string]bool{}
	for _, check := range checks {
		got[check.Name] = check.Present
	}
	want := map[string]bool{}
	for _, name := range names {
		want[name] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checks = %#v, want present names %#v", checks, names)
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
