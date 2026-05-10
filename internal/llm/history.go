package llm

import "fmt"

// MessageHistory is the opaque, invariant-preserving wrapper around []Message
// that the cog and agents use as conversation state. Add() is the single
// chokepoint that enforces Anthropic's alternation invariants; FromList()
// runs the sanitisation pipeline at ingest.
//
// The four Anthropic Messages-API invariants from message-history.md:
//   1. Every assistant tool_use must have a matching tool_result in the
//      next user message.
//   2. Every user tool_result must have a matching tool_use in the prior
//      assistant message.
//   3. Messages alternate user/assistant.
//   4. First message is user-role.
//
// (1) and (2) require ToolUseBlock / ToolResultBlock; their handling lands
// alongside those types. (3) and (4) are enforced now.
type MessageHistory struct {
	msgs []Message
}

// Messages returns the underlying slice. Callers must not mutate it.
func (h MessageHistory) Messages() []Message { return h.msgs }

// Len returns the number of messages currently in the history.
func (h MessageHistory) Len() int { return len(h.msgs) }

// Add appends msg, returning a new MessageHistory. The original is
// unchanged. Returns an error if msg violates the alternation or
// leading-role invariants.
func (h MessageHistory) Add(msg Message) (MessageHistory, error) {
	if len(h.msgs) == 0 {
		if msg.Role != RoleUser {
			return h, fmt.Errorf("history: first message must be user-role, got %q", msg.Role)
		}
	} else {
		last := h.msgs[len(h.msgs)-1].Role
		if msg.Role == last {
			return h, fmt.Errorf("history: roles must alternate, got %q after %q", msg.Role, last)
		}
	}
	next := make([]Message, len(h.msgs)+1)
	copy(next, h.msgs)
	next[len(h.msgs)] = msg
	return MessageHistory{msgs: next}, nil
}

// Truncate returns a MessageHistory with at most `maxMessages`
// most-recent messages. Older messages are dropped from the front.
//
// Anthropic invariant 4 (first message is user-role) is preserved
// by walking back to the most recent user-role start, even if that
// means keeping one fewer than maxMessages — better that than
// emit an assistant-led history the API rejects.
//
// Tool-call pair invariants (1, 2) are preserved by FromList's
// orphan-stripping pipeline; we route the truncated slice through
// FromList so any pair severed by the truncation gets cleaned up.
//
// maxMessages <= 0 is treated as "no cap" — the original history
// is returned unchanged. Used by the cog when
// `[cog].max_context_messages` is configured.
func (h MessageHistory) Truncate(maxMessages int) MessageHistory {
	if maxMessages <= 0 || len(h.msgs) <= maxMessages {
		return h
	}
	// Take the last maxMessages, then walk forward to the first
	// user-role message so the result starts with a user turn.
	tail := h.msgs[len(h.msgs)-maxMessages:]
	for len(tail) > 0 && tail[0].Role != RoleUser {
		tail = tail[1:]
	}
	// Re-run the FromList pipeline so orphan tool_use / tool_result
	// pairs the cut may have severed get cleaned up.
	return FromList(tail)
}

// FromList builds a MessageHistory from raw messages, applying the
// sanitisation pipeline:
//  1. Drop leading assistant messages.
//  2. Coalesce consecutive same-role messages into one (concatenating
//     their content blocks).
//  3. Drop orphan tool_result blocks (no matching tool_use in the prior
//     assistant message — Anthropic invariant 2).
//  4. Inject stub tool_result blocks for orphan tool_use (no matching
//     tool_result in the next user message — Anthropic invariant 1).
func FromList(msgs []Message) MessageHistory {
	start := 0
	for start < len(msgs) && msgs[start].Role != RoleUser {
		start++
	}
	if start >= len(msgs) {
		return MessageHistory{}
	}

	var out []Message
	for _, m := range msgs[start:] {
		if len(out) > 0 && out[len(out)-1].Role == m.Role {
			out[len(out)-1].Content = append(out[len(out)-1].Content, m.Content...)
			continue
		}
		out = append(out, Message{
			Role:    m.Role,
			Content: append([]ContentBlock{}, m.Content...),
		})
	}

	// Step 3: drop orphan tool_results.
	for i := range out {
		if out[i].Role != RoleUser {
			continue
		}
		var prevToolUseIDs map[string]struct{}
		if i > 0 && out[i-1].Role == RoleAssistant {
			prevToolUseIDs = collectToolUseIDs(out[i-1].Content)
		}
		filtered := out[i].Content[:0]
		for _, b := range out[i].Content {
			if tr, ok := b.(ToolResultBlock); ok {
				if _, has := prevToolUseIDs[tr.ToolUseID]; !has {
					continue
				}
			}
			filtered = append(filtered, b)
		}
		out[i].Content = filtered
	}

	// Step 4: stub missing tool_results for assistant tool_use blocks.
	for i := range out {
		if out[i].Role != RoleAssistant {
			continue
		}
		ids := collectToolUseIDs(out[i].Content)
		if len(ids) == 0 {
			continue
		}
		var have map[string]struct{}
		if i+1 < len(out) && out[i+1].Role == RoleUser {
			have = collectToolResultIDs(out[i+1].Content)
		} else {
			have = map[string]struct{}{}
		}
		var stubs []ContentBlock
		for id := range ids {
			if _, ok := have[id]; ok {
				continue
			}
			stubs = append(stubs, ToolResultBlock{
				ToolUseID: id,
				Content:   "(tool result missing — recovered by history sanitiser)",
				IsError:   true,
			})
		}
		if len(stubs) == 0 {
			continue
		}
		if i+1 < len(out) && out[i+1].Role == RoleUser {
			out[i+1].Content = append(stubs, out[i+1].Content...)
		} else {
			stub := Message{Role: RoleUser, Content: stubs}
			out = append(out[:i+1], append([]Message{stub}, out[i+1:]...)...)
		}
	}

	// Re-filter empty messages that step 3 may have produced.
	pruned := out[:0]
	for _, m := range out {
		if len(m.Content) == 0 {
			continue
		}
		pruned = append(pruned, m)
	}
	return MessageHistory{msgs: pruned}
}

func collectToolUseIDs(blocks []ContentBlock) map[string]struct{} {
	out := map[string]struct{}{}
	for _, b := range blocks {
		if tu, ok := b.(ToolUseBlock); ok {
			out[tu.ID] = struct{}{}
		}
	}
	return out
}

func collectToolResultIDs(blocks []ContentBlock) map[string]struct{} {
	out := map[string]struct{}{}
	for _, b := range blocks {
		if tr, ok := b.(ToolResultBlock); ok {
			out[tr.ToolUseID] = struct{}{}
		}
	}
	return out
}
