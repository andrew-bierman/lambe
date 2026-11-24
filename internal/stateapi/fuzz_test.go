package stateapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// FuzzClientDecode serves arbitrary status codes and bodies to every typed
// route. The client must never panic: a 200 yields a typed value or a
// DecodeError, a non-200 always yields a StatusError (unknown fields and
// garbage envelopes tolerated), and an oversized body yields a bounded
// transport error rather than being buffered.
func FuzzClientDecode(f *testing.F) {
	for _, name := range []string{"descriptor", "error", "history", "issue", "queue", "snapshot"} {
		body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "contract", "v1", name+".json")) //nolint:gosec // G304: fixed in-tree fixture names.
		if err != nil {
			f.Fatal(err)
		}
		f.Add(200, body)
		f.Add(404, body)
	}
	for _, s := range []string{
		"", "null", "[]", "{}", "0", "\"str\"", "{\"unknown_field\":{\"deep\":[1,2,3]}}",
		"{\"schema_version\":\"1\"}", "{\"runs\":{}}", "{\"runs\":[null,1,\"x\"]}",
		"{\"error\":5}", "\xff\xfe\x00", "{\"a\":\"\\ud800\"}", "{\"schema_version\":1e999}",
		strings.Repeat("[", 100000), strings.Repeat("{\"a\":", 10000),
		strings.Repeat("x", maxBodyBytes+1), strings.Repeat(" ", maxErrorBodyBytes+1),
	} {
		f.Add(200, []byte(s))
		f.Add(500, []byte(s))
	}

	var mu sync.Mutex
	var status int
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		code, b := status, body
		mu.Unlock()
		w.WriteHeader(code)
		_, _ = w.Write(b)
	}))
	f.Cleanup(srv.Close)
	ep := Endpoint{Addr: strings.TrimPrefix(srv.URL, "http://"), Token: "t"}

	f.Fuzz(func(t *testing.T, code int, data []byte) {
		if code < 200 || code > 599 || code == http.StatusNoContent || code == http.StatusNotModified || code/100 == 3 {
			t.Skip()
		}
		mu.Lock()
		status, body = code, data
		mu.Unlock()
		c, err := NewClient(ep, WithCacheTTL(0), WithAttempts(1))
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		calls := map[string]func() error{
			"state":   func() error { _, err := c.State(ctx); return err },
			"queue":   func() error { _, err := c.Queue(ctx); return err },
			"history": func() error { _, err := c.History(ctx); return err },
			"issue":   func() error { _, err := c.Issue(ctx, "X-1"); return err },
		}
		for route, call := range calls {
			err := call()
			var se *StatusError
			var de *DecodeError
			var te *TransportError
			switch {
			case code != http.StatusOK && !errors.As(err, &se) && !errors.As(err, &te):
				t.Fatalf("%s: status %d gave %v, want StatusError", route, code, err)
			case code == http.StatusOK && err != nil && !errors.As(err, &de) && !errors.As(err, &te):
				t.Fatalf("%s: 200 gave untyped error %v", route, err)
			}
			if err != nil && strings.Contains(err.Error(), "Bearer") {
				t.Fatalf("%s: error leaks the authorization header: %v", route, err)
			}
		}
	})
}

// FuzzReadDescriptor decodes arbitrary descriptor files. A descriptor is
// either rejected or yields a loopback endpoint with a token; it never
// panics.
func FuzzReadDescriptor(f *testing.F) {
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "contract", "v1", "descriptor.json")) //nolint:gosec // G304: fixed in-tree fixture.
	if err != nil {
		f.Fatal(err)
	}
	f.Add(body)
	for _, s := range []string{
		"", "null", "[]", "{}", "{\"schema_version\":1}",
		"{\"schema_version\":1,\"instance\":\"i\",\"bearer_token\":\"t\",\"addr\":\"127.0.0.1:1\"}",
		"{\"schema_version\":1,\"instance\":\"i\",\"bearer_token\":\"t\",\"addr\":\"localhost:1\"}",
		"{\"schema_version\":1,\"instance\":\"i\",\"bearer_token\":\"t\",\"addr\":\"127.0.0.1:99999\"}",
		"{\"schema_version\":1,\"pid\":-1,\"identity\":\"x\",\"instance\":\"i\",\"bearer_token\":\"t\",\"addr\":\"127.0.0.1:1\"}",
		"{\"schema_version\":\"1\"}", "\xff\x00", strings.Repeat("[", 100000),
		"{\"x\":\"" + strings.Repeat("a", maxDescriptorBytes) + "\"}",
	} {
		f.Add([]byte(s))
	}
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(dir, "d.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		ep, ok := readDescriptor(path)
		if !ok {
			return
		}
		if ep.Token == "" || ep.Instance == "" || !isLoopbackAddr(ep.Addr) {
			t.Fatalf("accepted unusable endpoint: %+v", ep)
		}
		if _, err := NewClient(ep); err != nil {
			t.Fatalf("accepted endpoint rejected by NewClient: %v", err)
		}
	})
}
