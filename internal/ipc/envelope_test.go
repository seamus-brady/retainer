package ipc

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// ---- Request validation ----

func TestRequest_Validate_RequiresFields(t *testing.T) {
	cases := []struct {
		name string
		req  Request
	}{
		{"missing id", Request{Agent: "x", Instruction: "y", WorkspaceRoot: "/w"}},
		{"missing agent", Request{ID: "i", Instruction: "y", WorkspaceRoot: "/w"}},
		{"missing instruction", Request{ID: "i", Agent: "x", WorkspaceRoot: "/w"}},
		{"missing workspace_root", Request{ID: "i", Agent: "x", Instruction: "y"}},
	}
	for _, tc := range cases {
		if err := tc.req.Validate(); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
}

func TestRequest_Validate_HappyPath(t *testing.T) {
	req := Request{ID: "i", Agent: "researcher", Instruction: "do work", WorkspaceRoot: "/ws"}
	if err := req.Validate(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRequest_Validate_RejectsBadTimestamp(t *testing.T) {
	req := Request{ID: "i", Agent: "x", Instruction: "y", WorkspaceRoot: "/w", Timestamp: "yesterday"}
	if err := req.Validate(); err == nil {
		t.Error("expected timestamp error")
	}
}

// ---- Response validation ----

func TestResponse_Validate_StatusEnum(t *testing.T) {
	cases := []struct {
		status  string
		wantErr bool
	}{
		{StatusProgress, false},
		{StatusComplete, false},
		{StatusError, true}, // missing error field
		{"unknown", true},
		{"", true},
	}
	for _, tc := range cases {
		err := Response{ID: "x", Status: tc.status}.Validate()
		if tc.wantErr && err == nil {
			t.Errorf("status %q should error", tc.status)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("status %q unexpected error: %v", tc.status, err)
		}
	}
}

func TestResponse_Validate_ErrorRequiresErrorField(t *testing.T) {
	if err := (Response{ID: "x", Status: StatusError}).Validate(); err == nil {
		t.Error("error response without Error field should fail")
	}
	if err := (Response{ID: "x", Status: StatusError, Error: "boom"}).Validate(); err != nil {
		t.Errorf("error response with Error should pass: %v", err)
	}
}

func TestResponse_IsTerminal(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{StatusProgress, false},
		{StatusComplete, true},
		{StatusError, true},
	}
	for _, tc := range cases {
		got := Response{Status: tc.status}.IsTerminal()
		if got != tc.want {
			t.Errorf("status %q IsTerminal = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// ---- WriteRequest / ReadRequest round-trip ----

func TestRequest_RoundTrip(t *testing.T) {
	original := Request{
		ID:            "dispatch-1",
		ParentCycleID: "cycle-1",
		InstanceID:    "f47ac10b",
		Agent:         "researcher",
		Instruction:   "investigate auth",
		Context:       "extra context",
		WorkspaceRoot: "/abs/ws",
		ArtefactDir:   "data/run/dispatch-1/artefacts/",
		InputPaths:    []string{"data/in.txt"},
	}
	var buf bytes.Buffer
	if err := WriteRequest(&buf, original); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRequest(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != original.ID {
		t.Errorf("ID = %q, want %q", got.ID, original.ID)
	}
	if got.Instruction != original.Instruction {
		t.Errorf("instruction round-trip wrong")
	}
	if got.WorkspaceRoot != original.WorkspaceRoot {
		t.Errorf("workspace_root round-trip wrong")
	}
	if got.ArtefactDir != original.ArtefactDir {
		t.Errorf("artefact_dir round-trip wrong")
	}
	if len(got.InputPaths) != 1 || got.InputPaths[0] != "data/in.txt" {
		t.Errorf("input_paths round-trip wrong: %+v", got.InputPaths)
	}
	if got.InstanceID != original.InstanceID {
		t.Errorf("instance_id round-trip = %q, want %q", got.InstanceID, original.InstanceID)
	}
	// WriteRequest stamps timestamp when caller leaves it empty.
	if _, err := time.Parse(time.RFC3339, got.Timestamp); err != nil {
		t.Errorf("timestamp = %q, want RFC3339: %v", got.Timestamp, err)
	}
}

func TestRequest_InstanceIDOmittedWhenEmpty(t *testing.T) {
	// instance_id should disappear from the wire entirely when
	// empty so the cog can boot without identity (degraded mode)
	// without leaving an empty-string artifact in every envelope.
	var buf bytes.Buffer
	req := Request{
		ID:            "x",
		Agent:         "researcher",
		Instruction:   "do thing",
		WorkspaceRoot: "/ws",
	}
	if err := WriteRequest(&buf, req); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	if strings.Contains(body, "instance_id") {
		t.Errorf("empty instance_id should not appear in wire bytes; got: %s", body)
	}
}

func TestWriteRequest_RejectsInvalid(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRequest(&buf, Request{}); err == nil {
		t.Error("invalid request should fail to write")
	}
}

func TestReadRequest_EOFOnEmptyInput(t *testing.T) {
	if _, err := ReadRequest(strings.NewReader("")); !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF on empty stream; got %v", err)
	}
}

func TestReadRequest_RejectsMalformedJSON(t *testing.T) {
	if _, err := ReadRequest(strings.NewReader("{not valid json\n")); err == nil {
		t.Error("expected decode error")
	}
}

// ---- Response read stream ----

func TestReader_ReadsProgressThenComplete(t *testing.T) {
	body := `{"id":"d1","agent":"x","status":"progress","turn":1}` + "\n" +
		`{"id":"d1","agent":"x","status":"progress","turn":2}` + "\n" +
		`{"id":"d1","agent":"x","status":"complete","success":true,"result":"ok"}` + "\n"
	r := NewReader(strings.NewReader(body))

	resp, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != StatusProgress || resp.Turn != 1 {
		t.Errorf("first read wrong: %+v", resp)
	}
	if resp.IsTerminal() {
		t.Error("progress shouldn't be terminal")
	}

	resp, _ = r.Next()
	if resp.Status != StatusProgress || resp.Turn != 2 {
		t.Errorf("second read wrong: %+v", resp)
	}

	resp, _ = r.Next()
	if !resp.IsTerminal() {
		t.Error("complete should be terminal")
	}
	if resp.Result != "ok" {
		t.Errorf("result = %q, want ok", resp.Result)
	}

	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF after stream end; got %v", err)
	}
}

func TestReader_RejectsMalformedLine(t *testing.T) {
	body := `{"id":"d1","agent":"x","status":"progress"}` + "\n" +
		"{not valid\n"
	r := NewReader(strings.NewReader(body))
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(); err == nil {
		t.Error("malformed line should error")
	}
}

func TestWriteResponse_StampsTimestamp(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteResponse(&buf, Response{ID: "x", Status: StatusComplete}); err != nil {
		t.Fatal(err)
	}
	r := NewReader(&buf)
	resp, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, resp.Timestamp); err != nil {
		t.Errorf("timestamp not RFC3339: %v", err)
	}
}
