package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/watzon/ship/internal/agent"
)

// ExitError carries the exit code that ship should terminate with, such as the
// exit code of a failed `ship exec` command.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode returns the process exit code for an error returned by Execute.
func ExitCode(err error) int {
	var exitErr *ExitError
	if errors.As(err, &exitErr) && exitErr.Code > 0 {
		return exitErr.Code
	}
	return 1
}

// execCommandString turns the arguments after `--` into the single string the
// agent runs with `sh -lc`. One argument is kept as a shell string (so
// `-- 'cd /app && ls'` still works); several arguments are treated as argv and
// each one is POSIX-quoted so it reaches the command unchanged.
func execCommandString(args []string) string {
	if len(args) == 1 {
		return strings.TrimSpace(args[0])
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = posixQuoteArg(arg)
	}
	return strings.Join(quoted, " ")
}

// posixQuoteArg quotes value for a POSIX shell, leaving plain words as they are.
func posixQuoteArg(value string) string {
	if value == "" {
		return "''"
	}
	safe := true
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("_-./:=@%+,", r):
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// execFailure reports the output of a command that ran and exited non-zero
// and returns an ExitError with the remote exit code. It returns false when err
// is not a failed command (for example a transport error).
func execFailure(out, errOut io.Writer, header string, err error, result agent.CommandResult, jsonOutput bool) (error, bool) {
	var remote agent.RemoteError
	if !errors.As(err, &remote) || remote.Code != agent.ErrorCommandFailed || result.ExitCode == 0 {
		return nil, false
	}
	if !jsonOutput {
		fmt.Fprintf(out, "%s\n%s\n", header, result.Output)
		if result.Stderr != "" {
			fmt.Fprintln(errOut, result.Stderr)
		}
	}
	return &ExitError{Code: result.ExitCode, Err: err}, true
}

const execLongHelp = `Run a command inside a deployed container.

Arguments after -- are passed to the command unchanged: each one is shell-quoted,
so spaces, quotes and $ survive. A single argument is treated as a shell string,
so 'cd /app && ls' works. If the command fails, its stdout and stderr are printed
and ship exits with its exit code; on success only stdout is shown.`
