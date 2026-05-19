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

	// notifHandlers route server → client notifications. Keyed by
	// LSP method name. Each handler receives the raw params and
	// runs synchronously on the read goroutine — keep them fast,
	// or hand off to a buffered channel.
	notifMu       sync.RWMutex
	notifHandlers map[string]NotificationHandler

	// reqHandlers route server → client *requests* (reverse RPC).
	// LSP servers issue these for things like
	// `workspace/applyEdit`, `workspace/configuration`, and
	// `client/registerCapability`. The handler returns a result
	// (or an error) which we send back framed as the response.
	reqMu       sync.RWMutex
	reqHandlers map[string]RequestHandler
}

// NotificationHandler processes a notification from the server.
type NotificationHandler func(method string, params json.RawMessage)

// RequestHandler processes a request from the server. Either result
// or err must be set; nil/nil is treated as a null success result.
type RequestHandler func(method string, params json.RawMessage) (result any, err *jsonRPCError)

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

// NewClient spawns an LSP server subprocess and returns a connected
// client. env carries extra KEY=VALUE entries appended to the daemon's
// own environment — used to pin a JRE for jdtls and similar.
func NewClient(command string, args, env []string, workspaceRoot string, logger *zap.Logger) (*Client, error) {
	cmd := exec.Command(command, args...)
	cmd.Dir = workspaceRoot
	cmd.Stderr = os.Stderr
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

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
		cmd:           cmd,
		stdin:         stdin,
		stdout:        bufio.NewReader(stdout),
		logger:        logger,
		done:          make(chan struct{}),
		notifHandlers: make(map[string]NotificationHandler),
		reqHandlers:   make(map[string]RequestHandler),
	}

	// Start response reader goroutine.
	go c.readResponses()

	return c, nil
}

// OnNotification registers a handler for server→client notifications
// for the given method (e.g. "textDocument/publishDiagnostics"). One
// handler per method; later registrations replace earlier ones.
func (c *Client) OnNotification(method string, h NotificationHandler) {
	c.notifMu.Lock()
	defer c.notifMu.Unlock()
	c.notifHandlers[method] = h
}

// OnRequest registers a handler for server→client requests (reverse
// RPC). The reply is framed and sent back automatically.
func (c *Client) OnRequest(method string, h RequestHandler) {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()
	c.reqHandlers[method] = h
}

// Done returns a channel that closes when the client's read loop
// terminates (server exited or stdin/stdout error).
func (c *Client) Done() <-chan struct{} { return c.done }

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
		// The read loop already closed `done`; just wait for the
		// subprocess to exit so we don't leak it.
		if c.cmd != nil && c.cmd.Process != nil {
			_ = c.cmd.Wait()
		}
		return nil
	}
	c.closed = true
	close(c.done)
	c.mu.Unlock()

	// Send shutdown request — best-effort, the server may already be gone.
	_ = c.Call("shutdown", nil, nil)
	_ = c.Notify("exit", nil)
	_ = c.stdin.Close()
	if c.cmd == nil || c.cmd.Process == nil {
		return nil
	}
	return c.cmd.Wait()
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
//
// Three message shapes are framed identically:
//   - response (has "id" + "result" or "error"): routed to pending Call.
//   - notification (has "method" but no "id"): dispatched to
//     OnNotification handlers.
//   - request (has both "method" and "id"): the server is asking the
//     client to do something (e.g. workspace/applyEdit). The handler
//     in OnRequest returns a result that we frame and send back.
func (c *Client) readResponses() {
	defer func() {
		// On EOF / read error, signal done so pending Call() return
		// promptly instead of blocking forever. The select probe
		// covers the case where someone (typically a test) closed
		// c.done directly — close-of-closed-channel would panic.
		select {
		case <-c.done:
			return
		default:
		}
		c.mu.Lock()
		if !c.closed {
			c.closed = true
			close(c.done)
		}
		c.mu.Unlock()
	}()

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

		// Inspect the message to decide if it's a response or a
		// server-initiated message (notification or request).
		var probe struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(body, &probe); err != nil {
			c.logger.Debug("LSP: failed to parse message", zap.Error(err))
			continue
		}

		// Server-initiated notification: method present, id absent.
		if probe.Method != "" && len(probe.ID) == 0 {
			var notif struct {
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(body, &notif); err != nil {
				continue
			}
			c.dispatchNotification(notif.Method, notif.Params)
			continue
		}

		// Server-initiated request: method present and id present.
		if probe.Method != "" && len(probe.ID) > 0 {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				continue
			}
			c.dispatchRequest(req.ID, req.Method, req.Params)
			continue
		}

		// Otherwise it's a response to one of our requests.
		var resp jsonRPCResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			c.logger.Debug("LSP: failed to parse response", zap.Error(err))
			continue
		}
		if ch, ok := c.pending.Load(resp.ID); ok {
			// Best-effort, non-blocking — pending channel is buffered.
			select {
			case ch.(chan *jsonRPCResponse) <- &resp:
			default:
			}
		}
	}
}

// dispatchNotification fans a server notification out to its handler.
func (c *Client) dispatchNotification(method string, params json.RawMessage) {
	c.notifMu.RLock()
	h, ok := c.notifHandlers[method]
	c.notifMu.RUnlock()
	if !ok {
		return
	}
	defer func() {
		// A panicking handler must not kill the read loop.
		if r := recover(); r != nil {
			c.logger.Debug("LSP: notification handler panicked",
				zap.String("method", method),
				zap.Any("recover", r),
			)
		}
	}()
	h(method, params)
}

// dispatchRequest answers a server-initiated request. When no handler
// is registered we reply with a JSON-RPC method-not-found error so the
// server doesn't hang waiting forever.
func (c *Client) dispatchRequest(rawID json.RawMessage, method string, params json.RawMessage) {
	c.reqMu.RLock()
	h, ok := c.reqHandlers[method]
	c.reqMu.RUnlock()

	type respWithRawID struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result,omitempty"`
		Error   *jsonRPCError   `json:"error,omitempty"`
	}

	resp := respWithRawID{JSONRPC: "2.0", ID: rawID}
	if !ok {
		resp.Error = &jsonRPCError{Code: -32601, Message: "method not found: " + method}
	} else {
		func() {
			defer func() {
				if r := recover(); r != nil {
					resp.Error = &jsonRPCError{Code: -32603, Message: "handler panicked"}
				}
			}()
			res, err := h(method, params)
			if err != nil {
				resp.Error = err
			} else {
				resp.Result = res
			}
		}()
	}
	_ = c.send(resp)
}
