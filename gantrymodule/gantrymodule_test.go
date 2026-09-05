package gantrymodule

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"testing/fstest"
)

const validManifest = `{
  "namespace": "whitegantry",
  "title": "WhiteGantry",
  "module": "github.com/BlueBeard63/WhiteGantry",
  "version": "v0.2.0",
  "gantry": ">=0.4.0",
  "provides": {
    "docs": {},
    "lib": { "import": "github.com/BlueBeard63/WhiteGantry", "summary": "WhiteFlower bindings" }
  }
}`

func TestParseManifestNormalizes(t *testing.T) {
	m, err := ParseManifest([]byte(validManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Provides.Docs == nil {
		t.Fatal("docs capability lost")
	}
	if got, want := m.Provides.Docs.Prefix, "/whitegantry"; got != want {
		t.Errorf("prefix = %q, want %q (defaulted from namespace)", got, want)
	}
	if got, want := m.Provides.Docs.Title, "WhiteGantry"; got != want {
		t.Errorf("docs title = %q, want %q (defaulted from module title)", got, want)
	}
}

func TestParseManifestRejects(t *testing.T) {
	cases := map[string]string{
		"bad namespace":      `{"namespace":"White Gantry","title":"X","module":"m","provides":{"lib":{"import":"m"}}}`,
		"missing title":      `{"namespace":"wg","module":"m","provides":{"lib":{"import":"m"}}}`,
		"no capabilities":    `{"namespace":"wg","title":"X","module":"m","provides":{}}`,
		"prefix mismatch":    `{"namespace":"wg","title":"X","module":"m","provides":{"docs":{"prefix":"/other"}}}`,
		"lib without import": `{"namespace":"wg","title":"X","module":"m","provides":{"lib":{}}}`,
		"unknown field":      `{"namespace":"wg","title":"X","module":"m","nope":1,"provides":{"lib":{"import":"m"}}}`,
	}
	for name, src := range cases {
		if _, err := ParseManifest([]byte(src)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func docsTree() fstest.MapFS {
	return fstest.MapFS{
		"notifications.md":   {Data: []byte("# Notifications\n\nbody")},
		"permissions.md":     {Data: []byte("# Permissions\n\nbody")},
		"guide/setup.md":     {Data: []byte("# Setup\n\nnested")},
		"assets/diagram.txt": {Data: []byte("not-a-page-but-travels")},
	}
}

func TestRunManifest(t *testing.T) {
	var out bytes.Buffer
	if err := run([]byte(validManifest), docsTree(), []string{"gantry", "manifest"}, &out); err != nil {
		t.Fatalf("run manifest: %v", err)
	}
	// The printed manifest must itself parse and be normalized.
	m, err := ParseManifest(out.Bytes())
	if err != nil {
		t.Fatalf("printed manifest does not round-trip: %v", err)
	}
	if m.Namespace != "whitegantry" {
		t.Errorf("namespace = %q", m.Namespace)
	}
}

func TestRunEmitTarRoundTrip(t *testing.T) {
	tree := docsTree()
	var out bytes.Buffer
	if err := run([]byte(validManifest), tree, []string{"gantry", "docs", "emit"}, &out); err != nil {
		t.Fatalf("run docs emit: %v", err)
	}

	got := map[string]string{}
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar body: %v", err)
		}
		got[h.Name] = string(b)
	}

	if len(got) != len(tree) {
		t.Fatalf("emitted %d files, want %d", len(got), len(tree))
	}
	for name, f := range tree {
		if got[name] != string(f.Data) {
			t.Errorf("%s: content mismatch\n got %q\nwant %q", name, got[name], f.Data)
		}
	}
}

func TestRunEmitJSON(t *testing.T) {
	var out bytes.Buffer
	if err := run([]byte(validManifest), docsTree(), []string{"gantry", "docs", "emit", "--format=json"}, &out); err != nil {
		t.Fatalf("run docs emit json: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("emit json not valid: %v", err)
	}
	if got["guide/setup.md"] != "# Setup\n\nnested" {
		t.Errorf("nested page missing/wrong: %q", got["guide/setup.md"])
	}
}

func TestRunUsage(t *testing.T) {
	var out bytes.Buffer
	for _, args := range [][]string{{}, {"help"}, {"gantry"}} {
		out.Reset()
		err := run([]byte(validManifest), docsTree(), args, &out)
		if !errors.Is(err, errUsage) {
			t.Errorf("args %v: got %v, want errUsage", args, err)
		}
		if out.Len() != 0 {
			t.Errorf("args %v: wrote %q to stdout, want clean", args, out.String())
		}
	}
}

func TestRunUnknownFlag(t *testing.T) {
	var out bytes.Buffer
	err := run([]byte(validManifest), docsTree(), []string{"gantry", "docs", "emit", "--zip"}, &out)
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
}
