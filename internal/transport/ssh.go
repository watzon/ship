package transport

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
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
	Port           int
	IdentityFile   string
	KnownHostsFile string
	JumpHost       string
	Options        map[string]string
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

// CopyTo streams the output of readCommand on this host into writeCommand's
// stdin on the destination host without buffering the payload in memory. It is
// used to move large files (such as accessory backup artifacts) between hosts
// through the local machine.
func (s SSH) CopyTo(ctx context.Context, readCommand string, dst SSH, writeCommand string) error {
	if s.DryRun || dst.DryRun {
		return nil
	}
	ctx, cancel := s.commandContext(ctx)
	defer cancel()
	read := exec.CommandContext(ctx, "ssh", s.args(readCommand)...)
	write := exec.CommandContext(ctx, "ssh", dst.args(writeCommand)...)
	pipe, err := read.StdoutPipe()
	if err != nil {
		return err
	}
	write.Stdin = pipe
	var readErr, writeErr bytes.Buffer
	read.Stderr = &readErr
	write.Stderr = &writeErr
	if err := read.Start(); err != nil {
		return fmt.Errorf("ssh %s failed: %v", s.Target(), err)
	}
	if err := write.Start(); err != nil {
		read.Process.Kill()
		read.Wait()
		return fmt.Errorf("ssh %s failed: %v", dst.Target(), err)
	}
	writeWaitErr := write.Wait()
	readWaitErr := read.Wait()
	if readWaitErr != nil {
		return fmt.Errorf("ssh %s failed: %s", s.Target(), commandFailure(readWaitErr, readErr, ctx))
	}
	if writeWaitErr != nil {
		return fmt.Errorf("ssh %s failed: %s", dst.Target(), commandFailure(writeWaitErr, writeErr, ctx))
	}
	return nil
}

// CopyFromLocal streams a local file into writeCommand's stdin on this host.
func (s SSH) CopyFromLocal(ctx context.Context, localPath, writeCommand string) error {
	if s.DryRun {
		return nil
	}
	file, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer file.Close()
	ctx, cancel := s.commandContext(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", s.args(writeCommand)...)
	cmd.Stdin = file
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh %s failed: %s", s.Target(), commandFailure(err, stderr, ctx))
	}
	return nil
}

func commandFailure(err error, stderr bytes.Buffer, ctx context.Context) string {
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = err.Error()
	}
	if ctx.Err() != nil {
		msg = ctx.Err().Error()
	}
	return msg
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
	var args []string
	for _, def := range [][2]string{
		{"BatchMode", "yes"},
		{"StrictHostKeyChecking", "accept-new"},
		{"ConnectTimeout", "15"},
	} {
		if !s.hasOption(def[0]) {
			args = append(args, "-o", def[0]+"="+def[1])
		}
	}
	if s.Port > 0 {
		args = append(args, "-p", strconv.Itoa(s.Port))
	}
	if identityFile := strings.TrimSpace(s.IdentityFile); identityFile != "" {
		args = append(args, "-i", identityFile)
	}
	if knownHostsFile := s.knownHostsFile(); knownHostsFile != "" {
		args = append(args, "-o", "UserKnownHostsFile="+knownHostsFile)
	}
	if jumpHost := strings.TrimSpace(s.JumpHost); jumpHost != "" {
		args = append(args, "-J", jumpHost)
	}
	for _, option := range s.sortedOptions() {
		args = append(args, "-o", option)
	}
	args = append(args, s.Target(), command)
	return args
}

func (s SSH) hasOption(name string) bool {
	for key := range s.Options {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			return true
		}
	}
	return false
}

func (s SSH) knownHostsFile() string {
	if strings.TrimSpace(s.KnownHostsFile) != "" {
		return strings.TrimSpace(s.KnownHostsFile)
	}
	return strings.TrimSpace(os.Getenv(knownHostsEnv))
}

func (s SSH) sortedOptions() []string {
	if len(s.Options) == 0 {
		return nil
	}
	keys := make([]string, 0, len(s.Options))
	for key := range s.Options {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	options := make([]string, 0, len(keys))
	for _, key := range keys {
		options = append(options, key+"="+strings.TrimSpace(s.Options[key]))
	}
	return options
}

func Available(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "ssh", "-V")
	return cmd.Run()
}
