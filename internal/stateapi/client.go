// Package stateapi is the Go shared client over the daemon's local HTTP
// state surface: the §13.7 routes plus the additive queue and history
// reads, all documented in docs/api/state-api.md. It resolves the
// per-instance descriptor a daemon publishes under the state dir's live
// directory by enumerating the files — never by rebuilding any lock-name
// hash — and serves typed reads over internal/httpstate's DTOs, so no Go
// consumer duplicates discovery or wire decoding.
//
// The client is read-only: every call is a GET, and the mutating refresh
// POST is an operator action no client here makes. The hardening is the
// contract's, shared with the Swift client: the endpoint address must be
// the literal IPv4 loopback 127.0.0.1 before the bearer token is attached,
// redirects are refused rather than followed, requests are bounded by a
// per-attempt timeout with a small retry budget, and the token never
// appears in logs or error values. Last responses are cached per route for
// a short TTL outside the call path, so repeated reads — a smoke's or a
// viewer's — do not multiply daemon or tracker work.
package stateapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/andrew-bierman/lambe/internal/httpstate"
	"github.com/andrew-bierman/lambe/internal/proc"
)

// Defaults for a Client built without options. The timeout bounds one
// HTTP attempt; the retry budget covers a refused connection during a
// daemon's launchd restart, not a hung server; the cache TTL keeps
// repeated assertions cheap while staying well under a poll cadence.
const (
	// DefaultTimeout bounds one HTTP attempt.
	DefaultTimeout = 5 * time.Second
	// DefaultAttempts is the small transport-failure retry budget.
	DefaultAttempts = 2
	// DefaultCacheTTL is how long a route's last response is served from
	// the client's own cache before the daemon is asked again.
	DefaultCacheTTL = 2 * time.Second
)

// Bounds for the three unbounded-length reads a client performs: one
// descriptor document, one non-2xx error body kept for the typed
// envelope, and one 2xx contract response. Success gets its own larger
// bound — a busy history page legitimately exceeds an error envelope's
// size — and a client never buffers more than its bound.
const (
	maxDescriptorBytes = 64 << 10
	maxErrorBodyBytes  = 64 << 10
	// maxBodyBytes bounds a 2xx body: generous headroom over the largest
	// bounded response the daemon serves (100 history entries), still a
	// hard cap.
	maxBodyBytes = 1 << 20
)

// Descriptor is the decode target for one daemon's published discovery
// document (live/stateapi-*.json). The field names mirror the wire
// contract the daemon writes; parity with the daemon's own
// daemon.StateAPIDescriptor is pinned by the package tests and by the
// dashboard smoke resolving a real daemon's descriptor.
type Descriptor struct {
	// SchemaVersion is the descriptor document's own version. The client
	// resolves only documents it understands (version 1).
	SchemaVersion int `json:"schema_version"`
	// Instance is the daemon instance name: the lock name without its
	// .lock suffix.
	Instance string `json:"instance"`
	// Addr is the bound host:port the state API answers on.
	Addr string `json:"addr"`
	// BearerToken authenticates every request. It is carried only in
	// memory and request headers: never logged, never embedded in errors.
	BearerToken string `json:"bearer_token"`
	// PID is the daemon's process id.
	PID int `json:"pid"`
	// Identity identifies the process incarnation; empty when the
	// platform cannot provide one.
	Identity string `json:"identity,omitempty"`
	// Version is the daemon build version.
	Version string `json:"version"`
	// ConfigPath is the configuration file the daemon loaded.
	ConfigPath string `json:"config_path,omitempty"`
	// StartedAt is when the daemon started, RFC 3339 in UTC.
	StartedAt string `json:"started_at"`
}

// Endpoint is one resolved, validated attachment point: a live
// descriptor's facts carried through unchanged after Resolve accepted it.
type Endpoint struct {
	// Instance is the daemon instance name the descriptor carries.
	Instance string
	// Addr is the validated literal-127.0.0.1 host:port.
	Addr string
	// Token is the per-start bearer token. Treat as a secret.
	Token string
	// PID is the daemon's process id.
	PID int
	// Identity is the daemon's process identity, when the platform
	// provides one.
	Identity string
	// Version is the daemon build version.
	Version string
	// ConfigPath is the configuration file the daemon loaded.
	ConfigPath string
	// StartedAt is when the daemon started, RFC 3339 in UTC.
	StartedAt string
}

