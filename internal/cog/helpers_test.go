package cog

import (
	"errors"
	"strings"
	"testing"

	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/librarian"
	"github.com/seamus-brady/retainer/internal/llm"
)

func TestStatus_String(t *testing.T) {
	cases := []struct {
		s    Status
		want string
	}{
		{StatusIdle, "idle"},
		{StatusEvaluatingPolicy, "evaluating_policy"},
		{StatusThinking, "thinking"},
		{StatusUsingTools, "using_tools"},
		{Status(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Errorf("Status(%d).String() = %q, want %q", int(c.s), got, c.want)
		}
	}
}

func TestReplyKind_String(t *testing.T) {
	cases := []struct {
		k    ReplyKind
		want string
	}{
		{ReplyKindText, "text"},
		{ReplyKindRefusal, "refusal"},
		{ReplyKindError, "error"},
		{ReplyKind(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.k.String(); got != c.want {
			t.Errorf("ReplyKind(%d).String() = %q, want %q", int(c.k), got, c.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("under-cap = %q", got)
	}
	if got := truncate("0123456789abc", 5); got != "01234..." {
		t.Errorf("over-cap = %q", got)
	}
}

func TestErrString(t *testing.T) {
	if got := errString(nil); got != "" {
		t.Errorf("nil = %q", got)
	}
	if got := errString(errors.New("boom")); got != "boom" {
		t.Errorf("err = %q", got)
	}
}

func TestErrMsg(t *testing.T) {
	if errMsg(nil) != "" {
		t.Error("nil err should be empty")
	}
	if errMsg(errors.New("x")) != "x" {
		t.Error("err msg should pass through")
	}
}

func TestBuildNarrativeSummary_Complete(t *testing.T) {
	got := buildNarrativeSummary(librarian.NarrativeStatusComplete, "what time", "noon", nil)
	if !strings.Contains(got, "User: what time") || !strings.Contains(got, "Reply: noon") {
		t.Errorf("complete summary = %q", got)
	}
}

func TestBuildNarrativeSummary_Blocked(t *testing.T) {
	got := buildNarrativeSummary(librarian.NarrativeStatusBlocked, "bad", "", errors.New("policy"))
	if !strings.Contains(got, "Blocked: policy") {
		t.Errorf("blocked summary = %q", got)
	}
}

func TestBuildNarrativeSummary_Abandoned(t *testing.T) {
	got := buildNarrativeSummary(librarian.NarrativeStatusAbandoned, "x", "", errors.New("wd"))
	if !strings.Contains(got, "Abandoned: wd") {
		t.Errorf("abandoned summary = %q", got)
	}
}

func TestBuildNarrativeSummary_Error(t *testing.T) {
	got := buildNarrativeSummary(librarian.NarrativeStatusError, "x", "", errors.New("boom"))
	if !strings.Contains(got, "Error: boom") {
		t.Errorf("error summary = %q", got)
	}
}

func TestBuildNarrativeSummary_TruncatesLongInputs(t *testing.T) {
	long := strings.Repeat("a", 250)
	got := buildNarrativeSummary(librarian.NarrativeStatusComplete, long, long, nil)
	if !strings.Contains(got, "...") {
		t.Errorf("expected truncation marker, got %q", got)
	}
}

func TestNarrativeStatusFromCycleLog(t *testing.T) {
	cases := []struct {
		in   cyclelog.CycleStatus
		want librarian.NarrativeStatus
	}{
		{cyclelog.StatusComplete, librarian.NarrativeStatusComplete},
		{cyclelog.StatusBlocked, librarian.NarrativeStatusBlocked},
		{cyclelog.StatusAbandon, librarian.NarrativeStatusAbandoned},
		{cyclelog.StatusError, librarian.NarrativeStatusError},
		{cyclelog.CycleStatus("nonsense"), librarian.NarrativeStatusError},
	}
	for _, c := range cases {
		if got := narrativeStatusFromCycleLog(c.in); got != c.want {
			t.Errorf("status %q → %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCollectToolUses_FiltersByType(t *testing.T) {
	got := collectToolUses([]llm.ContentBlock{
		llm.TextBlock{Text: "x"},
		llm.ToolUseBlock{ID: "1"},
		llm.ToolResultBlock{ToolUseID: "z"},
		llm.ToolUseBlock{ID: "2"},
	})
	if len(got) != 2 || got[0].ID != "1" || got[1].ID != "2" {
		t.Fatalf("got %+v", got)
	}
}

func TestCollectToolUses_Empty(t *testing.T) {
	if got := collectToolUses(nil); got != nil {
		t.Errorf("nil → %+v, want nil", got)
	}
	if got := collectToolUses([]llm.ContentBlock{llm.TextBlock{Text: "no tools"}}); got != nil {
		t.Errorf("text-only → %+v, want nil", got)
	}
}

func TestTextFromContent(t *testing.T) {
	got := textFromContent([]llm.ContentBlock{
		llm.TextBlock{Text: "hello "},
		llm.ToolUseBlock{ID: "x"}, // ignored
		llm.TextBlock{Text: "world"},
	})
	if got != "hello world" {
		t.Errorf("got %q", got)
	}
}

func TestTextFromContent_Empty(t *testing.T) {
	if got := textFromContent(nil); got != "" {
		t.Errorf("nil → %q", got)
	}
}
