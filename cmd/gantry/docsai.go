package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// The docs assistant is an OPT-IN, on-device feature: `gantry docs --ai`
// wires a chat widget to an OpenAI-compatible chat endpoint. It defaults to
// a local Ollama server so your questions (and any pasted errors) never
// leave the machine; llama.cpp, LM Studio, or a hosted provider work too by
// overriding the env vars. The docs themselves stay fully offline - the
// assistant is purely additive.

// aiConfig selects and configures the assistant backend.
type aiConfig struct {
	backend string // "claude" | "codex" | "ollama" (the OpenAI-compatible HTTP path)
	baseURL string // e.g. http://localhost:11434/v1 (no trailing slash); used by "ollama"
	model   string // e.g. qwen2.5; used by "ollama"
	apiKey  string // optional; local servers usually need none
}

func newAIConfig() aiConfig {
	return aiConfig{
		backend: resolveBackend(strings.ToLower(strings.TrimSpace(os.Getenv("GANTRY_DOCS_AI_BACKEND")))),
		baseURL: strings.TrimRight(aiEnvOr("GANTRY_DOCS_AI_URL", "http://localhost:11434/v1"), "/"),
		model:   aiEnvOr("GANTRY_DOCS_AI_MODEL", "qwen2.5"),
		apiKey:  os.Getenv("GANTRY_DOCS_AI_KEY"),
	}
}

// resolveBackend picks the assistant backend. An explicit GANTRY_DOCS_AI_BACKEND
// wins; otherwise "auto" prefers an installed agent CLI (Claude Code, then
// Codex) and falls back to the OpenAI-compatible HTTP model ("ollama").
func resolveBackend(choice string) string {
	switch choice {
	case "claude", "codex":
		return choice
	case "ollama", "http", "openai", "local":
		return "ollama"
	}
	// auto (unset or unknown): prefer an agent CLI on PATH.
	if _, err := exec.LookPath("claude"); err == nil {
		return "claude"
	}
	if _, err := exec.LookPath("codex"); err == nil {
		return "codex"
	}
	return "ollama"
}

func aiEnvOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// aiDoc is one documentation page prepared for retrieval: Plain is the
// lowercased body used for scoring, Raw is the full markdown fed to the
// model as grounding context.
type aiDoc struct {
	Title    string
	Route    string
	Category string
	Raw      string
	Plain    string // lowercased plain text, for scoring
}

// aiChatMessage is one turn in the OpenAI chat shape.
type aiChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type aiAskRequest struct {
	Messages []aiChatMessage `json:"messages"`
}

var aiStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"how": true, "does": true, "did": true, "can": true, "you": true, "are": true,
	"was": true, "but": true, "not": true, "has": true, "have": true, "from": true,
	"what": true, "when": true, "why": true, "where": true, "use": true, "using": true,
	"about": true, "into": true, "your": true, "gantry": true,
}

// aiTokenize lowercases and splits a query into scoring terms (>=3 chars,
// stopwords dropped).
func aiTokenize(s string) []string {
	var toks []string
	var b strings.Builder
	flush := func() {
		if b.Len() >= 3 {
			t := b.String()
			if !aiStopwords[t] {
				toks = append(toks, t)
			}
		}
		b.Reset()
	}
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return toks
}

// retrieve scores every page by term overlap with the query and returns the
// top k pages. Title hits weigh more than body hits. This is what lets a
// small local-model context window still draw on the whole knowledge base:
// only the most relevant pages are sent as context, per question.
func (s *docsSite) retrieve(query string, k int) []aiDoc {
	return retrieveFrom(s.aiDocs, query, k)
}

