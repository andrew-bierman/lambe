package tracker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newHTTPFixture serves handler and returns a transport pointed at it plus a
// request counter.
func newHTTPFixture(t *testing.T, handler http.HandlerFunc) (*HTTPTransport, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	tr, err := NewHTTPTransport(srv.URL + "/") // trailing slash must be tolerated
	if err != nil {
		t.Fatal(err)
	}
	return tr, &hits
}

// TestHTTPTransportUnsupportedOps pins that every not-yet-wired operation
// reports ErrUnsupported and never touches the network.
func TestHTTPTransportUnsupportedOps(t *testing.T) {
	tr, hits := newHTTPFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ctx := t.Context()
	ops := map[string]func() error{
		"list":      func() error { _, err := tr.List(ctx, DispatchQuery{}); return err },
		"show":      func() error { _, err := tr.Show(ctx, "x-1"); return err },
		"create":    func() error { _, err := tr.Create(ctx, CreateRequest{}); return err },
		"update":    func() error { _, err := tr.Update(ctx, UpdateRequest{}); return err },
		"heartbeat": func() error { _, err := tr.Heartbeat(ctx, "x-1"); return err },
		"unclaim":   func() error { _, err := tr.Unclaim(ctx, "x-1", "me"); return err },
		"reclaim":   func() error { _, err := tr.Reclaim(ctx, time.Minute, nil); return err },
		"defer":     func() error { _, err := tr.Defer(ctx, "x-1", nil, "why"); return err },
		"batch":     func() error { _, err := tr.Batch(ctx, nil); return err },
		"config":    func() error { _, err := tr.ConfigGet(ctx, "k"); return err },
	}
	for name, call := range ops {
		t.Run(name, func(t *testing.T) {
			err := call()
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("got %v, want ErrUnsupported", err)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error %q does not name the operation %q", err, name)
			}
		})
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("unsupported ops made %d HTTP requests, want 0", n)
	}
}

func TestHTTPTransportEvents(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantData string
		wantErr  bool
		wantOut  string // ExitError.Stdout when wantErr
		wantStat string // ExitError.Stderr when wantErr
	}{
		{name: "enveloped", status: 200, body: `{"data":{"events":[1]}}`, wantData: `{"events":[1]}`},
		{name: "bare body", status: 200, body: ` [1,2] `, wantData: `[1,2]`},
		{name: "truncated 410", status: 410, body: `{"error":"truncated"}`, wantErr: true, wantOut: `{"error":"truncated"}`},
		{name: "conflict 409", status: 409, body: `conflict`, wantErr: true, wantOut: `conflict`},
		{name: "unauthorized 401", status: 401, body: `nope`, wantErr: true, wantOut: `nope`, wantStat: "http status 401"},
		{name: "server error 503", status: 503, body: ``, wantErr: true, wantStat: "http status 503"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotQuery string
			tr, _ := newHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			res, err := tr.Events(t.Context(), 42)
			if gotPath != "/v0/beads/events" || gotQuery != "since=42" {
				t.Errorf("request = %s?%s, want /v0/beads/events?since=42", gotPath, gotQuery)
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Events: %v", err)
				}
				if string(res.Data) != tc.wantData {
					t.Errorf("Data = %q, want %q", res.Data, tc.wantData)
				}
				if string(res.Raw) != tc.body {
					t.Errorf("Raw = %q, want %q", res.Raw, tc.body)
				}
				return
			}
			var ee *ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("got %v, want *ExitError", err)
			}
			if ee.Stdout != tc.wantOut || ee.Stderr != tc.wantStat {
				t.Errorf("ExitError = %+v, want stdout %q stderr %q", ee, tc.wantOut, tc.wantStat)
			}
		})
	}
}

func TestHTTPTransportVersion(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		want    string
		wantErr bool
	}{
		{name: "enveloped", status: 200, body: `{"data":{"version":"1.4.2"}}`, want: "1.4.2"},
		{name: "bare", status: 200, body: `{"version":"1.3.9"}`, want: "1.3.9"},
		{name: "malformed json", status: 200, body: `{"data":`, wantErr: true},
		{name: "wrong shape", status: 200, body: `[1]`, wantErr: true},
		{name: "server error", status: 500, body: `boom`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr, _ := newHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v0/beads/context" {
					t.Errorf("path = %s", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			got, err := tr.Version(t.Context())
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("version = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHTTPTransportTransportFailures(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close() // port now refuses
		tr, err := NewHTTPTransport(url)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tr.Events(t.Context(), 0); err == nil || !strings.Contains(err.Error(), "/v0/beads/events") {
			t.Fatalf("got %v, want wrapped dial error naming the path", err)
		}
	})
	t.Run("client timeout", func(t *testing.T) {
		release := make(chan struct{})
		tr, _ := newHTTPFixture(t, func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		})
		t.Cleanup(func() { close(release) })
		tr.client.Timeout = 50 * time.Millisecond
		if _, err := tr.Events(t.Context(), 0); err == nil {
			t.Fatal("hung server did not time out")
		}
	})
	t.Run("context cancel", func(t *testing.T) {
		release := make(chan struct{})
		tr, _ := newHTTPFixture(t, func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		})
		t.Cleanup(func() { close(release) })
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(20*time.Millisecond, cancel)
		_, err := tr.Version(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	})
}

func TestNewHTTPTransportRejectsBadURLs(t *testing.T) {
	for _, u := range []string{"", "ftp://x", "127.0.0.1:8398", "http://%zz"} {
		if _, err := NewHTTPTransport(u); err == nil {
			t.Errorf("NewHTTPTransport(%q) accepted", u)
		}
	}
}
