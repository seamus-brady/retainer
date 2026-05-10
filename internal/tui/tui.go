package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/seamus-brady/retainer/internal/agent"
	"github.com/seamus-brady/retainer/internal/cog"
	"github.com/seamus-brady/retainer/internal/observer"
	"github.com/seamus-brady/retainer/internal/tui/mdrender"
)

type role int

const (
	roleUser role = iota
	roleAssistant
	roleError
	roleSystem
)

type message struct {
	role role
	text string
}

type assistantReplyMsg struct{ text string }
type assistantRefusalMsg struct{ text string }
// assistantErrMsg carries a cycle failure to the TUI render
// path. text is the operator-facing summary the cog produced
// via FormatErrorForUser; raw is preserved for slog/cycle-log
// elsewhere but the TUI renders only text — provider-internal
// details ("mistral: status 429: ...") are not appropriate as
// chat surface.
type assistantErrMsg struct {
	text string
	raw  error
}
type activityMsg cog.Activity
type agentActivityMsg agent.Activity

// Fixed hex palette — these RGB values stay the same regardless of
// the user's terminal theme. lipgloss falls back to the nearest 256-
// or 16-color match on terminals without truecolor support, so the
// chrome reads consistently across themes (dark, light, high-
// contrast) instead of remapping to whatever ANSI codes 9 / 10 / 12
// / 13 happen to mean in the user's profile.
//
// Mid-tone colors chosen so they remain legible on both dark and
// light backgrounds. Glamour markdown rendering is separately pinned
// to its "dark" style (see Model.mdRenderer), so the assistant's
// reply body's colours are also theme-independent.
const (
	colorUser      = "#7aa2f7" // soft blue
	colorAssistant = "#9ece6a" // soft green
	colorError     = "#f7768e" // soft red
	colorSystem    = "#bb9af7" // soft purple
	colorSeparator = "#414868" // dim slate grey
)

var (
	userStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colorUser))
	assistantStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colorAssistant))
	errorStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colorError))
	systemStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colorSystem))
	statusStyle    = lipgloss.NewStyle().Faint(true).Padding(0, 1)
	thinkingStyle  = lipgloss.NewStyle().Italic(true).Faint(true)
	headerStyle    = lipgloss.NewStyle().Bold(true)
	versionStyle   = lipgloss.NewStyle().Faint(true)
	separatorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(colorSeparator))
)

const replyTimeout = 90 * time.Second

type Model struct {
	name     string
	version  string
	cog      *cog.Cog
	agents   []*agent.Agent
	observer *observer.Observer
	input    textinput.Model
	viewport viewport.Model
	messages []message
	width    int
	// busy is true between user submission and final reply — controls
	// whether we accept new input.
	busy bool
	// activity is the most-recent ambient signal from the cog, used to
	// render a richer status label than just "thinking...". Set by the
	// activitiesPump command on every push from cog.Activities().
	activity cog.Activity
	// agentActivities holds the latest Activity from each running
	// specialist agent, keyed by agent name. When the cog is mid-
	// delegation (UsingTools with agent_<name> in ToolNames), the
	// status label nests the agent's own state — "researcher: using
	// tool: brave_web_search" — so the user can see what the
	// specialist is doing inside the delegation.
	agentActivities map[string]agent.Activity
	ready           bool
	// mdRenderer pretty-prints assistant replies — handles headings,
	// lists, code blocks, inline emphasis/code/links, and tables.
	// Tables get a dedicated path with proper rune-aware column
	// alignment + a records-mode fallback for tables too wide for
	// the viewport. Pinned to the current width; recreated on
	// window resize.
	mdRenderer *mdrender.Renderer
	// mdRendererWidth is the width the current renderer was constructed
	// for. Cached so we only rebuild when it actually changes.
	mdRendererWidth int
}

// New constructs the TUI model. agents is the list of running
// specialist agents whose Activities() channel should feed the
// nested status display (typically just the researcher today; more
// will land as observer/comms/etc. ship). Pass nil/empty to keep the
// status bar showing only the cog's view.
func New(name, version string, c *cog.Cog, obs *observer.Observer, agents []*agent.Agent) Model {
	ti := textinput.New()
	ti.Placeholder = "Type a message... (Ctrl+C to quit)"
	ti.Focus()
	ti.CharLimit = 4096
	ti.Prompt = "> "
	return Model{
		name:            name,
		version:         version,
		cog:             c,
		agents:          agents,
		observer:        obs,
		input:           ti,
		agentActivities: make(map[string]agent.Activity),
	}
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink, waitForActivity(m.cog.Activities())}
	for _, a := range m.agents {
		cmds = append(cmds, waitForAgentActivity(a.Activities()))
	}
	return tea.Batch(cmds...)
}

