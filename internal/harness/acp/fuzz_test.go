package acp

import (
	"encoding/json"
	"errors"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
)

// FuzzSessionRestoreError feeds arbitrary JSON-RPC error codes and data
// payloads through sessionRestoreError. It must never panic, must keep the
// original error reachable, and may only report ErrSessionNotFound for the
// two documented backend shapes.
func FuzzSessionRestoreError(f *testing.F) {
	type seed struct {
		code int
		data string
		id   string
	}
	for _, s := range []seed{
		{-32002, `{"sessionId":"s1"}`, "s1"},
		{-32002, `{"sessionId":"other"}`, "s1"},
		{-32603, `{"details":"no rollout found for thread id s1"}`, "s1"},
		{-32603, `{"details":"boom"}`, "s1"},
		{-32601, `null`, "s1"},
		{0, `{}`, ""},
		{-32002, `{"sessionId":5}`, "5"},
		{-32002, `[1,2,3]`, "s1"},
		{-32002, `"string"`, "s1"},
		{-32002, `{"sessionId":null,"details":{"nested":[]}}`, ""},
		{2147483647, `{"sessionId":1e999}`, "\x00"},
		{-32002, `{"sessionId":"\ud800"}`, "\xff"},
		{-32603, `{"details":"no rollout found for thread id "}`, ""},
		{-32002, `{"sessionId":"` + longString(1<<20) + `"}`, longString(1 << 20)},
	} {
		f.Add(s.code, []byte(s.data), s.id)
	}
	f.Fuzz(func(t *testing.T, code int, data []byte, id string) {
		if !json.Valid(data) {
			t.Skip()
		}
		orig := &acpsdk.RequestError{Code: code, Message: "m", Data: json.RawMessage(data)}
		got := sessionRestoreError(orig, id)
		if got == nil {
			t.Fatal("non-nil error became nil")
		}
		var back *acpsdk.RequestError
		if !errors.As(got, &back) || back != orig {
			t.Fatalf("original error not reachable from %v", got)
		}
		if errors.Is(got, ErrSessionNotFound) && code != -32002 && code != -32603 {
			t.Fatalf("code %d classified as session-not-found", code)
		}
		if sessionRestoreError(nil, id) != nil {
			t.Fatal("nil error became non-nil")
		}
	})
}

func longString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
