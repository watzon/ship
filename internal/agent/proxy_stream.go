package agent

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/watzon/ship/internal/config"
)

const (
	proxyHeaderLimit       = 16 * 1024
	proxyDataLimit         = 64 * 1024
	proxyErrorLimit        = 4 * 1024
	proxySetupTimeout      = 30 * time.Second
	proxyWriteTimeout      = 30 * time.Second
	proxyFrameData    byte = 1
	proxyFrameFIN     byte = 2
	proxyFrameError   byte = 3
)

type proxyRequest struct {
	Version int         `json:"version"`
	Target  ProxyTarget `json:"target"`
}
type proxyResponse struct {
	Version   int    `json:"version"`
	OK        bool   `json:"ok"`
	ErrorCode string `json:"error_code,omitempty"`
	Error     string `json:"error,omitempty"`
}

func proxyStreamFailure(code string) *ProxyError {
	message := "Proxy connection failed. Try again."
	switch code {
	case ProxyErrorTargetChanged:
		message = "Proxy target changed. Run the proxy command again."
	case ProxyErrorProtocol:
		message = "Invalid proxy stream. Check the agent version and try again."
	case ProxyErrorConnect:
		message = "Cannot connect to the container port. Check the service and try again."
	default:
		code = ProxyErrorUnavailable
	}
	return &ProxyError{Code: code, Message: message}
}

func proxySafeError(err error) *ProxyError {
	var typed *ProxyError
	if errors.As(err, &typed) {
		return proxyStreamFailure(typed.Code)
	}
	return proxyStreamFailure(ProxyErrorUnavailable)
}

func readProxyHeader(r *bufio.Reader, value any) error {
	line, err := r.ReadSlice('\n')
	if err != nil || len(line) > proxyHeaderLimit {
		return proxyStreamFailure(ProxyErrorProtocol)
	}
	if err := json.Unmarshal(line, value); err != nil {
		return proxyStreamFailure(ProxyErrorProtocol)
	}
	return nil
}

func writeProxyHeader(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil || len(data)+1 > proxyHeaderLimit {
		return proxyStreamFailure(ProxyErrorProtocol)
	}
	return proxyWriteAll(w, append(data, '\n'))
}

func readProxyFrame(r io.Reader, buffer []byte) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, proxyStreamFailure(ProxyErrorProtocol)
		}
		return 0, nil, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	switch header[0] {
	case proxyFrameData:
		if size == 0 || size > proxyDataLimit {
			return 0, nil, proxyStreamFailure(ProxyErrorProtocol)
		}
	case proxyFrameFIN:
		if size != 0 {
			return 0, nil, proxyStreamFailure(ProxyErrorProtocol)
		}
	case proxyFrameError:
		if size == 0 || size > proxyErrorLimit {
			return 0, nil, proxyStreamFailure(ProxyErrorProtocol)
		}
	default:
		return 0, nil, proxyStreamFailure(ProxyErrorProtocol)
	}
	if int(size) > len(buffer) {
		return 0, nil, proxyStreamFailure(ProxyErrorProtocol)
	}
	if _, err := io.ReadFull(r, buffer[:size]); err != nil {
		return 0, nil, proxyStreamFailure(ProxyErrorProtocol)
	}
	return header[0], buffer[:size], nil
}

func proxyWriteAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		chunk := data
		if len(chunk) > 4096 {
			chunk = chunk[:4096]
		}
		n, err := w.Write(chunk)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
		data = data[n:]
	}
	return nil
}

// proxyStallWriter arms a timer only while bytes are pending. Closing the
// owned descriptors interrupts pipe writes as well as TCP writes.
type proxyStallWriter struct {
	writer  io.Writer
	abort   func()
	timeout time.Duration
}

func (w proxyStallWriter) Write(data []byte) (int, error) {
	timer := time.AfterFunc(w.timeout, w.abort)
	n, err := w.writer.Write(data)
	timer.Stop()
	return n, err
}

type proxyFrameWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *proxyFrameWriter) write(kind byte, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var header [5]byte
	header[0] = kind
	binary.BigEndian.PutUint32(header[1:], uint32(len(data)))
	if err := proxyWriteAll(w.writer, header[:]); err != nil {
		return err
	}
	return proxyWriteAll(w.writer, data)
}

// ServeProxy serves one connection and owns in and out, closing both on return.
// Only the hidden agent proxy command should pass process stdin and stdout.
func (s Server) ServeProxy(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	return serveProxy(ctx, in, out, s.DialProxyTarget)
}

