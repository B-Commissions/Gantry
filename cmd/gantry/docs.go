package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"

	"github.com/BlueBeard63/Gantry/docs"
	"github.com/BlueBeard63/Gantry/internal/launch"
)

// cmdDocs browses the embedded documentation: a sidebar with search and
// the category tree on the left, the rendered page on the right. Works
// fully offline - the docs travel inside the gantry exe.
func cmdDocs(args []string) error {
	fs := flag.NewFlagSet("docs", flag.ExitOnError)
	tui := fs.Bool("tui", false, "browse the docs in the terminal instead of the web viewer")
	printOnly := fs.Bool("print", false, "print the page as plain markdown instead of the browser")
	frameOnly := fs.Bool("frame", false, "render one frame at the terminal size to stdout, then exit (layout diagnostic)")
	frameSize := fs.String("size", "", "WxH to force the -frame size, e.g. 188x41 (when redirecting to a file)")
	ai := fs.Bool("ai", false, "enable the on-device docs assistant (agent CLI like Claude Code/Codex if installed, else a local model)")
	mcpMode := fs.Bool("mcp", false, "run a stdio MCP server exposing the docs (search_docs/read_doc/list_docs) - no auth; add with: claude mcp add gantry-docs -- gantry docs --mcp")
	port := fs.Int("port", 8331, "port for the docs web viewer, so the URL stays stable across runs (0 picks an ephemeral port)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	pages, err := loadDocs()
	if err != nil {
		return err
	}

	// MCP mode is a headless stdio server for coding agents - no web/TUI, no
	// stdout chatter (the transport owns stdin/stdout).
	if *mcpMode {
		return serveDocsMCP(pages)
	}

	start := 0
	if topic := strings.Join(fs.Args(), " "); topic != "" {
		start = bestMatch(pages, topic)
	}

	if *printOnly {
		fmt.Println(pages[start].raw)
		return nil
	}
	if *frameOnly {
		return docsFrameProbe(pages, start, *frameSize)
	}

	// The web viewer is the default; -tui keeps the terminal browser.
	if !*tui {
		return serveDocsWeb(pages, start, *ai, *port)
	}

	m := newDocsModel(pages, start)
	_, err = tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

// docsFrameProbe renders one frame at the terminal's (or forced) size and
// prints it plainly - no Bubble Tea program, so only this text reaches the
// screen. When redirected (or -size is set) it strips colors and annotates
// each row with its display width, for inspecting the layout without the
// live terminal.
func docsFrameProbe(pages []docPage, start int, size string) error {
	fd := int(os.Stdout.Fd())
	w, h := 0, 0
	if size != "" {
		fmt.Sscanf(size, "%dx%d", &w, &h)
	}
	if w <= 0 || h <= 0 {
		if dw, dh, err := term.GetSize(uintptr(fd)); err == nil && dw > 0 && dh > 0 {
			w, h = dw, dh
		} else {
			w, h = 100, 30
		}
	}
	var m tea.Model = newDocsModel(pages, start)
	m, _ = m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	frame := m.View()
	if size != "" || !term.IsTerminal(uintptr(fd)) {
		fmt.Printf("[size %dx%d - plain, |NN| = measured display width]\n", w, h)
		for i, ln := range strings.Split(frame, "\n") {
			clean := ansi.Strip(ln)
			fmt.Printf("%2d |%3d| %s\n", i, lipgloss.Width(clean), clean)
		}
		return nil
	}
	fmt.Printf("[terminal %dx%d]\n", w, h)
	fmt.Println(frame)
	return nil
}

type docPage struct {
	path     string // e.g. "shell/window.md"
	title    string // first heading
	category string // folder name, "" for root
	raw      string
	lower    string // lowercased raw for search
}

func loadDocs() ([]docPage, error) {
	var pages []docPage
	err := fs.WalkDir(docs.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		data, err := docs.FS.ReadFile(p)
		if err != nil {
			return err
		}
		raw := string(data)
		title := p
		for _, line := range strings.Split(raw, "\n") {
			if strings.HasPrefix(line, "# ") {
				title = strings.TrimSpace(strings.TrimPrefix(line, "# "))
				break
			}
		}
		category := ""
		if i := strings.Index(p, "/"); i >= 0 {
			category = p[:i]
		}
		pages = append(pages, docPage{path: p, title: title, category: category, raw: raw, lower: strings.ToLower(raw)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("no docs embedded (build the CLI from the Gantry repo)")
	}

	// Fold in installed-module docs. Best-effort: the framework docs must still
	// serve if a module's cache is unreadable.
	if mods, err := moduleDocPages(); err != nil {
		warn("skipping module docs: %v", err)
	} else {
		pages = append(pages, mods...)
	}

	// README first, then the framework reading order; installed modules sort
	// after (rank 100), grouped by namespace.
	rank := map[string]int{"": 0, "getting-started": 1, "shell": 2, "ui": 3, "mobile": 4, "testing": 5, "cli": 6, "advanced": 7}
	rankOf := func(cat string) int {
		if r, ok := rank[cat]; ok {
			return r
		}
		return 100
	}
	sort.SliceStable(pages, func(i, j int) bool {
		ri, rj := rankOf(pages[i].category), rankOf(pages[j].category)
		if ri != rj {
			return ri < rj
		}
		if pages[i].category != pages[j].category {
			return pages[i].category < pages[j].category
		}
		return pages[i].path < pages[j].path
	})
	return pages, nil
}

func bestMatch(pages []docPage, topic string) int {
	t := strings.ToLower(topic)
	best, bestScore := 0, -1
	for i, p := range pages {
		score := 0
		if strings.Contains(strings.ToLower(p.title), t) {
			score += 100
		}
		if strings.Contains(p.path, t) {
			score += 50
		}
		score += strings.Count(p.lower, t)
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	return best
}

var linkRe = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)

type docLink struct{ label, target string }

func pageLinks(p docPage) []docLink {
	var links []docLink
	for _, m := range linkRe.FindAllStringSubmatch(p.raw, -1) {
		if strings.HasPrefix(m[2], "#") {
			continue
		}
		links = append(links, docLink{label: m[1], target: m[2]})
	}
	return links
}

// docsModel is the bubbletea model: focus moves between the sidebar
// (search + page list) and the content viewport; f enters link mode.
type docsModel struct {
	pages    []docPage
	filtered []int // indexes into pages matching the search
	cursor   int   // position in filtered
	current  int   // open page index
	history  []int
	future   []int

	search  textinput.Model
	view    viewport.Model
	links   []docLink
	linkIdx int
	mode    string // "browse" | "search" | "links"
	focus   string // "side" | "main"
	status  string
	width   int
	height  int
	ready   bool
}

func newDocsModel(pages []docPage, start int) *docsModel {
	ti := textinput.New()
	ti.Placeholder = "search (/)"
	ti.CharLimit = 64
	m := &docsModel{pages: pages, current: start, mode: "browse", focus: "side"}
	m.search = ti
	m.refilter("")
	for i, idx := range m.filtered {
		if idx == start {
			m.cursor = i
		}
	}
	return m
}

func (m *docsModel) refilter(q string) {
	q = strings.ToLower(strings.TrimSpace(q))
	m.filtered = m.filtered[:0]
	for i, p := range m.pages {
		if q == "" || strings.Contains(strings.ToLower(p.title), q) || strings.Contains(p.lower, q) {
			m.filtered = append(m.filtered, i)
		}
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = 0
	}
}

func (m *docsModel) open(idx int, pushHistory bool) {
	if idx < 0 || idx >= len(m.pages) {
		return
	}
	if pushHistory && idx != m.current {
		m.history = append(m.history, m.current)
		m.future = nil
	}
	m.current = idx
	m.links = pageLinks(m.pages[idx])
	m.linkIdx = 0
	m.render()
	m.view.GotoTop()
}

func (m *docsModel) render() {
	if !m.ready {
		return
	}
	width := m.view.Width
	if width < 20 {
		width = 20
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(width-2),
	)
	if err != nil {
		m.view.SetContent(m.pages[m.current].raw)
		return
	}
	out, err := r.Render(m.pages[m.current].raw)
	if err != nil {
		out = m.pages[m.current].raw
	}
	m.view.SetContent(out)
}

func (m *docsModel) followLink(l docLink) {
	if strings.HasPrefix(l.target, "http://") || strings.HasPrefix(l.target, "https://") {
		if err := launch.OpenInBrowser(l.target); err != nil {
			if clipboard.WriteAll(l.target) == nil {
				m.status = "could not open browser - link copied to clipboard"
			} else {
				m.status = "could not open: " + l.target
			}
		} else {
			m.status = "opened in browser: " + l.target
		}
		return
	}
	// Internal: resolve relative to the current page's folder.
	target := path.Clean(path.Join(path.Dir(m.pages[m.current].path), strings.Split(l.target, "#")[0]))
	for i, p := range m.pages {
		if p.path == target {
			m.open(i, true)
			m.mode = "browse"
			return
		}
	}
	m.status = "page not found: " + target
}

func (m *docsModel) Init() tea.Cmd { return nil }

func (m *docsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		cw, ch := m.contentSize()
		if m.ready {
			// Resize in place so the scroll position survives.
			m.view.Width, m.view.Height = cw, ch
		} else {
			m.view = viewport.New(cw, ch)
			m.ready = true
		}
		if sw := m.sidebarTextWidth() - 4; sw >= 4 {
			m.search.Width = sw
		} else {
			m.search.Width = 4
		}
		m.render()
		return m, nil

	case tea.KeyMsg:
		m.status = ""
		if m.mode == "search" {
			switch msg.String() {
			case "esc":
				m.mode = "browse"
				m.search.Blur()
			case "enter":
				m.mode = "browse"
				m.search.Blur()
				if len(m.filtered) > 0 {
					m.open(m.filtered[m.cursor], true)
				}
			case "up":
				if m.cursor > 0 {
					m.cursor--
				}
			case "down":
				if m.cursor < len(m.filtered)-1 {
					m.cursor++
				}
			default:
				var cmd tea.Cmd
				m.search, cmd = m.search.Update(msg)
				m.refilter(m.search.Value())
				return m, cmd
			}
			return m, nil
		}
		if m.mode == "links" {
			switch msg.String() {
			case "esc", "f":
				m.mode = "browse"
			case "up", "k":
				if m.linkIdx > 0 {
					m.linkIdx--
				}
			case "down", "j":
				if m.linkIdx < len(m.links)-1 {
					m.linkIdx++
				}
			case "enter":
				if len(m.links) > 0 {
					m.followLink(m.links[m.linkIdx])
				}
				if m.mode == "links" {
					m.mode = "browse"
				}
			case "q", "ctrl+c":
				return m, tea.Quit
			}
			return m, nil
		}
		// browse mode
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "/":
			m.mode = "search"
			m.focus = "side"
			m.search.Focus()
			return m, textinput.Blink
		case "f":
			if len(m.links) > 0 {
				m.mode = "links"
				m.linkIdx = 0
			} else {
				m.status = "no links on this page"
			}
		case "tab":
			if m.focus == "side" {
				m.focus = "main"
			} else {
				m.focus = "side"
			}
		case "b", "backspace":
			if n := len(m.history); n > 0 {
				idx := m.history[n-1]
				m.history = m.history[:n-1]
				m.future = append(m.future, m.current)
				m.open(idx, false)
			}
		case "n":
			if n := len(m.future); n > 0 {
				idx := m.future[n-1]
				m.future = m.future[:n-1]
				m.history = append(m.history, m.current)
				m.open(idx, false)
			}
		case "up", "k":
			if m.focus == "side" {
				if m.cursor > 0 {
					m.cursor--
				}
			} else {
				m.view.ScrollUp(1)
			}
		case "down", "j":
			if m.focus == "side" {
				if m.cursor < len(m.filtered)-1 {
					m.cursor++
				}
			} else {
				m.view.ScrollDown(1)
			}
		case "pgup":
			m.view.PageUp()
		case "pgdown", " ":
			m.view.PageDown()
		case "enter":
			if m.focus == "side" && len(m.filtered) > 0 {
				m.open(m.filtered[m.cursor], true)
			}
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.view, cmd = m.view.Update(msg)
	return m, cmd
}

// Layout floors: the content pane never renders below docsMinContent
// columns, and the sidebar collapses entirely once the whole terminal is
// narrower than docsCollapseBelow (there is no room for two panes).
const (
	docsMinContent    = 20
	docsCollapseBelow = 50
)

// sidebarWidth is the sidebar column width, or 0 when the terminal is too
// narrow to show a sidebar and content side by side (collapsed mode).
func (m *docsModel) sidebarWidth() int {
	if m.width < docsCollapseBelow {
		return 0
	}
	w := m.width / 3
	if w < 28 {
		w = 28
	}
	if w > 44 {
		w = 44
	}
	// Never let the sidebar, its border and the spacer crowd the content
	// pane below its floor - keep sidebar + 3 + content <= width.
	if maxW := m.width - docsMinContent - 3; w > maxW {
		w = maxW
	}
	return w
}

// contentSize is the viewport's clamped width and height. The 3-column
// budget covers the sidebar's right border and the spacer; collapsed mode
// gives the content the full width less a single margin.
func (m *docsModel) contentSize() (int, int) {
	side := m.sidebarWidth()
	w := m.width - 1
	if side > 0 {
		w = m.width - side - 3
	}
	if w < docsMinContent {
		w = docsMinContent
	}
	h := m.height - 2
	if h < 1 {
		h = 1
	}
	return w, h
}

// sidebarTextWidth is the width available for sidebar text (inside the
// border). In collapsed mode the sidebar, when shown, spans the terminal.
func (m *docsModel) sidebarTextWidth() int {
	w := m.sidebarWidth()
	if w == 0 {
		w = m.width
	}
	if w -= 2; w < 1 {
		w = 1
	}
	return w
}

// truncRunes trims a string to at most w runes (byte slicing would split
// multibyte titles when widths are tight).
func truncRunes(s string, w int) string {
	if w < 0 {
		w = 0
	}
	r := []rune(s)
	if len(r) > w {
		return string(r[:w])
	}
	return s
}

var (
	sideStyle     = lipgloss.NewStyle().BorderStyle(lipgloss.NormalBorder()).BorderRight(true).BorderForeground(lipgloss.Color("240"))
	selStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true)
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	catStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("110")).Bold(true)
	statusStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	linkSelStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true)
	linkItemStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
)

func (m *docsModel) View() string {
	if !m.ready {
		return "loading..."
	}
	main := m.view.View()
	var body string
	if m.sidebarWidth() == 0 {
		// Collapsed: show one pane at a time. The sidebar takes over when
		// it has focus (browsing the list, searching, or picking links);
		// otherwise the content fills the terminal. Tab toggles.
		if m.focus == "side" || m.mode == "search" || m.mode == "links" {
			body = m.renderSidebar()
		} else {
			body = main
		}
	} else {
		h := m.height - 2
		if h < 0 {
			h = 0
		}
		// Fixed width keeps the border a straight column aligned with the
		// content budget; safeFrame later strips the padding lipgloss adds.
		sidebar := sideStyle.Width(m.sidebarWidth()).Height(h).Render(m.renderSidebar())
		body = lipgloss.JoinHorizontal(lipgloss.Top, sidebar, " "+main)
	}
	help := "tab focus | enter open | / search | f links | b back | q quit"
	if m.mode == "links" {
		help = "up/down pick link | enter follow | esc cancel"
	}
	if m.status != "" {
		help = m.status
	}
	frame := body + "\n" + statusStyle.Render(" "+help)
	return safeFrame(frame, m.width, m.height)
}

// trailBlank matches a trailing run of spaces/tabs and SGR (color/reset)
// escape sequences at the end of a line.
var trailBlank = regexp.MustCompile(`(?:\x1b\[[0-9;]*m|[ \t])+$`)

// safeFrame makes a frame that cannot trip the Windows Terminal full-width
// auto-wrap bug (lines that reach the right edge wrap and stagger the rows
// below). For every row it strips the trailing padding lipgloss adds to
// square the layout - even when that padding hides before an ANSI reset,
// which a plain TrimRight misses - so no row reaches the edge, then hard-
// truncates to w-1 cells (ANSI-aware). The whole frame is capped to h-1
// rows so it never fills the last screen row (another known mis-render).
func safeFrame(frame string, w, h int) string {
	lines := strings.Split(frame, "\n")
	if h > 1 && len(lines) > h-1 {
		lines = lines[:h-1]
	}
	limit := w - 1
	if limit < 1 {
		limit = 1
	}
	for i, ln := range lines {
		ln = trailBlank.ReplaceAllString(ln, "")
		if strings.IndexByte(ln, 0x1b) >= 0 {
			ln += "\x1b[0m" // re-close a style whose trailing reset we stripped
		}
		lines[i] = ansi.Truncate(ln, limit, "")
	}
	return strings.Join(lines, "\n")
}

func (m *docsModel) renderSidebar() string {
	w := m.sidebarTextWidth()
	var b strings.Builder
	b.WriteString(" " + m.search.View() + "\n\n")

	if m.mode == "links" {
		b.WriteString(catStyle.Render(" Links on this page") + "\n")
		for i, l := range m.links {
			line := truncRunes(fmt.Sprintf(" %d. %s", i+1, l.label), w)
			if i == m.linkIdx {
				b.WriteString(linkSelStyle.Render(line) + "\n")
			} else {
				b.WriteString(linkItemStyle.Render(line) + "\n")
			}
		}
		return b.String()
	}

	// Build every row, then show only the window that fits the pane, scrolled
	// to keep the cursor visible - a list taller than the terminal must never
	// render more rows than there is room for.
	var lines []string
	cursorLine := 0
	lastCat := "\x00"
	for i, idx := range m.filtered {
		p := m.pages[idx]
		if p.category != lastCat {
			lastCat = p.category
			label := p.category
			if label == "" {
				label = "start"
			}
			lines = append(lines, catStyle.Render(" "+label))
		}
		line := truncRunes("  "+p.title, w)
		switch {
		case i == m.cursor && m.focus == "side":
			lines = append(lines, selStyle.Render("> "+strings.TrimPrefix(line, "  ")))
		case idx == m.current:
			lines = append(lines, selStyle.Render(line))
		default:
			lines = append(lines, dimStyle.Render(line))
		}
		if i == m.cursor {
			cursorLine = len(lines) - 1
		}
	}

	// Rows left after the two the search box took (search + blank line).
	avail := m.height - 4
	if avail < 1 {
		avail = 1
	}
	start := 0
	if len(lines) > avail {
		start = cursorLine - avail/2
		if start < 0 {
			start = 0
		}
		if maxStart := len(lines) - avail; start > maxStart {
			start = maxStart
		}
	}
	end := start + avail
	if end > len(lines) {
		end = len(lines)
	}
	for _, ln := range lines[start:end] {
		b.WriteString(ln + "\n")
	}
	return b.String()
}
