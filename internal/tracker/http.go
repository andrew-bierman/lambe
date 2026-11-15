package tracker

// HTTPTransport talks to a supervised bd serve on a loopback address.
//
// bd serve requires a server-mode Dolt backend, which only the daemon unit
// supervises, so this type is the wiring point it will complete: read
// operations against the documented /v0 endpoints work now; write
// operations report ErrUnsupported rather than guessing request shapes the
// daemon has not pinned against a live OpenAPI document.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTPTransport talks to a supervised bd serve on a loopback address.
type HTTPTransport struct {
	baseURL *url.URL
	client  *http.Client
}

// NewHTTPTransport returns a transport for the bd serve instance at
// baseURL (for example http://127.0.0.1:8398). The daemon constructs this
// once its supervised bd serve child reports ready.
func NewHTTPTransport(baseURL string) (*HTTPTransport, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse bd serve base url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("bd serve base url scheme must be http(s): %q", baseURL)
	}
	return &HTTPTransport{baseURL: u, client: &http.Client{Timeout: 30 * time.Second}}, nil
}

// unsupported is the shared reply for operations awaiting daemon wiring.
func (t *HTTPTransport) unsupported(op string) (Result, error) {
	return Result{}, fmt.Errorf("%w: %s over bd serve http api", ErrUnsupported, op)
}

func (t *HTTPTransport) get(ctx context.Context, path string) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.baseURL.String()+path, nil)
	if err != nil {
		return Result{}, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("bd serve %s: %w", path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	return decodeResponse(resp)
}

// decodeResponse converts an HTTP status into a Result or ExitError using
// the journal's documented status contract.
func decodeResponse(resp *http.Response) (Result, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Result{}, fmt.Errorf("read bd serve response: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return Result{Data: unwrapEnvelope(body), Raw: body}, nil
	case http.StatusConflict:
		return Result{}, &ExitError{Code: 1, Stdout: string(body)}
	case http.StatusGone:
		return Result{}, &ExitError{Code: 1, Stdout: string(body)}
	default:
		return Result{}, &ExitError{Code: 1, Stdout: string(body), Stderr: fmt.Sprintf("http status %d", resp.StatusCode)}
	}
}

// List implements Transport.
func (t *HTTPTransport) List(_ context.Context, _ DispatchQuery) (Result, error) {
	return t.unsupported("list")
}

// Show implements Transport.
func (t *HTTPTransport) Show(_ context.Context, _ string) (Result, error) {
	return t.unsupported("show")
}

// Create implements Transport.
func (t *HTTPTransport) Create(_ context.Context, _ CreateRequest) (Result, error) {
	return t.unsupported("create")
}

// Update implements Transport.
func (t *HTTPTransport) Update(_ context.Context, _ UpdateRequest) (Result, error) {
	return t.unsupported("update")
}

// Heartbeat implements Transport.
func (t *HTTPTransport) Heartbeat(_ context.Context, _ string) (Result, error) {
	return t.unsupported("heartbeat")
}

// Unclaim implements Transport.
func (t *HTTPTransport) Unclaim(_ context.Context, _ string, _ string) (Result, error) {
	return t.unsupported("unclaim")
}

// Reclaim implements Transport.
func (t *HTTPTransport) Reclaim(_ context.Context, _ time.Duration, _ []string) (Result, error) {
	return t.unsupported("reclaim")
}

// Defer implements Transport.
func (t *HTTPTransport) Defer(_ context.Context, _ string, _ *time.Time, _ string) (Result, error) {
	return t.unsupported("defer")
}

// Events implements Transport: GET /v0/beads/events?since=N, where HTTP 410
// maps to the truncation contract via ExitError.
func (t *HTTPTransport) Events(ctx context.Context, since int64) (Result, error) {
	return t.get(ctx, fmt.Sprintf("/v0/beads/events?since=%d", since))
}

// Version implements Transport via the capability document.
func (t *HTTPTransport) Version(ctx context.Context) (string, error) {
	res, err := t.get(ctx, "/v0/beads/context")
	if err != nil {
		return "", err
	}
	var doc struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(res.Data, &doc); err != nil {
		return "", fmt.Errorf("decode bd serve context: %w", err)
	}
	return doc.Version, nil
}

// ConfigGet implements Transport.
func (t *HTTPTransport) ConfigGet(_ context.Context, key string) (string, error) {
	return "", fmt.Errorf("%w: config get %s over bd serve http api", ErrUnsupported, key)
}

// compile-time interface checks.
var (
	_ Transport = (*ShellTransport)(nil)
	_ Transport = (*HTTPTransport)(nil)
)
