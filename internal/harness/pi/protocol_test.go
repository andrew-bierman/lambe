package pi

import (
	"encoding/json"
	"testing"
)

// TestRecordDecode covers response/event record decoding, including the
// fields the driver branches on.
func TestRecordDecode(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantResp  bool
		wantOK    bool
		wantError string
	}{
		{
			name:     "successful response",
			line:     `{"id":"c1","type":"response","command":"prompt","success":true,"data":{"disposition":"started"}}`,
			wantResp: true,
			wantOK:   true,
		},
		{
			name:      "failed response",
			line:      `{"id":"c2","type":"response","command":"prompt","success":false,"error":"agent is streaming"}`,
			wantResp:  true,
			wantError: "agent is streaming",
		},
		{
			name:     "agent_settled event",
			line:     `{"type":"agent_settled","aborted":false}`,
			wantResp: false,
			wantOK:   false,
		},
		{
			name:     "agent_settled aborted",
			line:     `{"type":"agent_settled","aborted":true}`,
			wantResp: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rec Record
			if err := json.Unmarshal([]byte(tt.line), &rec); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if rec.IsResponse() != tt.wantResp {
				t.Fatalf("IsResponse = %v, want %v", rec.IsResponse(), tt.wantResp)
			}
			if tt.wantResp && rec.OK() != tt.wantOK {
				t.Fatalf("OK = %v, want %v", rec.OK(), tt.wantOK)
			}
			if tt.wantError != "" && rec.Error != tt.wantError {
				t.Fatalf("error = %q, want %q", rec.Error, tt.wantError)
			}
		})
	}
}

// TestRecordSettledAborted checks the aborted flag round-trips.
func TestRecordSettledAborted(t *testing.T) {
	var rec Record
	if err := json.Unmarshal([]byte(`{"type":"agent_settled","aborted":true}`), &rec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !rec.Aborted {
		t.Fatal("aborted flag lost in decode")
	}
}

// TestWireMessageText covers text extraction from both content shapes.
func TestWireMessageText(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "plain string", content: `"hello"`, want: "hello"},
		{name: "text blocks", content: `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, want: "ab"},
		{name: "mixed blocks", content: `[{"type":"thinking","text":"x"},{"type":"text","text":"keep"}]`, want: "keep"},
		{name: "other json", content: `{"nested":true}`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &WireMessage{Role: "assistant", Content: json.RawMessage(tt.content)}
			if got := m.Text(); got != tt.want {
				t.Fatalf("Text() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCommandWireShape verifies commands serialize flat with the
// correlation id, matching the protocol's examples.
func TestCommandWireShape(t *testing.T) {
	b, err := Command{Type: CmdPrompt, Message: "hello", StreamingBehavior: StreamingSteer}.withID("req-1")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]any{"id": "req-1", "type": "prompt", "message": "hello", "streamingBehavior": "steer"}
	if len(got) != len(want) {
		t.Fatalf("wire command = %s, want exactly %v", b, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("field %q = %v, want %v (full: %s)", k, got[k], v, b)
		}
	}
}

// TestStatsResponseDecode covers the get_session_stats payload including
// the optional contextUsage shape.
func TestStatsResponseDecode(t *testing.T) {
	line := `{"sessionId":"s1","tokens":{"input":10,"output":5,"cacheRead":1,"cacheWrite":2,"total":18},"cost":0.5,` +
		`"contextUsage":{"tokens":100,"contextWindow":200,"percent":50}}`
	var s StatsResponse
	if err := json.Unmarshal([]byte(line), &s); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if s.SessionID != "s1" || s.Tokens.Total != 18 || s.Cost != 0.5 {
		t.Fatalf("stats = %+v", s)
	}
	if s.ContextUsage == nil || s.ContextUsage.Percent == nil || *s.ContextUsage.Percent != 50 {
		t.Fatalf("context usage missing: %+v", s.ContextUsage)
	}
}
