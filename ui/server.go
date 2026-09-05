package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/BlueBeard63/Gantry/gerr"
	"github.com/coder/websocket"
)

// Wire protocol (one websocket, JSON text frames).
//
// client -> server:
//
//	{"t":"ready","page":"pages/index"}          page mounted; start/attach its program
//	{"t":"event","h":"h42","p":<json>}          Tea handler event (id from the render tree)
//	{"t":"event","key":"pages/index","name":"buttonPress","p":<json>}   paired event
//
// server -> client:
//
//	{"t":"render","seq":1,"tree":{...}}          full tree for the active page
//	{"t":"patch","seq":2,"ops":[...]}            edits against the last tree
//	{"t":"push","key":"components/gauge","name":"state","p":<json>}     paired push
type clientMsg struct {
	T    string          `json:"t"`
	Page string          `json:"page,omitempty"`
	H    string          `json:"h,omitempty"`
	Key  string          `json:"key,omitempty"`
	Name string          `json:"name,omitempty"`
	ID   string          `json:"id,omitempty"`
	P    json.RawMessage `json:"p,omitempty"`
	// Params carries a dynamic page's captured route params, normalized to
	// arrays ([id] -> ["7"], [...slug] -> its segments). Sent with "ready".
	Params map[string][]string `json:"params,omitempty"`
}

type replyMsg struct {
	T   string `json:"t"`
	ID  string `json:"id"`
	OK  bool   `json:"ok"`
	P   any    `json:"p,omitempty"`
	Err string `json:"err,omitempty"`
	// Code carries the gerr code of a failed call ("panic.call", or the
	// code of the error the handler returned), so the frontend can
	// switch on it.
	Code string `json:"code,omitempty"`
}

type renderMsg struct {
	T    string   `json:"t"`
	Seq  uint64   `json:"seq"`
	Tree wireNode `json:"tree"`
}

type patchMsg struct {
	T   string    `json:"t"`
	Seq uint64    `json:"seq"`
	Ops []patchOp `json:"ops"`
}

type pushMsg struct {
	T    string `json:"t"`
	Key  string `json:"key"`
	Name string `json:"name"`
	P    any    `json:"p,omitempty"`
}

// conn is one connected client. A desktop app has exactly one real
// client; a new connection (webview reload, React StrictMode remount,
// dev HMR) replaces the old. Observer connections (?observer=1) are
// additional read-write taps that never replace the real client - the
// test driver uses one to watch the protocol while the webview drives
// the UI.
type conn struct {
	ws       *websocket.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	observer bool

	mu     sync.Mutex
	seq    uint64
	page   string // active page key
	closed bool
	// hasBase reports whether this conn has received a full tree for the
	// current page. Until it has, the next delivery must be a full tree,
	// not a patch (a fresh/reconnected client, or one that just switched
	// pages, has nothing to apply a patch against). Reset on every "ready".
	hasBase bool
}

func (c *conn) write(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
		c.closed = true
	}
}

func (c *conn) sendFull(tree wireNode) {
	c.mu.Lock()
	c.seq++
	seq := c.seq
	c.hasBase = true
	c.mu.Unlock()
	c.write(renderMsg{T: "render", Seq: seq, Tree: tree})
}

func (c *conn) sendPatch(ops []patchOp) {
	c.mu.Lock()
	c.seq++
	seq := c.seq
	c.mu.Unlock()
	c.write(patchMsg{T: "patch", Seq: seq, Ops: ops})
}

func (c *conn) push(key, event string, payload any) {
	c.write(pushMsg{T: "push", Key: key, Name: event, P: payload})
}

