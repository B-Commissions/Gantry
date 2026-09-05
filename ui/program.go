package ui

import (
	"encoding/json"
	"fmt"
	"log"
	"runtime/debug"
	"strconv"
	"sync"
	"time"
)

// Msg is anything an event handler or Cmd produces; Update switches on
// its concrete type. Same shape as Bubble Tea.
type Msg any

// Cmd is work that runs on its own goroutine and feeds its result back
// into Update. Return nil from Update for "no work".
type Cmd func() Msg

// batchMsg carries Batch's commands through the runtime.
type batchMsg []Cmd

// Batch runs several commands concurrently.
func Batch(cmds ...Cmd) Cmd {
	var live []Cmd
	for _, c := range cmds {
		if c != nil {
			live = append(live, c)
		}
	}
	if len(live) == 0 {
		return nil
	}
	return func() Msg { return batchMsg(live) }
}

// Tick waits d, then feeds fn(now) into Update. Re-issue it from Update
// for a repeating timer.
func Tick(d time.Duration, fn func(time.Time) Msg) Cmd {
	return func() Msg {
		t := time.NewTimer(d)
		defer t.Stop()
		return fn(<-t.C)
	}
}

// Model is a page's state machine: Init returns the first command,
// Update folds a Msg into a new model, View renders the tree.
type Model interface {
	Init() Cmd
	Update(msg Msg) (Model, Cmd)
	View() Node
}

// program runs one page's Model: a serialized Update loop, command
// goroutines, and render delivery with per-generation handler tables.
type program struct {
	key  string
	msgs chan Msg
	stop chan struct{}
	once sync.Once

	mu       sync.Mutex
	model    Model
	handlers map[string]handlerFn // current render generation
	prev     map[string]handlerFn // previous generation - an event racing a re-render still resolves
	nextID   int
	// deliver sends a serialized tree to the active client (and any
	// observers); swapped by the server as connections and pages
	// change. nil = page inactive. owner is the conn the delivery
	// belongs to, so a departing observer can't cut off the webview.
	deliver func(tree wireNode)
	owner   *conn
	// report feeds recovered panics into the app's error pipeline.
	report func(ErrorInfo)
}

func newProgram(key string, model Model, report func(ErrorInfo)) *program {
	p := &program{
		key:    key,
		msgs:   make(chan Msg, 64),
		stop:   make(chan struct{}),
		model:  model,
		report: report,
	}
	go p.loop()
	return p
}

// reportPanic logs a recovered panic and feeds it to the error
// pipeline.
func (p *program) reportPanic(kind, code string, r any) {
	log.Printf("ui: %s: %s: %v", p.key, kind, r)
	if p.report != nil {
		p.report(ErrorInfo{
			Kind:    kind,
			Code:    code,
			Source:  p.key,
			Message: fmt.Sprint(r),
			Stack:   string(debug.Stack()),
		})
	}
}

// loop is the single goroutine that touches the model.
func (p *program) loop() {
	p.runCmd(p.model.Init())
	// No unconditional first render: delivery starts when the page
	// activates (setDeliver sends a rerenderMsg), and rendering here
	// too would race it - the client would see two initial trees.
	for {
		select {
		case <-p.stop:
			return
		case msg := <-p.msgs:
			changed := p.apply(msg)
			// Coalesce: drain every already-queued message before
			// rendering, so a burst of rapid events produces ONE
			// render instead of hammering the frontend with a full
			// tree per click.
		drain:
			for {
				select {
				case more := <-p.msgs:
					if p.apply(more) {
						changed = true
					}
				default:
					break drain
				}
			}
			if changed {
				p.render()
			}
		}
	}
}

// apply folds one message into the model; reports whether a render is
// due. An Update panic is recovered (the model keeps its last good
// state) instead of killing the page's loop goroutine.
func (p *program) apply(msg Msg) (changed bool) {
	if batch, ok := msg.(batchMsg); ok {
		for _, c := range batch {
			p.runCmd(c)
		}
		return false
	}
	if _, ok := msg.(rerenderMsg); ok {
		return true
	}
	defer func() {
		if r := recover(); r != nil {
			p.reportPanic("tea-update-panic", "panic.update", r)
			changed = false
		}
	}()
	model, cmd := p.model.Update(msg)
	p.model = model
	p.runCmd(cmd)
	return true
}

func (p *program) runCmd(cmd Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				p.reportPanic("cmd-panic", "panic.cmd", r)
			}
		}()
		if msg := cmd(); msg != nil {
			p.send(msg)
		}
	}()
}

// send feeds a message into Update (external senders included).
func (p *program) send(msg Msg) {
	select {
	case <-p.stop:
	case p.msgs <- msg:
	}
}

// render serializes View with a fresh handler generation and delivers
// the tree if the page is active.
func (p *program) render() {
	defer func() {
		if r := recover(); r != nil {
			p.reportPanic("tea-view-panic", "panic.view", r)
		}
	}()
	tree := p.model.View()

	p.mu.Lock()
	p.prev = p.handlers
	p.handlers = map[string]handlerFn{}
	wire := p.serialize(tree)
	deliver := p.deliver
	p.mu.Unlock()

	if deliver != nil {
		deliver(wire)
	}
}

// serialize walks the tree assigning handler IDs (caller holds p.mu).
func (p *program) serialize(n Node) wireNode {
	w := wireNode{Type: n.Type, Key: n.Key, Props: n.Props}
	if len(n.handlers) > 0 {
		w.Handlers = make(map[string]string, len(n.handlers))
		for event, fn := range n.handlers {
			p.nextID++
			id := "h" + strconv.Itoa(p.nextID)
			p.handlers[id] = fn
			w.Handlers[event] = id
		}
	}
	if len(n.Children) > 0 {
		w.Children = make([]wireNode, 0, len(n.Children))
		for _, c := range n.Children {
			w.Children = append(w.Children, p.serialize(c))
		}
	}
	return w
}

// handleEvent resolves a handler ID against the current, then previous,
// generation and feeds the produced Msg into Update. Stale IDs (older
// than one render) are dropped silently.
func (p *program) handleEvent(id string, payload json.RawMessage) {
	p.mu.Lock()
	fn := p.handlers[id]
	if fn == nil {
		fn = p.prev[id]
	}
	p.mu.Unlock()
	if fn == nil {
		return
	}
	if msg := fn(payload); msg != nil {
		p.send(msg)
	}
}

// setDeliver activates render delivery on behalf of owner and pushes a
// full render immediately.
func (p *program) setDeliver(owner *conn, deliver func(wireNode)) {
	p.mu.Lock()
	p.owner = owner
	p.deliver = deliver
	p.mu.Unlock()
	if deliver != nil {
		// Re-render rather than caching the last tree: handlers must be
		// a fresh generation for the new client.
		p.send(rerenderMsg{})
	}
}

// clearDeliver deactivates delivery, but only when owner still owns it.
func (p *program) clearDeliver(owner *conn) {
	p.mu.Lock()
	if p.owner == owner {
		p.owner = nil
		p.deliver = nil
	}
	p.mu.Unlock()
}

// hasDeliverer reports whether anyone currently receives renders.
func (p *program) hasDeliverer() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.deliver != nil
}

// rerender requests a fresh render without changing delivery.
func (p *program) rerender() { p.send(rerenderMsg{}) }

// rerenderMsg makes the loop emit a render without changing the model.
type rerenderMsg struct{}

func (p *program) close() {
	p.once.Do(func() { close(p.stop) })
}
