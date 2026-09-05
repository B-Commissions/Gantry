package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"flag"
	"fmt"
	"image"
	_ "image/png"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/tc-hib/winres"
)

// buildTarget is one os/arch build. OS uses gantry's names: windows,
// linux, mac (mapped to GOOS darwin).
type buildTarget struct {
	OS   string
	Arch string
}

func (t buildTarget) goos() string {
	if t.OS == "mac" {
		return "darwin"
	}
	return t.OS
}

// cmdBuild builds the frontend once, then every configured target into
// dist/<os>/<arch>/, optionally packaging installers.
func cmdBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	console := fs.Bool("console", false, "keep the console window on Windows builds (debug)")
	targetsFlag := fs.String("targets", "", "comma list of os/arch targets (overrides gantry.json), e.g. windows/amd64,linux/amd64")
	installer := fs.Bool("installer", false, "also produce install artifacts (overrides gantry.json)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	appDir, cfg, err := findApp()
	if err != nil {
		return err
	}
	if err := prepareApp(appDir, cfg); err != nil {
		return err
	}

	targets, err := resolveTargets(*targetsFlag, cfg)
	if err != nil {
		return err
	}
	makeInstaller := *installer || cfg.Build.Installer
	useConsole := *console || cfg.Build.Console

	// Android arches build together (one multi-ABI APK), so collect
	// mobile targets out of the per-target loop.
	var androidArches, iosArches []string
	built := 0
	for _, t := range targets {
		switch {
		case t.OS == "android":
			androidArches = appendUnique(androidArches, t.Arch)
			continue
		case t.OS == "ios":
			iosArches = appendUnique(iosArches, t.Arch)
			continue
		case t.goos() == "linux" && runtime.GOOS != "linux":
			warn("skipping %s/%s - Linux builds need a Linux machine (WSL works: run gantry build there)", t.OS, t.Arch)
			continue
		}
		if err := buildOne(appDir, cfg, t, useConsole, makeInstaller); err != nil {
			return err
		}
		built++
	}
	if len(androidArches) > 0 {
		ok, err := buildAndroid(appDir, cfg, androidArches)
		if err != nil {
			return err
		}
		if ok {
			built++
		}
	}
	if len(iosArches) > 0 {
		ok, err := buildIOS(appDir, cfg, iosArches)
		if err != nil {
			return err
		}
		if ok {
			built++
		}
	}
	if built == 0 {
		return fmt.Errorf("no targets were built")
	}
	success("done - %s", filepath.Join(appDir, "dist"))
	return nil
}

// prepareApp regenerates the synth dir + generated files and runs the
// vite build: everything a target build needs before go compiles.
func prepareApp(appDir string, cfg appConfig) error {
	synthDir, err := writeSynth(appDir, cfg)
	if err != nil {
		return err
	}
	if err := writeRegistry(appDir); err != nil {
		return err
	}
	if err := writeWidgetRegistry(appDir, cfg); err != nil {
		return err
	}
	if err := writeIcons(appDir, cfg); err != nil {
		return err
	}
	if err := writeResources(appDir); err != nil {
		return err
	}
	if err := validateArgSpecs(cfg); err != nil {
		return fmt.Errorf("gantry.json: %w", err)
	}
	if err := writeArgsRegistry(appDir, cfg); err != nil {
		return err
	}

	step("building frontend (vite)")
	vite := exec.Command(npx(), "vite", "build")
	vite.Dir = synthDir
	vite.Stdout = os.Stdout
	vite.Stderr = os.Stderr
	if err := vite.Run(); err != nil {
		return fmt.Errorf("vite build failed: %w", err)
	}
	reportBundleSize(filepath.Join(appDir, "webdist"))
	return nil
}

// reportBundleSize prints the built frontend's total size and its
// largest chunks, so a build makes bundle growth visible (the main
// lever on cold-start time). Best-effort: a walk error is skipped
// silently rather than failing the build.
func reportBundleSize(webdist string) {
	type entry struct {
		name string
		size int64
	}
	var files []entry
	var total int64
	_ = filepath.WalkDir(webdist, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(webdist, p)
		if relErr != nil {
			rel = p
		}
		files = append(files, entry{name: filepath.ToSlash(rel), size: info.Size()})
		total += info.Size()
		return nil
	})
	if len(files) == 0 {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].size > files[j].size })
	step("frontend bundle: %s across %d files", humanBytes(total), len(files))
	for i, f := range files {
		if i >= 5 {
			break
		}
		fmt.Printf("    %9s  %s\n", humanBytes(f.size), f.name)
	}
}

