package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/watzon/ship/internal/transport"
)

func TestClientCallSendsRPCPayloadOverSSHStdin(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	stdinPath := filepath.Join(dir, "stdin")
	sshPath := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(argsPath) + "\ncat > " + shellQuote(stdinPath) + "\nprintf '{\"id\":\"req-test\",\"ok\":true}\\n'\n"
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	secret := "literal-secret-value"
	client := Client{
		SSH: transport.SSH{User: "deploy", Host: "example.com"},
		NewRequestID: func() string {
			return "req-test"
		},
	}
	err := client.Call(context.Background(), "write_file", WriteFileParams{
		Path:    "/var/lib/ship/secrets/production/service-web.env",
		Content: "TOKEN=" + secret + "\n",
		Mode:    0o600,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), secret) {
		t.Fatalf("ssh arguments leaked secret: %s", args)
	}
	if !strings.Contains(string(args), "agent rpc") {
		t.Fatalf("ssh arguments missing agent rpc command: %s", args)
	}

	stdin, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stdin), secret) {
		t.Fatalf("ssh stdin missing secret payload: %s", stdin)
	}
	var req Request
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(stdin))), &req); err != nil {
		t.Fatal(err)
	}
	if req.Method != "write_file" {
		t.Fatalf("method = %q", req.Method)
	}
	var params WriteFileParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.Content != "TOKEN="+secret+"\n" {
		t.Fatalf("content = %q", params.Content)
	}
}
