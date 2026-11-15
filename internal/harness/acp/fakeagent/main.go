// Command fakeagent is a minimal scriptable ACP agent used by the acp
// driver's tests. It speaks the real protocol through the acp-go-sdk agent
// side over its own stdio and selects one behavior via its first argument:
//
//	work            tool call (in_progress → completed) + message, then
//	                end_turn; honors session/cancel by answering cancelled
//	stream          a long agent-message chunk stream, then end_turn; honors
//	                cancel
//	error           answers session/prompt with a JSON-RPC error
//	permission      asks session/request_permission, records the denial,
//	                then end_turn; honors cancel
//	ignore-interrupt streams past a session/cancel and still ends the turn
//	silent          accepts the prompt and never answers; ignores cancel
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
)

// chunkDelay spaces streamed chunks so silence-watchdog and streaming
// behavior is observable in tests.
const chunkDelay = 30 * time.Millisecond

func main() {
	behavior := "work"
	if len(os.Args) > 1 {
		behavior = os.Args[1]
	}
	agent := &fakeAgent{behavior: behavior, sessions: map[string]string{}}
	conn := acpsdk.NewAgentSideConnection(agent, os.Stdout, os.Stdin)
	currentConn = conn
	<-conn.Done()
}

// currentConn is the agent-side connection, set once in main so turn
// handlers can push session/update notifications.
var currentConn *acpsdk.AgentSideConnection

// sendUpdate pushes one session/update notification to the client.
func sendUpdate(ctx context.Context, conn *acpsdk.AgentSideConnection, sessionID acpsdk.SessionId, u acpsdk.SessionUpdate) error {
	return conn.SessionUpdate(ctx, acpsdk.SessionNotification{SessionId: sessionID, Update: u})
}

// sleepOrCancel waits for d, returning ctx.Err() when the turn was
// cancelled.
func sleepOrCancel(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cancelled is the stop response for an honored interrupt.
func cancelled() acpsdk.PromptResponse {
	return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled}
}

// ptrStatus boxes a tool-call status.
func ptrStatus(s acpsdk.ToolCallStatus) *acpsdk.ToolCallStatus { return &s }

// ptrKind boxes a tool kind.
func ptrKind(k acpsdk.ToolKind) *acpsdk.ToolKind { return &k }

// ptrString boxes a string.
func ptrString(s string) *string { return &s }

// fakeAgent implements the acp-go-sdk Agent interface for one behavior.
type fakeAgent struct {
	behavior string
	sessions map[string]string // session id → behavior at creation
	nextID   int
}

func (a *fakeAgent) Initialize(_ context.Context, _ acpsdk.InitializeRequest) (acpsdk.InitializeResponse, error) {
	return acpsdk.InitializeResponse{
		ProtocolVersion: 1,
		AuthMethods:     []acpsdk.AuthMethod{},
	}, nil
}

func (a *fakeAgent) NewSession(_ context.Context, _ acpsdk.NewSessionRequest) (acpsdk.NewSessionResponse, error) {
	a.nextID++
	id := fmt.Sprintf("fake-%d", a.nextID)
	a.sessions[id] = a.behavior
	return acpsdk.NewSessionResponse{SessionId: acpsdk.SessionId(id)}, nil
}

func (a *fakeAgent) Authenticate(context.Context, acpsdk.AuthenticateRequest) (acpsdk.AuthenticateResponse, error) {
	return acpsdk.AuthenticateResponse{}, acpsdk.NewMethodNotFound("authenticate")
}

func (a *fakeAgent) Logout(context.Context, acpsdk.LogoutRequest) (acpsdk.LogoutResponse, error) {
	return acpsdk.LogoutResponse{}, nil
}

func (a *fakeAgent) Cancel(context.Context, acpsdk.CancelNotification) error { return nil }

func (a *fakeAgent) CloseSession(context.Context, acpsdk.CloseSessionRequest) (acpsdk.CloseSessionResponse, error) {
	return acpsdk.CloseSessionResponse{}, nil
}

