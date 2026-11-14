package agent

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRunOneOffContainerFailureReturnsOutputAndExitCode(t *testing.T) {
	server := testServer(t)
	server.CommandRunner = func(ctx context.Context, name string, args ...string) (string, error) {
		return "", &CommandError{Name: name, Stdout: "Migrating: 2026_users\n", Stderr: "SQLSTATE[42S01]: table exists\n", ExitCode: 1, Message: "docker failed: SQLSTATE[42S01]: table exists"}
	}
	resp := server.Handle(context.Background(), request(t, "oneoff-fail", "run_oneoff_container", RunOneOffContainerParams{Name: "ship_release", Image: "demo/web@sha256:1", Command: "php artisan migrate --force"}))
	if resp.OK || resp.ErrorCode != ErrorDocker {
		t.Fatalf("response = %+v", resp)
	}
	var result CommandResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.Output != "Migrating: 2026_users" || result.Stderr != "SQLSTATE[42S01]: table exists" || result.ExitCode != 1 {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunOneOffContainerPlainFailureHasNoResult(t *testing.T) {
	server := testServer(t)
	server.CommandRunner = func(ctx context.Context, name string, args ...string) (string, error) {
		return "", context.DeadlineExceeded
	}
	resp := server.Handle(context.Background(), request(t, "oneoff-timeout", "run_oneoff_container", RunOneOffContainerParams{Name: "ship_release", Image: "demo/web@sha256:1", Command: "sleep 9"}))
	if resp.OK || len(resp.Result) != 0 {
		t.Fatalf("response = %+v", resp)
	}
}
