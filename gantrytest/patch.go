package gantrytest

import "encoding/json"

// patchOp mirrors ui.patchOp on the wire: one edit to the render tree.
// The driver materializes patches back into a full tree so the tree-based
// waiters (Tree, WaitTree, NextRender) work whether the server sent a full
// render or a diff.
type patchOp struct {
	Op       string          `json:"op"`
	Path     []int           `json:"path"`
	Node     json.RawMessage `json:"node,omitempty"`
	Props    json.RawMessage `json:"props,omitempty"`
	Handlers json.RawMessage `json:"handlers,omitempty"`
}

// applyPatchJSON applies ops to a full tree (both as JSON) and returns the
// new full tree. It mirrors applyPatch in web/src/patch.ts, but on decoded
// JSON since the driver only needs the resulting shape, not React identity.
func applyPatchJSON(tree json.RawMessage, opsRaw json.RawMessage) (json.RawMessage, error) {
	var root any
	if err := json.Unmarshal(tree, &root); err != nil {
		return nil, err
	}
	var ops []patchOp
	if err := json.Unmarshal(opsRaw, &ops); err != nil {
		return nil, err
	}
	for _, op := range ops {
		root = applyOneJSON(root, op, 0)
	}
	return json.Marshal(root)
}

func applyOneJSON(node any, op patchOp, depth int) any {
	if depth == len(op.Path) {
		if op.Op == "replace" {
			var n any
			_ = json.Unmarshal(op.Node, &n)
			return n
		}
		// "set": replace props and handlers wholesale (absent = cleared),
		// keeping the node's children.
		m, ok := node.(map[string]any)
		if !ok {
			return node
		}
		setOrDelete(m, "props", op.Props)
		setOrDelete(m, "handlers", op.Handlers)
		return m
	}
	m, ok := node.(map[string]any)
	if !ok {
		return node
	}
	children, _ := m["children"].([]any)
	idx := op.Path[depth]
	if idx < 0 || idx >= len(children) {
		return node
	}
	children[idx] = applyOneJSON(children[idx], op, depth+1)
	return m
}

func setOrDelete(m map[string]any, key string, raw json.RawMessage) {
	if len(raw) == 0 {
		delete(m, key)
		return
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		delete(m, key)
		return
	}
	m[key] = v
}
