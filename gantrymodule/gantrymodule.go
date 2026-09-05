// Package gantrymodule is the author-side helper for building a Gantry module:
// an out-of-tree Go module that extends Gantry (docs pages, a Go library, and
// later CLI subcommands). A module ships an installable provider command whose
// main is a few lines:
//
//	package main
//
//	import (
//	    "embed"
//	    "github.com/BlueBeard63/Gantry/gantrymodule"
//	)
//
//	//go:embed gantry-module.json
//	var manifest []byte
//
//	//go:embed all:docs
//	var docs embed.FS
//
//	func main() { gantrymodule.Main(manifest, docs) }
//
// The `gantry module install` command runs `go install` on that command, then
// drives it over a tiny reserved CLI contract to learn what the module provides
// and to pull its docs:
//
//	<bin> gantry manifest             print the (validated, normalized) manifest JSON
//	<bin> gantry docs emit            stream the docs tree as a tar of files
//	<bin> gantry docs emit --format=json   stream the docs tree as a {path: content} map
//
// Everything on the contract writes ONLY the payload to stdout so the CLI can
// consume it directly; diagnostics go to stderr. The package is pure Go and
// compiles on every platform (a module builds cross-platform, so its provider
// must too).
package gantrymodule

import (
	"archive/tar"
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
)

// docsRoot is the directory a module embeds its pages under, matching the
// `//go:embed all:docs` convention in the Main doc comment.
const docsRoot = "docs"

// namespaceRe is the allowed shape of a module namespace: it is also used
// verbatim as the docs route prefix, so it must be URL-path safe and stable.
var namespaceRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Manifest is the single description of a module's identity and capabilities.
// It is embedded in the provider binary as gantry-module.json and printed by
// the `gantry manifest` contract command. The CLI imports this same type, so
// the two halves never drift.
type Manifest struct {
	Schema    string   `json:"$schema,omitempty"`
	Namespace string   `json:"namespace"`         // unique id; also the docs route prefix
	Title     string   `json:"title"`             // display name
	Module    string   `json:"module"`            // the Go module path
	Version   string   `json:"version,omitempty"` // informational; the installed version is what the toolchain resolved
	Gantry    string   `json:"gantry,omitempty"`  // minimum framework version, e.g. ">=0.4.0"
	Provides  Provides `json:"provides"`
}

// Provides lists the capabilities a module contributes. Unknown kinds added in
// future manifests are ignored by older CLIs, so the format stays forward
// compatible.
type Provides struct {
	Docs *DocsCapability `json:"docs,omitempty"`
	Lib  *LibCapability  `json:"lib,omitempty"`
}

// DocsCapability declares a tree of markdown pages merged into `gantry docs`
// under Prefix. Prefix is always "/" + Namespace; it is filled in for you when
// omitted and rejected when it disagrees.
type DocsCapability struct {
	Prefix string `json:"prefix,omitempty"`
	Title  string `json:"title,omitempty"` // sidebar/category label; defaults to the module Title
}

// LibCapability declares an importable Go package apps depend on. It is
// informational and drives the optional "also add to this app's go.mod" prompt.
type LibCapability struct {
	Import  string `json:"import"`
	Summary string `json:"summary,omitempty"`
}

// ParseManifest unmarshals and validates a manifest, returning the normalized
// value (Prefix and docs Title defaulted). Both the provider helper and the
// gantry CLI use it, so a manifest that installs is a manifest that a provider
// would print.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("parsing manifest: %w", err)
	}
	if err := m.normalizeAndValidate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// normalizeAndValidate fills in defaults and enforces the invariants the rest
// of the system relies on. It mutates the receiver.
func (m *Manifest) normalizeAndValidate() error {
	m.Namespace = strings.TrimSpace(m.Namespace)
	m.Title = strings.TrimSpace(m.Title)
	m.Module = strings.TrimSpace(m.Module)

	if !namespaceRe.MatchString(m.Namespace) {
		return fmt.Errorf("namespace %q must match %s", m.Namespace, namespaceRe.String())
	}
	if m.Title == "" {
		return errors.New("title is required")
	}
	if m.Module == "" {
		return errors.New("module (the Go module path) is required")
	}
	if m.Provides.Docs == nil && m.Provides.Lib == nil {
		return errors.New("provides must declare at least one capability (docs or lib)")
	}

	if d := m.Provides.Docs; d != nil {
		want := "/" + m.Namespace
		if d.Prefix == "" {
			d.Prefix = want
		} else if d.Prefix != want {
			return fmt.Errorf("provides.docs.prefix %q must be %q (namespace)", d.Prefix, want)
		}
		if d.Title == "" {
			d.Title = m.Title
		}
	}
	if l := m.Provides.Lib; l != nil {
		if strings.TrimSpace(l.Import) == "" {
			return errors.New("provides.lib.import is required when lib is declared")
		}
	}
	return nil
}

