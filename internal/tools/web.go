package tools

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

	"github.com/seamus-brady/retainer/internal/llm"
)

// FetchURL is a minimal HTTP-GET tool. The cog uses it when the
// agent decides to read a URL the operator referenced or that
// web_search returned. Returns the response body trimmed to a
// readable size — the agent can request a follow-up read if
// needed, but most pages are well-served by the first 64KB.
//
// No JS execution, no extraction heuristics, no auth. The agent
// should expect raw HTML/JSON/text and reason about it directly.
type FetchURL struct {
	// Client is the HTTP client used. Tests inject; production
	// uses a sane default.
	Client *http.Client
}

func (f FetchURL) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Tool returns the LLM tool definition. The model sees this; the
// schema constrains the call.
func (FetchURL) Tool() llm.Tool {
	return llm.Tool{
		Name:        "fetch_url",
		Description: "Fetch the contents of a URL via HTTP GET and return the response body (trimmed to ~64KB). Use this to read a specific page the operator named or that web_search returned.",
		InputSchema: llm.Schema{
			Name: "fetch_url",
			Properties: map[string]llm.Property{
				"url": {Type: "string", Description: "Absolute URL (https or http)."},
			},
			Required: []string{"url"},
		},
	}
}

const fetchURLMaxBytes = 64 * 1024

// Execute runs one fetch. Fails closed on non-2xx responses; the
// LLM sees the error so it can adjust.
func (f FetchURL) Execute(ctx context.Context, input []byte) (string, error) {
	var p struct {
		URL string `json:"url"`
	}
	if len(input) == 0 {
		return "", fmt.Errorf("fetch_url: empty input")
	}
	if err := json.Unmarshal(input, &p); err != nil {
		return "", fmt.Errorf("fetch_url: decode input: %w", err)
	}
	p.URL = strings.TrimSpace(p.URL)
	if p.URL == "" {
		return "", fmt.Errorf("fetch_url: url is required")
	}
	parsed, err := url.Parse(p.URL)
	if err != nil {
		return "", fmt.Errorf("fetch_url: bad url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("fetch_url: scheme must be http or https; got %q", parsed.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return "", fmt.Errorf("fetch_url: build request: %w", err)
	}
	req.Header.Set("User-Agent", "Retainer/1.0 (+https://github.com/seamus-brady/retainer)")
	req.Header.Set("Accept", "text/html,text/plain,application/json,*/*;q=0.5")

	resp, err := f.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch_url: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch_url: status %d %s", resp.StatusCode, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchURLMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("fetch_url: read body: %w", err)
	}
	truncated := false
	if len(body) > fetchURLMaxBytes {
		body = body[:fetchURLMaxBytes]
		truncated = true
	}
	out := string(body)
	if truncated {
		out += "\n\n[truncated to 64KB]"
	}
	return out, nil
}

// WebSearch hits DuckDuckGo's HTML endpoint and returns the top
// results. Zero third-party dependency, no API key, no auth.
//
// Reference-implementation choice: Brave / Tavily / Kagi all
// require an account + key to behave. DDG's HTML interface is a
// stable lowest-common-denominator. If it ever stops working,
// operators swap in their preferred backend by replacing this
// tool's Execute body.
type WebSearch struct {
	Client *http.Client
}