// retrieveFrom is retrieve over an explicit pool, so the MCP namespace filter
// can score just one module's pages without duplicating the ranking.
func retrieveFrom(pool []aiDoc, query string, k int) []aiDoc {
	toks := aiTokenize(query)
	if len(toks) == 0 {
		return nil
	}
	type scored struct {
		d     aiDoc
		score int
	}
	var out []scored
	for _, d := range pool {
		titleLower := strings.ToLower(d.Title)
		sc := 0
		for _, t := range toks {
			sc += strings.Count(d.Plain, t)
			sc += strings.Count(titleLower, t) * 8
		}
		if sc > 0 {
			out = append(out, scored{d, sc})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	if len(out) > k {
		out = out[:k]
	}
	res := make([]aiDoc, len(out))
	for i, o := range out {
		res[i] = o.d
	}
	return res
}

// aiSystemPrompt builds the grounding prompt: an index of EVERY page (so the
// model can route to any page by link) plus the full text of the retrieved
// pages.
func (s *docsSite) aiSystemPrompt(retrieved []aiDoc) string {
	var b strings.Builder
	b.WriteString(`You are the Gantry documentation assistant. Gantry is a Go framework for building native desktop (and mobile) apps whose UI is React/TSX with a Go backend, paired over one local websocket via a "paired file" convention (a <name>.go and <name>.tsx sharing a folder-path key, wired with usePaired).

Answer ONLY from the documentation provided below. Be concise and concrete, and prefer short code examples drawn from the docs. When you use a page, cite it as a markdown link to its route, e.g. [Dynamic routes](/ui/dynamic-routes). If the user pastes an error or stack trace, identify the likely cause and point them to the most relevant page (runtime/crash errors and gerr codes: /advanced/errors; Windows-specific issues: /advanced/win32-notes; the wire protocol: /advanced/protocol). If the documentation does not cover the question, say so plainly rather than guessing.

# Documentation index (every page - link to any of these by its route)
`)
	b.WriteString(s.aiTOC)
	b.WriteString("\n# Relevant pages (full text)\n")
	if len(retrieved) == 0 {
		b.WriteString("(no page strongly matched the question - use the index above to point the user to the closest page.)\n")
	}
	for _, d := range retrieved {
		fmt.Fprintf(&b, "\n---\n## %s  (%s)\n\n%s\n", d.Title, d.Route, d.Raw)
	}
	return b.String()
}

// docByRoute finds a page by its route, tolerating a missing leading slash or
// a trailing ".md" (so "/ui/state", "ui/state" and "ui/state.md" all match).
func (s *docsSite) docByRoute(route string) (aiDoc, bool) {
	route = strings.TrimSuffix(strings.TrimSpace(route), ".md")
	if !strings.HasPrefix(route, "/") {
		route = "/" + route
	}
	for _, d := range s.aiDocs {
		if d.Route == route {
			return d, true
		}
	}
	return aiDoc{}, false
}

// aiTrimHistory keeps only user/assistant turns, last max, to bound context.
func aiTrimHistory(msgs []aiChatMessage, max int) []aiChatMessage {
	var conv []aiChatMessage
	for _, m := range msgs {
		if m.Role == "user" || m.Role == "assistant" {
			conv = append(conv, aiChatMessage{Role: m.Role, Content: m.Content})
		}
	}
	if len(conv) > max {
		conv = conv[len(conv)-max:]
	}
	return conv
}

// handleAsk streams an answer to the browser as Server-Sent Events. It
// retrieves the relevant pages for the latest user message, grounds the
// backend on them, and forwards the answer as content deltas (plus an
// up-front list of sources). The backend is either an OpenAI-compatible model
// server or a local agent CLI (Claude Code / Codex).
func (s *docsSite) handleAsk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req aiAskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	lastUser := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			lastUser = req.Messages[i].Content
			break
		}
	}
	retrieved := s.retrieve(lastUser, 6)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	sse := func(obj any) {
		b, _ := json.Marshal(obj)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	// Tell the widget which pages we pulled, up front.
	srcs := make([]map[string]string, len(retrieved))
	for i, d := range retrieved {
		srcs[i] = map[string]string{"title": d.Title, "route": d.Route}
	}
	sse(map[string]any{"sources": srcs})

	system := s.aiSystemPrompt(retrieved)
	conv := aiTrimHistory(req.Messages, 8)

	switch s.aiCfg.backend {
	case "claude", "codex":
		s.streamAgentCLI(r.Context(), sse, buildAgentPrompt(system, conv))
	default:
		s.streamHTTP(r.Context(), sse, system, conv)
	}
	sse(map[string]bool{"done": true})
}

