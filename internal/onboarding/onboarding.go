// Package onboarding asks the deployment, live, whether the owner still has a
// first-run step to complete — on a PCS, claiming the box's local account and
// choosing a backup password — and where the browser should go for it.
//
// The deployment names a CHECK: a URL and an optional bearer token, set through the
// managed `onboarding` setting (docs/config.md). On each browser navigation the
// dashboard asks that URL and gets back {"show": bool, "url": "..."}. Maison never
// decides for itself what "onboarded" means and never remembers the answer: what
// setup consists of is the deployment's business, and a boolean cached here would go
// stale exactly where it hurts — a restored or migrated box can carry a "done" it
// no longer deserves. Asking each time keeps Maison out of the business of
// remembering.
//
// There is deliberately no completion callback. A callback would be a
// client-controlled state transition: anything that can reach Maison — every
// container on a flat app network, or a curious owner with the address bar — could
// mark the box set up. The answer comes from the deployment, on a request Maison
// makes.
//
// EVERY failure answers "nothing to show". The check is an outbound call on the path
// of every page load, and the two ways of being wrong are not symmetrical: guessing
// "nothing to show" costs a redundant trip the owner can still make by the
// deployment's own URL, while guessing "show" locks them out of their own dashboard
// behind a redirect to somewhere that is down. Fail open.
//
// No check configured = feature off, which is what a standalone Maison always sees.
package onboarding

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yundera/maison/internal/brand"
)

// Timeout bounds one check. It sits on the path of a page load, so it is short: a
// deployment that cannot answer in that time gets the dashboard served, not a hung
// browser.
const Timeout = 1500 * time.Millisecond

// maxAnswer caps what is read of the check's response. The answer is a boolean and a
// URL.
const maxAnswer = 8 << 10

// client follows no redirects: the check is a question with a JSON answer, and a 3xx
// — a login page in front of a misconfigured endpoint — is not one. The timeout comes
// from the request context, bounded by Timeout.
var client = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Check is the deployment's onboarding check, as configured right now.
type Check struct {
	// URL is asked with a GET on each navigation. Empty is the feature switched off.
	URL string
	// Token, when set, is sent as `Authorization: Bearer <token>`.
	Token string
}

// Configured reports whether there is a check to ask.
func (c Check) Configured() bool { return strings.TrimSpace(c.URL) != "" }

// answer is the check's response body.
type answer struct {
	Show bool   `json:"show"`
	URL  string `json:"url"`
}

// Ask asks the check and reports where to send the browser, if anywhere. It is never
// cached: a "show" that outlived the step it stood for would bounce the owner back
// into a wizard they had just finished.
func (c Check) Ask(ctx context.Context) (string, bool) {
	if !c.Configured() {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(c.URL), nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", brand.Name+"-Onboarding")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAnswer))
		return "", false
	}
	var a answer
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswer)).Decode(&a); err != nil || !a.Show {
		return "", false
	}
	return Valid(a.URL)
}

// Valid applies the fail-open rule to a setup URL: it must be absolute http(s), or
// there is nowhere to send the browser.
func Valid(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	// The URL becomes a Location header on the dashboard's own origin. A scheme like
	// `javascript:` or `data:` there, or a relative path that loops back into the
	// gate, would turn a mistake in the deployment's answer into a broken dashboard.
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return raw, true
}
