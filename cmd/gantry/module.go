package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/BlueBeard63/Gantry/gantrymodule"
	"github.com/charmbracelet/x/term"
)

// cmdModule manages Gantry modules: out-of-tree Go modules that extend the
// framework (docs pages merged into `gantry docs`, a Go library apps import,
// and later CLI subcommands). See design/modules-and-docs.md.
//
//	gantry module install <source>[@version]   go install + register + cache docs
//	gantry module list                          list installed modules
//	gantry module uninstall <name>              remove a module and its docs
//	gantry module update [<name>]               reinstall at @latest
//	gantry module info <name>                   show one module's details
func cmdModule(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gantry module <install|list|uninstall|update|info> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "install", "add":
		return cmdModuleInstall(rest)
	case "list", "ls":
		return cmdModuleList(rest)
	case "uninstall", "remove", "rm":
		return cmdModuleUninstall(rest)
	case "update", "upgrade":
		return cmdModuleUpdate(rest)
	case "info", "show":
		return cmdModuleInfo(rest)
	case "help", "-h", "--help":
		info("gantry module <install|list|uninstall|update|info>")
		return nil
	default:
		return fmt.Errorf("unknown module command %q (want install|list|uninstall|update|info)", sub)
	}
}

// --- registry ---------------------------------------------------------------

// moduleRegistry is the durable record of installed modules, at
// <config>/gantry/modules.json. It is the single source of truth for list,
// uninstall, and the docs merge.
type moduleRegistry struct {
	Modules []moduleEntry `json:"modules"`
}

type moduleEntry struct {
	Namespace   string   `json:"namespace"`
	Title       string   `json:"title"`
	Module      string   `json:"module"`
	Version     string   `json:"version"`
	Binary      string   `json:"binary"`
	Provides    []string `json:"provides"`
	DocsPrefix  string   `json:"docsPrefix,omitempty"`
	LibImport   string   `json:"libImport,omitempty"`
	InstalledAt string   `json:"installedAt"`
}

func moduleRegistryPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "gantry", "modules.json"), nil
}

// modulesCacheDir is <cache>/gantry/modules: one subdir per <namespace>@<version>
// holding the provider binary (bin/) and the emitted docs (docs/).
func modulesCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "gantry", "modules"), nil
}

func loadRegistry() (moduleRegistry, error) {
	var reg moduleRegistry
	p, err := moduleRegistryPath()
	if err != nil {
		return reg, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return reg, nil // no modules installed yet
	}
	if err != nil {
		return reg, err
	}
	if err := json.Unmarshal(data, &reg); err != nil {
		return reg, fmt.Errorf("parsing %s: %w", p, err)
	}
	return reg, nil
}

func saveRegistry(reg moduleRegistry) error {
	p, err := moduleRegistryPath()
	if err != nil {
		return err
	}
	sort.Slice(reg.Modules, func(i, j int) bool { return reg.Modules[i].Namespace < reg.Modules[j].Namespace })
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0o600)
}

func (reg moduleRegistry) find(arg string) (moduleEntry, bool) {
	a := strings.TrimSpace(arg)
	// 1. exact source, 2. exact namespace, 3. case-insensitive title.
	for _, m := range reg.Modules {
		if m.Module == a {
			return m, true
		}
	}
	for _, m := range reg.Modules {
		if m.Namespace == a {
			return m, true
		}
	}
	for _, m := range reg.Modules {
		if strings.EqualFold(m.Title, a) {
			return m, true
		}
	}
	// 4. unambiguous prefix of source or namespace.
	var hits []moduleEntry
	for _, m := range reg.Modules {
		if strings.HasPrefix(m.Module, a) || strings.HasPrefix(m.Namespace, a) {
			hits = append(hits, m)
		}
	}
	if len(hits) == 1 {
		return hits[0], true
	}
	return moduleEntry{}, false
}

// --- install ----------------------------------------------------------------

