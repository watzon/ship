package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/watzon/ship/internal/transport"
	"testing"
	"time"
)

func proxyTCPPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	server, err := listener.AcceptTCP()
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

func proxyAwait(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not stop")
		return nil
	}
}

func TestProxyStreamRoundTrip(t *testing.T) {
	for _, serverFirst := range []bool{false, true} {
		name := "client_half_close_first"
		if serverFirst {
			name = "server_half_close_first"
		}
		t.Run(name, func(t *testing.T) {
			app, local := proxyTCPPair(t)
			remote, service := proxyTCPPair(t)
			clientIn, serverOut := io.Pipe()
			serverIn, clientOut := io.Pipe()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			clientDone, serverDone := make(chan error, 1), make(chan error, 1)
			go func() { clientDone <- relayProxy(ctx, bufio.NewReader(clientIn), clientIn, clientOut, local) }()
			go func() { serverDone <- relayProxy(ctx, bufio.NewReader(serverIn), serverIn, serverOut, remote) }()
			body := bytes.Repeat([]byte{0, 1, 255, 10, 13, 0, 128}, 50000)
			greeting := []byte("server greeting\x00\xff")
			exchange := make(chan error, 1)
			go func() {
				if _, err := service.Write(greeting); err != nil {
					exchange <- err
					return
				}
				if serverFirst {
					if err := service.CloseWrite(); err != nil {
						exchange <- err
						return
					}
				}
				got, err := io.ReadAll(service)
				if err != nil {
					exchange <- err
					return
				}
				if !bytes.Equal(got, body) {
					exchange <- errors.New("binary body differs")
					return
				}
				if !serverFirst {
					_, err = service.Write(body)
					if err == nil {
						err = service.CloseWrite()
					}
				}
				exchange <- err
			}()
			actual := make([]byte, len(greeting))
			if _, err := io.ReadFull(app, actual); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(actual, greeting) {
				t.Fatal("greeting differs")
			}
			if serverFirst {
				var one [1]byte
				if _, err := app.Read(one[:]); err != io.EOF {
					t.Fatalf("want server FIN, got %v", err)
				}
			}
			if _, err := app.Write(body); err != nil {
				t.Fatal(err)
			}
			if err := app.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(app)
			if err != nil {
				t.Fatal(err)
			}
			if !serverFirst && !bytes.Equal(got, body) {
				t.Fatal("echo differs")
			}
			if err := proxyAwait(t, exchange); err != nil {
				t.Fatal(err)
			}
			if err := proxyAwait(t, clientDone); err != nil {
				t.Fatal(err)
			}
			if err := proxyAwait(t, serverDone); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProxyStreamHandshakeReadAhead(t *testing.T) {
	service, target := proxyTCPPair(t)
	in, request := io.Pipe()
	response, out := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveProxy(ctx, in, out, func(context.Context, ProxyTarget) (net.Conn, error) { return target, nil })
	}()
	greeting := []byte{255, 0, 10, 128}
	serviceDone := make(chan error, 1)
	go func() {
		_, err := service.Write(greeting)
		if err == nil {
			err = service.CloseWrite()
		}
		if err == nil {
			_, err = io.Copy(io.Discard, service)
		}
		serviceDone <- err
	}()
	var wire bytes.Buffer
	if err := writeProxyHeader(&wire, proxyRequest{Version: ProxyStreamVersion}); err != nil {
		t.Fatal(err)
	}
	if err := (&proxyFrameWriter{writer: &wire}).write(proxyFrameFIN, nil); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = request.Write(wire.Bytes()) }()
	reader := bufio.NewReaderSize(response, proxyHeaderLimit)
	var ready proxyResponse
	if err := readProxyHeader(reader, &ready); err != nil || !ready.OK {
		t.Fatalf("ready=%+v error=%v", ready, err)
	}
	buffer := make([]byte, proxyDataLimit)
	kind, payload, err := readProxyFrame(reader, buffer)
	if err != nil || kind != proxyFrameData || !bytes.Equal(payload, greeting) {
		t.Fatalf("greeting frame=%v %v", kind, err)
	}
	kind, _, err = readProxyFrame(reader, buffer)
	if err != nil || kind != proxyFrameFIN {
		t.Fatalf("FIN=%v %v", kind, err)
	}
	if err := proxyAwait(t, done); err != nil {
		t.Fatal(err)
	}
	if err := proxyAwait(t, serviceDone); err != nil {
		t.Fatal(err)
	}
	request.Close()
	response.Close()
}

func TestProxyStreamRejectedHandshake(t *testing.T) {
	for _, version := range []int{0, ProxyStreamVersion} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			var request bytes.Buffer
			writeProxyHeader(&request, proxyRequest{Version: version})
			request.WriteString("secret application bytes")
			out := &proxyTestBuffer{}
			calls := 0
			err := serveProxy(context.Background(), io.NopCloser(&request), out, func(context.Context, ProxyTarget) (net.Conn, error) {
				calls++
				return nil, &ProxyError{Code: ProxyErrorTargetChanged, Message: "secret from inspect"}
			})
			if err == nil {
				t.Fatal("accepted rejected target")
			}
			if version == 0 && calls != 0 {
				t.Fatal("dialed unsupported version")
			}
			if bytes.Contains(out.Bytes(), []byte("secret")) {
				t.Fatal("leaked diagnostic or application bytes")
			}
			var response proxyResponse
			if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &response); err != nil || response.OK {
				t.Fatalf("invalid error response: %v", err)
			}
		})
	}
}

