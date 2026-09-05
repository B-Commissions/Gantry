// Applying server render patches to the Tea wire tree. Kept dependency-
// free (only a type import from socket, erased at build) so it can be unit
// tested in isolation.
//
// The server sends a patch instead of the whole tree when it already has a
// base tree on this client (ui/program.go diffTree). Applying a patch
// rebuilds only the nodes on the path to each edit; every untouched subtree
// keeps its object identity, so React.memo(NodeView) skips re-rendering it.

import type { WireNode } from "./socket";

export type PatchOp =
  | { op: "replace"; path: number[]; node: WireNode }
  | {
      op: "set";
      path: number[];
      props?: Record<string, unknown>;
      handlers?: Record<string, string>;
    };

/** applyPatch returns a new tree with every op applied in order. */
export function applyPatch(tree: WireNode, ops: PatchOp[]): WireNode {
  let out = tree;
  for (const op of ops) out = editAt(out, op, 0);
  return out;
}

function editAt(node: WireNode, op: PatchOp, depth: number): WireNode {
  if (depth === op.path.length) {
    if (op.op === "replace") return op.node;
    // "set": new node object (so it re-renders), same children reference
    // (so unchanged descendants keep identity), replaced props/handlers.
    return { ...node, props: op.props, handlers: op.handlers };
  }
  const children = node.children;
  const idx = op.path[depth];
  if (!children || idx < 0 || idx >= children.length) return node; // defensive
  const child = children[idx];
  const newChild = editAt(child, op, depth + 1);
  if (newChild === child) return node;
  const next = children.slice();
  next[idx] = newChild;
  return { ...node, children: next };
}