// Handler returns the websocket endpoint. Mount it where the frontend
// expects it: mux.Handle("/gantry/ws", app.Handler()).
func (a *App) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			// Local-only server; the origin is the app's own page or the
			// Vite dev server, so skip origin checks.
			InsecureSkipVerify: true,
		})
		if err != nil {
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		c := &conn{ws: ws, ctx: ctx, cancel: cancel,
			observer: r.URL.Query().Get("observer") == "1"}

		if c.observer {
			// Observers ride alongside the real client: they see every
			// frame the app sends and their events/calls work, but they
			// never displace the webview's connection.
			a.mu.Lock()
			if a.observers == nil {
				a.observers = map[*conn]struct{}{}
			}
			a.observers[c] = struct{}{}
			a.mu.Unlock()
		} else {
			// This client is now THE client; detach and close any predecessor.
			a.mu.Lock()
			old := a.conn
			a.conn = c
			a.mu.Unlock()
			if old != nil {
				old.detach(a)
				old.cancel()
				_ = old.ws.Close(websocket.StatusNormalClosure, "replaced")
			}
		}

		// A fresh client starts from the current shared state.
		for _, msg := range a.snapshotStates() {
			c.write(msg)
		}

		// Keepalive pings.
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
					_ = ws.Ping(pctx)
					pcancel()
				}
			}
		}()

		a.readLoop(c)

		c.detach(a)
		cancel()
		a.mu.Lock()
		if c.observer {
			delete(a.observers, c)
		} else if a.conn == c {
			a.conn = nil
		}
		a.mu.Unlock()
		_ = ws.Close(websocket.StatusNormalClosure, "")
	})
}

// detach stops render delivery to this conn's active page - but only
// when this conn owns the delivery (an observer leaving must not cut
// off the webview's renders).
func (c *conn) detach(a *App) {
	c.mu.Lock()
	page := c.page
	c.page = ""
	c.mu.Unlock()
	if page == "" {
		return
	}
	a.mu.Lock()
	prog := a.programs[page]
	a.mu.Unlock()
	if prog != nil {
		prog.clearDeliver(c)
	}
}

// allClients snapshots every connected client - the real one plus any
// observers - for fan-out writes (pushes, state, error frames).
func (a *App) allClients() []*conn {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*conn, 0, 1+len(a.observers))
	if a.conn != nil {
		out = append(out, a.conn)
	}
	for o := range a.observers {
		out = append(out, o)
	}
	return out
}

// deliverTo builds a program's render-delivery function: the owning
// conn (nil for an observers-only delivery) plus every observer at the
// time of each render. Each conn gets the full tree if it has no base yet
// (or the render carries no diff), otherwise the patch - and nothing at
// all when a caught-up conn's diff is empty.
func (a *App) deliverTo(owner *conn) func(renderPayload) {
	send := func(c *conn, pl renderPayload) {
		c.mu.Lock()
		// Observers (the test driver, DOM-plane taps) always get full trees:
		// they assert on the raw render frame and do not apply patches. Only
		// the real webview client - where the render cost lives - is patched.
		full := c.observer || !c.hasBase || !pl.hasOps
		c.mu.Unlock()
		if full {
			c.sendFull(pl.full)
		} else if len(pl.ops) > 0 {
			c.sendPatch(pl.ops)
		}
	}
	return func(pl renderPayload) {
		if owner != nil {
			send(owner, pl)
		}
		a.mu.Lock()
		obs := make([]*conn, 0, len(a.observers))
		for o := range a.observers {
			if o != owner {
				obs = append(obs, o)
			}
		}
		a.mu.Unlock()
		for _, o := range obs {
			send(o, pl)
		}
	}
}