// humanBytes formats a byte count as a short human-readable string.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// resolveTargets parses the flag or gantry.json list; empty = the
// current machine. Mobile targets: "android" (bare = arm64, or
// android/arm64, android/amd64) and "ios" (arm64 only).
func resolveTargets(flagVal string, cfg appConfig) ([]buildTarget, error) {
	raw := cfg.Build.Targets
	if flagVal != "" {
		raw = strings.Split(flagVal, ",")
	}
	if len(raw) == 0 {
		hostOS := runtime.GOOS
		if hostOS == "darwin" {
			hostOS = "mac"
		}
		return []buildTarget{{OS: hostOS, Arch: runtime.GOARCH}}, nil
	}
	var out []buildTarget
	for _, r := range raw {
		parts := strings.Split(strings.TrimSpace(strings.ToLower(r)), "/")
		osName := parts[0]
		if osName == "darwin" {
			osName = "mac"
		}
		if len(parts) == 1 && (osName == "android" || osName == "ios") {
			out = append(out, buildTarget{OS: osName, Arch: "arm64"})
			continue
		}
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("bad target %q (want os/arch, e.g. windows/amd64, or bare android/ios)", r)
		}
		arch := parts[1]
		switch osName {
		case "windows", "linux", "mac":
		case "android":
			if arch != "arm64" && arch != "amd64" {
				return nil, fmt.Errorf("unknown android arch %q (arm64 or amd64)", arch)
			}
		case "ios":
			if arch != "arm64" {
				return nil, fmt.Errorf("unknown ios arch %q (arm64 only)", arch)
			}
		default:
			return nil, fmt.Errorf("unknown target os %q (windows, linux, mac, android or ios)", parts[0])
		}
		out = append(out, buildTarget{OS: osName, Arch: arch})
	}
	return out, nil
}

