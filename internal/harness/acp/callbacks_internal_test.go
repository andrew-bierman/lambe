package acp

import (
	"errors"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
)

// recordingSink is an inbound that records what the callbacks forward.
type recordingSink struct {
	updates []acpsdk.SessionNotification
	denied  []string
}

func (r *recordingSink) handleUpdate(n acpsdk.SessionNotification) { r.updates = append(r.updates, n) }

func (r *recordingSink) handlePermissionDenied(title string) { r.denied = append(r.denied, title) }

// TestClientCallbacksRefuseUnadvertisedCapabilities pins that every fs and
// terminal request is answered method-not-found (JSON-RPC -32601) and never
// panics, because lambe advertises neither capability.
func TestClientCallbacksRefuseUnadvertisedCapabilities(t *testing.T) {
	cb := &clientCallbacks{sink: &recordingSink{}}
	ctx := t.Context()
	calls := map[string]func() error{
		"fs/readTextFile": func() error {
			_, err := cb.ReadTextFile(ctx, acpsdk.ReadTextFileRequest{})
			return err
		},
		"fs/writeTextFile": func() error {
			_, err := cb.WriteTextFile(ctx, acpsdk.WriteTextFileRequest{})
			return err
		},
		"terminal/create": func() error {
			_, err := cb.CreateTerminal(ctx, acpsdk.CreateTerminalRequest{})
			return err
		},
		"terminal/kill": func() error {
			_, err := cb.KillTerminal(ctx, acpsdk.KillTerminalRequest{})
			return err
		},
		"terminal/output": func() error {
			_, err := cb.TerminalOutput(ctx, acpsdk.TerminalOutputRequest{})
			return err
		},
		"terminal/release": func() error {
			_, err := cb.ReleaseTerminal(ctx, acpsdk.ReleaseTerminalRequest{})
			return err
		},
		"terminal/waitForExit": func() error {
			_, err := cb.WaitForTerminalExit(ctx, acpsdk.WaitForTerminalExitRequest{})
			return err
		},
	}
	for method, call := range calls {
		t.Run(method, func(t *testing.T) {
			var rpc *acpsdk.RequestError
			if err := call(); !errors.As(err, &rpc) {
				t.Fatalf("err = %v, want *RequestError", err)
			}
			if rpc.Code != -32601 {
				t.Errorf("code = %d, want -32601 method not found", rpc.Code)
			}
			data, _ := rpc.Data.(map[string]any)
			if data["method"] != method {
				t.Errorf("error data = %v, want method %q", rpc.Data, method)
			}
		})
	}
}

// TestRequestPermissionAlwaysDenies pins the unattended posture: a reject
// option is selected when one exists, otherwise the request is cancelled;
// allow options are never chosen.
func TestRequestPermissionAlwaysDenies(t *testing.T) {
	opt := func(id string, kind acpsdk.PermissionOptionKind) acpsdk.PermissionOption {
		return acpsdk.PermissionOption{OptionId: acpsdk.PermissionOptionId(id), Kind: kind, Name: id}
	}
	title := "rm -rf"
	tests := []struct {
		name         string
		options      []acpsdk.PermissionOption
		wantSelected string // empty means cancelled
	}{
		{"no options", nil, ""},
		{"only allow options", []acpsdk.PermissionOption{opt("a1", acpsdk.PermissionOptionKindAllowOnce), opt("a2", acpsdk.PermissionOptionKindAllowAlways)}, ""},
		{"reject once", []acpsdk.PermissionOption{opt("a1", acpsdk.PermissionOptionKindAllowOnce), opt("r1", acpsdk.PermissionOptionKindRejectOnce)}, "r1"},
		{"reject always", []acpsdk.PermissionOption{opt("r2", acpsdk.PermissionOptionKindRejectAlways), opt("a1", acpsdk.PermissionOptionKindAllowOnce)}, "r2"},
		{"reject once listed before reject always", []acpsdk.PermissionOption{opt("r1", acpsdk.PermissionOptionKindRejectOnce), opt("r2", acpsdk.PermissionOptionKindRejectAlways)}, "r2"},
		{"reject always listed before reject once", []acpsdk.PermissionOption{opt("r2", acpsdk.PermissionOptionKindRejectAlways), opt("r1", acpsdk.PermissionOptionKindRejectOnce)}, "r2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sink := &recordingSink{}
			cb := &clientCallbacks{sink: sink}
			req := acpsdk.RequestPermissionRequest{Options: tc.options}
			req.ToolCall.Title = &title
			resp, err := cb.RequestPermission(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantSelected == "" {
				if resp.Outcome.Cancelled == nil || resp.Outcome.Selected != nil {
					t.Fatalf("outcome = %+v, want cancelled", resp.Outcome)
				}
			} else if resp.Outcome.Selected == nil || string(resp.Outcome.Selected.OptionId) != tc.wantSelected {
				t.Fatalf("outcome = %+v, want selected %q", resp.Outcome, tc.wantSelected)
			}
			if len(sink.denied) != 1 || sink.denied[0] != title {
				t.Errorf("denial reported to driver = %v, want [%q]", sink.denied, title)
			}
		})
	}
}

func TestRequestPermissionWithoutToolTitleReportsEmptyTitle(t *testing.T) {
	sink := &recordingSink{}
	cb := &clientCallbacks{sink: sink}
	if _, err := cb.RequestPermission(t.Context(), acpsdk.RequestPermissionRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(sink.denied) != 1 || sink.denied[0] != "" {
		t.Fatalf("denied = %q, want one empty title", sink.denied)
	}
}

func TestSessionUpdateForwardsToSink(t *testing.T) {
	sink := &recordingSink{}
	cb := &clientCallbacks{sink: sink}
	n := acpsdk.SessionNotification{SessionId: "s-1"}
	if err := cb.SessionUpdate(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	if len(sink.updates) != 1 || sink.updates[0].SessionId != "s-1" {
		t.Fatalf("updates = %+v", sink.updates)
	}
}
