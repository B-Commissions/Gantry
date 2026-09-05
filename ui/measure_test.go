package ui

import (
	"encoding/json"
	"fmt"
	"testing"
)

// TestMeasurePatchVsFull reports how much smaller a diffed patch is than a
// full re-render for a representative page (a list plus a counter), where a
// typical update touches only one counter and one row. Run with:
//
//	go test ./ui/ -run TestMeasurePatchVsFull -v
func TestMeasurePatchVsFull(t *testing.T) {
	build := func(counter, changedRow, n int) Node {
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

	for _, n := range []int{10, 50, 200} {
		p := &program{handlers: map[string]handlerFn{}}
		v1 := p.serialize(build(0, -1, n), "")
		p.handlers = map[string]handlerFn{}
		v2 := p.serialize(build(1, 3, n), "") // counter++ and one row changes

		fullBytes, _ := json.Marshal(renderMsg{T: "render", Seq: 2, Tree: v2})
		var ops []patchOp
		diffTree(v1, v2, []int{}, &ops)
		patchBytes, _ := json.Marshal(patchMsg{T: "patch", Seq: 2, Ops: ops})

		reduction := 100 * (1 - float64(len(patchBytes))/float64(len(fullBytes)))
		t.Logf("n=%-3d  full=%6d B   patch=%5d B (%d ops)   wire reduction=%.1f%%",
			n, len(fullBytes), len(patchBytes), len(ops), reduction)
	}
}