// versionLdflag stamps gantry.json's version into the gantry package
// (read back by gantry.Version and the built-in appInfo service).
func versionLdflag(cfg appConfig) string {
	return "-X " + modulePath + "/gantry.injectedVersion=" + cfg.Version
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

func buildOne(appDir string, cfg appConfig, t buildTarget, console, installer bool) error {
	outDir := filepath.Join(appDir, "dist", t.OS, t.Arch)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	exeName := cfg.Name
	if t.OS == "windows" {
		exeName += ".exe"
		if err := writeSyso(appDir, cfg, t.Arch); err != nil {
			warn("exe icon skipped: %v", err)
		}
	}
	outPath := filepath.Join(outDir, exeName)

	step("building %s/%s (go)", t.OS, t.Arch)
	// gantry.json's version is stamped into the framework so the app
	// can show it at runtime (gantry.Version / the appInfo service).
	ldflags := versionLdflag(cfg)
	if t.OS == "windows" && !console {
		ldflags = "-H windowsgui " + ldflags
	}
	goArgs := []string{"build", "-o", outPath, "-ldflags", ldflags, "."}
	cmd := exec.Command("go", goArgs...)
	cmd.Dir = appDir
	cmd.Env = append(os.Environ(), "GOOS="+t.goos(), "GOARCH="+t.Arch)
	if t.OS == "mac" {
		// Mac has no native shell yet (browser fallback), so the build
		// is pure Go and cross-compiles from anywhere.
		cmd.Env = append(cmd.Env, "CGO_ENABLED=0")
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build %s/%s failed: %w", t.OS, t.Arch, err)
	}

	if !installer {
		return nil
	}
	switch t.OS {
	case "windows":
		return windowsInstaller(appDir, cfg, t, outDir, exeName)
	case "linux":
		archive := filepath.Join(outDir, fmt.Sprintf("%s-linux-%s.tar.gz", cfg.Name, t.Arch))
		step("packaging %s", filepath.Base(archive))
		return tarGz(archive, outPath, cfg.Name)
	case "mac":
		archive := filepath.Join(outDir, fmt.Sprintf("%s-mac-%s.zip", cfg.Name, t.Arch))
		step("packaging %s", filepath.Base(archive))
		return zipOne(archive, outPath, cfg.Name)
	}
	return nil
}

// writeSyso embeds icons/icon.ico (or .png) into the exe as its icon
// resource by generating rsrc_windows_<arch>.syso, which go build
// picks up automatically. The file is gitignored and regenerated.
func writeSyso(appDir string, cfg appConfig, arch string) error {
	dir := cfg.Icons
	if dir == "" {
		dir = "icons"
	}
	icoPath := filepath.Join(appDir, dir, "icon.ico")
	pngPath := filepath.Join(appDir, dir, "icon.png")

	var icon *winres.Icon
	switch {
	case fileExists(icoPath):
		f, err := os.Open(icoPath)
		if err != nil {
			return err
		}
		defer f.Close()
		icon, err = winres.LoadICO(f)
		if err != nil {
			return fmt.Errorf("reading %s: %w", icoPath, err)
		}
	case fileExists(pngPath):
		f, err := os.Open(pngPath)
		if err != nil {
			return err
		}
		defer f.Close()
		img, _, err := image.Decode(f)
		if err != nil {
			return fmt.Errorf("reading %s: %w", pngPath, err)
		}
		icon, err = winres.NewIconFromResizedImage(img, nil)
		if err != nil {
			return err
		}
	default:
		return nil // no icons: plain exe
	}

	var wrArch winres.Arch
	switch arch {
	case "amd64":
		wrArch = winres.ArchAMD64
	case "arm64":
		wrArch = winres.ArchARM64
	case "386":
		wrArch = winres.ArchI386
	default:
		return fmt.Errorf("no syso support for arch %s", arch)
	}

	rs := winres.ResourceSet{}
	if err := rs.SetIcon(winres.ID(1), icon); err != nil {
		return err
	}
	out, err := os.Create(filepath.Join(appDir, "rsrc_windows_"+arch+".syso"))
	if err != nil {
		return err
	}
	defer out.Close()
	return rs.WriteObject(out, wrArch)
}

// windowsInstaller generates an Inno Setup script and compiles it to
// <name>-setup.exe when the Inno Setup compiler (iscc) is available;
// otherwise the .iss is left ready to compile.
func windowsInstaller(appDir string, cfg appConfig, t buildTarget, outDir, exeName string) error {
	iss := fmt.Sprintf(`; Generated by gantry build - compile with Inno Setup 6 (iscc)
[Setup]
AppName=%s
AppVersion=%s
AppPublisher=%s
DefaultDirName={autopf}\%s
DefaultGroupName=%s
DisableProgramGroupPage=yes
UninstallDisplayIcon={app}\%s
OutputDir=.
OutputBaseFilename=%s-setup
ArchitecturesInstallIn64BitMode=x64compatible

[Files]
Source: "%s"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{group}\%s"; Filename: "{app}\%s"
Name: "{autodesktop}\%s"; Filename: "{app}\%s"; Tasks: desktopicon

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; Flags: unchecked

[Run]
Filename: "{app}\%s"; Description: "{cm:LaunchProgram,%s}"; Flags: nowait postinstall skipifsilent
`,
		cfg.Title, cfg.Version, cfg.Title, cfg.Title, cfg.Title, exeName,
		cfg.Name, exeName, cfg.Title, exeName, cfg.Title, exeName, exeName, cfg.Title)

	issPath := filepath.Join(outDir, cfg.Name+".iss")
	if err := os.WriteFile(issPath, []byte(iss), 0o644); err != nil {
		return err
	}

	iscc := findISCC()
	if iscc == "" {
		warn("Inno Setup not found - %s is ready; install Inno Setup 6 (https://jrsoftware.org/isinfo.php) to produce %s-setup.exe", issPath, cfg.Name)
		return nil
	}
	step("packaging %s-setup.exe (Inno Setup)", cfg.Name)
	cmd := exec.Command(iscc, "/Q", filepath.Base(issPath))
	cmd.Dir = outDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("iscc failed: %w", err)
	}
	return nil
}

func findISCC() string {
	if p, err := exec.LookPath("iscc"); err == nil {
		return p
	}
	if p, err := exec.LookPath("ISCC.exe"); err == nil {
		return p
	}
	for _, guess := range []string{
		`C:\Program Files (x86)\Inno Setup 6\ISCC.exe`,
		`C:\Program Files\Inno Setup 6\ISCC.exe`,
	} {
		if fileExists(guess) {
			return guess
		}
	}
	return ""
}

// tarGz packs one file (as name, executable) into a .tar.gz.
func tarGz(archivePath, filePath, name string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	out, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data))}); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// zipOne packs one file into a .zip.
func zipOne(archivePath, filePath, name string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	out, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
	hdr.SetMode(0o755)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	return zw.Close()
}
