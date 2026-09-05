package gantrytest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/BlueBeard63/Gantry/ui"
)

// ErrorInfo is one captured app error - kind, code, message, the page
// the user was on and the breadcrumb trail. Alias of ui.ErrorInfo.
type ErrorInfo = ui.ErrorInfo

// CallError is a rejected awaited call: the wire reply's err and gerr
// code ("panic.call", "auth.expired", ...).
type CallError struct {
	Code    string
	Message string
}

func (e *CallError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("call failed [%s]: %s", e.Code, e.Message)
	}
	return "call failed: " + e.Message
}

// frame is one received wire frame, minimally decoded for scanning;
// raw keeps the exact bytes for traces and failure dumps.
type frame struct {
	T    string          `json:"t"`
	Seq  int             `json:"seq,omitempty"`
	Key  string          `json:"key,omitempty"`
	Name string          `json:"name,omitempty"`
	ID   string          `json:"id,omitempty"`
	OK   *bool           `json:"ok,omitempty"`
	Err  string          `json:"err,omitempty"`
	Code string          `json:"code,omitempty"`
	P    json.RawMessage `json:"p,omitempty"`
	Tree json.RawMessage `json:"tree,omitempty"`
	Ops  json.RawMessage `json:"ops,omitempty"`

	raw []byte
}

// wireMsg is one outgoing frame.
type wireMsg struct {
	T    string          `json:"t"`
	Page string          `json:"page,omitempty"`
	H    string          `json:"h,omitempty"`
	Key  string          `json:"key,omitempty"`
	Name string          `json:"name,omitempty"`
	ID   string          `json:"id,omitempty"`
	P    json.RawMessage `json:"p,omitempty"`
}

// callSeq numbers awaited calls across every client in the process so
// ids stay unique through restarts.
var callSeq atomic.Int64

// client is the protocol-plane connection: it dials /gantry/ws like
// the frontend would, keeps every received frame (renders, pushes,
// state, errors, replies), and lets waits scan that history under one
// condition variable. All waiting APIs honor the test deadline and
// fail with the last protocol frames attached.
type client struct {
	t       testing.TB
	tr      *trace
	conn    *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	timeout time.Duration

	mu           sync.Mutex
	cond         *sync.Cond
	frames       []frame
	states       map[string]json.RawMessage
	errs         []ErrorInfo
	closed       bool
	readErr      error
	cursors      map[string]int  // consumption cursors (pushes, error frames)
	renderCursor int             // frames index of the last render NextRender returned
	consumed     map[string]bool // error signatures already returned by WaitError
	currentTree  json.RawMessage // running tree, updated by render/patch frames
}

func dialClient(t testing.TB, tr *trace, port int, timeout time.Duration, observer bool, token string) (*client, error) {
	ctx, cancel := context.WithCancel(context.Background())
	url := fmt.Sprintf("ws://127.0.0.1:%d/gantry/ws", port)
	var params []string
	if observer {
		// With a webview attached, the driver rides alongside it as an
		// observer instead of displacing its connection.
		params = append(params, "observer=1")
	}
	if token != "" {
		// Device backends run the server token-guarded (the mobile
		// shell's loopback protection); the runner chose the token.
		params = append(params, "gantry_token="+token)
	}
	if len(params) > 0 {
		url += "?" + strings.Join(params, "&")
	}
	dialCtx, dialDone := context.WithTimeout(ctx, 15*time.Second)
	defer dialDone()
	conn, _, err := websocket.Dial(dialCtx, url, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("dialing %s: %w", url, err)
	}
	conn.SetReadLimit(8 << 20) // whole-tree renders can be large

	c := &client{
		t: t, tr: tr, conn: conn, ctx: ctx, cancel: cancel, timeout: timeout,
		states:       map[string]json.RawMessage{},
		cursors:      map[string]int{},
		consumed:     map[string]bool{},
		renderCursor: -1,
	}
	c.cond = sync.NewCond(&c.mu)
	go c.readLoop()
	return c, nil
}

