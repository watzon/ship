package transport

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

const streamStderrLimit = 16 * 1024

// SSHStream owns one foreground SSH process. Close releases its descriptors and
// cancels the process. Wait may be called concurrently, including after Close.
type SSHStream struct {
	Stdin     io.WriteCloser
	Stdout    io.ReadCloser
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
	err       error
}

// Wait returns the result of the single process wait.
func (s *SSHStream) Wait() error { <-s.done; return s.err }

// Close interrupts pending reads and writes, terminates the process, and reaps it.
func (s *SSHStream) Close() error {
	s.closeOnce.Do(func() { s.cancel(); s.Stdin.Close(); s.Stdout.Close() })
	return s.Wait()
}

// StartStream starts a binary command session whose lifetime is controlled only
// by ctx. The caller owns setup deadlines and must close the returned stream.
func (s SSH) StartStream(ctx context.Context, command string) (*SSHStream, error) {
	if s.DryRun {
		return nil, fmt.Errorf("cannot start an SSH stream in dry-run mode")
	}
	args, err := s.streamArgs(command)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.WaitDelay = 5 * time.Second
	terminateGroup := configureSSHStreamProcess(cmd)
	// Explicit pipes keep Wait from closing stdout before the caller drains it.
	inR, inW, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		cancel()
		inR.Close()
		inW.Close()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout = inR, outW
	stderr := &streamStderr{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cancel()
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, fmt.Errorf("ssh %s failed: %w", s.Target(), err)
	}
	inR.Close()
	outW.Close()
	stream := &SSHStream{Stdin: inW, Stdout: outR, cancel: cancel, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		// Clean up descendants even if ssh exited before cancellation. Run
		// this before publishing completion, while this stream owns the group.
		_ = terminateGroup()
		if err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			msg := strings.TrimSpace(string(stderr.data))
			if msg != "" && ctx.Err() == nil {
				err = fmt.Errorf("%w: %s", err, msg)
			}
			stream.err = fmt.Errorf("ssh %s failed: %w", s.Target(), err)
		}
		close(stream.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			inW.Close()
			outR.Close()
		case <-stream.done:
		}
	}()
	return stream, nil
}

type streamStderr struct{ data []byte }

func (b *streamStderr) Write(p []byte) (int, error) {
	n := len(p)
	remaining := streamStderrLimit - len(b.data)
	if len(p) > remaining {
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (s SSH) streamArgs(command string) ([]string, error) {
	// Validate all explicit values, including duplicate keys with different case.
	for key, value := range s.Options {
		value = strings.ToLower(strings.TrimSpace(value))
		conflict := false
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "requesttty":
			conflict = value != "no"
		case "remotecommand":
			conflict = value != "" && value != "none"
		case "sessiontype":
			conflict = value != "default"
		case "forkafterauthentication", "stdinnull":
			conflict = value != "no"
		}
		if conflict {
			return nil, fmt.Errorf("SSH option %s conflicts with a binary stream", strings.TrimSpace(key))
		}
	}
	mandatory := [][2]string{
		{"RequestTTY", "no"}, {"ClearAllForwardings", "yes"},
		{"ControlMaster", "no"}, {"ControlPath", "none"}, {"ControlPersist", "no"},
		{"ForkAfterAuthentication", "no"}, {"StdinNull", "no"},
		{"SessionType", "default"}, {"RemoteCommand", "none"},
		{"ServerAliveInterval", "15"}, {"ServerAliveCountMax", "3"},
	}
	args := []string{"-T"}
	seen := make(map[string]bool)
	for _, option := range mandatory {
		args = append(args, "-o", option[0]+"="+option[1])
		seen[strings.ToLower(option[0])] = true
	}
	// Normalize a copy so whitespace keys and case duplicates are deterministic.
	keys := make([]string, 0, len(s.Options))
	for key := range s.Options {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	options := make(map[string]string)
	for _, key := range keys {
		normalized := strings.ToLower(strings.TrimSpace(key))
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		options[normalized] = strings.TrimSpace(s.Options[key])
	}
	s.Options = options
	// Reuse the ordinary connection arguments, deduplicating settings such as
	// UserKnownHostsFile when both a dedicated field and a custom option exist.
	seen = make(map[string]bool)
	for _, option := range mandatory {
		seen[strings.ToLower(option[0])] = true
	}
	connection := s.args(command)
	for i := 0; i < len(connection)-2; i++ {
		if connection[i] == "-o" {
			option := connection[i+1]
			i++
			key, _, _ := strings.Cut(option, "=")
			key = strings.ToLower(strings.TrimSpace(key))
			if seen[key] {
				continue
			}
			seen[key] = true
			args = append(args, "-o", option)
		} else {
			args = append(args, connection[i])
		}
	}
	return append(args, s.Target(), command), nil
}
