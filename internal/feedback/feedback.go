// Package feedback forwards a user's feedback to whoever operates the box.
//
// Maison does not know who that is, and must not: it ships on stock boxes with no
// operator at all. The deployment names a SINK — a URL and a bearer token — and the
// feature exists exactly when the sink does. No URL, no token, or a sink that does
// not answer its descriptor: no feedback entry anywhere in the dashboard. A form that
// cannot be sent is worse than no form.
//
// Both calls are made by Maison's backend, never by the browser. The browser would
// need CORS on the sink and a session at whatever gate stands in front of it, and the
// token would have to reach the page — which is the one thing it must never do, since
// Maison's own API has no authentication behind its gate.
//
// The wire contract is docs/feedback.md. It is kept small and plain JSON on purpose,
// so that anything that can take a webhook can be a sink.
package feedback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yundera/maison/internal/brand"
)

// Sink is where feedback goes, as the deployment configured it.
type Sink struct {
	URL   string
	Token string
}

// Configured reports whether there is enough here to ever send anything. A URL
// without a token is not enough: the sink is reachable by every container on the
// box's network, and an unauthenticated one would take mail from any of them.
func (s Sink) Configured() bool { return s.URL != "" && s.Token != "" }

// Descriptor is what the sink says about itself. Operator is the only required
// field: it is the name the form shows next to "Send", so that the user knows who
// is going to read what they type.
type Descriptor struct {
	Operator    string `json:"operator"`
	PrivacyNote string `json:"privacyNote,omitempty"`
	MaxLength   int    `json:"maxLength,omitempty"`
	SupportURL  string `json:"supportUrl,omitempty"`
}

// DefaultMaxLength applies when the sink does not name one.
const DefaultMaxLength = 5000

// Limit is the message length the sink accepts.
func (d Descriptor) Limit() int {
	if d.MaxLength > 0 {
		return d.MaxLength
	}
	return DefaultMaxLength
}

// Categories are the kinds of feedback the form offers. Closed, so a sink can route
// on it without guarding against free text.
var Categories = []string{"idea", "bug", "other"}

// Context is what Maison attaches about where the feedback came from. Everything in
// it is listed on the form before the user sends — nothing here is collected quietly.
type Context struct {
	Source  string `json:"source"`
	Version string `json:"version"`
	Page    string `json:"page,omitempty"`
	AppID   string `json:"appId,omitempty"`
}

// Submission is one piece of feedback, as POSTed to the sink.
//
// Reporter is always null today: Maison sits behind a gate it does not read
// identity from, so it does not know who is typing. It is in the contract now so a
// sink does not have to change shape when Maison learns.
type Submission struct {
	Message  string  `json:"message"`
	Category string  `json:"category"`
	Context  Context `json:"context"`
	Reporter *string `json:"reporter"`
}

// ErrInvalid marks a submission refused before it left the box.
var ErrInvalid = errors.New("invalid feedback")

// Validate normalises s against the sink's limits.
func (s *Submission) Validate(d Descriptor) error {
	s.Message = strings.TrimSpace(s.Message)
	if s.Message == "" {
		return fmt.Errorf("%w: message is empty", ErrInvalid)
	}
	if n := len([]rune(s.Message)); n > d.Limit() {
		return fmt.Errorf("%w: message is %d characters, the limit is %d", ErrInvalid, n, d.Limit())
	}
	if s.Category == "" {
		s.Category = "other"
	}
	for _, c := range Categories {
		if s.Category == c {
			return nil
		}
	}
	return fmt.Errorf("%w: unknown category %q", ErrInvalid, s.Category)
}

// How long a descriptor answer is trusted. The dashboard asks on every load; the
// sink should not hear about every one of them. A failure is retried sooner than a
// success is refreshed, so a sink that was briefly down reappears within a minute.
const (
	descriptorTTL = 5 * time.Minute
	failureTTL    = time.Minute

	// Neither response is ever large: a descriptor is a handful of strings, and a
	// submission's answer is an id or an error line.
	maxResponse = 64 << 10
)

// Client talks to one sink.
type Client struct {
	sink Sink
	http *http.Client
	now  func() time.Time

	mu        sync.Mutex
	cached    *Descriptor
	cachedErr error
	expires   time.Time
}

// New returns a client for sink, or nil when the sink is not configured — the nil
// client is the disabled feature, and every method on it says so.
func New(sink Sink) *Client {
	if !sink.Configured() {
		return nil
	}
	return &Client{
		sink: sink,
		// Its own client, with a timeout: the default one has none, and a sink that
		// hangs would hang the dashboard's settings menu with it.
		http: &http.Client{Timeout: 5 * time.Second},
		now:  time.Now,
	}
}

// ErrDisabled is returned by every call on a nil client.
var ErrDisabled = errors.New("feedback is not configured")

// Descriptor returns the sink's self-description, from cache when fresh.
func (c *Client) Descriptor(ctx context.Context) (Descriptor, error) {
	if c == nil {
		return Descriptor{}, ErrDisabled
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.now().Before(c.expires) {
		if c.cachedErr != nil {
			return Descriptor{}, c.cachedErr
		}
		return *c.cached, nil
	}
	d, err := c.fetchDescriptor(ctx)
	if err != nil {
		c.cached, c.cachedErr, c.expires = nil, err, c.now().Add(failureTTL)
		return Descriptor{}, err
	}
	c.cached, c.cachedErr, c.expires = &d, nil, c.now().Add(descriptorTTL)
	return d, nil
}

func (c *Client) fetchDescriptor(ctx context.Context) (Descriptor, error) {
	req, err := c.request(ctx, http.MethodGet, nil)
	if err != nil {
		return Descriptor{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Descriptor{}, fmt.Errorf("feedback sink unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if resp.StatusCode != http.StatusOK {
		return Descriptor{}, fmt.Errorf("feedback sink answered %d: %s", resp.StatusCode, upstreamError(body))
	}
	var d Descriptor
	if err := json.Unmarshal(body, &d); err != nil {
		return Descriptor{}, fmt.Errorf("feedback sink descriptor is not JSON: %w", err)
	}
	if strings.TrimSpace(d.Operator) == "" {
		return Descriptor{}, errors.New("feedback sink descriptor names no operator")
	}
	return d, nil
}

// Send validates s against the sink's descriptor and delivers it.
func (c *Client) Send(ctx context.Context, s Submission) error {
	if c == nil {
		return ErrDisabled
	}
	d, err := c.Descriptor(ctx)
	if err != nil {
		return err
	}
	if err := s.Validate(d); err != nil {
		return err
	}
	payload, err := json.Marshal(s)
	if err != nil {
		return err
	}
	req, err := c.request(ctx, http.MethodPost, payload)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("feedback sink unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("feedback sink refused it (%d): %s", resp.StatusCode, upstreamError(body))
	}
	return nil
}

func (c *Client) request(ctx context.Context, method string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.sink.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("feedback sink URL: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.sink.Token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", brand.Name+"-Feedback")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// upstreamError extracts the sink's {"error": …} line, or a short excerpt of
// whatever it said instead.
func upstreamError(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "no detail"
	}
	return s
}