// Marshal renders the manifest as indented JSON with a trailing newline - the
// exact bytes the `gantry manifest` command prints.
func (m Manifest) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Main is the provider entry point. It handles the reserved `gantry` subcommand
// group and exits; any other invocation prints a one-line identification and
// exits 0 (a human running the binary directly should not see an error). docs
// is the module's embed.FS following the `//go:embed all:docs` convention; a
// module with no docs may pass a zero embed.FS.
func Main(manifest []byte, docs embed.FS) {
	sub, err := fs.Sub(docs, docsRoot)
	if err != nil {
		// No docs/ directory embedded: a lib-only module. Serve an empty tree
		// so `docs emit` still succeeds with nothing.
		sub = emptyFS{}
	}
	err = run(manifest, sub, os.Args[1:], os.Stdout)
	switch {
	case errors.Is(err, errUsage):
		fmt.Fprintf(os.Stderr, "This is a Gantry module provider. It is managed by the 'gantry module' command.\n%s\n", err)
		os.Exit(0)
	case err != nil:
		fmt.Fprintln(os.Stderr, "gantry module provider:", err)
		os.Exit(1)
	}
}

// errUsage marks an invocation that is not part of the provider contract (bare
// run, or a non-"gantry" first argument) - informational, not a failure.
var errUsage = errors.New("usage: <provider> gantry {manifest | docs emit [--format=json]}")

// run dispatches the reserved contract. It writes only the requested payload to
// out; callers keep stdout clean for the gantry CLI to parse.
func run(manifest []byte, docs fs.FS, args []string, out io.Writer) error {
	if len(args) < 1 || args[0] != "gantry" {
		return errUsage
	}
	rest := args[1:]
	if len(rest) == 0 {
		return errUsage
	}
	switch rest[0] {
	case "manifest":
		m, err := ParseManifest(manifest)
		if err != nil {
			return err
		}
		b, err := m.Marshal()
		if err != nil {
			return err
		}
		_, err = out.Write(b)
		return err

	case "docs":
		if len(rest) < 2 || rest[1] != "emit" {
			return fmt.Errorf("unknown docs command %q (want: docs emit)", strings.Join(rest[1:], " "))
		}
		format := "tar"
		for _, a := range rest[2:] {
			switch {
			case a == "--format=json" || a == "-format=json":
				format = "json"
			case a == "--format=tar" || a == "-format=tar":
				format = "tar"
			default:
				return fmt.Errorf("unknown flag %q for docs emit", a)
			}
		}
		if format == "json" {
			return emitJSON(docs, out)
		}
		return emitTar(docs, out)

	default:
		return fmt.Errorf("unknown command %q under 'gantry'", rest[0])
	}
}

// collectDocs walks the docs tree and returns every regular file's path
// (relative, forward-slashed) and bytes, in lexical order. Markdown pages plus
// any assets (images, data) travel together so a page's images resolve.
func collectDocs(docsFS fs.FS) ([]string, map[string][]byte, error) {
	files := map[string][]byte{}
	var paths []string
	err := fs.WalkDir(docsFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || p == "." {
			return nil
		}
		b, err := fs.ReadFile(docsFS, p)
		if err != nil {
			return err
		}
		files[p] = b
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(paths)
	return paths, files, nil
}

func emitTar(docsFS fs.FS, out io.Writer) error {
	paths, files, err := collectDocs(docsFS)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(out)
	for _, p := range paths {
		body := files[p]
		// Fixed metadata (no mode/mtime) keeps the stream deterministic - the
		// CLI only cares about path and content.
		if err := tw.WriteHeader(&tar.Header{
			Name:     p,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			return err
		}
		if _, err := tw.Write(body); err != nil {
			return err
		}
	}
	return tw.Close()
}

func emitJSON(docsFS fs.FS, out io.Writer) error {
	paths, files, err := collectDocs(docsFS)
	if err != nil {
		return err
	}
	m := make(map[string]string, len(paths))
	for _, p := range paths {
		m[p] = string(files[p])
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// emptyFS is a stand-in docs tree for a module that embeds no docs/ directory.
type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) {
	if name == "." {
		return nil, &fs.PathError{Op: "open", Path: name, Err: errors.New("is a directory")}
	}
	return nil, fs.ErrNotExist
}

func (emptyFS) ReadDir(string) ([]fs.DirEntry, error) { return nil, nil }