// Selector narrows Resolve to one instance when several live daemons
// publish descriptors under one state dir. Zero-value fields match any
// descriptor; both set means both must match.
type Selector struct {
	// Instance matches the descriptor's instance name.
	Instance string
	// ConfigPath matches the configuration file the daemon loaded.
	ConfigPath string
}

// Sentinel errors a caller can branch on with errors.Is.
var (
	// ErrNotFound reports that no live descriptor matched under the state
	// dir: nothing is serving, or every candidate was stale.
	ErrNotFound = errors.New("stateapi: no live endpoint descriptor")
	// ErrAmbiguous reports several live descriptors matching the selector;
	// the caller must narrow it with Selector.
	ErrAmbiguous = errors.New("stateapi: multiple live endpoints match")
	// ErrRedirectRefused is wrapped into a TransportError when the server
	// answers a redirect: the contract forbids following it.
	ErrRedirectRefused = errors.New("stateapi: redirect refused")
	// ErrBadEndpoint reports an endpoint rejected before any request was
	// sent: a non-loopback address or a missing bearer token.
	ErrBadEndpoint = errors.New("stateapi: invalid endpoint")
	// ErrBodyTooLarge reports a response body over its read bound — the
	// success bound for 2xx answers, the error bound for non-2xx ones.
	// Overflow is never silently truncated.
	ErrBodyTooLarge = errors.New("stateapi: response body over bound")
)

// TransportError is a failed HTTP attempt: connection refused, timeout, a
// refused redirect, or an unreadable body. It never carries the bearer
// token.
type TransportError struct {
	// Route is the request path (also the cache key family).
	Route string
	// Err is the underlying failure.
	Err error
}

func (e *TransportError) Error() string {
	return "stateapi: " + e.Route + ": " + e.Err.Error()
}

// Unwrap supports errors.Is and errors.As over the underlying failure,
// ErrRedirectRefused included.
func (e *TransportError) Unwrap() error { return e.Err }

// StatusError is a non-2xx answer: Code is the HTTP status and Envelope
// the contract's error DTO decoded from the body. A body that does not
// decode leaves the zero Envelope; the status is still typed.
type StatusError struct {
	// Code is the HTTP status code.
	Code int
	// Envelope is the decoded error envelope, zero when the body was not
	// the documented JSON.
	Envelope httpstate.ErrorResponse
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("stateapi: %s: status %d", e.Envelope.Error, e.Code)
}

// DecodeError is a 2xx answer whose body was not the documented JSON.
type DecodeError struct {
	// Route is the request path the body came from.
	Route string
	// Err is the JSON decode failure.
	Err error
}

func (e *DecodeError) Error() string {
	return "stateapi: " + e.Route + ": decode response: " + e.Err.Error()
}

// Unwrap supports errors.Is over the decode failure.
func (e *DecodeError) Unwrap() error { return e.Err }

// AmbiguityError reports several live endpoints matching the selector.
// It carries instance names only — never tokens.
type AmbiguityError struct {
	// Instances are the matching descriptors' instance names.
	Instances []string
}

func (e *AmbiguityError) Error() string {
	return ErrAmbiguous.Error() + ": " + fmt.Sprint(e.Instances)
}

// Unwrap makes AmbiguityError match errors.Is(err, ErrAmbiguous).
func (e *AmbiguityError) Unwrap() error { return ErrAmbiguous }

// Resolve enumerates the state dir's live descriptors like any client,
// skips stale entries (identity no longer matching the pid's live
// process), rejects addresses that are not the literal 127.0.0.1, and
// selects by the given selector. It never deletes a descriptor — the
// owning daemon removes its own file at shutdown.
func Resolve(ctx context.Context, stateDir string, sel Selector) (Endpoint, error) {
	dir := filepath.Join(stateDir, "live")
	matches, err := filepath.Glob(filepath.Join(dir, "stateapi-*.json"))
	if err != nil {
		return Endpoint{}, fmt.Errorf("stateapi: enumerate %s: %w", dir, err)
	}
	var live []Endpoint
	for _, path := range matches {
		if err := ctx.Err(); err != nil {
			return Endpoint{}, err
		}
		ep, ok := readDescriptor(path)
		if !ok {
			continue
		}
		if sel.Instance != "" && ep.Instance != sel.Instance {
			continue
		}
		if sel.ConfigPath != "" && ep.ConfigPath != sel.ConfigPath {
			continue
		}
		live = append(live, ep)
	}
	switch len(live) {
	case 0:
		return Endpoint{}, fmt.Errorf("%w under %s", ErrNotFound, dir)
	case 1:
		return live[0], nil
	default:
		names := make([]string, 0, len(live))
		for _, ep := range live {
			names = append(names, ep.Instance)
		}
		return Endpoint{}, &AmbiguityError{Instances: names}
	}
}