func (w WebSearch) client() *http.Client {
	if w.Client != nil {
		return w.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Tool returns the LLM tool definition.
func (WebSearch) Tool() llm.Tool {
	return llm.Tool{
		Name:        "web_search",
		Description: "Search the web via DuckDuckGo. Returns up to 10 results, each with title, url, and a short snippet. Use this when you need current info you don't have. Follow up with fetch_url on any result you want to read in full.",
		InputSchema: llm.Schema{
			Name: "web_search",
			Properties: map[string]llm.Property{
				"query": {Type: "string", Description: "Search terms."},
			},
			Required: []string{"query"},
		},
	}
}

// WebSearchResult is one row from the result list. Exported so
// tests in the same package can build expected values directly.
type WebSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// duckDuckGoEndpoint — the HTML interface. Stable enough that
// every public DDG-scraper points here.
const duckDuckGoEndpoint = "https://html.duckduckgo.com/html/"

// Execute runs one search. Returns a JSON array of WebSearchResult.
// On HTTP failure or zero results, surfaces the condition so the
// LLM sees nothing-came-back rather than a silent empty.
func (w WebSearch) Execute(ctx context.Context, input []byte) (string, error) {
	var p struct {
		Query string `json:"query"`
	}
	if len(input) == 0 {
		return "", fmt.Errorf("web_search: empty input")
	}
	if err := json.Unmarshal(input, &p); err != nil {
		return "", fmt.Errorf("web_search: decode input: %w", err)
	}
	p.Query = strings.TrimSpace(p.Query)
	if p.Query == "" {
		return "", fmt.Errorf("web_search: query is required")
	}

	form := url.Values{}
	form.Set("q", p.Query)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, duckDuckGoEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("web_search: build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 Retainer/1.0")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := w.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("web_search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("web_search: DuckDuckGo returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", fmt.Errorf("web_search: read body: %w", err)
	}

	results := parseDDGHTML(string(body), 10)
	if len(results) == 0 {
		return "", fmt.Errorf("web_search: no results parsed (DuckDuckGo HTML may have changed shape)")
	}
	out, err := json.Marshal(results)
	if err != nil {
		return "", fmt.Errorf("web_search: marshal results: %w", err)
	}
	return string(out), nil
}

// parseDDGHTML is a minimal regex-based DDG HTML parser. Pure
// function for testability — the live HTTP wrapper is in Execute.
//
// If DDG changes their template the regexes break and operators
// see "no results parsed". Swap the parser body when that
// happens; nothing else in the project depends on the shape.
func parseDDGHTML(body string, max int) []WebSearchResult {
	blockRE := regexp.MustCompile(`(?s)<div class="result(?:\s+[^"]*)?">(.*?)</div>\s*</div>`)
	titleURLRE := regexp.MustCompile(`(?s)<a[^>]*class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	snippetRE := regexp.MustCompile(`(?s)<a[^>]*class="result__snippet"[^>]*>(.*?)</a>`)

	var out []WebSearchResult
	for _, m := range blockRE.FindAllStringSubmatch(body, -1) {
		if len(out) >= max {
			break
		}
		block := m[1]
		tu := titleURLRE.FindStringSubmatch(block)
		if len(tu) < 3 {
			continue
		}
		raw := tu[1]
		title := stripHTML(tu[2])
		actual := unwrapDDGRedirect(raw)
		snippet := ""
		if s := snippetRE.FindStringSubmatch(block); len(s) >= 2 {
			snippet = stripHTML(s[1])
		}
		out = append(out, WebSearchResult{
			Title:   strings.TrimSpace(title),
			URL:     strings.TrimSpace(actual),
			Snippet: strings.TrimSpace(snippet),
		})
	}
	return out
}

// stripHTML pulls plain text out of a snippet by removing tags
// and decoding the few HTML entities DDG actually emits.
func stripHTML(s string) string {
	tagRE := regexp.MustCompile(`<[^>]*>`)
	s = tagRE.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", `"`)
	s = strings.ReplaceAll(s, "&#x27;", "'")
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	return s
}

// unwrapDDGRedirect decodes DDG's redirect wrapper if present.
// DDG returns links like `//duckduckgo.com/l/?uddg=https%3A%2F%2F...`;
// we want the inner URL. Falls through to the input unchanged when
// the wrapper isn't recognised.
func unwrapDDGRedirect(href string) string {
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if !strings.Contains(u.Path, "/l/") {
		return href
	}
	if inner := u.Query().Get("uddg"); inner != "" {
		if decoded, err := url.QueryUnescape(inner); err == nil {
			return decoded
		}
	}
	return href
}