// streamHTTP proxies an OpenAI-compatible streaming completion and forwards
// its content deltas as SSE.
func (s *docsSite) streamHTTP(ctx context.Context, sse func(any), system string, conv []aiChatMessage) {
	msgs := append([]aiChatMessage{{Role: "system", Content: system}}, conv...)
	body, _ := json.Marshal(map[string]any{
		"model":    s.aiCfg.model,
		"messages": msgs,
		"stream":   true,
	})
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.aiCfg.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		sse(map[string]string{"error": err.Error()})
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	if s.aiCfg.apiKey != "" {
		upReq.Header.Set("Authorization", "Bearer "+s.aiCfg.apiKey)
	}
	resp, err := http.DefaultClient.Do(upReq)
	if err != nil {
		sse(map[string]string{"error": "Could not reach a model at " + s.aiCfg.baseURL + ". Is your local model server running? (" + err.Error() + ")"})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		sse(map[string]string{"error": fmt.Sprintf("Model server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))})
		return
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			sse(map[string]string{"delta": chunk.Choices[0].Delta.Content})
		}
	}
}

// buildAgentPrompt flattens the grounding system prompt plus the conversation
// into a single prompt string for an agent CLI (piped via stdin, so there is
// no command-line length limit on the embedded docs).
func buildAgentPrompt(system string, conv []aiChatMessage) string {
	var b strings.Builder
	b.WriteString(system)
	b.WriteString("\n\n----\n")
	for _, m := range conv {
		role := "User"
		if m.Role == "assistant" {
			role = "Assistant"
		}
		fmt.Fprintf(&b, "\n%s: %s\n", role, m.Content)
	}
	b.WriteString("\nAssistant:")
	return b.String()
}

// streamAgentCLI runs a local agent CLI (Claude Code or Codex) in headless
// mode with the grounded prompt on stdin, and forwards its answer as SSE
// deltas. It uses the agent already installed on the machine - no model
// download, no GPU.
func (s *docsSite) streamAgentCLI(ctx context.Context, sse func(any), prompt string) {
	backend := s.aiCfg.backend
	bin, err := exec.LookPath(backend)
	if err != nil {
		sse(map[string]string{"error": fmt.Sprintf("The %q CLI isn't on your PATH. Install it, or set GANTRY_DOCS_AI_BACKEND=ollama to use a local model.", backend)})
		return
	}

	var args []string
	switch backend {
	case "claude":
		// Headless, streaming JSON events (token deltas need partial messages).
		args = []string{"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages"}
	case "codex":
		// Non-interactive: progress goes to stderr, the final message to stdout.
		args = []string{"exec"}
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(prompt)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		sse(map[string]string{"error": err.Error()})
		return
	}
	if err := cmd.Start(); err != nil {
		sse(map[string]string{"error": err.Error()})
		return
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var emitted bool
	if backend == "claude" {
		emitted = streamClaudeEvents(scanner, sse)
	} else {
		emitted = streamPlainStdout(scanner, sse)
	}
	waitErr := cmd.Wait()

	if !emitted {
		msg := strings.TrimSpace(errBuf.String())
		if len(msg) > 800 {
			msg = msg[:800] + "…"
		}
		if msg == "" && waitErr != nil {
			msg = waitErr.Error()
		}
		if msg == "" {
			msg = "the agent produced no output"
		}
		sse(map[string]string{"error": fmt.Sprintf("%s returned no answer: %s", backend, msg)})
	}
}

// streamClaudeEvents parses Claude Code's stream-json output. It prefers
// token-level deltas (content_block_delta events from --include-partial-messages)
// and falls back to the final assistant message or the result string, so it
// works whether or not partial messages are available.
func streamClaudeEvents(scanner *bufio.Scanner, sse func(any)) bool {
	emitted := false
	result := ""
	for scanner.Scan() {
		var ev struct {
			Type   string `json:"type"`
			Result string `json:"result"`
			Event  struct {
				Type  string `json:"type"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			} `json:"event"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "stream_event":
			if ev.Event.Type == "content_block_delta" && ev.Event.Delta.Type == "text_delta" && ev.Event.Delta.Text != "" {
				sse(map[string]string{"delta": ev.Event.Delta.Text})
				emitted = true
			}
		case "assistant":
			// Only fires here if partial deltas weren't emitted (the final
			// assistant message always arrives after its stream_events).
			if !emitted {
				for _, c := range ev.Message.Content {
					if c.Type == "text" && c.Text != "" {
						sse(map[string]string{"delta": c.Text})
						emitted = true
					}
				}
			}
		case "result":
			result = ev.Result
		}
	}
	if !emitted && strings.TrimSpace(result) != "" {
		sse(map[string]string{"delta": result})
		emitted = true
	}
	return emitted
}

// streamPlainStdout forwards raw stdout lines as deltas (Codex prints the final
// message to stdout; progress goes to stderr).
func streamPlainStdout(scanner *bufio.Scanner, sse func(any)) bool {
	emitted := false
	for scanner.Scan() {
		sse(map[string]string{"delta": scanner.Text() + "\n"})
		emitted = true
	}
	return emitted
}

// ensureOllamaModel makes `gantry docs --ai` self-provisioning for the
// common case: if the backend is a local Ollama and the `ollama` CLI is
// installed but the configured model isn't pulled yet, it pulls it (the CLI
// starts the server if needed and there are no interactive prompts). It runs
// in the background so the docs serve immediately; progress streams to the
// terminal. It is a no-op for non-Ollama backends (llama.cpp, LM Studio, a
// hosted provider) and when the CLI isn't found - the widget's offline hint
// covers those.
func ensureOllamaModel(cfg aiConfig) {
	if !strings.Contains(cfg.baseURL, "11434") { // not the Ollama default
		return
	}
	bin, err := exec.LookPath("ollama")
	if err != nil {
		return // Ollama not installed; nothing to auto-pull
	}
	if ollamaHasModel(cfg) {
		return // already pulled
	}
	info("docs assistant: model %q not found locally; pulling it with ollama (this downloads several GB)...", cfg.model)
	cmd := exec.Command(bin, "pull", cfg.model)
	cmd.Stdout = os.Stderr // stream pull progress to the terminal
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		info("docs assistant: `ollama pull %s` failed: %v (pull it manually, or set GANTRY_DOCS_AI_MODEL to one you have)", cfg.model, err)
		return
	}
	info("docs assistant: model %q is ready", cfg.model)
}

// ollamaHasModel asks the running Ollama server whether the configured model
// is already present. If the server isn't running it returns false, so the
// subsequent `ollama pull` (which starts the server) still fires.
func ollamaHasModel(cfg aiConfig) bool {
	root := strings.TrimSuffix(cfg.baseURL, "/v1")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return false
	}
	for _, m := range out.Models {
		if m.Name == cfg.model || strings.HasPrefix(m.Name, cfg.model+":") {
			return true
		}
	}
	return false
}

// handleAIStatus reports the active backend so the widget can show it (and,
// for the HTTP model backend, whether the server is answering).
func (s *docsSite) handleAIStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.aiCfg
	out := map[string]any{"backend": cfg.backend}
	switch cfg.backend {
	case "claude", "codex":
		// The CLI was found on PATH at startup; report it as ready.
		out["reachable"] = true
		out["model"] = cfg.backend
	default:
		reachable := false
		ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
		defer cancel()
		if req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.baseURL+"/models", nil); err == nil {
			if cfg.apiKey != "" {
				req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
			}
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
				reachable = resp.StatusCode < 500
			}
		}
		out["reachable"] = reachable
		out["model"] = cfg.model
		out["baseURL"] = cfg.baseURL
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