// readDescriptor decodes one descriptor file, dropping stale or malformed
// entries: an unreadable or undecodable document, an unknown descriptor
// schema, an address that is not the literal loopback form, or an
// identity that no longer matches the pid's live process. A descriptor
// without a platform identity cannot be liveness-checked and is taken
// as-is.
func readDescriptor(path string) (Endpoint, bool) {
	f, err := os.Open(path) // #nosec G304 -- the path comes from enumerating the operator's state dir
	if err != nil {
		return Endpoint{}, false
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxDescriptorBytes+1))
	if err != nil || len(data) > maxDescriptorBytes {
		return Endpoint{}, false
	}
	var d Descriptor
	if err := json.Unmarshal(data, &d); err != nil {
		return Endpoint{}, false
	}
	if d.SchemaVersion != 1 || d.Instance == "" || d.BearerToken == "" || !isLoopbackAddr(d.Addr) {
		return Endpoint{}, false
	}
	if d.Identity != "" {
		id, idErr := proc.ProcessIdentity(d.PID)
		if idErr != nil || id != d.Identity {
			return Endpoint{}, false
		}
	}
	return Endpoint{
		Instance:   d.Instance,
		Addr:       d.Addr,
		Token:      d.BearerToken,
		PID:        d.PID,
		Identity:   d.Identity,
		Version:    d.Version,
		ConfigPath: d.ConfigPath,
		StartedAt:  d.StartedAt,
	}, true
}

// isLoopbackAddr reports whether addr is host:port whose host is the
// literal IPv4 loopback 127.0.0.1 — never a name, never ::1, never a
// wider 127/8 range: the contract's strict form, the shape the daemon
// publishes.
func isLoopbackAddr(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

// cacheEntry is one cached response body with the read's timestamp.
type cacheEntry struct {
	at   time.Time
	body []byte
}

// Client is a typed read-only client for one resolved endpoint. Construct
// with NewClient; all methods are safe for concurrent use.
type Client struct {
	endpoint Endpoint
	http     *http.Client
	attempts int
	ttl      time.Duration
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// NewClient builds a client for a validated endpoint. The endpoint must
// already be loopback-literal and carry a token — NewClient re-checks so
// a programmatically constructed endpoint cannot bypass what Resolve
// enforces, and no request is sent until both hold.
func NewClient(ep Endpoint, opts ...Option) (*Client, error) {
	if !isLoopbackAddr(ep.Addr) {
		return nil, fmt.Errorf("%w: addr %q is not the literal 127.0.0.1 loopback", ErrBadEndpoint, ep.Addr)
	}
	if ep.Token == "" {
		return nil, fmt.Errorf("%w: missing bearer token", ErrBadEndpoint)
	}
	c := &Client{
		endpoint: ep,
		attempts: DefaultAttempts,
		ttl:      DefaultCacheTTL,
		now:      time.Now,
		cache:    make(map[string]cacheEntry),
	}
	c.http = &http.Client{
		Timeout: DefaultTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrRedirectRefused
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Option adjusts a Client at construction. Zero or negative values fall
// back to their default, except WithCacheTTL(0), which disables caching.
type Option func(*Client)

// WithTimeout bounds each HTTP attempt.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.http.Timeout = d
		}
	}
}

// WithAttempts sets the transport-failure retry budget. Values below one
// are clamped to one.
func WithAttempts(n int) Option {
	return func(c *Client) {
		if n < 1 {
			n = 1
		}
		c.attempts = n
	}
}

// WithCacheTTL sets how long a route's last response is reused. Zero
// disables the cache entirely.
func WithCacheTTL(d time.Duration) Option {
	return func(c *Client) { c.ttl = d }
}

// WithNow replaces the clock the cache ages against (tests).
func WithNow(now func() time.Time) Option {
	return func(c *Client) { c.now = now }
}

// Endpoint returns the resolved endpoint the client was built for.
func (c *Client) Endpoint() Endpoint { return c.endpoint }

// State returns the full supervised snapshot.
func (c *Client) State(ctx context.Context) (httpstate.SnapshotResponse, error) {
	body, err := c.get(ctx, "state", "state")
	if err != nil {
		return httpstate.SnapshotResponse{}, err
	}
	var out httpstate.SnapshotResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return httpstate.SnapshotResponse{}, &DecodeError{Route: "state", Err: err}
	}
	return out, nil
}

