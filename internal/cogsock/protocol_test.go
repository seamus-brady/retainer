package cogsock

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClientMsg_ValidateRequiresType(t *testing.T) {
	if err := (ClientMsg{}).Validate(); err == nil {
		t.Error("empty type should fail validation")
	}
}

func TestClientMsg_ValidateSubmitRequiresInput(t *testing.T) {
	if err := (ClientMsg{Type: MsgTypeSubmit}).Validate(); err == nil {
		t.Error("submit without input should fail")
	}
	if err := (ClientMsg{Type: MsgTypeSubmit, Input: "  "}).Validate(); err == nil {
		t.Error("submit with whitespace input should fail")
	}
	if err := (ClientMsg{Type: MsgTypeSubmit, Input: "hi"}).Validate(); err != nil {
		t.Errorf("submit with input should pass: %v", err)
	}
}

func TestClientMsg_ValidatePingNoFields(t *testing.T) {
	if err := (ClientMsg{Type: MsgTypePing}).Validate(); err != nil {
		t.Errorf("ping should pass: %v", err)
	}
}

func TestClientMsg_ValidateSubscribeCycleLogNoFields(t *testing.T) {
	if err := (ClientMsg{Type: MsgTypeSubscribeCycleLog}).Validate(); err != nil {
		t.Errorf("subscribe_cycle_log should pass: %v", err)
	}
}

func TestClientMsg_ValidateUnknownTypeReturnsErrUnknown(t *testing.T) {
	err := (ClientMsg{Type: "something_new"}).Validate()
	if err == nil {
		t.Fatal("unknown type should fail")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("expected ErrUnknownMessageType-shaped error; got %v", err)
	}
}

func TestServerMsg_RoundTrip(t *testing.T) {
	original := ServerMsg{
		Type:       MsgTypeReady,
		AgentName:  "retainer",
		InstanceID: "f47ac10b",
		Timestamp:  "2026-05-03T12:00:00Z",
	}
	body, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var got ServerMsg
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != MsgTypeReady || got.AgentName != "retainer" || got.InstanceID != "f47ac10b" {
		t.Errorf("round-trip drift: %+v", got)
	}
}

func TestServerMsg_OmitemptyOnUnusedFields(t *testing.T) {
	body, err := json.Marshal(ServerMsg{Type: MsgTypePong})
	if err != nil {
		t.Fatal(err)
	}
	wire := string(body)
	for _, key := range []string{"agent_name", "instance_id", "cycle_id", "body", "tools", "event"} {
		if strings.Contains(wire, key) {
			t.Errorf("wire %q should omit %q for pong-only message", wire, key)
		}
	}
}