func cmdModuleInstall(args []string) error {
	fs := newFlagSet("module install")
	versionFlag := fs.String("version", "", "version to install (alternative to @version in the source)")
	project := fs.Bool("project", false, "also add the library to the nearest app's go.mod without prompting")
	noProject := fs.Bool("no-project", false, "never touch the nearest app's go.mod")
	reinstall := fs.Bool("reinstall", false, "reinstall even if the same version is already registered")
	noGoprivate := fs.Bool("no-goprivate", false, "do not offer to add the module to GOPRIVATE on a private-access failure")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: gantry module install <module>[@version] | <local-dir>")
	}
	pm := projectMode(*project, *noProject)

	// A local module directory builds from source (honoring go.mod replace), so
	// authors can install and preview a module before publishing it.
	if isLocalSource(fs.Arg(0)) {
		return installModule(installOpts{
			source:      fs.Arg(0),
			projectMode: pm,
			reinstall:   *reinstall,
			local:       true,
		})
	}

	source, reqVersion := splitSourceVersion(fs.Arg(0))
	if *versionFlag != "" {
		reqVersion = *versionFlag
	}
	if reqVersion == "" {
		reqVersion = "latest"
	}
	return installModule(installOpts{
		source:         source,
		reqVersion:     reqVersion,
		projectMode:    pm,
		reinstall:      *reinstall,
		offerGoprivate: !*noGoprivate,
	})
}

type installOpts struct {
	source         string
	reqVersion     string
	projectMode    string // "prompt" | "yes" | "no"
	reinstall      bool
	offerGoprivate bool
	local          bool // source is a local module directory (go build, honors replace)
}

func installModule(o installOpts) error {
	cacheDir, err := modulesCacheDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return err
	}

	// Stage the provider binary into our own dir (GOBIN override) so it never
	// collides with the user's tools or other modules, and uninstall is a
	// directory delete.
	staging, err := os.MkdirTemp(cacheDir, ".staging-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	var stagedBin string
	if o.local {
		stagedBin, err = goBuildLocalProvider(o.source, staging)
	} else {
		stagedBin, err = goInstallProvider(o, staging)
	}
	if err != nil {
		return err
	}

	// Learn what the module is by driving the reserved provider contract.
	manifest, err := providerManifest(stagedBin)
	if err != nil {
		return fmt.Errorf("installed %s but it is not a Gantry module: %w", o.source, err)
	}
	// The real import path comes from the manifest: for a remote install it should
	// match the source; for a local build the source is a directory.
	moduleID := manifest.Module
	if moduleID == "" {
		moduleID = o.source
	}
	if !o.local && manifest.Module != "" && manifest.Module != o.source {
		warn("manifest module path %q differs from the installed source %q", manifest.Module, o.source)
	}

	version := manifest.Version
	if !o.local {
		version = resolveVersion(o.source, o.reqVersion, manifest.Version)
	} else if version == "" {
		version = "(local)"
	}

	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	for _, m := range reg.Modules {
		if m.Namespace == manifest.Namespace && m.Module != moduleID {
			return fmt.Errorf("namespace %q is already used by %s - uninstall it first", manifest.Namespace, m.Module)
		}
	}
	if !o.reinstall {
		for _, m := range reg.Modules {
			if m.Namespace == manifest.Namespace && m.Version == version {
				success("%s %s is already installed (use --reinstall to force)", manifest.Title, version)
				return maybeAddToProject(o.projectMode, manifest, version)
			}
		}
	}

	// Lay down the final <namespace>@<version> tree: docs first (from the
	// staged binary), then move the binary in.
	finalDir := filepath.Join(cacheDir, manifest.Namespace+"@"+version)
	if err := os.RemoveAll(finalDir); err != nil {
		return err
	}
	if manifest.Provides.Docs != nil {
		if err := emitDocsInto(stagedBin, filepath.Join(finalDir, "docs")); err != nil {
			return fmt.Errorf("pulling docs: %w", err)
		}
	}
	binDir := filepath.Join(finalDir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return err
	}
	finalBin := filepath.Join(binDir, filepath.Base(stagedBin))
	if err := os.Rename(stagedBin, finalBin); err != nil {
		return fmt.Errorf("placing provider binary: %w", err)
	}
	// Snapshot the manifest beside the tree for offline inspection.
	if mb, err := manifest.Marshal(); err == nil {
		_ = os.WriteFile(filepath.Join(finalDir, "manifest.json"), mb, 0o644)
	}

	// Upsert the registry, dropping any older version dirs for this namespace.
	provides := manifestProvides(manifest)
	entry := moduleEntry{
		Namespace:   manifest.Namespace,
		Title:       manifest.Title,
		Module:      moduleID,
		Version:     version,
		Binary:      finalBin,
		Provides:    provides,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}
	if manifest.Provides.Docs != nil {
		entry.DocsPrefix = manifest.Provides.Docs.Prefix
	}
	if manifest.Provides.Lib != nil {
		entry.LibImport = manifest.Provides.Lib.Import
	}
	pruneOldVersions(cacheDir, manifest.Namespace, version)
	reg.Modules = upsert(reg.Modules, entry)
	if err := saveRegistry(reg); err != nil {
		return err
	}

	success("installed %s %s (namespace %q, provides %s)", manifest.Title, version, manifest.Namespace, strings.Join(provides, "+"))
	if manifest.Provides.Docs != nil {
		info("its docs are now served under %s in `gantry docs` and `gantry docs --mcp` (restart a running server to pick them up)", manifest.Provides.Docs.Prefix)
	}
	return maybeAddToProject(o.projectMode, manifest, version)
}