func (a *fakeAgent) ListSessions(context.Context, acpsdk.ListSessionsRequest) (acpsdk.ListSessionsResponse, error) {
	return acpsdk.ListSessionsResponse{}, acpsdk.NewMethodNotFound("session/list")
}

func (a *fakeAgent) ResumeSession(context.Context, acpsdk.ResumeSessionRequest) (acpsdk.ResumeSessionResponse, error) {
	return acpsdk.ResumeSessionResponse{}, acpsdk.NewMethodNotFound("session/resume")
}

func (a *fakeAgent) SetSessionConfigOption(context.Context, acpsdk.SetSessionConfigOptionRequest) (acpsdk.SetSessionConfigOptionResponse, error) {
	return acpsdk.SetSessionConfigOptionResponse{}, acpsdk.NewMethodNotFound("session/setConfigOption")
}

func (a *fakeAgent) SetSessionMode(context.Context, acpsdk.SetSessionModeRequest) (acpsdk.SetSessionModeResponse, error) {
	return acpsdk.SetSessionModeResponse{}, acpsdk.NewMethodNotFound("session/setMode")
}

// Prompt runs one turn according to the session's behavior. Honoring the
// interrupt is modeled the way real agents do it: session/cancel cancels the
// in-flight Prompt handler's context (the SDK wires this), and the handler
// answers with stopReason "cancelled".
func (a *fakeAgent) Prompt(ctx context.Context, p acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
	behavior := a.sessions[string(p.SessionId)]
	switch behavior {
	case "work":
		return a.turnWork(ctx, p)
	case "stream":
		return a.turnStream(ctx, p)
	case "error":
		return acpsdk.PromptResponse{}, acpsdk.NewInternalError(map[string]string{"cause": "boom"})
	case "permission":
		return a.turnPermission(ctx, p)
	case "ignore-interrupt":
		return a.turnIgnoreInterrupt(p)
	case "silent":
		<-make(chan struct{}) // never answer; only the watchdog ends this
		return acpsdk.PromptResponse{}, nil
	default:
		return acpsdk.PromptResponse{}, acpsdk.NewInvalidParams(map[string]string{"behavior": behavior})
	}
}

// turnWork: a tool call that starts and completes, an assistant message,
// then end_turn.
func (a *fakeAgent) turnWork(ctx context.Context, p acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
	conn := currentConn
	if conn == nil {
		return acpsdk.PromptResponse{}, errors.New("no connection")
	}
	sid := p.SessionId
	tool := acpsdk.ToolCallId("tool-1")
	if err := sendUpdate(ctx, conn, sid, acpsdk.SessionUpdate{ToolCall: &acpsdk.SessionUpdateToolCall{
		SessionUpdate: "tool_call",
		ToolCallId:    tool,
		Title:         "run tests",
		Kind:          acpsdk.ToolKindExecute,
		Status:        acpsdk.ToolCallStatusInProgress,
	}}); err != nil {
		return acpsdk.PromptResponse{}, err
	}
	if err := sleepOrCancel(ctx, chunkDelay); err != nil {
		return cancelled(), nil
	}
	if err := sendUpdate(ctx, conn, sid, acpsdk.SessionUpdate{ToolCallUpdate: &acpsdk.SessionToolCallUpdate{
		SessionUpdate: "tool_call_update",
		ToolCallId:    tool,
		Title:         ptrString("run tests"),
		Status:        ptrStatus(acpsdk.ToolCallStatusCompleted),
	}}); err != nil {
		return acpsdk.PromptResponse{}, err
	}
	if err := sendUpdate(ctx, conn, sid, acpsdk.SessionUpdate{AgentMessageChunk: &acpsdk.SessionUpdateAgentMessageChunk{
		SessionUpdate: "agent_message_chunk",
		Content:       acpsdk.TextBlock("work finished"),
	}}); err != nil {
		return acpsdk.PromptResponse{}, err
	}
	return acpsdk.PromptResponse{
		StopReason: acpsdk.StopReasonEndTurn,
		Usage:      &acpsdk.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120},
	}, nil
}

