package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/watzon/ship/internal/agent"
	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/scheduler"
	"github.com/watzon/ship/internal/state"
)

func TestExecCommandStringPreservesArgv(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"single arg stays a shell string", []string{"cd /app && ls"}, "cd /app && ls"},
		{"plain words are unchanged", []string{"bin/rails", "db:migrate"}, "bin/rails db:migrate"},
		{"spaces", []string{"echo", "a b"}, "echo 'a b'"},
		{"double quotes", []string{"sh", "-c", `echo "a b"`}, `sh -c 'echo "a b"'`},
		{"single quote", []string{"echo", "it's"}, `echo 'it'\''s'`},
		{"dollar", []string{"echo", "$HOME"}, `echo '$HOME'`},
		{"empty string", []string{"echo", ""}, "echo ''"},
		{"parens and semicolon", []string{"php", "artisan", "tinker", "--execute", `echo strlen("a b");`}, `php artisan tinker --execute 'echo strlen("a b");'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := execCommandString(tc.args); got != tc.want {
				t.Fatalf("execCommandString(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// TestExecCommandStringRoundTripsThroughShell runs the produced string through
// a real POSIX shell and checks that the command receives the original argv.
func TestExecCommandStringRoundTripsThroughShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	argv := []string{"a b", `say "hi"`, "it's", "$HOME", "`id`", "", "x;y", "*", "new\nline"}
	command := execCommandString(append([]string{"printf", "%s\\0"}, argv...))
	out, err := exec.Command("sh", "-c", command).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(got) != len(argv) {
		t.Fatalf("argv = %q, want %q", got, argv)
	}
	for i := range argv {
		if got[i] != argv[i] {
			t.Fatalf("arg %d = %q, want %q", i, got[i], argv[i])
		}
	}
}

func TestExecPassesQuotedArgsToAgent(t *testing.T) {
	path, _ := execTestConfig(t)
	var calls []agent.ExecContainerParams
	installDeployHooks(t, &recordingDeployDocker{events: &[]string{}}, func(host scheduler.Host) deployAgent {
		return deployAgentFunc(func(ctx context.Context, method string, params any, out any) error {
			if method == "exec_container" {
				calls = append(calls, params.(agent.ExecContainerParams))
			}
			return nil
		})
	})
	cmd := execServiceCmd(&options{configPath: path})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"production", "web", "--", "sh", "-c", `echo "a b" $HOME`, ""})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	want := `sh -c 'echo "a b" $HOME' ''`
	if len(calls) != 1 || calls[0].Command != want {
		t.Fatalf("calls = %+v, want command %q", calls, want)
	}
}

func TestExecFailureShowsOutputAndPropagatesExitCode(t *testing.T) {
	path, _ := execTestConfig(t)
	installDeployHooks(t, &recordingDeployDocker{events: &[]string{}}, func(host scheduler.Host) deployAgent {
		return deployAgentFunc(func(ctx context.Context, method string, params any, out any) error {
			if method != "exec_container" {
				return nil
			}
			*(out.(*agent.CommandResult)) = agent.CommandResult{Output: "to stdout", Stderr: "to stderr", ExitCode: 3}
			return agent.RemoteError{Code: agent.ErrorCommandFailed, Message: "exec failed: docker failed: exit status 3"}
		})
	})
	var stdout, stderr bytes.Buffer
	cmd := execServiceCmd(&options{configPath: path})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"production", "web", "--", "false"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error")
	}
	if got := ExitCode(err); got != 3 {
		t.Fatalf("ExitCode = %d, want 3 (err=%v)", got, err)
	}
	if !strings.Contains(stdout.String(), "to stdout") || !strings.Contains(stderr.String(), "to stderr") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestExecTransportErrorKeepsExitCodeOne(t *testing.T) {
	path, _ := execTestConfig(t)
	installDeployHooks(t, &recordingDeployDocker{events: &[]string{}}, func(host scheduler.Host) deployAgent {
		return deployAgentFunc(func(ctx context.Context, method string, params any, out any) error {
			if method != "exec_container" {
				return nil
			}
			return errors.New("ssh: connection refused")
		})
	})
	cmd := execServiceCmd(&options{configPath: path})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"production", "web", "--", "ls"})
	err := cmd.Execute()
	if err == nil || ExitCode(err) != 1 {
		t.Fatalf("err = %v, exit = %d", err, ExitCode(err))
	}
}

func execTestConfig(t *testing.T) (string, state.Store) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultConfigFile)
	if err := os.WriteFile(path, []byte(deployBuildConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store := state.NewStore(filepath.Join(dir, config.LocalStateDir))
	if err := store.SaveRelease(state.Release{ID: "release-1", Environment: "production", Images: map[string]string{"web": "image"}, CreatedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	return path, store
}