// goInstallProvider tries the provider-command locations in order and installs
// the first that exists into staging (GOBIN override). It returns the staged
// binary path. On a private-access failure it offers to add the module to
// GOPRIVATE and retries once.
func goInstallProvider(o installOpts, staging string) (string, error) {
	candidates := providerCandidates(o.source)
	env := append(os.Environ(), "GOBIN="+staging)

	var lastErr error
	triedGoprivate := false
	for i := 0; i < len(candidates); i++ {
		pkg := candidates[i]
		step("go install %s@%s", pkg, o.reqVersion)
		out, err := runGo(staging, env, "install", pkg+"@"+o.reqVersion)
		if err == nil {
			bin, ferr := singleFileIn(staging)
			if ferr != nil {
				return "", ferr
			}
			return bin, nil
		}
		lastErr = fmt.Errorf("%v\n%s", err, strings.TrimSpace(out))

		if looksLikePrivateAccess(out) {
			if o.offerGoprivate && !triedGoprivate {
				if offerGoprivate(o.source) {
					triedGoprivate = true
					env = append(os.Environ(), "GOBIN="+staging) // re-read env after go env -w
					i--                                          // retry the same candidate
					continue
				}
			}
			return "", fmt.Errorf("could not fetch %s (private repo or network): %w\n%s", o.source, err,
				"fix: ensure git auth (SSH insteadOf, ~/.netrc, or a credential helper) and that GOPRIVATE covers the owner")
		}
		// Otherwise assume this candidate path just does not exist; try the next.
	}
	return "", fmt.Errorf("no provider command found for %s (looked at %s): %w",
		o.source, strings.Join(providerSuffixes(), ", "), lastErr)
}

// providerCandidates is where a module's installable provider command may live,
// most-specific first. The documented convention is <module>/gantrymod.
func providerCandidates(source string) []string {
	base := path.Base(source)
	return []string{
		source + "/gantrymod",
		source + "/cmd/" + base,
		source, // module root is main (tool-only modules)
	}
}

func providerSuffixes() []string { return []string{"/gantrymod", "/cmd/<name>", " (root)"} }

// isLocalSource reports whether the install argument names a local module
// directory rather than a remote module path.
func isLocalSource(s string) bool {
	if s == "." || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") ||
		strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") {
		return true
	}
	fi, err := os.Stat(s)
	return err == nil && fi.IsDir()
}

// goBuildLocalProvider builds the provider from a local module directory into
// staging, honoring the module's go.mod (including replace directives) so an
// author can install a module before it is published. Same candidate order as a
// remote install.
func goBuildLocalProvider(dir, staging string) (string, error) {
	abs, err := filepath.Abs(expandHome(dir))
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(abs, "go.mod")); err != nil {
		return "", fmt.Errorf("%s is not a Go module (no go.mod)", abs)
	}
	out := filepath.Join(staging, "provider")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	candidates := []string{"./gantrymod", "./cmd/" + filepath.Base(abs), "."}
	var lastErr error
	for _, rel := range candidates {
		if rel != "." {
			if _, err := os.Stat(filepath.Join(abs, filepath.FromSlash(strings.TrimPrefix(rel, "./")))); err != nil {
				continue
			}
		}
		step("go build %s (in %s)", rel, abs)
		if outText, err := runGo(abs, os.Environ(), "build", "-o", out, rel); err == nil {
			return out, nil
		} else {
			lastErr = fmt.Errorf("%v\n%s", err, strings.TrimSpace(outText))
		}
	}
	return "", fmt.Errorf("no buildable provider command in %s (tried gantrymod, cmd/<name>, root): %w", abs, lastErr)
}

// expandHome resolves a leading ~ to the user's home directory.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

// --- provider contract ------------------------------------------------------

