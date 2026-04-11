package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
)

// Client manages a JSON-RPC 2.0 connection to an LSP server subprocess.
type Client struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	reqID   atomic.Int64
	pending sync.Map // reqID → chan *jsonRPCResponse
	logger  *zap.Logger
	done    chan struct{}

	mu     sync.Mutex
	closed bool
}

// jsonRPCRequest is a JSON-RPC 2.0 request.
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// jsonRPCResponse is a JSON-RPC 2.0 response.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// jsonRPCNotification is a JSON-RPC 2.0 notification (no ID).
type jsonRPCNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *jsonRPCError) Error() string {
	return fmt.Sprintf("LSP error %d: %s", e.Code, e.Message)
}

// NewClient spawns an LSP server subprocess and returns a connected client.
func NewClient(command string, args []string, workspaceRoot string, logger *zap.Logger) (*Client, error) {
	cmd := exec.Command(command, args...)
	cmd.Dir = workspaceRoot
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", command, err)
	}

	c := &Client{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
		logger: logger,
		done:   make(chan struct{}),
	}

	// Start response reader goroutine.
	go c.readResponses()

	return c, nil
}

// Call sends a request and waits for the response.
func (c *Client) Call(method string, params any, result any) error {
	id := c.reqID.Add(1)

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	respCh := make(chan *jsonRPCResponse, 1)
	c.pending.Store(id, respCh)
	defer c.pending.Delete(id)

	if err := c.send(req); err != nil {
		return fmt.Errorf("send %s: %w", method, err)
	}

	// Wait for response.
	select {
	case resp := <-respCh:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	case <-c.done:
		return fmt.Errorf("LSP server exited")
	}
}

// Notify sends a notification (no response expected).
func (c *Client) Notify(method string, params any) error {
	notif := jsonRPCNotification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	return c.send(notif)
}

// Shutdown sends the LSP shutdown and exit sequence.
func (c *Client) Shutdown() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	// Send shutdown request.
	_ = c.Call("shutdown", nil, nil)

	// Send exit notification.
	_ = c.Notify("exit", nil)

	// Close stdin to signal EOF.
	_ = c.stdin.Close()

	// Wait for the process to exit.
	err := c.cmd.Wait()
	close(c.done)
	return err
}

// send writes a JSON-RPC message using the LSP content-length framing.
func (c *Client) send(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return fmt.Errorf("client is closed")
	}

	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	if _, err := io.WriteString(c.stdin, header); err != nil {
		return err
	}
	if _, err := c.stdin.Write(data); err != nil {
		return err
	}
	return nil
}

// readResponses continuously reads responses from the LSP server.
func (c *Client) readResponses() {
	for {
		// Read headers.
		contentLength := -1
		for {
			line, err := c.stdout.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break // End of headers.
			}
			if strings.HasPrefix(line, "Content-Length:") {
				val := strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
				contentLength, _ = strconv.Atoi(val)
			}
		}

		if contentLength < 0 {
			continue
		}

		// Read body.
		body := make([]byte, contentLength)
		if _, err := io.ReadFull(c.stdout, body); err != nil {
			return
		}

		// Parse response.
		var resp jsonRPCResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			c.logger.Debug("LSP: failed to parse response", zap.Error(err))
			continue
		}

		// Route to pending request.
		if ch, ok := c.pending.Load(resp.ID); ok {
			ch.(chan *jsonRPCResponse) <- &resp
		}
	}
}