type proxyTestBuffer struct{ bytes.Buffer }

func (*proxyTestBuffer) Close() error { return nil }

func TestProxyFrameRejectsMalformed(t *testing.T) {
	tests := []struct {
		name   string
		kind   byte
		length uint32
		body   []byte
	}{
		{"unknown", 255, 0, nil}, {"empty_data", proxyFrameData, 0, nil}, {"huge_data", proxyFrameData, ^uint32(0), nil},
		{"fin_payload", proxyFrameFIN, 1, nil}, {"huge_error", proxyFrameError, proxyErrorLimit + 1, nil}, {"truncated", proxyFrameData, 2, []byte{1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var header [5]byte
			header[0] = tt.kind
			binary.BigEndian.PutUint32(header[1:], tt.length)
			input := append(header[:], tt.body...)
			if _, _, err := readProxyFrame(bytes.NewReader(input), make([]byte, proxyDataLimit)); err == nil {
				t.Fatal("accepted malformed frame")
			}
		})
	}
}

func TestProxyStreamRejectsFramesAfterFIN(t *testing.T) {
	for _, kind := range []byte{proxyFrameFIN, proxyFrameData} {
		app, socket := proxyTCPPair(t)
		_ = app
		var input bytes.Buffer
		writer := &proxyFrameWriter{writer: &input}
		writer.write(proxyFrameFIN, nil)
		payload := []byte(nil)
		if kind == proxyFrameData {
			payload = []byte{1}
		}
		writer.write(kind, payload)
		in := io.NopCloser(&input)
		err := relayProxy(context.Background(), bufio.NewReader(in), in, &proxyTestBuffer{}, socket)
		var typed *ProxyError
		if !errors.As(err, &typed) || typed.Code != ProxyErrorProtocol {
			t.Fatalf("got %v", err)
		}
	}
}