func providerManifest(bin string) (gantrymodule.Manifest, error) {
	out, err := runGoRaw(bin, "gantry", "manifest")
	if err != nil {
		return gantrymodule.Manifest{}, fmt.Errorf("running `%s gantry manifest`: %w", filepath.Base(bin), err)
	}
	return gantrymodule.ParseManifest(out)
}

func emitDocsInto(bin, docsDir string) error {
	out, err := runGoRaw(bin, "gantry", "docs", "emit")
	if err != nil {
		return err
	}
	return untarInto(bytes.NewReader(out), docsDir)
}

// --- helpers ----------------------------------------------------------------

func splitSourceVersion(s string) (source, version string) {
	if i := strings.LastIndex(s, "@"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

func projectMode(project, noProject bool) string {
	switch {
	case project:
		return "yes"
	case noProject:
		return "no"
	default:
		return "prompt"
	}
}

func manifestProvides(m gantrymodule.Manifest) []string {
	var p []string
	if m.Provides.Docs != nil {
		p = append(p, "docs")
	}
	if m.Provides.Lib != nil {
		p = append(p, "lib")
	}
	return p
}

func upsert(list []moduleEntry, e moduleEntry) []moduleEntry {
	for i, m := range list {
		if m.Namespace == e.Namespace {
			list[i] = e
			return list
		}
	}
	return append(list, e)
}

// pruneOldVersions removes cached <namespace>@* dirs other than keepVersion.
func pruneOldVersions(cacheDir, namespace, keepVersion string) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	keep := namespace + "@" + keepVersion
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), namespace+"@") && e.Name() != keep {
			_ = os.RemoveAll(filepath.Join(cacheDir, e.Name()))
		}
	}
}

// resolveVersion asks the toolchain for the version it actually resolved; a
// concrete request is trusted as-is, and manifestVersion/req are fallbacks.
func resolveVersion(source, req, manifestVersion string) string {
	if releaseTagRe.MatchString(req) {
		return req
	}
	out, err := runGo("", os.Environ(), "list", "-m", "-f", "{{.Version}}", source+"@"+req)
	if v := strings.TrimSpace(out); err == nil && v != "" {
		return v
	}
	if manifestVersion != "" {
		return manifestVersion
	}
	return req
}

// singleFileIn returns the one regular file in dir (the just-installed binary).
func singleFileIn(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", errors.New("go install produced no binary")
}

