package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestMock_Name(t *testing.T) {
	if NewMock().Name() != "mock" {
		t.Fatal("name should be 'mock'")
	}
}

func TestMock_Chat_DefaultEcho(t *testing.T) {
	m := NewMock()
	resp, err := m.Chat(context.Background(), Request{Messages: []Message{UserText("hello world")}})
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Text()
	if got != "mock: hello world" {
		t.Fatalf("text = %q, want 'mock: hello world'", got)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q", resp.StopReason)
	}
}

func TestMock_Chat_SkipsToolResultBlocksInLastUser(t *testing.T) {
	// A user message that contains only ToolResultBlocks should not
	// error in the default echo path — it just produces an empty echo.
	m := NewMock()
	resp, err := m.Chat(context.Background(), Request{
		Messages: []Message{
			UserText("first"),
			AssistantText("ack"),
			{Role: RoleUser, Content: []ContentBlock{
				ToolResultBlock{ToolUseID: "t1", Content: "data"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Text() != "mock: " {
		t.Errorf("text = %q, want 'mock: '", resp.Text())
	}
}

func TestMock_Chat_RejectsUnknownBlockInLastUser(t *testing.T) {
	m := NewMock()
	_, err := m.Chat(context.Background(), Request{
		Messages: []Message{
			{Role: RoleUser, Content: []ContentBlock{
				ThinkingBlock{Text: "should not be here on user"},
			}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "no handling for content block") {
		t.Fatalf("err = %v, want unknown-block error", err)
	}
}

func TestMock_SetChatFunc_OverridesDefault(t *testing.T) {
	m := NewMock()
	m.SetChatFunc(func(req Request) (Response, error) {
		return Response{
			Content:    []ContentBlock{TextBlock{Text: "scripted"}},
			StopReason: "scripted",
		}, nil
	})
	resp, err := m.Chat(context.Background(), Request{Messages: []Message{UserText("anything")}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text() != "scripted" || resp.StopReason != "scripted" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestMock_SetChatFunc_PropagatesError(t *testing.T) {
	m := NewMock()
	sentinel := errors.New("scripted boom")
	m.SetChatFunc(func(req Request) (Response, error) {
		return Response{}, sentinel
	})
	_, err := m.Chat(context.Background(), Request{Messages: []Message{UserText("x")}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestMock_SetChatFunc_NilFallsBackToEcho(t *testing.T) {
	m := NewMock()
	m.SetChatFunc(func(req Request) (Response, error) {
		return Response{Content: []ContentBlock{TextBlock{Text: "first"}}}, nil
	})
	if r, _ := m.Chat(context.Background(), Request{Messages: []Message{UserText("x")}}); r.Text() != "first" {
		t.Fatalf("first = %q", r.Text())
	}
	m.SetChatFunc(nil)
	if r, _ := m.Chat(context.Background(), Request{Messages: []Message{UserText("hi")}}); r.Text() != "mock: hi" {
		t.Fatalf("after nil = %q", r.Text())
	}
}

func TestLastUserText_NoUserMessage(t *testing.T) {
	got, err := lastUserText([]Message{AssistantText("only assistant")})
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestLastUserText_FindsMostRecentUser(t *testing.T) {
	got, err := lastUserText([]Message{
		UserText("old"),
		AssistantText("a"),
		UserText("recent"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "recent" {
		t.Errorf("got %q, want 'recent'", got)
	}
}

func TestResponse_Text_ConcatenatesAllTextBlocks(t *testing.T) {
	r := Response{Content: []ContentBlock{
		TextBlock{Text: "alpha "},
		ToolUseBlock{ID: "t", Name: "x", Input: []byte(`{}`)},
		TextBlock{Text: "beta"},
	}}
	if got := r.Text(); got != "alpha beta" {
		t.Errorf("Text() = %q, want 'alpha beta'", got)
	}
}

func TestResponse_Text_EmptyContent(t *testing.T) {
	if got := (Response{}).Text(); got != "" {
		t.Errorf("Text() = %q, want empty", got)
	}
}

func TestProviderNames(t *testing.T) {
	if NewMock().Name() != "mock" {
		t.Error("Mock.Name")
	}
	if (&Anthropic{}).Name() != "anthropic" {
		t.Error("Anthropic.Name")
	}
}

func TestMessageHistory_Messages(t *testing.T) {
	h, err := MessageHistory{}.Add(UserText("hi"))
	if err != nil {
		t.Fatal(err)
	}
	msgs := h.Messages()
	if len(msgs) != 1 || msgs[0].Role != RoleUser {
		t.Fatalf("Messages() = %+v", msgs)
	}
}