func serveProxy(ctx context.Context, in io.ReadCloser, out io.WriteCloser, dial func(context.Context, ProxyTarget) (net.Conn, error)) error {
	session, cancel := context.WithCancel(ctx)
	defer cancel()
	closeIO := func() { _ = in.Close(); _ = out.Close() }
	stopClose := context.AfterFunc(session, closeIO)
	defer func() { stopClose(); closeIO() }()
	setup, stopSetup := context.WithTimeout(session, proxySetupTimeout)
	defer stopSetup()
	stopTimeout := context.AfterFunc(setup, cancel)
	defer stopTimeout()
	reader := bufio.NewReaderSize(in, proxyHeaderLimit)
	writer := proxyStallWriter{out, cancel, proxyWriteTimeout}
	var request proxyRequest
	err := readProxyHeader(reader, &request)
	if err == nil && request.Version != ProxyStreamVersion {
		err = proxyStreamFailure(ProxyErrorProtocol)
	}
	var remote net.Conn
	if err == nil {
		remote, err = dial(setup, request.Target)
	}
	if err != nil {
		if errors.Is(setup.Err(), context.DeadlineExceeded) {
			err = proxyStreamFailure(ProxyErrorConnect)
		}
		failure := proxySafeError(err)
		_ = writeProxyHeader(writer, proxyResponse{Version: ProxyStreamVersion, ErrorCode: failure.Code, Error: failure.Message})
		return failure
	}
	defer remote.Close()
	if err := writeProxyHeader(writer, proxyResponse{Version: ProxyStreamVersion, OK: true}); err != nil {
		return proxySafeError(err)
	}
	if !stopTimeout() || setup.Err() != nil {
		return proxyStreamFailure(ProxyErrorUnavailable)
	}
	stopSetup()
	return relayProxy(session, reader, in, out, remote)
}

// Proxy connects one local TCP socket to the pinned target. It owns local and
// the SSH stream and closes them before returning, including on setup failure.
func (c Client) Proxy(ctx context.Context, target ProxyTarget, local net.Conn) (result error) {
	defer local.Close()
	session, cancel := context.WithCancel(ctx)
	defer cancel()
	var setupExpired atomic.Bool
	setupTimer := time.AfterFunc(proxySetupTimeout, func() { setupExpired.Store(true); cancel() })
	defer func() {
		if setupExpired.Load() {
			result = proxyStreamFailure(ProxyErrorConnect)
		}
	}()
	defer setupTimer.Stop()
	stream, err := c.SSH.StartStream(session, shellQuote(config.RemoteBinaryPath)+" agent proxy")
	if err != nil {
		return proxySafeError(err)
	}
	defer stream.Close()
	stopClose := context.AfterFunc(session, func() { _ = local.Close(); _ = stream.Close() })
	defer stopClose()
	reader := bufio.NewReaderSize(stream.Stdout, proxyHeaderLimit)
	writer := proxyStallWriter{stream.Stdin, cancel, proxyWriteTimeout}
	if err := writeProxyHeader(writer, proxyRequest{Version: ProxyStreamVersion, Target: target}); err != nil {
		return proxySafeError(err)
	}
	var response proxyResponse
	if err := readProxyHeader(reader, &response); err != nil {
		return err
	}
	if response.Version != ProxyStreamVersion || (response.OK && (response.ErrorCode != "" || response.Error != "")) {
		return proxyStreamFailure(ProxyErrorProtocol)
	}
	if !response.OK {
		if !validProxyStreamErrorCode(response.ErrorCode) || response.Error == "" {
			return proxyStreamFailure(ProxyErrorProtocol)
		}
		return proxyStreamFailure(response.ErrorCode)
	}
	if !setupTimer.Stop() || session.Err() != nil {
		return proxyStreamFailure(ProxyErrorUnavailable)
	}
	err = relayProxy(session, reader, stream.Stdout, stream.Stdin, local)
	_ = stream.Stdin.Close()
	if err != nil {
		return err
	}
	wait := make(chan error, 1)
	go func() { wait <- stream.Wait() }()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err := <-wait:
		if err != nil {
			return proxySafeError(err)
		}
	case <-timer.C:
		_ = stream.Close()
		<-wait
		return proxyStreamFailure(ProxyErrorUnavailable)
	case <-session.Done():
		_ = stream.Close()
		<-wait
		return proxyStreamFailure(ProxyErrorUnavailable)
	}
	return nil
}

