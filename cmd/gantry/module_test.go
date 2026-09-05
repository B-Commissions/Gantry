package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSplitSourceVersion(t *testing.T) {
	cases := []struct{ in, src, ver string }{
		{"github.com/BlueBeard63/WhiteGantry", "github.com/BlueBeard63/WhiteGantry", ""},
		{"github.com/BlueBeard63/WhiteGantry@v0.2.0", "github.com/BlueBeard63/WhiteGantry", "v0.2.0"},
		{"github.com/BlueBeard63/WhiteGantry@latest", "github.com/BlueBeard63/WhiteGantry", "latest"},
	}
	for _, c := range cases {
		src, ver := splitSourceVersion(c.in)
		if src != c.src || ver != c.ver {
			t.Errorf("splitSourceVersion(%q) = (%q,%q), want (%q,%q)", c.in, src, ver, c.src, c.ver)
		}
	}
}

func TestOwnerGlob(t *testing.T) {
	if got, want := ownerGlob("github.com/BlueBeard63/WhiteGantry"), "github.com/BlueBeard63/*"; got != want {
		t.Errorf("ownerGlob = %q, want %q", got, want)
	}
	if got, want := ownerGlob("example.com/mod"), "example.com/mod"; got != want {
		t.Errorf("ownerGlob short = %q, want %q", got, want)
	}
}

func TestMergeCSV(t *testing.T) {
	if got, want := mergeCSV("", "github.com/BlueBeard63/*"), "github.com/BlueBeard63/*"; got != want {
		t.Errorf("mergeCSV empty = %q, want %q", got, want)
	}
	if got, want := mergeCSV("a.com/*,b.com/*", "github.com/BlueBeard63/*"), "a.com/*,b.com/*,github.com/BlueBeard63/*"; got != want {
		t.Errorf("mergeCSV append = %q, want %q", got, want)
	}
	if got, want := mergeCSV("github.com/BlueBeard63/*", "github.com/BlueBeard63/*"), "github.com/BlueBeard63/*"; got != want {
		t.Errorf("mergeCSV dedup = %q, want %q", got, want)
	}
}

func TestProviderCandidates(t *testing.T) {
	got := providerCandidates("github.com/BlueBeard63/WhiteGantry")
	want := []string{
		"github.com/BlueBeard63/WhiteGantry/gantrymod",
		"github.com/BlueBeard63/WhiteGantry/cmd/WhiteGantry",
		"github.com/BlueBeard63/WhiteGantry",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("providerCandidates = %v, want %v", got, want)
	}
}

func TestLooksLikePrivateAccess(t *testing.T) {
	priv := []string{
		"fatal: could not read Username for 'https://github.com'",
		"remote: terminal prompts disabled",
		"x509: certificate signed by unknown authority",
		"dial tcp: i/o timeout",
	}
	for _, s := range priv {
		if !looksLikePrivateAccess(s) {
			t.Errorf("expected private-access for %q", s)
		}
	}
	notPriv := []string{
		"no matching versions for query \"latest\"",
		"cannot find module providing package .../gantrymod",
	}
	for _, s := range notPriv {
		if looksLikePrivateAccess(s) {
			t.Errorf("did not expect private-access for %q", s)
		}
	}
}

func testRegistry() moduleRegistry {
	return moduleRegistry{Modules: []moduleEntry{
		{Namespace: "whitegantry", Title: "WhiteGantry", Module: "github.com/BlueBeard63/WhiteGantry"},
		{Namespace: "widgets", Title: "Widgets Kit", Module: "github.com/other/widgets"},
	}}
}

func TestRegistryFind(t *testing.T) {
	reg := testRegistry()
	cases := map[string]string{
		"github.com/BlueBeard63/WhiteGantry": "whitegantry", // exact source
		"whitegantry":                        "whitegantry", // exact namespace
		"WhiteGantry":                        "whitegantry", // case-insensitive title
		"github.com/other/widgets":           "widgets",
		"widg":                               "widgets", // unambiguous prefix
	}
	for arg, wantNS := range cases {
		m, ok := reg.find(arg)
		if !ok || m.Namespace != wantNS {
			t.Errorf("find(%q) = (%q,%v), want namespace %q", arg, m.Namespace, ok, wantNS)
		}
	}
	if _, ok := reg.find("nope"); ok {
		t.Error("find(nope) should miss")
	}
}

func TestRegistryFindAmbiguousPrefix(t *testing.T) {
	reg := moduleRegistry{Modules: []moduleEntry{
		{Namespace: "wf-one", Module: "github.com/x/wf-one"},
		{Namespace: "wf-two", Module: "github.com/x/wf-two"},
	}}
	if _, ok := reg.find("wf-"); ok {
		t.Error("ambiguous prefix should not resolve")
	}
}

func TestUntarIntoContainsPaths(t *testing.T) {
	// Build a tar with a normal file, a nested file, and a traversal attempt.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	files := map[string]string{
		"notifications.md": "# Notifications",
		"guide/setup.md":   "# Setup",
		"../escape.md":     "should stay contained",
	}
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	tw.Close()

	dir := t.TempDir()
	if err := untarInto(&buf, dir); err != nil {
		t.Fatalf("untarInto: %v", err)
	}

	// The normal files land where expected.
	if b, _ := os.ReadFile(filepath.Join(dir, "notifications.md")); string(b) != "# Notifications" {
		t.Errorf("notifications.md content = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "guide", "setup.md")); string(b) != "# Setup" {
		t.Errorf("guide/setup.md content = %q", b)
	}
	// The traversal attempt is neutralized to a path INSIDE dir, never a parent.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.md")); err == nil {
		t.Error("traversal escaped the target directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.md")); err != nil {
		t.Errorf("traversal file should be contained inside dir: %v", err)
	}
}
