package transport

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The test executable acts as ssh and can create a ProxyCommand-like child.
// init runs before the testing flag parser sees SSH flags.
func init() {
	if os.Getenv("SHIP_TEST_SSH_STREAM_CHILD") != "1" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "echo":
		io.Copy(os.Stdout, os.Stdin)
	case "stderr":
		os.Stderr.Write(bytes.Repeat([]byte("x"), streamStderrLimit*8))
		os.Exit(7)
	case "descendant":
		executable, err := os.Executable()
		if err != nil {
			os.Exit(2)
		}
		child := exec.Command(executable, "descendant-worker")
		child.Stdout = os.Stdout
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		_ = child.Wait()
	case "descendant-worker":
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stdout, "%d %s\n", os.Getpid(), listener.Addr())
		for {
			conn, err := listener.Accept()
			if err != nil {
				os.Exit(2)
			}
			conn.Close()
		}
	case "wait":
		os.Stdout.Write([]byte("ready"))
		for {
			time.Sleep(time.Hour)
		}
	}
	os.Exit(0)
}

func streamTestExecutable(t *testing.T) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(executable, filepath.Join(dir, "ssh")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("SHIP_TEST_SSH_STREAM_CHILD", "1")
}

func TestSSHStreamBinaryAndConcurrentWait(t *testing.T) {
	streamTestExecutable(t)
	stream, err := (SSH{Host: "host"}).StartStream(context.Background(), "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	payload := bytes.Repeat([]byte{0, 255, 10, 13, 128}, 16000)
	writeDone := make(chan error, 1)
	go func() { _, err := stream.Stdin.Write(payload); stream.Stdin.Close(); writeDone <- err }()
	got, err := io.ReadAll(stream.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("binary stream changed")
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := stream.Wait(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSSHStreamCancellationInterruptsBlockedWrite(t *testing.T) {
	streamTestExecutable(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := (SSH{Host: "host", Timeout: time.Millisecond}).StartStream(ctx, "wait")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	ready := make([]byte, 5)
	if _, err := io.ReadFull(stream.Stdout, ready); err != nil {
		t.Fatal(err)
	}
	// A stream outlives SSH.Timeout and has no commandContext deadline.
	time.Sleep(20 * time.Millisecond)
	select {
	case <-stream.done:
		t.Fatal("stream inherited command timeout")
	default:
	}
	written := make(chan error, 1)
	go func() { _, err := stream.Stdin.Write(make([]byte, 8*1024*1024)); written <- err }()
	cancel()
	select {
	case err := <-written:
		if err == nil {
			t.Fatal("blocked write succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write did not stop")
	}
	select {
	case <-stream.done:
	case <-time.After(5 * time.Second):
		t.Fatal("process was not reaped")
	}
	if !errors.Is(stream.Wait(), context.Canceled) {
		t.Fatalf("wait = %v", stream.Wait())
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if !errors.Is(stream.Close(), context.Canceled) {
				t.Error("close result changed")
			}
		})
	}
	wg.Wait()
}

func TestSSHStreamStderrBoundAndStartFailure(t *testing.T) {
	streamTestExecutable(t)
	stream, err := (SSH{Host: "host"}).StartStream(context.Background(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	err = stream.Wait()
	if err == nil || err.Error() != "ssh host failed: exit status 7: "+strings.Repeat("x", streamStderrLimit) {
		t.Fatal("stderr was not bounded")
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := (SSH{Host: "host"}).StartStream(context.Background(), "echo"); err == nil {
		t.Fatal("missing executable accepted")
	}
	if _, err := (SSH{DryRun: true}).StartStream(context.Background(), "echo"); err == nil {
		t.Fatal("dry run started stream")
	}
}

func TestSSHStreamArguments(t *testing.T) {
	s := SSH{Host: "host", User: "user", Port: 2222, IdentityFile: "key", KnownHostsFile: "known", JumpHost: "jump", Options: map[string]string{
		"controlmaster": "yes", "ControlMaster": "auto", "ClearAllForwardings": "no",
		"ServerAliveInterval": "0", "ProxyCommand": "connect %h %p", "StrictHostKeyChecking": "yes",
		"UserKnownHostsFile": "other", "connecttimeout": "8", "ConnectTimeout": "9",
	}}
	before := s.args("command")
	args, err := s.streamArgs("command")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, s.args("command")) {
		t.Fatal("stream changed ordinary arguments")
	}
	options := make(map[string]string)
	for i := 0; i < len(args); i++ {
		if args[i] != "-o" {
			continue
		}
		i++
		key, value, _ := strings.Cut(args[i], "=")
		key = strings.ToLower(key)
		if _, ok := options[key]; ok {
			t.Fatalf("duplicate option %s", key)
		}
		options[key] = value
	}
	for key, want := range map[string]string{"controlmaster": "no", "controlpath": "none", "controlpersist": "no", "clearallforwardings": "yes", "requesttty": "no", "stdinnull": "no", "forkafterauthentication": "no", "serveraliveinterval": "15", "serveralivecountmax": "3", "proxycommand": "connect %h %p", "stricthostkeychecking": "yes", "userknownhostsfile": "known", "connecttimeout": "9"} {
		if options[key] != want {
			t.Errorf("%s = %q, want %q", key, options[key], want)
		}
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-p 2222", "-i key", "-J jump", "user@host command"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in %s", want, joined)
		}
	}
	if args[0] != "-T" {
		t.Fatal("TTY not disabled")
	}
}

func TestSSHStreamRejectsConflictingOptions(t *testing.T) {
	for key, value := range map[string]string{"RequestTTY": "yes", "requesttty": "force", "RemoteCommand": "echo bad", "SessionType": "none", "ForkAfterAuthentication": "yes", "StdinNull": "yes"} {
		t.Run(key+value, func(t *testing.T) {
			_, err := (SSH{Options: map[string]string{key: value}}).streamArgs("command")
			if err == nil {
				t.Fatal("conflict accepted")
			}
		})
	}
	_, err := (SSH{Options: map[string]string{"RequestTTY": "no", "requesttty": "yes"}}).streamArgs("command")
	if err == nil {
		t.Fatal("conflicting duplicate accepted")
	}
}

func TestSSHStreamCloseTerminatesProcess(t *testing.T) {
	streamTestExecutable(t)
	stream, err := (SSH{Host: "host"}).StartStream(context.Background(), "wait")
	if err != nil {
		t.Fatal(err)
	}
	ready := make([]byte, 5)
	if _, err := io.ReadFull(stream.Stdout, ready); err != nil {
		stream.Close()
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- stream.Close() }()
	select {
	case err := <-closed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("close = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not reap the child")
	}
	if _, err := stream.Stdout.Read(make([]byte, 1)); err == nil {
		t.Fatal("stdout remained open")
	}
	if _, err := stream.Stdin.Write([]byte("x")); err == nil {
		t.Fatal("stdin remained open")
	}
}

func TestSSHStreamContextDeadline(t *testing.T) {
	streamTestExecutable(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	stream, err := (SSH{Host: "host"}).StartStream(ctx, "wait")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	select {
	case <-stream.done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadline did not reap child")
	}
	if !errors.Is(stream.Wait(), context.DeadlineExceeded) {
		t.Fatalf("wait = %v", stream.Wait())
	}
}

func TestSSHStreamCancellationTerminatesDescendants(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process-group ownership is tested on Ship release platforms")
	}
	streamTestExecutable(t)
	stream, err := (SSH{Host: "host"}).StartStream(context.Background(), "descendant")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stream.Stdout).ReadString('\n')
		ready <- line
	}()
	var line string
	select {
	case line = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("descendant did not start")
	}
	fields := strings.Fields(line)
	if len(fields) != 2 {
		t.Fatalf("invalid child readiness: %q", line)
	}
	descendant, err := strconv.Atoi(fields[0])
	if err != nil || descendant <= 0 {
		t.Fatalf("invalid child PID: %q", fields[0])
	}
	// This fallback also cleans up the child when the regression is run
	// against code that kills only the direct SSH process.
	defer func() {
		child, err := os.FindProcess(descendant)
		if err == nil {
			_ = child.Kill()
			_ = child.Release()
		}
	}()
	address := fields[1]
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatalf("descendant listener not reachable: %v", err)
	}
	conn.Close()
	started := time.Now()
	if err := stream.Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("close = %v", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("process group cleanup exceeded shutdown bound")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err != nil {
			break
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("ProxyCommand descendant survived stream cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