// turnStream: a long update stream, then end_turn. The stop only comes after
// the stream, so a driver that settled early on updates would be wrong.
func (a *fakeAgent) turnStream(ctx context.Context, p acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
	conn := currentConn
	if conn == nil {
		return acpsdk.PromptResponse{}, errors.New("no connection")
	}
	for i := range 10 {
		if err := sleepOrCancel(ctx, chunkDelay); err != nil {
			return cancelled(), nil
		}
		if err := sendUpdate(ctx, conn, p.SessionId, acpsdk.SessionUpdate{ToolCallUpdate: &acpsdk.SessionToolCallUpdate{
			SessionUpdate: "tool_call_update",
			ToolCallId:    acpsdk.ToolCallId(fmt.Sprintf("tool-%d", i)),
			Title:         ptrString(fmt.Sprintf("stream step %d", i)),
			Status:        ptrStatus(acpsdk.ToolCallStatusInProgress),
		}}); err != nil {
			return acpsdk.PromptResponse{}, err
		}
		if err := sendUpdate(ctx, conn, p.SessionId, acpsdk.SessionUpdate{AgentMessageChunk: &acpsdk.SessionUpdateAgentMessageChunk{
			SessionUpdate: "agent_message_chunk",
			Content:       acpsdk.TextBlock(fmt.Sprintf("chunk %d; ", i)),
		}}); err != nil {
			return acpsdk.PromptResponse{}, err
		}
	}
	return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
}

// turnPermission: asks the client for permission; the test driver
// auto-denies, which the agent observes as a rejected outcome.
func (a *fakeAgent) turnPermission(ctx context.Context, p acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
	conn := currentConn
	if conn == nil {
		return acpsdk.PromptResponse{}, errors.New("no connection")
	}
	resp, err := conn.RequestPermission(ctx, acpsdk.RequestPermissionRequest{
		SessionId: p.SessionId,
		ToolCall: acpsdk.ToolCallUpdate{
			ToolCallId: "tool-perm",
			Title:      ptrString("run dangerous tool"),
			Kind:       ptrKind(acpsdk.ToolKindExecute),
		},
		Options: []acpsdk.PermissionOption{
			{OptionId: "allow", Kind: acpsdk.PermissionOptionKindAllowOnce, Name: "Allow"},
			{OptionId: "reject", Kind: acpsdk.PermissionOptionKindRejectOnce, Name: "Reject"},
		},
	})
	if err != nil {
		return acpsdk.PromptResponse{}, err
	}
	rejected := resp.Outcome.Cancelled != nil ||
		(resp.Outcome.Selected != nil && resp.Outcome.Selected.OptionId == "reject")
	if !rejected {
		return acpsdk.PromptResponse{}, errors.New("permission was not denied")
	}
	return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
}

// turnIgnoreInterrupt: streams for a while regardless of cancellation, then
// ends the turn; proves Abort is advisory and the driver still completes.
func (a *fakeAgent) turnIgnoreInterrupt(p acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
	conn := currentConn
	if conn == nil {
		return acpsdk.PromptResponse{}, errors.New("no connection")
	}
	// Deliberately detached from the turn context so session/cancel cannot
	// stop this behavior.
	cancelCtx := context.Background() //nolint:contextcheck // ignoring the interrupt is the point of this behavior
	for i := range 8 {
		if err := sleepOrCancel(cancelCtx, chunkDelay); err != nil {
			return acpsdk.PromptResponse{}, err
		}
		if err := sendUpdate(cancelCtx, conn, p.SessionId, acpsdk.SessionUpdate{AgentMessageChunk: &acpsdk.SessionUpdateAgentMessageChunk{
			SessionUpdate: "agent_message_chunk",
			Content:       acpsdk.TextBlock(fmt.Sprintf("ignoring %d; ", i)),
		}}); err != nil {
			return acpsdk.PromptResponse{}, err
		}
	}
	return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
}