// Queue returns the daemon's cached readiness projection for the
// configured label lanes.
func (c *Client) Queue(ctx context.Context) (httpstate.QueueResponse, error) {
	body, err := c.get(ctx, "queue", "queue")
	if err != nil {
		return httpstate.QueueResponse{}, err
	}
	var out httpstate.QueueResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return httpstate.QueueResponse{}, &DecodeError{Route: "queue", Err: err}
	}
	return out, nil
}

// History returns the merged terminal-and-archived runs, newest first.
func (c *Client) History(ctx context.Context) (httpstate.HistoryResponse, error) {
	body, err := c.get(ctx, "history", "history")
	if err != nil {
		return httpstate.HistoryResponse{}, err
	}
	var out httpstate.HistoryResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return httpstate.HistoryResponse{}, &DecodeError{Route: "history", Err: err}
	}
	return out, nil
}

// Issue returns one issue's run detail. An empty identifier is a caller
// error; an unknown issue surfaces as a typed StatusError with the
// server's 404 envelope.
func (c *Client) Issue(ctx context.Context, id string) (httpstate.IssueResponse, error) {
	if id == "" {
		return httpstate.IssueResponse{}, errors.New("stateapi: issue identifier required")
	}
	body, err := c.get(ctx, "issue:"+id, url.PathEscape(id))
	if err != nil {
		return httpstate.IssueResponse{}, err
	}
	var out httpstate.IssueResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return httpstate.IssueResponse{}, &DecodeError{Route: "issue:" + id, Err: err}
	}
	return out, nil
}

// get serves route from the cache while fresh, otherwise fetches. The
// cache key and URL path segment are separate so the issue route's
// escaped path never collides with another issue's cache entry.
func (c *Client) get(ctx context.Context, cacheKey, path string) ([]byte, error) {
	if c.ttl > 0 {
		c.mu.Lock()
		ent, ok := c.cache[cacheKey]
		c.mu.Unlock()
		if ok && c.now().Sub(ent.at) < c.ttl {
			return ent.body, nil
		}
	}
	body, err := c.fetch(ctx, path)
	if err != nil {
		return nil, err
	}
	if c.ttl > 0 {
		c.mu.Lock()
		c.cache[cacheKey] = cacheEntry{at: c.now(), body: body}
		c.mu.Unlock()
	}
	return body, nil
}

// fetch performs one authenticated GET with the retry budget. Only
// transport failures consume the budget; a status or decode answer is
// final. The token rides the Authorization header alone: never the URL,
// never an error value.
func (c *Client) fetch(ctx context.Context, path string) ([]byte, error) {
	url := "http://" + c.endpoint.Addr + "/api/v1/" + path
	var lastErr error
	for attempt := 0; attempt < c.attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, &TransportError{Route: path, Err: err}
		}
		req.Header.Set("Authorization", "Bearer "+c.endpoint.Token)
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = &TransportError{Route: path, Err: err}
			if ctx.Err() != nil {
				return nil, lastErr
			}
			continue
		}
		// The bound follows the answer: 2xx bodies get the contract
		// bound, non-2xx bodies the error-envelope cap. Either way the
		// read is bounded and overflow is an explicit typed error, so a
		// large success body can never be silently truncated.
		limit := maxBodyBytes
		if resp.StatusCode != http.StatusOK {
			limit = maxErrorBodyBytes
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = &TransportError{Route: path, Err: readErr}
			continue
		}
		if len(body) > limit {
			return nil, &TransportError{Route: path, Err: fmt.Errorf("%w: %d bytes exceeds %d", ErrBodyTooLarge, len(body), limit)}
		}
		if resp.StatusCode != http.StatusOK {
			var env httpstate.ErrorResponse
			_ = json.Unmarshal(body, &env)
			return nil, &StatusError{Code: resp.StatusCode, Envelope: env}
		}
		return body, nil
	}
	return nil, lastErr
}
