// Package pi implements the harness driver for the Pi coding agent, driven
// over its RPC mode: a long-lived child process speaking strict
// LF-terminated JSONL on stdin/stdout, with stderr reserved for diagnostics.
package pi

import (
	"encoding/json"
	"fmt"
)

// CommandType is a Pi stdin command name.
type CommandType string

// The commands lambe uses. The full protocol is larger; only what the
// supervisor needs is modeled.
const (
	CmdPrompt         CommandType = "prompt"
	CmdSteer          CommandType = "steer"
	CmdFollowUp       CommandType = "follow_up"
	CmdAbort          CommandType = "abort"
	CmdCompact        CommandType = "compact"
	CmdNewSession     CommandType = "new_session"
	CmdSwitchSession  CommandType = "switch_session"
	CmdGetState       CommandType = "get_state"
	CmdGetSessionStat CommandType = "get_session_stats"
	CmdGetEntries     CommandType = "get_entries"
	CmdSetAutoRetry   CommandType = "set_auto_retry"
	CmdAbortRetry     CommandType = "abort_retry"
)

// StreamingBehavior selects how a prompt interacts with an in-flight run.
// Sending a plain prompt while the agent is streaming is a protocol error.
type StreamingBehavior string

// The streaming behaviors Pi accepts on the prompt command.
const (
	// StreamingSteer queues the message to interrupt mid-run.
	StreamingSteer StreamingBehavior = "steer"
	// StreamingFollowUp queues the message until the run finishes.
	StreamingFollowUp StreamingBehavior = "followUp"
)

// Disposition reports what the backend did with a submitted prompt.
type Disposition string

// The prompt dispositions Pi reports.
const (
	// DispositionStarted means a run started for the prompt.
	DispositionStarted Disposition = "started"
	// DispositionQueued means the prompt was queued during a run.
	DispositionQueued Disposition = "queued"
	// DispositionHandled means a handler consumed the prompt; no run
	// will start and no settle event will follow.
	DispositionHandled Disposition = "handled"
)

// Event type names on the stdout stream.
const (
	EventAgentStart    = "agent_start"
	EventAgentEnd      = "agent_end"
	EventAgentSettled  = "agent_settled"
	EventMessageEnd    = "message_end"
	EventToolExecStart = "tool_execution_start"
	EventToolExecEnd   = "tool_execution_end"
	EventAutoRetrySt   = "auto_retry_start"
	EventAutoRetryEnd  = "auto_retry_end"
	EventExtensionErr  = "extension_error"
	EventResponse      = "response"
)

// Command is one Pi stdin command. Command fields serialize flat onto the
// wire (Pi has no data wrapper for commands); only fields relevant to Type
// are set.
type Command struct {
	// Type selects the command.
	Type CommandType
	// Message is the prompt/steer/follow_up text.
	Message string
	// StreamingBehavior is optional on prompt; required when the agent is
	// already streaming.
	StreamingBehavior StreamingBehavior
	// SessionPath selects the session file for switch_session.
	SessionPath string
	// Since is the entry cursor for get_entries.
	Since string
	// Enabled toggles set_auto_retry.
	Enabled *bool
}

// wireCommand mirrors the flat command JSON. Field order follows the
// protocol examples.
type wireCommand struct {
	ID                string            `json:"id,omitempty"`
	Type              CommandType       `json:"type"`
	Message           string            `json:"message,omitempty"`
	StreamingBehavior StreamingBehavior `json:"streamingBehavior,omitempty"`
	SessionPath       string            `json:"sessionPath,omitempty"`
	Since             string            `json:"since,omitempty"`
	Enabled           *bool             `json:"enabled,omitempty"`
}

// withID serializes the command carrying the correlation id on the wire.
func (c Command) withID(id string) ([]byte, error) {
	return json.Marshal(wireCommand{
		ID:                id,
		Type:              c.Type,
		Message:           c.Message,
		StreamingBehavior: c.StreamingBehavior,
		SessionPath:       c.SessionPath,
		Since:             c.Since,
		Enabled:           c.Enabled,
	})
}