func (c *client) readLoop() {
	for {
		_, data, err := c.conn.Read(c.ctx)
		if err != nil {
			c.mu.Lock()
			c.closed = true
			c.readErr = err
			c.cond.Broadcast()
			c.mu.Unlock()
			return
		}
		var f frame
		_ = json.Unmarshal(data, &f)
		f.raw = data
		c.tr.frame("recv", data)

		c.mu.Lock()
		switch f.T {
		case "render":
			c.currentTree = f.Tree
		case "patch":
			// Materialize the diff against the running tree and surface it
			// as a render, so the tree waiters (Tree/WaitTree/NextRender)
			// observe the update the same as a full frame.
			if c.currentTree != nil {
				if materialized, err := applyPatchJSON(c.currentTree, f.Ops); err == nil {
					c.currentTree = materialized
				}
			}
			f.T = "render"
			f.Tree = c.currentTree
		case "state":
			c.states[f.Key] = f.P
		case "error":
			var e ErrorInfo
			if json.Unmarshal(f.P, &e) == nil {
				c.errs = append(c.errs, e)
			}
		}
		c.frames = append(c.frames, f)
		c.cond.Broadcast()
		c.mu.Unlock()
	}
}

func (c *client) close() {
	c.cancel()
	_ = c.conn.Close(websocket.StatusNormalClosure, "test done")
}

func (c *client) send(msg wireMsg) {
	c.t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		c.t.Fatalf("gantrytest: marshaling %s frame: %v", msg.T, err)
	}
	c.tr.frame("send", data)
	ctx, done := context.WithTimeout(c.ctx, 10*time.Second)
	defer done()
	if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
		c.t.Fatalf("gantrytest: writing %s frame: %v", msg.T, err)
	}
}

// marshalPayload turns a Go value into the wire payload; nil stays
// absent.
func (c *client) marshalPayload(v any) json.RawMessage {
	c.t.Helper()
	if v == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		c.t.Fatalf("gantrytest: marshaling payload: %v", err)
	}
	return data
}

// deadline is the effective wait deadline: the client timeout, capped
// by the test's own deadline with a little grace so the failure is
// ours (with frames attached) rather than the test binary's panic.
func (c *client) deadline() time.Time {
	d := time.Now().Add(c.timeout)
	if td, ok := c.t.(interface{ Deadline() (time.Time, bool) }); ok {
		if t, has := td.Deadline(); has {
			if g := t.Add(-2 * time.Second); g.Before(d) {
				d = g
			}
		}
	}
	return d
}

// wait blocks until pred (evaluated with c.mu held) is true. On
// timeout or a dead connection it fails the test with the last
// protocol frames attached.
func (c *client) wait(what string, pred func() bool) {
	c.t.Helper()
	deadline := c.deadline()
	timer := time.AfterFunc(time.Until(deadline), func() {
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	})
	defer timer.Stop()

	c.mu.Lock()
	for !pred() {
		if c.closed {
			dump := c.lastFramesLocked(20)
			err := c.readErr
			c.mu.Unlock()
			c.t.Fatalf("gantrytest: connection closed while waiting for %s (%v)\nlast protocol frames:\n%s", what, err, dump)
			return
		}
		if !time.Now().Before(deadline) {
			dump := c.lastFramesLocked(20)
			c.mu.Unlock()
			c.t.Fatalf("gantrytest: timed out waiting for %s\nlast protocol frames:\n%s", what, dump)
			return
		}
		c.cond.Wait()
	}
	c.mu.Unlock()
}

