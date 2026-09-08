// web.go — web_fetch and web_search, lifted from the desktop brain
// (brain/tool/web_fetch.go, web_search.go) and adapted to the operator's Tool
// interface (Schema()/Execute(ctx, args, token)). Both are stdlib-only and need
// no credential, so the token argument is ignored. web_search hits DuckDuckGo's
// HTML-lite endpoint (no API key); override with BRAIN_SEARCH_URL.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ─── web_fetch ───────────────────────────────────────────────────────

type WebFetch struct{}

func (WebFetch) Name() string { return "web_fetch" }

func (WebFetch) Description() string {
	return "Fetch a URL over HTTP(S). HTML pages are stripped to plain text (no tags, scripts, or styles). JSON and text responses are returned verbatim. 200KB cap; only http/https schemes; 20s timeout."
}

func (WebFetch) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{"type": "string", "description": "Full URL including scheme. Only http and https are allowed."},
			"raw": map[string]any{"type": "boolean", "description": "Return body verbatim even if HTML (default false)."},
		},
		"required": []string{"url"},
	}
}

var webFetchClient = &http.Client{Timeout: 20 * time.Second}

func (WebFetch) Execute(ctx context.Context, raw json.RawMessage, _ string) (string, error) {
	var in struct {
		URL string `json:"url"`
		Raw bool   `json:"raw"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if in.URL == "" {
		return "", fmt.Errorf("url is required")
	}
	if !strings.HasPrefix(in.URL, "http://") && !strings.HasPrefix(in.URL, "https://") {
		return "", fmt.Errorf("only http(s) URLs allowed")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", in.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "construct-operator/0.1 (+https://lisaos.dev)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := webFetchClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 200<<10))
	if err != nil {
		return "", err
	}
	ct := resp.Header.Get("Content-Type")
	header := fmt.Sprintf("HTTP %d %s\n%s\n\n", resp.StatusCode, in.URL, ct)
	text := string(body)
	if !in.Raw && strings.Contains(ct, "html") {
		text = stripHTML(text)
	}
	return header + text, nil
}

var (
	reScriptStyle = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	reTag         = regexp.MustCompile(`(?s)<[^>]+>`)
	reWhitespace  = regexp.MustCompile(`[ \t]+`)
	reBlankLines  = regexp.MustCompile(`\n{3,}`)
)

func stripHTML(s string) string {
	s = reScriptStyle.ReplaceAllString(s, " ")
	s = reTag.ReplaceAllString(s, " ")
	repl := strings.NewReplacer(
		"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", `"`, "&#39;", "'", "&apos;", "'",
	)
	s = repl.Replace(s)
	s = reWhitespace.ReplaceAllString(s, " ")
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		b.WriteString(strings.TrimSpace(line))
		b.WriteByte('\n')
	}
	return reBlankLines.ReplaceAllString(b.String(), "\n\n")
}

// ─── web_search ──────────────────────────────────────────────────────

type WebSearch struct{}

func (WebSearch) Name() string { return "web_search" }

func (WebSearch) Description() string {
	return "Search the web. Returns up to 10 results with title, URL, and snippet. Use this when you need current information not in the training data. Follow up with web_fetch on promising URLs."
}

func (WebSearch) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "Search query."},
			"limit": map[string]any{"type": "integer", "description": "Max results (default 10, cap 20)."},
		},
		"required": []string{"query"},
	}
}

var webSearchClient = &http.Client{Timeout: 20 * time.Second}

func (WebSearch) Execute(ctx context.Context, raw json.RawMessage, _ string) (string, error) {
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if strings.TrimSpace(in.Query) == "" {
		return "", fmt.Errorf("query is required")
	}
	if in.Limit <= 0 || in.Limit > 20 {
		in.Limit = 10
	}
	endpoint := env("BRAIN_SEARCH_URL", "https://html.duckduckgo.com/html/")
	form := url.Values{"q": {in.Query}}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120 Safari/537.36")

	resp, err := webSearchClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 500<<10))
	if err != nil {
		return "", err
	}
	results := parseDDG(string(body), in.Limit)
	if len(results) == 0 {
		return "no results", nil
	}
	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", r.Snippet)
		}
	}
	return b.String(), nil
}

type searchResult struct{ Title, URL, Snippet string }

var (
	reDDGTitle   = regexp.MustCompile(`(?is)<a[^>]+class="[^"]*result__a[^"]*"[^>]+href="([^"]+)"[^>]*>(.+?)</a>`)
	reDDGSnippet = regexp.MustCompile(`(?is)<a[^>]+class="[^"]*result__snippet[^"]*"[^>]*>(.+?)</a>`)
	reAnyTag     = regexp.MustCompile(`<[^>]+>`)
)

func parseDDG(body string, limit int) []searchResult {
	titles := reDDGTitle.FindAllStringSubmatch(body, -1)
	snippets := reDDGSnippet.FindAllStringSubmatch(body, -1)
	out := make([]searchResult, 0, len(titles))
	for i, m := range titles {
		if i >= limit {
			break
		}
		r := searchResult{
			URL:   decodeDDGRedirect(m[1]),
			Title: strings.TrimSpace(stripTags(m[2])),
		}
		if i < len(snippets) {
			r.Snippet = strings.TrimSpace(stripTags(snippets[i][1]))
		}
		out = append(out, r)
	}
	return out
}

func decodeDDGRedirect(u string) string {
	if !strings.Contains(u, "uddg=") {
		if strings.HasPrefix(u, "//") {
			return "https:" + u
		}
		return u
	}
	if idx := strings.Index(u, "uddg="); idx >= 0 {
		tail := u[idx+len("uddg="):]
		if amp := strings.Index(tail, "&"); amp >= 0 {
			tail = tail[:amp]
		}
		if dec, err := url.QueryUnescape(tail); err == nil {
			return dec
		}
	}
	return u
}

func stripTags(s string) string {
	s = reAnyTag.ReplaceAllString(s, "")
	repl := strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'")
	return repl.Replace(s)
}