// Record is one stdout record: either a response to a command (identified
// by Type "response" and a repeated id) or a session event. Fields are
// organized so both families decode from one shape; absent fields keep
// zero values.
type Record struct {
	// ID repeats the correlated command's id; empty on session events.
	ID string `json:"id,omitempty"`
	// Type is "response" or an event name.
	Type string `json:"type"`
	// Command echoes the command name on responses.
	Command string `json:"command,omitempty"`
	// Success is the response outcome; nil on events.
	Success *bool `json:"success,omitempty"`
	// Error describes a failed command or a parse failure.
	Error string `json:"error,omitempty"`
	// Data carries the response payload.
	Data json.RawMessage `json:"data,omitempty"`

	// Event fields below.
	Aborted      bool         `json:"aborted"`
	WillRetry    bool         `json:"willRetry"`
	Message      *WireMessage `json:"message"`
	ErrorMessage string       `json:"errorMessage"`
	FinalError   string       `json:"finalError"`
	Attempt      int          `json:"attempt"`
	MaxAttempts  int          `json:"maxAttempts"`
	DelayMs      int64        `json:"delayMs"`
	ToolCallID   string       `json:"toolCallId"`
	ToolName     string       `json:"toolName"`
	IsError      bool         `json:"isError"`
	Extension    string       `json:"extensionPath"`
}

// IsResponse reports whether the record responds to a command. Parse
// failures carry no id; correlate those by Command == "parse".
func (r Record) IsResponse() bool { return r.Type == EventResponse }

// OK reports whether the record is a successful response.
func (r Record) OK() bool { return r.IsSuccess() != nil && *r.Success }

// IsSuccess returns the raw success flag.
func (r Record) IsSuccess() *bool { return r.Success }

// WireMessage is one conversation message on the event stream.
type WireMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	StopReason string          `json:"stopReason"`
}

// Text extracts the message's text content. Content is either a plain
// string or an array of typed content blocks; anything else yields "".
func (m *WireMessage) Text() string {
	if m == nil || len(m.Content) == 0 {
		return ""
	}
	var plain string
	if err := json.Unmarshal(m.Content, &plain); err == nil {
		return plain
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &blocks); err == nil {
		out := ""
		for _, b := range blocks {
			if b.Type == "text" {
				out += b.Text
			}
		}
		return out
	}
	return ""
}

// PromptAck is the payload of a successful prompt/steer/follow_up response.
type PromptAck struct {
	Disposition Disposition `json:"disposition"`
}

// StateResponse is the payload of get_state.
type StateResponse struct {
	IsStreaming         bool   `json:"isStreaming"`
	IsCompacting        bool   `json:"isCompacting"`
	SessionFile         string `json:"sessionFile"`
	SessionID           string `json:"sessionId"`
	PendingMessageCount int    `json:"pendingMessageCount"`
	MessageCount        int    `json:"messageCount"`
}

// StatsResponse is the payload of get_session_stats.
type StatsResponse struct {
	SessionID   string `json:"sessionId"`
	SessionFile string `json:"sessionFile"`
	Tokens      struct {
		Input      int64 `json:"input"`
		Output     int64 `json:"output"`
		CacheRead  int64 `json:"cacheRead"`
		CacheWrite int64 `json:"cacheWrite"`
		Total      int64 `json:"total"`
	} `json:"tokens"`
	Cost float64 `json:"cost"`
	// ContextUsage is omitted when no model or context window is known.
	ContextUsage *struct {
		Tokens        *int64   `json:"tokens"`
		ContextWindow int64    `json:"contextWindow"`
		Percent       *float64 `json:"percent"`
	} `json:"contextUsage"`
}

// CommandError is a success:false response to one command. Pi has no typed
// error taxonomy; Message is the backend's error string.
type CommandError struct {
	Command CommandType
	Message string
}

// Error implements error.
func (e *CommandError) Error() string {
	return fmt.Sprintf("pi %s: %s", e.Command, e.Message)
}