// waitForActivity blocks on the cog's Activity channel and returns a
// single tea.Msg per receive. Bubble Tea convention: each receive
// re-arms the command via the Update handler so a long-running
// subscription doesn't tie up a goroutine across renders.
func waitForActivity(ch <-chan cog.Activity) tea.Cmd {
	return func() tea.Msg {
		a, ok := <-ch
		if !ok {
			return nil
		}
		return activityMsg(a)
	}
}

// waitForAgentActivity is the agent-side equivalent of waitForActivity.
// One subscription per agent; each receive re-arms via the activity
// handler in Update.
func waitForAgentActivity(ch <-chan agent.Activity) tea.Cmd {
	return func() tea.Msg {
		a, ok := <-ch
		if !ok {
			return nil
		}
		return agentActivityMsg(a)
	}
}

// findAgent returns the agent with the given name, or nil if not
// registered with this TUI. Used to re-arm the right Activities
// subscription on agentActivityMsg.
func (m Model) findAgent(name string) *agent.Agent {
	for _, a := range m.agents {
		if a.Name() == name {
			return a
		}
	}
	return nil
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		const chrome = 4
		m.width = msg.Width
		if !m.ready {
			m.viewport = viewport.New(msg.Width, msg.Height-chrome)
			m.ready = true
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = msg.Height - chrome
		}
		// Recreate the markdown renderer when the wrap target changes.
		// The renderer bakes wrap into its output (paragraphs +
		// table column shrinking), so a stale renderer would over-
		// or under-flow the new viewport width.
		wrapTarget := markdownWrapWidth(msg.Width)
		if m.mdRenderer == nil || m.mdRendererWidth != wrapTarget {
			m.mdRenderer = mdrender.New(wrapTarget)
			m.mdRendererWidth = wrapTarget
		}
		m.viewport.SetContent(m.renderMessages())
		m.viewport.GotoBottom()

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "enter":
			text := strings.TrimSpace(m.input.Value())
			if text == "" || m.busy {
				break
			}
			switch {
			case text == "/exit", text == "/quit":
				return m, tea.Quit
			case text == "/recall" || strings.HasPrefix(text, "/recall "):
				m.messages = append(m.messages, message{roleSystem, m.runRecall(text)})
				m.input.Reset()
				m.viewport.SetContent(m.renderMessages())
				m.viewport.GotoBottom()
			case strings.HasPrefix(text, "/inspect "):
				m.messages = append(m.messages, message{roleSystem, m.runInspect(text)})
				m.input.Reset()
				m.viewport.SetContent(m.renderMessages())
				m.viewport.GotoBottom()
			case text == "/help":
				m.messages = append(m.messages, message{roleSystem, helpText})
				m.input.Reset()
				m.viewport.SetContent(m.renderMessages())
				m.viewport.GotoBottom()
			case strings.HasPrefix(text, "/"):
				m.messages = append(m.messages, message{roleError, "unknown command: " + text + " (try /help)"})
				m.input.Reset()
				m.viewport.SetContent(m.renderMessages())
				m.viewport.GotoBottom()
			default:
				m.messages = append(m.messages, message{roleUser, text})
				m.input.Reset()
				m.busy = true
				m.viewport.SetContent(m.renderMessages())
				m.viewport.GotoBottom()
				cmds = append(cmds, runCog(m.cog, text))
			}
		}

	case assistantReplyMsg:
		m.messages = append(m.messages, message{roleAssistant, msg.text})
		m.busy = false
		m.viewport.SetContent(m.renderMessages())
		m.viewport.GotoBottom()

	case assistantRefusalMsg:
		m.messages = append(m.messages, message{roleAssistant, msg.text})
		m.busy = false
		m.viewport.SetContent(m.renderMessages())
		m.viewport.GotoBottom()

	case assistantErrMsg:
		m.messages = append(m.messages, message{roleError, msg.text})
		m.busy = false
		m.viewport.SetContent(m.renderMessages())
		m.viewport.GotoBottom()

	case activityMsg:
		m.activity = cog.Activity(msg)
		m.viewport.SetContent(m.renderMessages())
		// Re-arm the subscription so the next push lands as another msg.
		cmds = append(cmds, waitForActivity(m.cog.Activities()))

	case agentActivityMsg:
		act := agent.Activity(msg)
		if act.AgentName != "" {
			m.agentActivities[act.AgentName] = act
		}
		m.viewport.SetContent(m.renderMessages())
		// Re-arm the same agent's subscription. We look up the agent
		// by name rather than capturing the channel into the closure
		// so a stopped/restarted agent wouldn't leak a stale channel
		// reference.
		if a := m.findAgent(act.AgentName); a != nil {
			cmds = append(cmds, waitForAgentActivity(a.Activities()))
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	cmds = append(cmds, cmd)

	m.viewport, cmd = m.viewport.Update(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

func (m Model) View() string {
	if !m.ready {
		return "Initialising..."
	}
	header := headerStyle.Render(m.name) + "  " + versionStyle.Render("retainer "+m.version)
	separator := separatorStyle.Render(strings.Repeat("─", m.width))
	statusBar := statusStyle.Render(fmt.Sprintf(
		"[ %s | provider: %s | model: %s ]",
		m.statusLabel(), m.cog.Provider(), displayModel(m.cog.Model()),
	))
	return strings.Join([]string{
		header,
		separator,
		m.viewport.View(),
		m.input.View(),
		statusBar,
	}, "\n")
}

// labelWidth is the printed width of the role label + the two-space
// separator ("you  " or "agent" + "  "). Continuation lines after a wrap
// are indented this much so the message reads as one block.
const labelWidth = 7

const helpText = `commands:
  /recall [N]        last N cycles (default 5) — narrative entries from the librarian
  /inspect <id>      detail for one cycle — DAG status + duration + summary + error
  /help              this list
  /exit, /quit       leave (Ctrl+C also works)
anything else is sent to the agent.`

func (m Model) runRecall(text string) string {
	if m.observer == nil {
		return "observer not configured"
	}
	limit := 5
	if parts := strings.Fields(text); len(parts) > 1 {
		if n, err := strconv.Atoi(parts[1]); err == nil && n > 0 {
			limit = n
		}
	}
	entries := m.observer.RecentCycles(limit)
	if len(entries) == 0 {
		return "no cycles recorded yet"
	}
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s [%s] %s — %s\n",
			e.Timestamp.Format("15:04:05"),
			e.Status,
			shortID(e.CycleID),
			truncate(strings.ReplaceAll(e.Summary, "\n", " "), 100),
		)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) runInspect(text string) string {
	if m.observer == nil {
		return "observer not configured"
	}
	parts := strings.Fields(text)
	if len(parts) < 2 {
		return "usage: /inspect <cycle-id-or-prefix>"
	}
	wanted := parts[1]
	insp := m.observer.InspectCycle(wanted)
	if !insp.Found {
		// Try short-id match against recent cycles before giving up.
		for _, e := range m.observer.RecentCycles(500) {
			if strings.HasPrefix(e.CycleID, wanted) {
				insp = m.observer.InspectCycle(e.CycleID)
				break
			}
		}
	}
	if !insp.Found {
		return "cycle not found: " + wanted
	}

	var b strings.Builder
	fmt.Fprintf(&b, "cycle:    %s\n", insp.CycleID)
	if insp.Type != "" {
		fmt.Fprintf(&b, "type:     %s\n", insp.Type)
	}
	if insp.Status != "" {
		fmt.Fprintf(&b, "status:   %s\n", insp.Status)
	}
	if !insp.StartedAt.IsZero() {
		fmt.Fprintf(&b, "started:  %s\n", insp.StartedAt.Format("2006-01-02 15:04:05"))
	}
	if insp.Duration > 0 {
		fmt.Fprintf(&b, "duration: %s\n", insp.Duration.Round(time.Millisecond))
	}
	if insp.ErrorMessage != "" {
		fmt.Fprintf(&b, "error:    %s\n", insp.ErrorMessage)
	}
	if !insp.Intent.IsZero() {
		if insp.Intent.Classification != "" {
			fmt.Fprintf(&b, "intent:   %s", insp.Intent.Classification)
			if insp.Intent.Domain != "" {
				fmt.Fprintf(&b, " (%s)", insp.Intent.Domain)
			}
			b.WriteString("\n")
		} else if insp.Intent.Domain != "" {
			fmt.Fprintf(&b, "intent:   (%s)\n", insp.Intent.Domain)
		}
		if insp.Intent.Description != "" {
			fmt.Fprintf(&b, "  desc:   %s\n", insp.Intent.Description)
		}
	}
	if !insp.Outcome.IsZero() {
		fmt.Fprintf(&b, "outcome:  %s", insp.Outcome.Status)
		if insp.Outcome.Confidence > 0 {
			fmt.Fprintf(&b, " (confidence %.2f)", insp.Outcome.Confidence)
		}
		b.WriteString("\n")
		if insp.Outcome.Assessment != "" {
			fmt.Fprintf(&b, "  assess: %s\n", insp.Outcome.Assessment)
		}
	}
	if len(insp.Topics) > 0 {
		fmt.Fprintf(&b, "topics:   %s\n", strings.Join(insp.Topics, ", "))
	}
	if len(insp.Keywords) > 0 {
		fmt.Fprintf(&b, "keywords: %s\n", strings.Join(insp.Keywords, ", "))
	}
	if len(insp.DelegationChain) > 0 {
		b.WriteString("delegations:\n")
		for _, d := range insp.DelegationChain {
			fmt.Fprintf(&b, "  - %s", d.Agent)
			if d.AgentCycleID != "" {
				fmt.Fprintf(&b, " (%s)", shortID(d.AgentCycleID))
			}
			b.WriteString("\n")
			if d.Instruction != "" {
				fmt.Fprintf(&b, "    instr:  %s\n", truncate(d.Instruction, 200))
			}
			if d.OutcomeText != "" {
				fmt.Fprintf(&b, "    out:    %s\n", truncate(strings.ReplaceAll(d.OutcomeText, "\n", " "), 200))
			}
			if len(d.ToolsUsed) > 0 {
				fmt.Fprintf(&b, "    tools:  %s\n", strings.Join(d.ToolsUsed, ", "))
			}
		}
	}
	if !insp.Metrics.IsZero() {
		b.WriteString("metrics: ")
		var parts []string
		if insp.Metrics.TotalDurationMs > 0 {
			parts = append(parts, fmt.Sprintf("%dms", insp.Metrics.TotalDurationMs))
		}
		if insp.Metrics.InputTokens > 0 || insp.Metrics.OutputTokens > 0 {
			parts = append(parts, fmt.Sprintf("tokens=%d/%d", insp.Metrics.InputTokens, insp.Metrics.OutputTokens))
		}
		if insp.Metrics.ToolCalls > 0 {
			parts = append(parts, fmt.Sprintf("tools=%d", insp.Metrics.ToolCalls))
		}
		if insp.Metrics.AgentDelegations > 0 {
			parts = append(parts, fmt.Sprintf("delegations=%d", insp.Metrics.AgentDelegations))
		}
		if insp.Metrics.ModelUsed != "" {
			parts = append(parts, "model="+insp.Metrics.ModelUsed)
		}
		b.WriteString(strings.Join(parts, " "))
		b.WriteString("\n")
	}
	if insp.Summary != "" {
		b.WriteString("---\n")
		b.WriteString(insp.Summary)
	}
	return strings.TrimRight(b.String(), "\n")
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "..."
}

func (m Model) renderMessages() string {
	wrapWidth := m.width - labelWidth
	if wrapWidth < 20 {
		wrapWidth = 20
	}

	var b strings.Builder
	for _, msg := range m.messages {
		switch msg.role {
		case roleUser:
			b.WriteString(userStyle.Render("you  "))
		case roleAssistant:
			b.WriteString(assistantStyle.Render("agent"))
		case roleError:
			b.WriteString(errorStyle.Render("error"))
		case roleSystem:
			b.WriteString(systemStyle.Render("sys  "))
		}
		b.WriteString("  ")
		// Only assistant replies are markdown — user input is whatever
		// the operator typed (treat as plain text), and system / error
		// lines are TUI-internal output that we control the format of.
		// Running them through glamour would mangle their layout (e.g.
		// the `---` separator in /inspect output would become a
		// horizontal rule).
		if msg.role == roleAssistant {
			b.WriteString(m.renderAssistantMarkdown(msg.text, wrapWidth))
		} else {
			b.WriteString(wrapWithIndent(msg.text, wrapWidth, labelWidth))
		}
		b.WriteString("\n\n")
	}
	if m.busy {
		b.WriteString(assistantStyle.Render("agent"))
		b.WriteString("  ")
		b.WriteString(thinkingStyle.Render(m.statusLabel() + "..."))
		b.WriteString("\n")
	}
	return b.String()
}

// statusLabel turns the latest cog Activity into a friendly one-line
// label for the status bar and the in-flight "agent" indicator. When
// the cog is mid-delegation (using tool agent_X), nests the active
// specialist's own status — "researcher: using tool: brave_web_search"
// — so the user can see what the specialist is doing inside the
// delegation. Falls back to "idle" / "thinking" otherwise.
func (m Model) statusLabel() string {
	if !m.busy {
		return "idle"
	}
	a := m.activity
	switch a.Status {
	case cog.StatusEvaluatingPolicy:
		return "evaluating policy"
	case cog.StatusRetrying:
		// Surface the wait so the operator doesn't think the
		// agent died. Reason → human label; round delay to
		// whole seconds for the strip.
		reason := a.RetryReason
		if reason == "" {
			reason = "retrying"
		} else if reason == "rate_limited" {
			reason = "rate limited"
		}
		secs := (a.RetryDelayMs + 500) / 1000
		if a.RetryMaxAttempts > 0 {
			return fmt.Sprintf("%s · retry %d/%d · %ds", reason, a.RetryAttempt, a.RetryMaxAttempts, secs)
		}
		return fmt.Sprintf("%s · %ds", reason, secs)
	case cog.StatusUsingTools:
		// If the cog is invoking a single agent_<name> delegate tool
		// AND that agent has emitted at least one non-Idle activity,
		// surface the agent's own state.
		if nested, ok := m.nestedAgentLabel(a.ToolNames); ok {
			return nested
		}
		switch len(a.ToolNames) {
		case 0:
			return "using tools"
		case 1:
			return "using tool: " + a.ToolNames[0]
		default:
			return "using tools: " + strings.Join(a.ToolNames, ", ")
		}
	case cog.StatusThinking:
		if a.Turn > 1 {
			return fmt.Sprintf("thinking (turn %d/%d)", a.Turn, a.MaxTurns)
		}
		return "thinking"
	default:
		// busy=true but no Activity yet, or StatusIdle racing the
		// reply — fall back to the generic label.
		return "thinking"
	}
}

// nestedAgentLabel returns "<agent>: <agent-status>" when toolNames
// resolves to exactly one delegate-tool call AND we have a non-Idle
// Activity for that agent. Otherwise (false, "") so the caller falls
// back to the cog-level label.
func (m Model) nestedAgentLabel(toolNames []string) (string, bool) {
	if len(toolNames) != 1 {
		// Multiple tool calls in one turn — show the cog summary.
		// (Springdrift's parallel dispatch gets a richer treatment;
		// for now, surface the list.)
		return "", false
	}
	name, ok := agentNameFromToolName(toolNames[0])
	if !ok {
		return "", false
	}
	act, present := m.agentActivities[name]
	if !present {
		// Cog says "using agent_X" but the agent hasn't pushed an
		// activity yet — likely a brief window before the agent's
		// react loop fires. Fall back to the cog label.
		return "", false
	}
	inner := agentStatusLabel(act)
	if inner == "" {
		return "", false
	}
	return name + ": " + inner, true
}

// agentNameFromToolName strips the "agent_" prefix from a delegate
// tool name. Returns ("", false) when the tool isn't an
// agent-delegation tool (so the cog's own tool labels — memory_write
// etc. — surface unchanged).
func agentNameFromToolName(toolName string) (string, bool) {
	const prefix = "agent_"
	if !strings.HasPrefix(toolName, prefix) {
		return "", false
	}
	name := toolName[len(prefix):]
	if name == "" {
		return "", false
	}
	return name, true
}

// agentStatusLabel maps an agent.Activity to its display label —
// mirrors statusLabel's switch but for the agent's smaller state
// machine. Returns "" when the agent reports Idle (the parent
// caller falls back to the cog label so we don't show "agent: idle"
// while the cog is mid-delegation).
func agentStatusLabel(a agent.Activity) string {
	switch a.Status {
	case agent.StatusUsingTools:
		switch len(a.ToolNames) {
		case 0:
			return "using tools"
		case 1:
			return "using tool: " + a.ToolNames[0]
		default:
			return "using tools: " + strings.Join(a.ToolNames, ", ")
		}
	case agent.StatusThinking:
		if a.Turn > 1 {
			return fmt.Sprintf("thinking (turn %d/%d)", a.Turn, a.MaxTurns)
		}
		return "thinking"
	}
	return ""
}

// wrapWithIndent word-wraps text at width runes per line. Lines after the
// first are prefixed with `indent` spaces so they align under the message
// label rather than the role column.
func wrapWithIndent(text string, width, indent int) string {
	if width <= 0 {
		return text
	}
	pad := strings.Repeat(" ", indent)
	paragraphs := strings.Split(text, "\n")
	out := make([]string, 0, len(paragraphs))
	for i, para := range paragraphs {
		wrapped := wrapLine(para, width)
		if i == 0 {
			// First paragraph: first line takes the role label; subsequent
			// lines need padding.
			lines := strings.Split(wrapped, "\n")
			for j := 1; j < len(lines); j++ {
				lines[j] = pad + lines[j]
			}
			out = append(out, strings.Join(lines, "\n"))
		} else {
			// Subsequent paragraphs: every line indented under the label.
			lines := strings.Split(wrapped, "\n")
			for j := range lines {
				lines[j] = pad + lines[j]
			}
			out = append(out, strings.Join(lines, "\n"))
		}
	}
	return strings.Join(out, "\n")
}

// wrapLine word-wraps a single line (no embedded newlines) at width runes.
func wrapLine(s string, width int) string {
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	var lines []string
	var current []string
	currentLen := 0
	for _, word := range strings.Fields(s) {
		wordLen := utf8.RuneCountInString(word)
		if currentLen > 0 && currentLen+1+wordLen > width {
			lines = append(lines, strings.Join(current, " "))
			current = nil
			currentLen = 0
		}
		if currentLen > 0 {
			currentLen++
		}
		current = append(current, word)
		currentLen += wordLen
	}
	if len(current) > 0 {
		lines = append(lines, strings.Join(current, " "))
	}
	return strings.Join(lines, "\n")
}

func runCog(c *cog.Cog, input string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), replyTimeout)
		defer cancel()
		reply := <-c.Submit(ctx, input)
		switch reply.Kind {
		case cog.ReplyKindError:
			text := reply.Text
			if text == "" {
				// Defensive — every ReplyKindError from the cog
				// today carries Text via FormatErrorForUser. Fall
				// back to a generic message rather than leaking
				// reply.Err.Error() to the operator.
				text = cog.FormatErrorForUser(reply.Err)
			}
			return assistantErrMsg{text: text, raw: reply.Err}
		case cog.ReplyKindRefusal:
			return assistantRefusalMsg{text: reply.Text}
		default:
			return assistantReplyMsg{text: reply.Text}
		}
	}
}

