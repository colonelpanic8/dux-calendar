package external

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/duxlabs-dev/dux-go/log"
)

const (
	defaultCallTimeout = 60 * time.Second
	closeGrace         = 2 * time.Second
	maxMessageBytes    = 64 << 20
)

// errCodeReauth is the JSON-RPC error code a plugin returns when its
// credentials were rejected; it maps to calendar.ErrReauthRequired.
const errCodeReauth = -32001

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// client speaks newline-delimited JSON-RPC 2.0 to a plugin process over its
// stdin/stdout. Calls are serialized; the process lives until close. Every
// blocking step honors the call context, and terminate kills the plugin's
// whole process group and closes its pipes so nothing can wedge shutdown.
type client struct {
	label  string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	sem    chan struct{}
	nextID int64

	responses chan rpcMessage
	stopped   chan struct{}
	stopOnce  sync.Once
	exited    chan struct{}
	waitErr   error
}

func startClient(label string, argv []string, env map[string]string) (*client, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("external provider: command is empty")
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("external provider %s: start %q: %w", label, argv[0], err)
	}

	c := &client{
		label:     label,
		cmd:       cmd,
		stdin:     stdin,
		stdout:    stdout,
		stderr:    stderr,
		sem:       make(chan struct{}, 1),
		responses: make(chan rpcMessage),
		stopped:   make(chan struct{}),
		exited:    make(chan struct{}),
	}

	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		c.readStdout()
	}()
	go func() {
		defer readers.Done()
		c.readStderr()
	}()
	go func() {
		readers.Wait()
		c.waitErr = cmd.Wait()
		close(c.exited)
	}()
	return c, nil
}

func (c *client) readStdout() {
	defer close(c.responses)
	sc := bufio.NewScanner(c.stdout)
	sc.Buffer(make([]byte, 0, 64<<10), maxMessageBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			log.Warnf("external provider %s: invalid message: %v", c.label, err)
			continue
		}
		if msg.ID == nil {
			continue
		}
		select {
		case c.responses <- msg:
		case <-c.stopped:
			return
		}
	}
	if err := sc.Err(); err != nil && !c.isStopped() {
		log.Warnf("external provider %s: read: %v", c.label, err)
	}
}

func (c *client) readStderr() {
	sc := bufio.NewScanner(c.stderr)
	for sc.Scan() {
		log.Warnf("external provider %s: %s", c.label, sc.Text())
	}
}

func (c *client) isStopped() bool {
	select {
	case <-c.stopped:
		return true
	default:
		return false
	}
}

func (c *client) errorf(method, format string, args ...any) error {
	return fmt.Errorf("external provider %s: %s: "+format, append([]any{c.label, method}, args...)...)
}

func (c *client) call(ctx context.Context, method string, params, result any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultCallTimeout)
		defer cancel()
	}

	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return c.errorf(method, "%w", ctx.Err())
	}
	if c.isStopped() {
		return c.errorf(method, "provider was stopped")
	}

	c.nextID++
	id := c.nextID
	payload, err := json.Marshal(rpcMessage{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return err
	}

	// A plugin that stops reading can block a large write indefinitely.
	written := make(chan error, 1)
	go func() {
		_, err := c.stdin.Write(append(payload, '\n'))
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			c.terminate()
			return c.errorf(method, "write: %w", err)
		}
	case <-ctx.Done():
		c.terminate()
		return c.errorf(method, "%w", ctx.Err())
	}

	for {
		select {
		case <-ctx.Done():
			// The plugin is mid-response; its stream can no longer be trusted.
			c.terminate()
			return c.errorf(method, "%w", ctx.Err())
		case msg, ok := <-c.responses:
			if !ok {
				c.terminate()
				return c.exitError(method)
			}
			if *msg.ID != id {
				continue
			}
			if msg.Error != nil {
				return msg.Error
			}
			if result == nil || len(msg.Result) == 0 {
				return nil
			}
			if err := json.Unmarshal(msg.Result, result); err != nil {
				return c.errorf(method, "decode result: %w", err)
			}
			return nil
		}
	}
}

func (c *client) exitError(method string) error {
	select {
	case <-c.exited:
		return c.errorf(method, "provider exited: %v", c.waitErr)
	case <-time.After(closeGrace):
		return c.errorf(method, "provider closed its output")
	}
}

// terminate kills the plugin's process group and closes all pipes, which
// unblocks the reader goroutines even if a descendant still holds them.
func (c *client) terminate() {
	c.stopOnce.Do(func() {
		close(c.stopped)
		// Until exited closes the leader is unreaped, so its pid and process
		// group cannot have been reused.
		select {
		case <-c.exited:
		default:
			_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = c.stdin.Close()
		_ = c.stdout.Close()
		_ = c.stderr.Close()
	})
}

// close lets the plugin exit on stdin EOF, then terminates it after a grace
// period. It never blocks for more than two grace periods.
func (c *client) close() {
	_ = c.stdin.Close()
	select {
	case <-c.exited:
	case <-time.After(closeGrace):
	}
	c.terminate()
	select {
	case <-c.exited:
	case <-time.After(closeGrace):
		log.Warnf("external provider %s: did not exit after kill", c.label)
	}
}
