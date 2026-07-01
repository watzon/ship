package transport

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const DefaultCommandTimeout = 10 * time.Minute
const knownHostsEnv = "SHIP_SSH_KNOWN_HOSTS_FILE"

type SSH struct {
	User           string
	Host           string
	DryRun         bool
	Timeout        time.Duration
	KnownHostsFile string
}

func (s SSH) Target() string {
	if s.User == "" {
		return s.Host
	}
	return s.User + "@" + s.Host
}

func (s SSH) Run(ctx context.Context, command string) (string, error) {
	if s.DryRun {
		return "ssh " + s.Target() + " " + command, nil
	}
	ctx, cancel := s.commandContext(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", s.args(command)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if ctx.Err() != nil {
			msg = ctx.Err().Error()
		}
		return "", fmt.Errorf("ssh %s failed: %s", s.Target(), msg)
	}
	return string(out), nil
}

func (s SSH) RunWithStdin(ctx context.Context, command, stdin string) (string, error) {
	if s.DryRun {
		return fmt.Sprintf("ssh %s %s <stdin:%d bytes>", s.Target(), command, len(stdin)), nil
	}
	ctx, cancel := s.commandContext(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", s.args(command)...)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if ctx.Err() != nil {
			msg = ctx.Err().Error()
		}
		return "", fmt.Errorf("ssh %s failed: %s", s.Target(), msg)
	}
	return string(out), nil
}

func (s SSH) commandContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultCommandTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func (s SSH) args(command string) []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=15",
	}
	if knownHostsFile := s.knownHostsFile(); knownHostsFile != "" {
		args = append(args, "-o", "UserKnownHostsFile="+knownHostsFile)
	}
	args = append(args, s.Target(), command)
	return args
}

func (s SSH) knownHostsFile() string {
	if strings.TrimSpace(s.KnownHostsFile) != "" {
		return strings.TrimSpace(s.KnownHostsFile)
	}
	return strings.TrimSpace(os.Getenv(knownHostsEnv))
}

func Available(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "ssh", "-V")
	return cmd.Run()
}