func displayModel(m string) string {
	if m == "" {
		return "(unset)"
	}
	return m
}

// markdownWrapWidth picks the line width to pass to the markdown
// renderer. It mirrors the plain-text wrap (`width - labelWidth`)
// but with a 20-rune floor so a vanishingly small viewport still
// gets a sane target.
func markdownWrapWidth(viewportWidth int) int {
	w := viewportWidth - labelWidth
	if w < 20 {
		w = 20
	}
	return w
}

// renderAssistantMarkdown renders text as ANSI-styled markdown via the
// model's glamour renderer, then re-indents continuation lines under
// the role label so the block reads as one logical message.
//
// Falls back to plain word-wrap when:
//   - the renderer hasn't been constructed yet (pre first WindowSizeMsg)
//   - glamour returns an error
//   - the rendered output is empty (defensive: glamour shouldn't strip
//     a non-empty input to nothing, but if it does we'd rather show
//     the raw text than a blank line)
//
// glamour adds leading and trailing blank lines and runs its own
// padding; trim the surrounding blanks so the assistant message
// flows under its label cleanly. Continuation lines get `labelWidth`
// spaces prepended — same convention as wrapWithIndent.
func (m Model) renderAssistantMarkdown(text string, wrapWidth int) string {
	fallback := wrapWithIndent(text, wrapWidth, labelWidth)
	if m.mdRenderer == nil {
		return fallback
	}
	out := m.mdRenderer.Render(text)
	out = strings.Trim(out, "\n")
	if out == "" {
		return fallback
	}
	pad := strings.Repeat(" ", labelWidth)
	lines := strings.Split(out, "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}