func relayProxy(ctx context.Context, reader *bufio.Reader, in io.ReadCloser, out io.WriteCloser, socket net.Conn) error {
	session, cancel := context.WithCancel(ctx)
	defer cancel()
	closeAll := func() { _ = socket.Close(); _ = in.Close(); _ = out.Close() }
	stopClose := context.AfterFunc(session, closeAll)
	defer func() { stopClose(); closeAll() }()
	frames := &proxyFrameWriter{writer: proxyStallWriter{out, cancel, proxyWriteTimeout}}
	sent := make(chan error, 1)
	received := make(chan error, 1)
	fin := make(chan struct{}, 1)
	var finWrite sync.Mutex
	go func() {
		buffer := make([]byte, proxyDataLimit)
		for {
			n, err := socket.Read(buffer)
			if n > 0 {
				if writeErr := frames.write(proxyFrameData, buffer[:n]); writeErr != nil {
					sent <- proxySafeError(writeErr)
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					finWrite.Lock()
					sent <- frames.write(proxyFrameFIN, nil)
					finWrite.Unlock()
				} else {
					failure := proxyStreamFailure(ProxyErrorConnect)
					data, _ := json.Marshal(proxyResponse{ErrorCode: failure.Code, Error: failure.Message})
					_ = frames.write(proxyFrameError, data)
					sent <- failure
				}
				return
			}
		}
	}()
	go func() {
		buffer := make([]byte, proxyDataLimit)
		gotFIN := false
		for {
			kind, payload, err := readProxyFrame(reader, buffer)
			if err != nil {
				received <- err
				return
			}
			if gotFIN {
				received <- proxyStreamFailure(ProxyErrorProtocol)
				return
			}
			switch kind {
			case proxyFrameData:
				if err := proxyWriteAll(proxyStallWriter{socket, cancel, proxyWriteTimeout}, payload); err != nil {
					received <- proxyStreamFailure(ProxyErrorConnect)
					return
				}
			case proxyFrameFIN:
				half, ok := socket.(interface{ CloseWrite() error })
				if !ok {
					received <- proxyStreamFailure(ProxyErrorProtocol)
					return
				}
				if err := half.CloseWrite(); err != nil {
					received <- proxyStreamFailure(ProxyErrorConnect)
					return
				}
				gotFIN = true
				fin <- struct{}{}
			case proxyFrameError:
				var response proxyResponse
				if json.Unmarshal(payload, &response) != nil || !validProxyStreamErrorCode(response.ErrorCode) || response.Error == "" || response.OK {
					received <- proxyStreamFailure(ProxyErrorProtocol)
				} else {
					received <- proxyStreamFailure(response.ErrorCode)
				}
				return
			}
		}
	}()
	var sendDone, receiveDone, gotFIN bool
	var result error
	for !sendDone || !receiveDone {
		if sendDone && gotFIN && !receiveDone {
			_ = in.Close()
		}
		select {
		case err := <-sent:
			sendDone = true
			if err != nil && result == nil {
				result = proxySafeError(err)
				cancel()
			}
		case <-fin:
			gotFIN = true
		case err := <-received:
			receiveDone = true
			// The FIN notification precedes receiver completion, but select may pick
			// the completion first. Drain the notification before classifying EOF.
			select {
			case <-fin:
				gotFIN = true
			default:
			}
			if !sendDone {
				// A peer can consume FIN before its pipe Write returns. Wait
				// for that write to publish its result before classifying EOF.
				finWrite.Lock()
				select {
				case sendErr := <-sent:
					sendDone = true
					if sendErr != nil {
						result = proxySafeError(sendErr)
					}
				default:
				}
				finWrite.Unlock()
			}
			var protocol *ProxyError
			if errors.As(err, &protocol) || !gotFIN || !sendDone {
				if result == nil {
					result = proxySafeError(err)
				}
				cancel()
			}
		case <-session.Done():
			if result == nil {
				result = proxyStreamFailure(ProxyErrorUnavailable)
			}
			closeAll()
			// Wait for both workers after descriptor closure, without a busy select.
			if !sendDone {
				<-sent
				sendDone = true
			}
			if !receiveDone {
				<-received
				receiveDone = true
			}
		}
	}
	return result
}

func validProxyStreamErrorCode(code string) bool {
	switch code {
	case ProxyErrorTargetChanged, ProxyErrorProtocol, ProxyErrorUnavailable, ProxyErrorConnect:
		return true
	default:
		return false
	}
}
