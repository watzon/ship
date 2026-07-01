package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/watzon/ship/internal/config"
	"github.com/watzon/ship/internal/transport"
)

type Client struct {
	SSH          transport.SSH
	NewRequestID func() string
}

type RemoteError struct {
	Code    string
	Message string
}

func (e RemoteError) Error() string {
	if e.Code == "" {
		return "agent error: " + e.Message
	}
	return fmt.Sprintf("agent error %s: %s", e.Code, e.Message)
}

var requestCounter atomic.Uint64

func (c Client) Call(ctx context.Context, method string, params any, out any) error {
	if c.SSH.DryRun {
		if out != nil {
			switch value := out.(type) {
			case *Status:
				*value = Status{
					Hostname:        c.SSH.Host,
					StateDir:        config.RemoteStateDir,
					DockerOK:        true,
					AgentVersion:    AgentVersion,
					ProtocolVersion: AgentProtocol,
				}
			case *map[string]string:
				*value = map[string]string{"logs": "dry-run: logs would be fetched over SSH"}
			}
		}
		return nil
	}
	payload, err := json.Marshal(params)
	if err != nil {
		return err
	}
	requestID := c.requestID()
	req, err := json.Marshal(Request{ID: requestID, Method: method, Params: payload, ProtocolVersion: AgentProtocol})
	if err != nil {
		return err
	}
	command := agentRPCCommand(config.RemoteBinaryPath)
	raw, err := c.SSH.RunWithStdin(ctx, command, string(req)+"\n")
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	var resp Response
	if err := decoder.Decode(&resp); err != nil {
		return fmt.Errorf("decode agent response: %w", err)
	}
	if resp.ID != "" && resp.ID != requestID {
		return fmt.Errorf("agent response id %q did not match request id %q", resp.ID, requestID)
	}
	if !resp.OK {
		return RemoteError{Code: resp.ErrorCode, Message: resp.Error}
	}
	if out != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, out)
	}
	return nil
}

func (c Client) requestID() string {
	if c.NewRequestID != nil {
		return c.NewRequestID()
	}
	return fmt.Sprintf("req-%d", requestCounter.Add(1))
}

func agentRPCCommand(binaryPath string) string {
	return shellQuote(binaryPath) + " agent rpc"
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
