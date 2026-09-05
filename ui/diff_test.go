package ui

import (
	"reflect"
	"testing"
)

// applyOps is a reference implementation of the client's patch applier,
// used to prove diffTree is complete: apply(prev, diff(prev, next)) must
// reconstruct next. It mirrors editAt in web/src/patch.ts.
func applyOps(tree wireNode, ops []patchOp) wireNode {
	for _, op := range ops {
		tree = applyOp(tree, op, 0)
	}
	return tree
}

func applyOp(node wireNode, op patchOp, depth int) wireNode {
	if depth == len(op.Path) {
		if op.Op == "replace" {
			return *op.Node
		}
		node.Props = op.Props
		node.Handlers = op.Handlers
		return node
	}
	idx := op.Path[depth]
	children := make([]wireNode, len(node.Children))
	copy(children, node.Children)
	children[idx] = applyOp(children[idx], op, depth+1)
	node.Children = children
	return node
}

// wire serializes a Node with fresh handler generation, the same way
// render() does, so tests operate on real wire trees (path-based ids).
func wire(n Node) wireNode {
	p := &program{handlers: map[string]handlerFn{}}
	return p.serialize(n, "")
}

func TestDiffRoundTrip(t *testing.T) {
	cases := []struct {
		name       string
		prev, next Node
		wantOps    int // -1 = don't assert count
	}{
		{
			name: "identical",
			prev: Column(Text("a"), Button("x", "clk"), Text("c")),
			next: Column(Text("a"), Button("x", "clk"), Text("c")),
			// Handlers compare equal (same path/event), props equal -> no ops.
			wantOps: 0,
		},
		{
			name:    "leaf text change",
			prev:    Column(Text("a")),
			next:    Column(Text("b")),
			wantOps: 1,
		},
		{
			name:    "nested change",
			prev:    Column(Row(Text("x")), Text("y")),
			next:    Column(Row(Text("z")), Text("y")),
			wantOps: 1,
		},
		{
			name:    "child added -> replace parent",
			prev:    Column(Text("a")),
			next:    Column(Text("a"), Text("b")),
			wantOps: 1,
		},
		{
			name:    "child removed -> replace parent",
			prev:    Column(Text("a"), Text("b")),
			next:    Column(Text("a")),
			wantOps: 1,
		},
		{
			name:    "type change -> replace node",
			prev:    Column(Button("x", "clk")),
			next:    Column(Text("x")),
			wantOps: 1,
		},
		{
			name:    "props on a container change",
			prev:    Column(Text("a")).WithProps("gap", 4),
			next:    Column(Text("a")).WithProps("gap", 8),
			wantOps: 1,
		},
		{
			name:    "two independent changes",
			prev:    Column(Text("a"), Row(Text("b"), Text("c"))),
			next:    Column(Text("A"), Row(Text("b"), Text("C"))),
			wantOps: 2,
		},
		{
			name:    "key change -> replace",
			prev:    Column(Text("a").WithKey("k1")),
			next:    Column(Text("a").WithKey("k2")),
			wantOps: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prevW := wire(c.prev)
			nextW := wire(c.next)

			var ops []patchOp
			diffTree(prevW, nextW, []int{}, &ops)

			if c.wantOps >= 0 && len(ops) != c.wantOps {
				t.Fatalf("op count = %d, want %d\nops = %+v", len(ops), c.wantOps, ops)
			}

			got := applyOps(prevW, ops)
			if !reflect.DeepEqual(got, nextW) {
				t.Fatalf("apply(prev, diff) != next\n got  = %+v\n want = %+v\n ops  = %+v", got, nextW, ops)
			}
		})
	}
}

// TestSerializeHandlerIDsStable checks that a handler's id is a function
// of its path and event, so an unchanged node keeps the same id across
// renders - the property the diff relies on for event resolution.
func TestSerializeHandlerIDsStable(t *testing.T) {
	build := func() Node {
		return Column(Text("top"), Row(Button("press", "msg")))
	}
	p := &program{handlers: map[string]handlerFn{}}
	w1 := p.serialize(build(), "")
	p.handlers = map[string]handlerFn{}
	w2 := p.serialize(build(), "")

	// The button is at path .1.0 (child 1 of root, child 0 of the row).
	id1 := w1.Children[1].Children[0].Handlers["click"]
	id2 := w2.Children[1].Children[0].Handlers["click"]
	if id1 == "" {
		t.Fatalf("no click handler id assigned")
	}
	if id1 != id2 {
		t.Fatalf("handler id not stable across renders: %q vs %q", id1, id2)
	}
	if _, ok := p.handlers[id1]; !ok {
		t.Fatalf("handler id %q not registered in current generation", id1)
	}
}
