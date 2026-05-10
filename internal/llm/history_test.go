package llm

import (
	"strings"
	"testing"
)

func TestHistory_AddRejectsLeadingAssistant(t *testing.T) {
	var h MessageHistory
	_, err := h.Add(AssistantText("nope"))
	if err == nil {
		t.Fatal("expected error adding assistant to empty history")
	}
	if !strings.Contains(err.Error(), "first message must be user-role") {
		t.Fatalf("err = %v", err)
	}
}

func TestHistory_AddAlternates(t *testing.T) {
	var h MessageHistory
	h, err := h.Add(UserText("hello"))
	if err != nil {
		t.Fatalf("user add: %v", err)
	}
	h, err = h.Add(AssistantText("hi"))
	if err != nil {
		t.Fatalf("assistant add: %v", err)
	}
	h, err = h.Add(UserText("how are you"))
	if err != nil {
		t.Fatalf("user add 2: %v", err)
	}
	if h.Len() != 3 {
		t.Fatalf("len = %d, want 3", h.Len())
	}
}

func TestHistory_AddRejectsConsecutiveSameRole(t *testing.T) {
	var h MessageHistory
	h, err := h.Add(UserText("first"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Add(UserText("second"))
	if err == nil {
		t.Fatal("expected error adding user after user")
	}
	if !strings.Contains(err.Error(), "roles must alternate") {
		t.Fatalf("err = %v", err)
	}
}

func TestHistory_AddIsImmutable(t *testing.T) {
	var h0 MessageHistory
	h1, err := h0.Add(UserText("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if h0.Len() != 0 {
		t.Fatalf("h0 mutated, len = %d", h0.Len())
	}
	if h1.Len() != 1 {
		t.Fatalf("h1 len = %d, want 1", h1.Len())
	}
}

func TestFromList_DropsLeadingAssistant(t *testing.T) {
	in := []Message{
		AssistantText("orphan"),
		AssistantText("also orphan"),
		UserText("real start"),
		AssistantText("reply"),
	}
	h := FromList(in)
	if h.Len() != 2 {
		t.Fatalf("len = %d, want 2", h.Len())
	}
	if h.msgs[0].Role != RoleUser {
		t.Fatalf("first role = %q, want user", h.msgs[0].Role)
	}
}

func TestFromList_CoalescesSameRoleRuns(t *testing.T) {
	in := []Message{
		UserText("part one"),
		UserText("part two"),
		UserText("part three"),
		AssistantText("reply A"),
		AssistantText("reply B"),
	}
	h := FromList(in)
	if h.Len() != 2 {
		t.Fatalf("len = %d, want 2", h.Len())
	}
	if got := len(h.msgs[0].Content); got != 3 {
		t.Fatalf("user content blocks = %d, want 3", got)
	}
	if got := len(h.msgs[1].Content); got != 2 {
		t.Fatalf("assistant content blocks = %d, want 2", got)
	}
}

func TestFromList_OnlyAssistantInputReturnsEmpty(t *testing.T) {
	in := []Message{
		AssistantText("orphan"),
		AssistantText("orphan two"),
	}
	h := FromList(in)
	if h.Len() != 0 {
		t.Fatalf("len = %d, want 0", h.Len())
	}
}

func TestFromList_DoesNotAliasInput(t *testing.T) {
	in := []Message{UserText("original")}
	h := FromList(in)
	// Mutating the input slice's content must not affect h.
	in[0].Content[0] = TextBlock{Text: "mutated"}
	got := h.msgs[0].Content[0].(TextBlock).Text
	if got != "original" {
		t.Fatalf("history aliased input, content = %q", got)
	}
}

func TestFromList_EmptyInput(t *testing.T) {
	h := FromList(nil)
	if h.Len() != 0 {
		t.Fatalf("len = %d, want 0", h.Len())
	}
}

func TestFromList_DropsOrphanToolResult(t *testing.T) {
	in := []Message{
		{Role: RoleUser, Content: []ContentBlock{
			TextBlock{Text: "hi"},
			ToolResultBlock{ToolUseID: "ghost", Content: "stale"},
		}},
		AssistantText("ok"),
	}
	h := FromList(in)
	if h.Len() != 2 {
		t.Fatalf("len = %d, want 2", h.Len())
	}
	if got := len(h.msgs[0].Content); got != 1 {
		t.Fatalf("user content blocks = %d, want 1 (orphan tool_result dropped)", got)
	}
	if _, ok := h.msgs[0].Content[0].(TextBlock); !ok {
		t.Fatalf("remaining block = %T, want TextBlock", h.msgs[0].Content[0])
	}
}

func TestFromList_StubsMissingToolResult(t *testing.T) {
	in := []Message{
		UserText("search for cats"),
		{Role: RoleAssistant, Content: []ContentBlock{
			ToolUseBlock{ID: "call_1", Name: "search", Input: []byte(`{"q":"cats"}`)},
		}},
		// next user message missing — invariant 1 violated. Sanitiser
		// must inject a stub user message with a tool_result.
	}
	h := FromList(in)
	// Expect: user, assistant(tool_use), user(stub tool_result).
	if h.Len() != 3 {
		t.Fatalf("len = %d, want 3", h.Len())
	}
	stub, ok := h.msgs[2].Content[0].(ToolResultBlock)
	if !ok {
		t.Fatalf("stub block = %T, want ToolResultBlock", h.msgs[2].Content[0])
	}
	if stub.ToolUseID != "call_1" || !stub.IsError {
		t.Fatalf("stub = %+v", stub)
	}
}

func TestFromList_KeepsMatchedToolPair(t *testing.T) {
	in := []Message{
		UserText("search"),
		{Role: RoleAssistant, Content: []ContentBlock{
			ToolUseBlock{ID: "call_1", Name: "search", Input: []byte(`{}`)},
		}},
		{Role: RoleUser, Content: []ContentBlock{
			ToolResultBlock{ToolUseID: "call_1", Content: "results"},
		}},
		AssistantText("done"),
	}
	h := FromList(in)
	if h.Len() != 4 {
		t.Fatalf("len = %d, want 4", h.Len())
	}
	tr, ok := h.msgs[2].Content[0].(ToolResultBlock)
	if !ok || tr.ToolUseID != "call_1" {
		t.Fatalf("matched tool_result not preserved: %+v", h.msgs[2].Content)
	}
}

// ---- Truncate ----

func TestTruncate_NoOpWhenUnderLimit(t *testing.T) {
	h := FromList([]Message{
		UserText("a"),
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock{Text: "b"}}},
	})
	got := h.Truncate(10)
	if got.Len() != 2 {
		t.Errorf("len = %d, want 2", got.Len())
	}
}

func TestTruncate_NoOpWhenZeroOrNegative(t *testing.T) {
	h := FromList([]Message{
		UserText("a"),
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock{Text: "b"}}},
		UserText("c"),
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock{Text: "d"}}},
	})
	for _, n := range []int{0, -1, -100} {
		if got := h.Truncate(n); got.Len() != 4 {
			t.Errorf("Truncate(%d) len = %d, want 4 (no-op)", n, got.Len())
		}
	}
}

