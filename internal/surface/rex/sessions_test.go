package rex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// fakeClient scripts Client responses for tests.
type fakeClient struct {
	createResp string
	createErr  error
	created    []SessionRequest
	listResp   string
	listErr    error
}

func (f *fakeClient) CreateSession(_ context.Context, req SessionRequest) (Session, error) {
	f.created = append(f.created, req)
	if f.createErr != nil {
		return Session{}, f.createErr
	}
	var s Session
	if err := json.Unmarshal([]byte(f.createResp), &s); err != nil {
		return Session{}, err
	}
	return s, nil
}

func (f *fakeClient) ListSessions(_ context.Context) ([]Session, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []Session
	if err := json.Unmarshal([]byte(f.listResp), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func TestClientInterfaceSatisfied(_ *testing.T) {
	var _ Client = (*fakeClient)(nil)
	var _ Client = (*ShellClient)(nil)
}

func TestShellClientCreateSession(t *testing.T) {
	var gotName string
	var gotArgs []string
	sc := NewShellClient(func(_ context.Context, name string, args ...string) (string, int, error) {
		gotName, gotArgs = name, args
		return `{"session_id":"ses-7"}`, 0, nil
	})
	s, err := sc.CreateSession(context.Background(), SessionRequest{
		Name:   "issues",
		Labels: map[string]string{"app": "lambe", "role": "issues"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if s.ID != "ses-7" {
		t.Errorf("session id = %q, want ses-7", s.ID)
	}
	if gotName != "rex" {
		t.Errorf("command = %q, want rex", gotName)
	}
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{"new", "issues", "--json"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
}

func TestShellClientNonZeroExit(t *testing.T) {
	sc := NewShellClient(func(_ context.Context, _ string, _ ...string) (string, int, error) {
		return "boom", 3, nil
	})
	if _, err := sc.ListSessions(context.Background()); err == nil {
		t.Fatal("ListSessions should fail on non-zero exit")
	}
}