// runGo runs the go toolchain, returning combined output. dir/env may be empty.
func runGo(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runGoRaw runs an arbitrary binary and returns its raw stdout (stderr is
// surfaced only in the error), for the provider contract where stdout is data.
func runGoRaw(bin string, args ...string) ([]byte, error) {
	cmd := exec.Command(bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func looksLikePrivateAccess(out string) bool {
	o := strings.ToLower(out)
	for _, s := range []string{
		"terminal prompts disabled", "authentication failed", "could not read username",
		"403 forbidden", "permission denied", "x509", "tls handshake", "i/o timeout",
		"connection refused", "no such host", "fatal: could not read",
	} {
		if strings.Contains(o, s) {
			return true
		}
	}
	return false
}

// ownerGlob is the GOPRIVATE pattern for a module: host/owner/*.
func ownerGlob(source string) string {
	parts := strings.Split(source, "/")
	if len(parts) >= 3 {
		return parts[0] + "/" + parts[1] + "/*"
	}
	return source
}

func goprivateCovers(source string) bool {
	cur, _ := runGo("", os.Environ(), "env", "GOPRIVATE")
	cur = strings.TrimSpace(cur)
	if cur == "" {
		return false
	}
	for _, pat := range strings.Split(cur, ",") {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		prefix := strings.TrimSuffix(pat, "*")
		prefix = strings.TrimSuffix(prefix, "/")
		if pat == source || strings.HasPrefix(source, prefix) {
			return true
		}
	}
	return false
}

// offerGoprivate asks to add the module's owner glob to GOPRIVATE and, on yes,
// persists it with `go env -w`. Returns whether it was added.
func offerGoprivate(source string) bool {
	if goprivateCovers(source) {
		return false // already covered; the failure is not GOPRIVATE
	}
	glob := ownerGlob(source)
	if !isTTY() {
		info("set GOPRIVATE to fetch a private module: go env -w GOPRIVATE=%s (merge with any existing)", glob)
		return false
	}
	in := bufio.NewReader(os.Stdin)
	if !askYesNo(in, fmt.Sprintf("Add %s to GOPRIVATE so go can fetch it privately?", glob), true) {
		return false
	}
	cur, _ := runGo("", os.Environ(), "env", "GOPRIVATE")
	merged := mergeCSV(strings.TrimSpace(cur), glob)
	step("go env -w GOPRIVATE=%s", merged)
	if out, err := runGo("", os.Environ(), "env", "-w", "GOPRIVATE="+merged); err != nil {
		warn("could not set GOPRIVATE: %v\n%s", err, strings.TrimSpace(out))
		return false
	}
	return true
}

func mergeCSV(existing, add string) string {
	seen := map[string]bool{}
	var out []string
	for _, p := range strings.Split(existing, ",") {
		if p = strings.TrimSpace(p); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if !seen[add] {
		out = append(out, add)
	}
	return strings.Join(out, ",")
}

func isTTY() bool { return term.IsTerminal(uintptr(os.Stdin.Fd())) }

// untarInto extracts a provider docs tar into dir, rejecting path escapes.
func untarInto(r io.Reader, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		clean := path.Clean("/" + h.Name)
		if strings.Contains(clean, "..") {
			return fmt.Errorf("unsafe path in docs tar: %q", h.Name)
		}
		dest := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
		if h.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
}

// maybeAddToProject offers (or, in "yes"/"no" mode, decides) whether to add the
// module's library to the nearest app's go.mod.
func maybeAddToProject(mode string, m gantrymodule.Manifest, version string) error {
	if m.Provides.Lib == nil || mode == "no" {
		return nil
	}
	appDir, _, err := findApp()
	if err != nil {
		return nil // not inside an app; nothing to do
	}
	imp := m.Provides.Lib.Import
	if mode == "prompt" {
		if !isTTY() {
			return nil
		}
		in := bufio.NewReader(os.Stdin)
		if !askYesNo(in, fmt.Sprintf("Also add %s to this app's go.mod?", imp), false) {
			return nil
		}
	}
	step("go get %s@%s", imp, version)
	if out, err := runGo(appDir, os.Environ(), "get", imp+"@"+version); err != nil {
		warn("go get failed: %v\n%s", err, strings.TrimSpace(out))
		return nil
	}
	success("added %s to %s", imp, filepath.Join(appDir, "go.mod"))
	return nil
}

// --- list -------------------------------------------------------------------

func cmdModuleList(args []string) error {
	fs := newFlagSet("module list")
	asJSON := fs.Bool("json", false, "print the registry as JSON")
	outdated := fs.Bool("outdated", false, "only show modules with a newer version available")
	verbose := fs.Bool("v", false, "include binary path, install time and docs prefix")
	if err := fs.Parse(args); err != nil {
		return err
	}
	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	if *asJSON {
		b, _ := json.MarshalIndent(reg, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	if len(reg.Modules) == 0 {
		info("no modules installed - add one with: gantry module install <source>")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	if *verbose {
		fmt.Fprintln(tw, "NAMESPACE\tTITLE\tSOURCE\tVERSION\tCAPS\tUPDATE\tPREFIX\tINSTALLED")
	} else {
		fmt.Fprintln(tw, "NAMESPACE\tTITLE\tSOURCE\tVERSION\tCAPS\tUPDATE")
	}
	for _, m := range reg.Modules {
		update := ""
		if latest := latestOfModule(m.Module); latest != "" && semverLess(m.Version, latest) {
			update = latest
		}
		if *outdated && update == "" {
			continue
		}
		caps := strings.Join(m.Provides, ",")
		if *verbose {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				m.Namespace, m.Title, m.Module, m.Version, caps, dash(update), m.DocsPrefix, m.InstalledAt)
		} else {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				m.Namespace, m.Title, m.Module, m.Version, caps, dash(update))
		}
	}
	return tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// latestOfModule best-effort asks the module proxy for a module's newest
// version; silent on any failure (offline, private, unpublished).
func latestOfModule(modPath string) string {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("https://proxy.golang.org/" + proxyEscape(modPath) + "/@latest")
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return ""
	}
	defer resp.Body.Close()
	var info struct{ Version string }
	if json.NewDecoder(resp.Body).Decode(&info) != nil {
		return ""
	}
	return info.Version
}

// --- uninstall --------------------------------------------------------------

func cmdModuleUninstall(args []string) error {
	fs := newFlagSet("module uninstall")
	project := fs.Bool("project", false, "also drop the library from the nearest app's go.mod without prompting")
	noProject := fs.Bool("no-project", false, "never touch the nearest app's go.mod")
	asJSON := fs.Bool("json", false, "print the removed module as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: gantry module uninstall <name | namespace | source>")
	}
	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	m, ok := reg.find(fs.Arg(0))
	if !ok {
		return fmt.Errorf("no installed module matches %q (see: gantry module list)", fs.Arg(0))
	}

	// Drop the registry entry and delete every cached version dir.
	reg.Modules = removeByNamespace(reg.Modules, m.Namespace)
	if err := saveRegistry(reg); err != nil {
		return err
	}
	if cacheDir, err := modulesCacheDir(); err == nil {
		pruneOldVersions(cacheDir, m.Namespace, "\x00") // keep nothing
	}

	if err := maybeRemoveFromProject(projectMode(*project, *noProject), m); err != nil {
		return err
	}
	if *asJSON {
		b, _ := json.MarshalIndent(m, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	success("uninstalled %s (%s)", m.Title, m.Module)
	return nil
}

func removeByNamespace(list []moduleEntry, ns string) []moduleEntry {
	out := list[:0]
	for _, m := range list {
		if m.Namespace != ns {
			out = append(out, m)
		}
	}
	return out
}

func maybeRemoveFromProject(mode string, m moduleEntry) error {
	if m.LibImport == "" || mode == "no" {
		return nil
	}
	appDir, _, err := findApp()
	if err != nil {
		return nil
	}
	// Only offer when the app actually requires it.
	gomod, err := os.ReadFile(filepath.Join(appDir, "go.mod"))
	if err != nil || !strings.Contains(string(gomod), m.LibImport) {
		return nil
	}
	if mode == "prompt" {
		if !isTTY() {
			return nil
		}
		in := bufio.NewReader(os.Stdin)
		if !askYesNo(in, fmt.Sprintf("Also drop %s from this app's go.mod?", m.LibImport), false) {
			return nil
		}
	}
	step("go get %s@none && go mod tidy", m.LibImport)
	if out, err := runGo(appDir, os.Environ(), "get", m.LibImport+"@none"); err != nil {
		warn("go get @none failed: %v\n%s", err, strings.TrimSpace(out))
		return nil
	}
	if out, err := runGo(appDir, os.Environ(), "mod", "tidy"); err != nil {
		warn("go mod tidy failed: %v\n%s", err, strings.TrimSpace(out))
	}
	return nil
}

// --- update / info ----------------------------------------------------------

func cmdModuleUpdate(args []string) error {
	fs := newFlagSet("module update")
	if err := fs.Parse(args); err != nil {
		return err
	}
	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	var targets []moduleEntry
	if fs.NArg() == 1 {
		m, ok := reg.find(fs.Arg(0))
		if !ok {
			return fmt.Errorf("no installed module matches %q", fs.Arg(0))
		}
		targets = []moduleEntry{m}
	} else {
		targets = reg.Modules
	}
	if len(targets) == 0 {
		info("no modules to update")
		return nil
	}
	for _, m := range targets {
		if err := installModule(installOpts{
			source: m.Module, reqVersion: "latest", projectMode: "no",
			reinstall: true, offerGoprivate: true,
		}); err != nil {
			warn("updating %s: %v", m.Module, err)
		}
	}
	return nil
}

func cmdModuleInfo(args []string) error {
	fs := newFlagSet("module info")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: gantry module info <name | namespace | source>")
	}
	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	m, ok := reg.find(fs.Arg(0))
	if !ok {
		return fmt.Errorf("no installed module matches %q", fs.Arg(0))
	}
	fmt.Printf("%s (%s)\n", m.Title, m.Namespace)
	fmt.Printf("  module:    %s\n", m.Module)
	fmt.Printf("  version:   %s\n", m.Version)
	fmt.Printf("  provides:  %s\n", strings.Join(m.Provides, ", "))
	if m.DocsPrefix != "" {
		fmt.Printf("  docs:      %s\n", m.DocsPrefix)
	}
	if m.LibImport != "" {
		fmt.Printf("  import:    %s\n", m.LibImport)
	}
	fmt.Printf("  binary:    %s\n", m.Binary)
	fmt.Printf("  installed: %s\n", m.InstalledAt)
	return nil
}

// newFlagSet mirrors the rest of the CLI's flag handling (ExitOnError).
func newFlagSet(name string) *flag.FlagSet { return flag.NewFlagSet(name, flag.ExitOnError) }
