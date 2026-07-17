package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/watzon/ship/internal/config"
	secretspkg "github.com/watzon/ship/internal/secrets"
	"github.com/watzon/ship/internal/state"
)

func TestSecretsRenderDryRunRedactsValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(secretDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	secretValue := "postgres://user:pass@example/db"
	t.Setenv("SHIP_TEST_DATABASE_URL", secretValue)

	var out bytes.Buffer
	cmd := secretsCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"render", "production", "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "/var/lib/ship/secrets/production/service-web.env") {
		t.Fatalf("render output missing remote path:\n%s", text)
	}
	if !strings.Contains(text, "SHIP_TEST_DATABASE_URL=<redacted:") {
		t.Fatalf("render output missing redacted secret:\n%s", text)
	}
	if strings.Contains(text, secretValue) {
		t.Fatalf("render output leaked secret value:\n%s", text)
	}
}

func TestSecretsInitSetListExportWorkflow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	identityFile := filepath.Join(dir, "ship-secrets.identity")
	if err := os.WriteFile(identityFile, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := &options{configPath: path, secretsIdentityFile: identityFile}
	runSecrets := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		cmd := secretsCmd(opts)
		cmd.SetOut(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("ship secrets %s: %v\n%s", strings.Join(args, " "), err, out.String())
		}
		return out.String()
	}

	initOut := runSecrets("init", "production", "--recipient", identity.Recipient().String())
	if !strings.Contains(initOut, filepath.Join(dir, ".ship", "secrets", "production.age")) {
		t.Fatalf("init output = %q", initOut)
	}
	t.Setenv("DATABASE_URL", "postgres://user:pass@example/db")
	runSecrets("set", "production", "DATABASE_URL")
	runSecrets("set", "production", "SESSION_SECRET", "--value", "keyboard-cat")

	listOut := runSecrets("list", "production")
	if listOut != "DATABASE_URL\nSESSION_SECRET\n" {
		t.Fatalf("list output = %q", listOut)
	}
	exportOut := runSecrets("export", "production")
	for _, needle := range []string{"DATABASE_URL=postgres://user:pass@example/db", "SESSION_SECRET=keyboard-cat"} {
		if !strings.Contains(exportOut, needle) {
			t.Fatalf("export output missing %q:\n%s", needle, exportOut)
		}
	}
	redactedOut := runSecrets("export", "production", "--redacted")
	if strings.Contains(redactedOut, "postgres://") || strings.Contains(redactedOut, "keyboard-cat") {
		t.Fatalf("redacted export leaked secret values:\n%s", redactedOut)
	}
	for _, needle := range []string{"DATABASE_URL=<redacted:", "SESSION_SECRET=<redacted:"} {
		if !strings.Contains(redactedOut, needle) {
			t.Fatalf("redacted export missing %q:\n%s", needle, redactedOut)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".ship", "secrets", "production.recipients")); err != nil {
		t.Fatalf("recipients file missing: %v", err)
	}
}

func TestSecretsDiffReportsDriftWithoutValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(secretDeployConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	secretValue := "new-secret"
	t.Setenv("SHIP_TEST_DATABASE_URL", secretValue)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{
		ID:          "release-1",
		Environment: "production",
		Images:      map[string]string{"web": "registry.local/acme/web@sha256:" + strings.Repeat("1", 64)},
		SecretDigests: map[string]string{
			"service-web:SHIP_TEST_DATABASE_URL": secretspkg.Digest("old-secret"),
			"service-web:OLD_SECRET":             secretspkg.Digest("old"),
		},
		CreatedAt: time.Unix(30, 0),
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := secretsCmd(&options{configPath: path})
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"diff", "production"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "secret drift detected") {
		t.Fatalf("expected drift error, got %v", err)
	}
	text := out.String()
	for _, needle := range []string{"changed service-web:SHIP_TEST_DATABASE_URL", "extra service-web:OLD_SECRET"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("diff output missing %q:\n%s", needle, text)
		}
	}
	if strings.Contains(text, secretValue) || strings.Contains(text, "old-secret") {
		t.Fatalf("diff output leaked secret value:\n%s", text)
	}
}