func (a *App) readLoop(c *conn) {
	for {
		_, data, err := c.ws.Read(c.ctx)
		if err != nil {
			return
		}
		var msg clientMsg
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.T {
		case "ready":
			// The client mounted a page: move render delivery to it. It has
			// no tree for this page yet, so the next delivery must be full.
			c.detach(a)
			c.mu.Lock()
			c.page = msg.Page
			c.hasBase = false
			c.mu.Unlock()
			a.mu.Lock()
			samePage := a.activePage == msg.Page
			a.activePage = msg.Page
			a.activeParams = RouteParams(msg.Params)
			a.mu.Unlock()
			// Reconnects re-announce the same page; only real
			// navigation belongs in the error trail.
			if !samePage {
				a.recordCrumb("navigate", msg.Page, true)
			}
			if prog := a.program(msg.Page); prog != nil {
				if c.observer {
					// An observer never steals delivery from the real
					// client: it owns delivery only while nobody else
					// does, and otherwise just asks for a fresh tree
					// (renders fan out to observers anyway).
					if !prog.hasDeliverer() {
						prog.setDeliver(c, a.deliverTo(nil))
					} else {
						prog.rerender()
					}
				} else {
					prog.setDeliver(c, a.deliverTo(c))
				}
				// A dynamic page's Model gets the captured params on
				// activation and on every param change for the same key.
				prog.send(ParamsMsg{Params: RouteParams(msg.Params)})
			}

		case "event":
			switch {
			case msg.H != "":
				// Tea handler event for the conn's active page.
				c.mu.Lock()
				page := c.page
				c.mu.Unlock()
				a.recordCrumb("event", page+" tea:"+msg.H, true)
				a.mu.Lock()
				prog := a.programs[page]
				a.mu.Unlock()
				if prog != nil {
					prog.handleEvent(msg.H, msg.P)
				}
			case msg.Key != "":
				// Paired event for a page or component.
				a.recordCrumb("event", msg.Key+"."+msg.Name, true)
				if fn := a.pairedHandler(msg.Key, msg.Name); fn != nil {
					func() {
						defer func() {
							if r := recover(); r != nil {
								log.Printf("ui: %s.%s handler panicked: %v", msg.Key, msg.Name, r)
								a.ReportError(ErrorInfo{
									Kind:    "event-panic",
									Code:    "panic.event",
									Source:  msg.Key + "." + msg.Name,
									Message: fmt.Sprint(r),
									Stack:   string(debug.Stack()),
								})
							}
						}()
						fn(msg.P)
					}()
				} else {
					log.Printf("ui: no handler for %s.%s (check the Key in your Page/Component and that main.go registers it)", msg.Key, msg.Name)
				}
			}

		case "call":
			// Awaited request: run off the read loop (calls may be
			// slow) and reply with the result or the error.
			fn := a.callHandler(msg.Key, msg.Name)
			id := msg.ID
			if fn == nil {
				c.write(replyMsg{T: "reply", ID: id, OK: false,
					Err: fmt.Sprintf("no call %q on %q", msg.Name, msg.Key)})
				continue
			}
			payload := msg.P
			key, name := msg.Key, msg.Name
			go func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("ui: call %s.%s panicked: %v", key, name, r)
						a.recordCrumb("call", key+"."+name+" -> panic", false)
						a.ReportError(ErrorInfo{
							Kind:    "call-panic",
							Code:    "panic.call",
							Source:  key + "." + name,
							Message: fmt.Sprint(r),
							Stack:   string(debug.Stack()),
						})
						c.write(replyMsg{T: "reply", ID: id, OK: false,
							Err: fmt.Sprintf("panic in %s.%s: %v", key, name, r), Code: "panic.call"})
					}
				}()
				result, err := fn(payload)
				if err != nil {
					// A returned error is control flow, not a crash: it
					// rejects the awaiting call (with its gerr code) and
					// marks the trail, but never hits the error pipeline.
					if key != "gantry" {
						a.recordCrumb("call", key+"."+name+" -> err: "+err.Error(), false)
					}
					c.write(replyMsg{T: "reply", ID: id, OK: false, Err: err.Error(), Code: gerr.CodeOf(err)})
					return
				}
				if key != "gantry" {
					a.recordCrumb("call", key+"."+name+" -> ok", true)
				}
				c.write(replyMsg{T: "reply", ID: id, OK: true, P: result})
			}()

		case "setstate":
			a.recordCrumb("state", msg.Key, true)
			if a.applyFrontendState(msg.Key, msg.P) {
				// Mirror the write to every OTHER client (the writer
				// already has it): with an observer attached, both
				// planes see one state timeline.
				for _, o := range a.allClients() {
					if o != c {
						o.write(stateMsg{T: "state", Key: msg.Key, P: msg.P})
					}
				}
			}
		}
	}
}
