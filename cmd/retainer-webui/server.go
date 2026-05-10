package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/seamus-brady/retainer/internal/cogsock"
	"github.com/seamus-brady/retainer/internal/webui/transcript"
)

//go:embed static
var staticFS embed.FS

// server bridges browser HTTP requests to the cogClient. One
// instance per webui process; concurrent SSE consumers are fine
// (the client fans out internally).
//
// dataDir is the workspace's data directory — the day-view
// endpoints (`/api/days*`) read the cycle log under
// `<dataDir>/cycle-log/`. Required for the v2 day-navigation
// surface; passed through from main.go.
type server struct {
	client  *cogClient
	logger  *slog.Logger
	dataDir string
}

func newServer(c *cogClient, dataDir string, logger *slog.Logger) *server {
	return &server{client: c, dataDir: dataDir, logger: logger}
}

// cycleLogDir is the conventional location of the cog's cycle
// log under the workspace data directory. Centralised so the
// transcript endpoints don't drift if the cog ever moves it.
func (s *server) cycleLogDir() string {
	return filepath.Join(s.dataDir, "cycle-log")
}

// handler builds the HTTP mux for the webui. Routes:
//
//	GET  /             — embedded chat page (HTML + inline CSS/JS)
//	GET  /api/stream   — Server-Sent Events stream of cog envelopes
//	POST /api/submit   — body {input: "..."} → forwards to cog
//	GET  /api/health   — JSON {connected, agent_name, instance_id}
//
// Static assets under /static/* are served verbatim from the
// embedded FS for any extra files the page wants (today: nothing
// — the page is single-file).
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/m", s.handleMobileIndex)
	mux.HandleFunc("/m/", s.handleMobileIndex)
	mux.HandleFunc("/api/stream", s.handleStream)
	mux.HandleFunc("/api/submit", s.handleSubmit)
	mux.HandleFunc("/api/health", s.handleHealth)
	// Day-navigation surface (webui v2 PR 1). Read-only; backed
	// by the cycle-log JSONL the cog writes per day.
	mux.HandleFunc("/api/days", s.handleDays)
	mux.HandleFunc("/api/days/", s.handleDayPath)
	subFS, _ := fs.Sub(staticFS, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(subFS))))
	return mux
}

// handleDays returns the list of dates that have any cycle-log
// activity, newest-first. Empty array when the cog has never
// completed a cycle in this workspace.
func (s *server) handleDays(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	dates, err := transcript.LoadDir(s.cycleLogDir())
	if err != nil {
		s.logger.Warn("days list failed", "err", err)
		http.Error(w, "list days: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if dates == nil {
		dates = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"dates": dates})
}

// handleDayPath dispatches `/api/days/{date}` and
// `/api/days/{date}/export.md` based on the trailing path
// segment. Single handler keeps URL parsing local.
func (s *server) handleDayPath(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/days/")
	if rest == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	dateStr := parts[0]
	_, canonical, err := transcript.SafeDate(dateStr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if len(parts) > 1 {
		switch parts[1] {
		case "export.md":
			s.handleDayExport(w, r, canonical)
			return
		default:
			http.NotFound(w, r)
			return
		}
	}
	s.handleDayJSON(w, r, canonical)
}

// handleDayJSON returns the day's transcript turns as JSON.
// Empty `turns` array when no activity recorded.
func (s *server) handleDayJSON(w http.ResponseWriter, _ *http.Request, date string) {
	turns, err := transcript.LoadDay(s.cycleLogDir(), date)
	if err != nil {
		s.logger.Warn("day load failed", "date", date, "err", err)
		http.Error(w, "load day: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if turns == nil {
		turns = []transcript.Turn{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"date":  date,
		"turns": turns,
	})
}

// handleDayExport returns a markdown file the operator can
// download. Filename includes the agent name + date for easy
// archiving.
func (s *server) handleDayExport(w http.ResponseWriter, _ *http.Request, date string) {
	turns, err := transcript.LoadDay(s.cycleLogDir(), date)
	if err != nil {
		s.logger.Warn("day export failed", "date", date, "err", err)
		http.Error(w, "load day: "+err.Error(), http.StatusInternalServerError)
		return
	}
	agentName := "retainer"
	if ready := s.client.Ready(); ready.AgentName != "" {
		agentName = ready.AgentName
	}
	body := transcript.ExportMarkdown(date, agentName, turns)
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s-%s.md"`, agentName, date))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// handleMobileIndex serves the embedded mobile-first page at /m
// (and any /m/* path — the page is single-file and reads its own
// route from the URL hash). Separate from handleIndex so the
// desktop layout can evolve without disturbing the touch-optimised
// surface.
func (s *server) handleMobileIndex(w http.ResponseWriter, _ *http.Request) {
	body, err := staticFS.ReadFile("static/m.html")
	if err != nil {
		http.Error(w, "embedded mobile page missing: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// handleIndex serves the embedded chat page. Strips the leading
// `static/` prefix so the file lives at static/index.html on disk
// but is served at /.
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	body, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "embedded index missing: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// handleStream upgrades the connection to SSE and pushes every
// cog envelope as `event: <type>\ndata: <json>\n\n`. Subscribes
// for the duration of the connection; cancel on client
// disconnect.
func (s *server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := s.client.Subscribe(64)
	defer cancel()

	// Replay current ready state immediately so the page knows
	// what cog it's connected to without waiting for the next
	// activity tick.
	if ready := s.client.Ready(); ready.Type == cogsock.MsgTypeReady {
		writeSSE(w, flusher, ready)
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, flusher, msg)
		}
	}
}

// writeSSE serialises a server envelope as one SSE event. Event
// type is the envelope's Type field; data is the full JSON body
// so clients have access to every field without parsing the SSE
// event line.
func writeSSE(w http.ResponseWriter, flusher http.Flusher, msg cogsock.ServerMsg) {
	body, err := json.Marshal(msg)
	if err != nil {
		return
	}
	// SSE: event line + data line + blank line. Multi-line data
	// would need each line prefixed; our envelopes are single-line
	// JSON so this is fine.
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", sseEventName(msg.Type), body)
	flusher.Flush()
}

// sseEventName sanitises an envelope Type for use as the SSE
// `event:` field. Strips control chars defensively.
func sseEventName(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return "message"
	}
	return t
}

// handleSubmit forwards a `submit` to the cog. The reply lands
// asynchronously on /api/stream — submit returns immediately with
// HTTP 202 so the browser can keep streaming events without
// blocking on the cog cycle.
func (s *server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var body struct {
		Input string `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	body.Input = strings.TrimSpace(body.Input)
	if body.Input == "" {
		http.Error(w, "input must not be empty", http.StatusBadRequest)
		return
	}
	if !s.client.IsConnected() {
		http.Error(w, "cog not connected", http.StatusServiceUnavailable)
		return
	}
	if err := s.client.Send(cogsock.ClientMsg{Type: cogsock.MsgTypeSubmit, Input: body.Input}); err != nil {
		http.Error(w, "submit failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
}

// handleHealth reports whether the cog client is connected + the
// last-seen agent identity. Used by the page on load and during
// reconnect to update the status badge.
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ready := s.client.Ready()
	resp := struct {
		Connected  bool   `json:"connected"`
		AgentName  string `json:"agent_name"`
		InstanceID string `json:"instance_id"`
	}{
		Connected:  s.client.IsConnected(),
		AgentName:  ready.AgentName,
		InstanceID: ready.InstanceID,
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