func TestTruncate_KeepsMostRecentAndStartsWithUser(t *testing.T) {
	// 6 messages: u1, a1, u2, a2, u3, a3. Cap to 3 → would land
	// on a2/u3/a3 raw, but Truncate must walk forward to a user
	// turn → final history is u3/a3 (2 messages).
	h := FromList([]Message{
		UserText("u1"),
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock{Text: "a1"}}},
		UserText("u2"),
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock{Text: "a2"}}},
		UserText("u3"),
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock{Text: "a3"}}},
	})
	got := h.Truncate(3)
	if got.Len() != 2 {
		t.Errorf("len = %d, want 2 (most recent user-led pair)", got.Len())
	}
	msgs := got.Messages()
	if msgs[0].Role != RoleUser {
		t.Errorf("first message role = %v, want user", msgs[0].Role)
	}
	// Walk text: should be u3 then a3, NOT u1/a1.
	first := msgs[0].Content[0].(TextBlock).Text
	if first != "u3" {
		t.Errorf("first kept message = %q, want u3", first)
	}
}

func TestTruncate_DropsOrphanToolPairOnCut(t *testing.T) {
	// Build a history where cutting drops the matching tool_use,
	// leaving an orphan tool_result. FromList's sanitiser should
	// drop the orphan when Truncate routes through it.
	h := FromList([]Message{
		UserText("ask"),
		{Role: RoleAssistant, Content: []ContentBlock{
			ToolUseBlock{ID: "t1", Name: "x", Input: []byte(`{}`)},
		}},
		{Role: RoleUser, Content: []ContentBlock{
			ToolResultBlock{ToolUseID: "t1", Content: "ok"},
		}},
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock{Text: "result"}}},
		UserText("next"),
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock{Text: "more"}}},
	})
	// Cap to 2 → would keep "next" + "more". Tool pair from earlier
	// gets cut entirely. No orphan because both halves drop together.
	got := h.Truncate(2)
	for _, m := range got.Messages() {
		for _, b := range m.Content {
			if _, ok := b.(ToolResultBlock); ok {
				t.Errorf("orphan tool_result should have been stripped: %+v", b)
			}
			if _, ok := b.(ToolUseBlock); ok {
				t.Errorf("tool_use should have been cut: %+v", b)
			}
		}
	}
}
