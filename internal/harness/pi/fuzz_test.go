package pi

import (
	"encoding/json"
	"strings"
	"testing"
)

var piSeeds = []string{
	"",
	"null",
	"{}",
	"[]",
	`{"type":"response","id":"1","command":"prompt","success":true,"data":{"disposition":"handled"}}`,
	`{"type":"response","id":"2","command":"get_state","success":true,"data":{"isStreaming":true,"sessionId":"s","messageCount":3}}`,
	`{"type":"response","command":"parse","success":false,"error":"bad json"}`,
	`{"type":"response","id":"3","command":"get_session_stats","success":true,"data":{"sessionId":"s","tokens":{"input":1,"output":2,"cacheRead":3,"cacheWrite":4,"total":10},"cost":0.5,"contextUsage":{"tokens":null,"contextWindow":200000,"percent":1.5}}}`,
	`{"type":"message_end","message":{"role":"assistant","content":"plain","stopReason":"stop"}}`,
	`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]}}`,
	`{"type":"message_end","message":{"content":42}}`,
	`{"type":"auto_retry_start","attempt":1,"maxAttempts":3,"delayMs":9223372036854775807,"errorMessage":"429"}`,
	`{"type":"x","attempt":1e400}`,
	`{"type":"x","attempt":99999999999999999999}`,
	`{"type":"x","delayMs":-1.5}`,
	`{"type":"tool_execution_start","toolCallId":"c","toolName":"bash","isError":"yes"}`,
	"{\"type\":\"\\ud800\",\"error\":\"\xff\xfe\x00\"}",
	`{"type":"a","type":"b","data":{"tokens":{"input":-1}}}`,
	strings.Repeat("[", 100000),
	strings.Repeat(`{"a":`, 10000),
	`{"type":"x","error":"` + strings.Repeat("a", 1<<20) + `"}`,
	`{"type":"message_end","message":{"content":[` + strings.Repeat(`{"type":"text","text":"x"},`, 20000) + `{}]}}`,
}

// FuzzRecordDecode decodes arbitrary stdout lines the way the client's read
// loop does and exercises every accessor on the result. Nothing may panic.
func FuzzRecordDecode(f *testing.F) {
	for _, s := range piSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return
		}
		_ = rec.IsResponse()
		_ = rec.OK()
		if rec.Message != nil {
			_ = rec.Message.Text()
		}
		var m *WireMessage
		_ = m.Text()
		if rec.OK() && (rec.Success == nil || !*rec.Success) {
			t.Fatal("OK without a true success flag")
		}
	})
}

// FuzzDecodeStats feeds arbitrary response payloads to decodeStats. Empty
// payloads must be an error rather than zero stats; everything else must
// not panic.
func FuzzDecodeStats(f *testing.F) {
	for _, s := range piSeeds {
		f.Add([]byte(s))
	}
	d := New(Options{})
	f.Fuzz(func(t *testing.T, data []byte) {
		stats, err := d.decodeStats(Record{Data: data})
		if len(data) == 0 && err == nil {
			t.Fatalf("empty payload decoded to %+v without error", stats)
		}
		if err == nil {
			_ = d.sessionFor(stats)
		}
	})
}

// FuzzDecodeResponses covers get_state and prompt-ack payloads, which use the
// driver's plain json decoding; they must tolerate unknown fields and wrong types without panicking.
func FuzzDecodeResponses(f *testing.F) {
	for _, s := range piSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(_ *testing.T, data []byte) {
		var state StateResponse
		var ack PromptAck
		_ = json.Unmarshal(data, &state)
		_ = json.Unmarshal(data, &ack)
	})
}
