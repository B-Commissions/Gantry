package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serveDocsMCP runs a local stdio MCP server that exposes the embedded Gantry
// documentation as search_docs / read_doc / list_docs tools. It needs no auth
// and no network: an MCP client (Claude Code, Codex, Cursor, ...) spawns it as
// a subprocess and talks over stdin/stdout, and the docs travel inside the
// gantry binary. Add it once with, e.g.:
//
//	claude mcp add gantry-docs -- gantry docs --mcp
//
// so an agent working on a Gantry project can pull the docs for reference.
func serveDocsMCP(pages []docPage) error {
	// Reuse the site's retrieval corpus + page index without starting the web
	// server (aiOn=false: no assistant endpoints, just the loaded pages).
	site, err := newDocsSite(pages, false)
	if err != nil {
		return err
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "gantry-docs",
		Title:   "Gantry documentation",
		Version: docsDisplayVersion(),
	}, nil)

	type searchInput struct {
		Query     string `json:"query" jsonschema:"keywords or an error message to search the docs for"`
		Limit     int    `json:"limit,omitempty" jsonschema:"maximum pages to return (default 6, max 20)"`
		Namespace string `json:"namespace,omitempty" jsonschema:"optional: restrict results to one section/module namespace, e.g. whitegantry (omit to search everything)"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_docs",
		Description: "Search the Gantry documentation (framework docs plus any installed modules). Returns the most relevant pages with their route and a snippet. Use this first to find the right page, then read_doc to read it. Pass namespace to scope to one module.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, any, error) {
		k := in.Limit
		if k <= 0 || k > 20 {
			k = 6
		}
		var hits []aiDoc
		if in.Namespace != "" {
			hits = retrieveFrom(site.aiDocsIn(in.Namespace), in.Query, k)
		} else {
			hits = site.retrieve(in.Query, k)
		}
		var b strings.Builder
		if len(hits) == 0 {
			b.WriteString("No pages matched. Use list_docs to browse every page.\n")
		}
		for _, d := range hits {
			snip := strings.TrimSpace(d.Plain)
			if len(snip) > 240 {
				snip = snip[:240] + "…"
			}
			fmt.Fprintf(&b, "## %s\nroute: %s\n%s\n\n", d.Title, d.Route, snip)
		}
		return textResult(b.String()), nil, nil
	})

	type readInput struct {
		Route string `json:"route" jsonschema:"the page route or path, e.g. /ui/dynamic-routes (from search_docs or list_docs)"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_doc",
		Description: "Read the full markdown of one Gantry documentation page by its route.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, any, error) {
		d, ok := site.docByRoute(in.Route)
		if !ok {
			return errorResult(fmt.Sprintf("No page at %q. Use list_docs to see every route.", in.Route)), nil, nil
		}
		return textResult(d.Raw), nil, nil
	})

	type listInput struct {
		Namespace string `json:"namespace,omitempty" jsonschema:"optional: list only one section/module namespace, e.g. whitegantry (omit to list every page)"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_docs",
		Description: "List Gantry documentation pages (framework docs plus any installed modules) as title, route and category, so you can pick one to read_doc. Pass namespace to list just one module.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, any, error) {
		return textResult(site.tocFor(in.Namespace)), nil, nil
	})

	// Run blocks until the client disconnects; a clean stdin EOF is a normal
	// shutdown, not an error worth printing (the SDK wraps it as
	// "server is closing: EOF").
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil &&
		!errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) &&
		!strings.Contains(err.Error(), "EOF") {
		return err
	}
	return nil
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func errorResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}, IsError: true}
}
