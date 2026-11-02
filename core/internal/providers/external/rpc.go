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
// stdin/stdout. Calls are serialized; the process lives until Close.
type client struct {
	label string
	cmd   *exec.Cmd
	stdin io.WriteCloser

	mu     sync.Mutex
	nextID int64

	responses chan rpcMessage
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
		responses: make(chan rpcMessage, 1),
		exited:    make(chan struct{}),
	}

	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		c.readStdout(stdout)
	}()
	go func() {
		defer readers.Done()
		c.readStderr(stderr)
	}()
	go func() {
		readers.Wait()
		c.waitErr = cmd.Wait()
		close(c.exited)
	}()
	return c, nil
}

func (c *client) readStdout(r io.Reader) {
	defer close(c.responses)
	sc := bufio.NewScanner(r)
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
		c.responses <- msg
	}
	if err := sc.Err(); err != nil {
		log.Warnf("external provider %s: read: %v", c.label, err)
	}
}

func (c *client) readStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		log.Warnf("external provider %s: %s", c.label, sc.Text())
	}
}

func (c *client) call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultCallTimeout)
		defer cancel()
	}

	c.nextID++
	id := c.nextID
	payload, err := json.Marshal(rpcMessage{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return err
	}
	if _, err := c.stdin.Write(append(payload, '\n')); err != nil {
		return fmt.Errorf("external provider %s: %s: %w", c.label, method, err)
	}

	for {
		select {
		case <-ctx.Done():
			// The plugin is mid-response; its stream can no longer be trusted.
			c.kill()
			return fmt.Errorf("external provider %s: %s: %w", c.label, method, ctx.Err())
		case msg, ok := <-c.responses:
			if !ok {
				<-c.exited
				return fmt.Errorf("external provider %s exited during %s: %v", c.label, method, c.waitErr)
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
				return fmt.Errorf("external provider %s: %s: decode result: %w", c.label, method, err)
			}
			return nil
		}
	}
}

func (c *client) kill() {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

// close lets the plugin exit on stdin EOF, killing it after a grace period.
func (c *client) close() {
	_ = c.stdin.Close()
	select {
	case <-c.exited:
	case <-time.After(closeGrace):
		c.kill()
		<-c.exited
	}
}
