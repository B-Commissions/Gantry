package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// buildList makes a representative page: a heading, a counter, and a list
// of n rows. changedRow (>=0) marks one row as selected. counter and one
// row are what a typical update touches.
func buildList(counter, changedRow, n int) Node {
	rows := make([]Node, n)
	for i := range rows {
		label := fmt.Sprintf("Item number %d in the list", i)
		if i == changedRow {
			label = fmt.Sprintf("Item number %d in the list (selected)", i)
		}
		rows[i] = Row(
			Text(label),
			Button("Toggle", i),
		).WithKey(fmt.Sprintf("row-%d", i)).WithProps("gap", 8)
	}
	return Column(
		Heading("Inventory"),
		Textf("counter: %d", counter),
		Column(rows...).WithProps("gap", 4),
	)
}

// timeit runs f repeatedly for at least 60ms and returns ns/op.
func timeit(f func()) float64 {
	// warm up
	f()
	const budget = 60 * time.Millisecond
	iters := 0
	start := time.Now()
	for time.Since(start) < budget {
		f()
		iters++
	}
	elapsed := time.Since(start)
	if iters == 0 {
		iters = 1
	}
	return float64(elapsed.Nanoseconds()) / float64(iters)
}

// TestMeasureScaling measures, across a range of tree sizes, the server
// cost of a full render (startup / pre-3b per-update) vs a diffed patch
// (post-3b per-update), in both bytes and time. It also dumps each size's
// full tree and ops JSON so the JS side can time client apply on the same
// data. Writes a CSV to $GANTRY_MEASURE_DIR (or the OS temp dir).
//
//	go test ./ui/ -run TestMeasureScaling -v
func TestMeasureScaling(t *testing.T) {
	sizes := []int{10, 50, 100, 200, 400, 800, 1000, 2000, 4000, 8000, 16000, 32000}

	outDir := os.Getenv("GANTRY_MEASURE_DIR")
	if outDir == "" {
		outDir = t.TempDir()
	}
	_ = os.MkdirAll(filepath.Join(outDir, "trees"), 0o755)

	var csv []byte
	header := "n,full_bytes,patch_bytes,serialize_ns,diff_ns,full_marshal_ns,patch_marshal_ns,full_render_ns,patch_render_ns\n"
	csv = append(csv, header...)
	t.Log("n      full_B   patch_B   serialize_ns  diff_ns   fullMarshal_ns  patchMarshal_ns  fullRender_ns  patchRender_ns")

	for _, n := range sizes {
		treeV1 := buildList(0, -1, n)
		treeV2 := buildList(1, 3, n)

		p := &program{handlers: map[string]handlerFn{}}
		wireV1 := p.serialize(treeV1, "")
		p.handlers = map[string]handlerFn{}
		wireV2 := p.serialize(treeV2, "")

		var ops []patchOp
		diffTree(wireV1, wireV2, []int{}, &ops)

		fullMsg, _ := json.Marshal(renderMsg{T: "render", Seq: 2, Tree: wireV2})
		patchMsg2, _ := json.Marshal(patchMsg{T: "patch", Seq: 2, Ops: ops})

		serializeNs := timeit(func() {
			p.handlers = map[string]handlerFn{}
			_ = p.serialize(treeV2, "")
		})
		diffNs := timeit(func() {
			var o []patchOp
			diffTree(wireV1, wireV2, []int{}, &o)
		})
		fullMarshalNs := timeit(func() { _, _ = json.Marshal(renderMsg{T: "render", Seq: 2, Tree: wireV2}) })
		patchMarshalNs := timeit(func() { _, _ = json.Marshal(patchMsg{T: "patch", Seq: 2, Ops: ops}) })

		// A full render pays serialize + marshal-whole. A patch render pays
		// serialize + diff + marshal-small. serialize is common to both.
		fullRenderNs := serializeNs + fullMarshalNs
		patchRenderNs := serializeNs + diffNs + patchMarshalNs

		csv = append(csv, []byte(fmt.Sprintf("%d,%d,%d,%.0f,%.0f,%.0f,%.0f,%.0f,%.0f\n",
			n, len(fullMsg), len(patchMsg2), serializeNs, diffNs, fullMarshalNs, patchMarshalNs, fullRenderNs, patchRenderNs))...)
		t.Logf("%-6d %-8d %-9d %-13.0f %-9.0f %-15.0f %-16.0f %-14.0f %-.0f",
			n, len(fullMsg), len(patchMsg2), serializeNs, diffNs, fullMarshalNs, patchMarshalNs, fullRenderNs, patchRenderNs)

		// Dump the tree + ops for the JS client-side timing on identical data.
		_ = os.WriteFile(filepath.Join(outDir, "trees", fmt.Sprintf("full-%d.json", n)),
			mustJSON(wireV2), 0o644)
		_ = os.WriteFile(filepath.Join(outDir, "trees", fmt.Sprintf("ops-%d.json", n)),
			mustJSON(ops), 0o644)
	}

	path := filepath.Join(outDir, "scaling.csv")
	if err := os.WriteFile(path, csv, 0o644); err != nil {
		t.Fatalf("writing csv: %v", err)
	}
	t.Logf("CSV written to %s", path)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
