package onboarding

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestValidFailsOpen guards the asymmetry documented on the package. Every one of
// these is a nonsensical setup URL, and answering "show" to any of them would send
// the owner's browser nowhere useful — with no way back to the dashboard.
func TestValidFailsOpen(t *testing.T) {
	for _, raw := range []string{
		"", "   ", "/setup", "https://",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
	} {
		if u, ok := Valid(raw); ok {
			t.Errorf("Valid(%q) = (%q, true), want false", raw, u)
		}
	}
	if u, ok := Valid(" https://admin.box.example/ "); !ok || u != "https://admin.box.example/" {
		t.Errorf("Valid(absolute) = (%q, %v)", u, ok)
	}
}

// TestAskOffWithoutAURL: the zero Check is the standalone box, and asks nothing.
func TestAskOffWithoutAURL(t *testing.T) {
	if u, ok := (Check{}).Ask(context.Background()); ok {
		t.Errorf("zero Check answered (%q, true)", u)
	}
}

// TestAskSendsTheTokenOnlyWhenSet. The token is optional: a check on a private
// network may need none, and an empty "Bearer " header is worse than no header.
func TestAskSendsTheTokenOnlyWhenSet(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"show":true,"url":"https://admin.box.example/"}`))
	}))
	defer srv.Close()

	if u, ok := (Check{URL: srv.URL, Token: "t"}).Ask(context.Background()); !ok || u != "https://admin.box.example/" {
		t.Errorf("Ask = (%q, %v)", u, ok)
	}
	if _, ok := (Check{URL: srv.URL}).Ask(context.Background()); !ok {
		t.Error("a check without a token was not asked")
	}
	if len(auth) != 2 || auth[0] != "Bearer t" || auth[1] != "" {
		t.Errorf("Authorization headers = %q", auth)
	}
}