// lastFramesLocked renders the newest n received frames for failure
// messages.
func (c *client) lastFramesLocked(n int) string {
	start := len(c.frames) - n
	if start < 0 {
		start = 0
	}
	if len(c.frames) == 0 {
		return "  (none received)"
	}
	var b strings.Builder
	for _, f := range c.frames[start:] {
		raw := string(f.raw)
		if len(raw) > 300 {
			raw = raw[:300] + "..."
		}
		fmt.Fprintf(&b, "  %s\n", raw)
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---- wire operations ----

func (c *client) ready(page string) {
	c.t.Helper()
	c.tr.action("ready %s", page)
	c.send(wireMsg{T: "ready", Page: page})
}

func (c *client) teaEvent(handlerID string, payload any) {
	c.t.Helper()
	c.tr.action("tea event %s", handlerID)
	c.send(wireMsg{T: "event", H: handlerID, P: c.marshalPayload(payload)})
}

func (c *client) sendEvent(key, name string, payload any) {
	c.t.Helper()
	c.tr.action("event %s.%s", key, name)
	c.send(wireMsg{T: "event", Key: key, Name: name, P: c.marshalPayload(payload)})
}

func (c *client) setState(key string, value any) {
	c.t.Helper()
	c.tr.action("setstate %s", key)
	p := c.marshalPayload(value)
	c.send(wireMsg{T: "setstate", Key: key, P: p})
	// Writes are not echoed back to the sender - mirror locally so
	// State() reflects what the frontend would see.
	c.mu.Lock()
	c.states[key] = p
	c.mu.Unlock()
}

func (c *client) call(key, name string, payload any) (json.RawMessage, *CallError) {
	c.t.Helper()
	id := fmt.Sprintf("t%d", callSeq.Add(1))
	c.tr.action("call %s.%s (%s)", key, name, id)
	c.send(wireMsg{T: "call", Key: key, Name: name, ID: id, P: c.marshalPayload(payload)})

	var reply frame
	c.wait(fmt.Sprintf("reply to %s.%s", key, name), func() bool {
		for i := range c.frames {
			if c.frames[i].T == "reply" && c.frames[i].ID == id {
				reply = c.frames[i]
				return true
			}
		}
		return false
	})
	if reply.OK != nil && *reply.OK {
		return reply.P, nil
	}
	return nil, &CallError{Code: reply.Code, Message: reply.Err}
}

// nextRender waits for a render frame the test has not consumed yet
// and returns its tree.
func (c *client) nextRender() json.RawMessage {
	c.t.Helper()
	var tree json.RawMessage
	c.wait("the next render", func() bool {
		for i := c.renderCursor + 1; i < len(c.frames); i++ {
			if c.frames[i].T == "render" {
				c.renderCursor = i
				tree = c.frames[i].Tree
				return true
			}
		}
		return false
	})
	return tree
}

// latestTree waits until at least one render arrived and pred accepts
// the newest tree. A nil pred means "any render".
func (c *client) latestTree(what string, pred func(json.RawMessage) bool) json.RawMessage {
	c.t.Helper()
	var tree json.RawMessage
	c.wait(what, func() bool {
		for i := len(c.frames) - 1; i >= 0; i-- {
			if c.frames[i].T == "render" {
				if pred == nil || pred(c.frames[i].Tree) {
					tree = c.frames[i].Tree
					if i > c.renderCursor {
						c.renderCursor = i
					}
					return true
				}
				return false // newest render fails pred: keep waiting
			}
		}
		return false
	})
	return tree
}

// waitPush returns the next unconsumed push for key/name, including
// one that already landed before the wait started.
func (c *client) waitPush(key, name string) json.RawMessage {
	c.t.Helper()
	cursor := "push/" + key + "/" + name
	var p json.RawMessage
	c.wait(fmt.Sprintf("push %s.%s", key, name), func() bool {
		for i := c.cursors[cursor]; i < len(c.frames); i++ {
			if c.frames[i].T == "push" && c.frames[i].Key == key && c.frames[i].Name == name {
				c.cursors[cursor] = i + 1
				p = c.frames[i].P
				return true
			}
		}
		c.cursors[cursor] = len(c.frames)
		return false
	})
	return p
}

// errSignature identifies one captured error across its two delivery
// channels (the live frame and the ring buffer), so WaitError never
// returns the same error twice and ExpectNoErrors can skip expected
// ones.
func errSignature(e ErrorInfo) string {
	return e.Code + "/" + e.Time.UTC().Format(time.RFC3339Nano) + "/" + e.Message
}

// takeErrorFrame returns the first unconsumed live error frame with
// the code, if one arrived.
func (c *client) takeErrorFrame(code string) (ErrorInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.errs {
		if e.Code == code && !c.consumed[errSignature(e)] {
			c.consumed[errSignature(e)] = true
			return e, true
		}
	}
	return ErrorInfo{}, false
}

// snapshotErrors copies the error frames captured so far.
func (c *client) snapshotErrors() []ErrorInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ErrorInfo, len(c.errs))
	copy(out, c.errs)
	return out
}

func (c *client) markConsumed(e ErrorInfo) {
	c.mu.Lock()
	c.consumed[errSignature(e)] = true
	c.mu.Unlock()
}

func (c *client) isConsumed(e ErrorInfo) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.consumed[errSignature(e)]
}
