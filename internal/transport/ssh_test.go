package transport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSSHRunUsesCommandTimeout(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "ssh"), "#!/bin/sh\nsleep 5\n")
	t.Setenv("PATH", dir)

	_, err := (SSH{User: "root", Host: "example.test", Timeout: 10 * time.Millisecond}).Run(context.Background(), "true")
	if err == nil {
		t.Fatal("expected timeout")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("error = %v", err)
	}
}

func TestSSHRunWithStdinUsesCommandTimeout(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "ssh"), "#!/bin/sh\ncat >/dev/null\nsleep 5\n")
	t.Setenv("PATH", dir)

	_, err := (SSH{User: "root", Host: "example.test", Timeout: 10 * time.Millisecond}).RunWithStdin(context.Background(), "cat", "payload")
	if err == nil {
		t.Fatal("expected timeout")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("error = %v", err)
	}
}

func TestSSHUsesConfiguredKnownHostsFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	writeExecutable(t, filepath.Join(dir, "ssh"), "#!/bin/sh\nprintf '%s\\n' \"$@\" >"+shellQuote(logPath)+"\n")
	t.Setenv("PATH", dir)

	knownHostsPath := filepath.Join(dir, "known_hosts")
	if _, err := (SSH{User: "root", Host: "example.test", KnownHostsFile: knownHostsPath}).Run(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), "UserKnownHostsFile="+knownHostsPath) {
		t.Fatalf("ssh args missing known hosts file:\n%s", logged)
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