func TestProxyStreamCancelBlockedPeer(t *testing.T) {
	for _, blockTCP := range []bool{false, true} {
		t.Run(map[bool]string{false: "pipe_write", true: "tcp_write"}[blockTCP], func(t *testing.T) {
			app, socket := proxyTCPPair(t)
			socket.SetWriteBuffer(1024)
			app.SetReadBuffer(1024)
			in, peerOut := io.Pipe()
			peerIn, out := io.Pipe()
			defer peerIn.Close()
			defer peerOut.Close()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- relayProxy(ctx, bufio.NewReader(in), in, out, socket) }()
			writerDone := make(chan error, 1)
			go func() {
				body := make([]byte, proxyDataLimit)
				for i := 0; i < 1000; i++ {
					var err error
					if blockTCP {
						err = (&proxyFrameWriter{writer: peerOut}).write(proxyFrameData, body)
					} else {
						_, err = app.Write(body)
					}
					if err != nil {
						writerDone <- nil
						return
					}
				}
				writerDone <- errors.New("peer did not block")
			}()
			time.Sleep(30 * time.Millisecond)
			cancel()
			if err := proxyAwait(t, done); err == nil {
				t.Fatal("cancellation succeeded")
			}
			app.Close()
			peerOut.Close()
			if err := proxyAwait(t, writerDone); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProxyWriteStall(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	done := make(chan error, 1)
	go func() {
		done <- proxyWriteAll(proxyStallWriter{writer, func() { writer.Close() }, 20 * time.Millisecond}, []byte{1})
	}()
	if err := proxyAwait(t, done); err == nil {
		t.Fatal("blocked write succeeded")
	}
}

func FuzzProxyFrame(f *testing.F) {
	f.Add([]byte{proxyFrameFIN, 0, 0, 0, 0})
	f.Add([]byte{proxyFrameData, 0, 0, 0, 1, 255})
	f.Add([]byte{255, 255, 255, 255, 255})
	f.Fuzz(func(t *testing.T, data []byte) {
		kind, payload, err := readProxyFrame(bytes.NewReader(data), make([]byte, proxyDataLimit))
		if err != nil {
			return
		}
		if len(payload) > proxyDataLimit || (kind == proxyFrameFIN && len(payload) != 0) {
			t.Fatal("invalid accepted frame")
		}
	})
}

func TestProxyHeaderBounds(t *testing.T) {
	for _, input := range []string{"", `{"version":1}`, "null\n", "{} {}\n", string(bytes.Repeat([]byte("x"), proxyHeaderLimit)) + "\n"} {
		var value proxyRequest
		err := readProxyHeader(bufio.NewReaderSize(bytes.NewBufferString(input), proxyHeaderLimit), &value)
		if err == nil && input != "null\n" {
			t.Fatalf("accepted invalid header of length %d", len(input))
		}
	}
}

func TestProxyStreamAbruptEOF(t *testing.T) {
	_, socket := proxyTCPPair(t)
	in := io.NopCloser(bytes.NewReader(nil))
	if err := relayProxy(context.Background(), bufio.NewReader(in), in, &proxyTestBuffer{}, socket); err == nil {
		t.Fatal("accepted stream EOF before FIN")
	}
}

func TestProxyStreamIdleUntilCancellation(t *testing.T) {
	_, socket := proxyTCPPair(t)
	in, peerOut := io.Pipe()
	peerIn, out := io.Pipe()
	defer peerIn.Close()
	defer peerOut.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- relayProxy(ctx, bufio.NewReader(in), in, out, socket) }()
	select {
	case err := <-done:
		t.Fatalf("idle stream stopped: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := proxyAwait(t, done); err == nil {
		t.Fatal("cancellation succeeded")
	}
}

// The child replaces OpenSSH with this test process. It still uses real process
// pipes and TCP sockets, and it cannot call Docker or contact a remote host.
func TestProxyStreamHelper(t *testing.T) {
	if os.Getenv("SHIP_TEST_PROXY_STREAM_HELPER") != "1" {
		return
	}
	err := serveProxy(context.Background(), os.Stdin, os.Stdout, func(ctx context.Context, target ProxyTarget) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(target.Address, strconv.Itoa(target.Port)))
	})
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestProxyStreamClientChild(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nexec " + shellQuote(executable) + " -test.run='^TestProxyStreamHelper$'\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SHIP_TEST_PROXY_STREAM_HELPER", "1")
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	body := bytes.Repeat([]byte{255, 0, 13, 10, 128}, 30000)
	serviceDone := make(chan error, 1)
	go func() {
		service, err := listener.AcceptTCP()
		if err != nil {
			serviceDone <- err
			return
		}
		defer service.Close()
		service.SetDeadline(time.Now().Add(5 * time.Second))
		_, err = service.Write([]byte("ready\n"))
		if err == nil {
			var data []byte
			data, err = io.ReadAll(service)
			if err == nil && !bytes.Equal(data, body) {
				err = errors.New("child body differs")
			}
		}
		if err == nil {
			_, err = service.Write(body)
		}
		if err == nil {
			err = service.CloseWrite()
		}
		serviceDone <- err
	}()
	app, local := proxyTCPPair(t)
	app.SetDeadline(time.Now().Add(5 * time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	target := ProxyTarget{Address: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port}
	go func() { done <- (Client{SSH: transport.SSH{Host: "test.invalid"}}).Proxy(ctx, target, local) }()
	greeting := make([]byte, 6)
	if _, err := io.ReadFull(app, greeting); err != nil || string(greeting) != "ready\n" {
		t.Fatalf("greeting %q: %v", greeting, err)
	}
	if _, err := app.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := app.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(app)
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatalf("child reply: %v", err)
	}
	if err := proxyAwait(t, serviceDone); err != nil {
		t.Fatal(err)
	}
	if err := proxyAwait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestProxyStreamVerificationFailureForwardsNothing(t *testing.T) {
	target, service := proxyTCPPair(t)
	var request bytes.Buffer
	if err := writeProxyHeader(&request, proxyRequest{Version: ProxyStreamVersion}); err != nil {
		t.Fatal(err)
	}
	if err := (&proxyFrameWriter{writer: &request}).write(proxyFrameData, []byte("client secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Write([]byte("server secret")); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	output := &proxyTestBuffer{}
	go func() {
		done <- serveProxy(context.Background(), io.NopCloser(&request), output, func(context.Context, ProxyTarget) (net.Conn, error) {
			close(started)
			<-release
			target.Close()
			return nil, proxyStreamFailure(ProxyErrorTargetChanged)
		})
	}()
	<-started
	service.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	var data [1]byte
	n, err := service.Read(data[:])
	close(release)
	if n != 0 || err == nil {
		t.Fatal("forwarded client bytes before target verification")
	}
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("unexpected target read: %v", err)
	}
	if err := proxyAwait(t, done); err == nil {
		t.Fatal("accepted changed target")
	}
	if bytes.Contains(output.Bytes(), []byte("secret")) {
		t.Fatal("forwarded bytes after target verification failed")
	}
}
