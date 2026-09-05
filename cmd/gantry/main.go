// gantry is the Gantry framework CLI.
//
//	gantry new <name>       scaffold a new app
//	gantry dev              run the app with live reload in a native window
//	gantry build            build a single exe with the frontend embedded
//	gantry add <pkg...>     install npm packages into the app
//	gantry install --<feat> add an optional feature to the app (tailwind)
//	gantry gen              regenerate the registry files
//	gantry test [pattern]   run the app's end-to-end tests
//	gantry update           update the gantry CLI to the latest release
//	gantry upgrade          upgrade the current app to the CLI's version
//	gantry mobile dev <os>  build + run on a plugged-in phone (android|ios)
//	gantry docs [topic]     browse the documentation offline
//	gantry --version        print the CLI version
package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var usageCommands = []struct{ cmd, desc string }{
	{"gantry new <name>", "scaffold a new app (interactive; see flags below)"},
	{"gantry dev", "run the current app with live reload"},
	{"gantry build", "build the current app into a single exe"},
	{"gantry add <pkg...>", "install npm packages into the app"},
	{"gantry install --tailwind", "add Tailwind v4 to an existing app (theme tokens migrated)"},
	{"gantry gen", "regenerate gantry_registry.go (dev/build do this too)"},
	{"gantry test [pattern]", "run the app's end-to-end tests (tests/)"},
	{"gantry update", "update the gantry CLI itself to the latest release"},
	{"gantry upgrade", "upgrade the current app to the CLI's version (templates + packages)"},
	{"gantry mobile dev <os>", "build + run on a plugged-in phone with live logcat (android|ios)"},
	{"gantry docs [topic]", "browse the documentation offline"},
	{"gantry module <cmd>", "manage Gantry modules (install|list|uninstall|update|info)"},
	{"gantry --version", "print the CLI version"},
}

// usageText renders the usage block with the command column coloured;
// the renderer decides whether colour actually applies (stdout for
// help, stderr for unknown commands).
func usageText(r *lipgloss.Renderer) string {
	cmdStyle := r.NewStyle().Foreground(lipgloss.Color("39"))
	var b strings.Builder
	b.WriteString("gantry - build native desktop apps with Go and React\n\nUsage:\n")
	for _, c := range usageCommands {
		b.WriteString("  " + cmdStyle.Render(fmt.Sprintf("%-22s", c.cmd)) + "  " + c.desc + "\n")
	}
	b.WriteString("\nRun gantry new -h for scaffolding flags.\n")
	return b.String()
}

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usageText(stdoutStyles))
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "new":
		checkForUpdate()
		err = cmdNew(os.Args[2:])
	case "dev":
		checkForUpdate()
		err = cmdDev(os.Args[2:])
	case "build":
		checkForUpdate()
		err = cmdBuild(os.Args[2:])
	case "add":
		err = cmdAdd(os.Args[2:])
	case "install":
		err = cmdInstall(os.Args[2:])
	case "gen":
		err = cmdGen(os.Args[2:])
	case "test":
		checkForUpdate()
		err = cmdTest(os.Args[2:])
	case "update":
		err = cmdUpdate(os.Args[2:])
	case "upgrade":
		err = cmdUpgrade(os.Args[2:])
	case "mobile":
		checkForUpdate()
		err = cmdMobile(os.Args[2:])
	case "docs":
		err = cmdDocs(os.Args[2:])
	case "module":
		err = cmdModule(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("Version: " + fullVersion())
	case "help", "-h", "--help":
		fmt.Print(usageText(stdoutStyles))
	default:
		fail("unknown command %q", os.Args[1])
		fmt.Fprint(os.Stderr, "\n"+usageText(stderrStyles))
		os.Exit(2)
	}
	if err != nil {
		failErr(err)
		os.Exit(1)
	}
}

// fullVersion is cliVersion plus the vcs revision when this is a
// local path install (go install ./cmd/gantry reports "(devel)").
func fullVersion() string {
	v := cliVersion()
	if v == "" {
		return "(unknown)"
	}
	if v == "(devel)" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" && s.Value != "" {
					rev := s.Value
					if len(rev) > 12 {
						rev = rev[:12]
					}
					return v + " " + rev
				}
			}
		}
	}
	return v
}
