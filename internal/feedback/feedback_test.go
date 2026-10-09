package feedback

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSink answers the descriptor with desc (or status, when non-zero) and records
// the last submission it accepted.
type fakeSink struct {
	desc     string
	status   int
	gets     atomic.Int32
	lastAuth string
	lastBody []byte
	postCode int
	postBody string
}

func (f *fakeSink) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastAuth = r.Header.Get("Authorization")
		switch r.Method {
		case http.MethodGet:
			f.gets.Add(1)
			if f.status != 0 {
				w.WriteHeader(f.status)
				return
			}
			io.WriteString(w, f.desc)
		case http.MethodPost:
			f.lastBody, _ = io.ReadAll(r.Body)
			code := f.postCode
			if code == 0 {
				code = http.StatusAccepted
			}
			w.WriteHeader(code)
			io.WriteString(w, f.postBody)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAnUnconfiguredSinkIsTheDisabledFeature(t *testing.T) {
	for _, s := range []Sink{{}, {URL: "http://x"}, {Token: "t"}} {
		c := New(s)
		if c != nil {
			t.Errorf("%+v built a client", s)
		}
		if _, err := c.Descriptor(context.Background()); !errors.Is(err, ErrDisabled) {
			t.Errorf("Descriptor on %+v = %v, want ErrDisabled", s, err)
		}
		if err := c.Send(context.Background(), Submission{Message: "hi"}); !errors.Is(err, ErrDisabled) {
			t.Errorf("Send on %+v = %v, want ErrDisabled", s, err)
		}
	}
}

func TestTheDescriptorIsAskedWithTheTokenAndCached(t *testing.T) {
	f := &fakeSink{desc: `{"operator":"Acme","maxLength":10}`}
	srv := f.serve(t)
	c := New(Sink{URL: srv.URL, Token: "s3cret"})

	for i := 0; i < 3; i++ {
		d, err := c.Descriptor(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if d.Operator != "Acme" || d.Limit() != 10 {
			t.Fatalf("descriptor = %+v", d)
		}
	}
	if f.lastAuth != "Bearer s3cret" {
		t.Errorf("Authorization = %q", f.lastAuth)
	}
	if n := f.gets.Load(); n != 1 {
		t.Errorf("sink asked %d times, want 1", n)
	}
}

// A sink that says no — or says nothing useful — hides the feature, and is asked
// again after the short failure window rather than the long success one.
func TestAFailingDescriptorDisablesAndIsRetriedSoon(t *testing.T) {
	for name, f := range map[string]*fakeSink{
		"404":         {status: http.StatusNotFound},
		"not json":    {desc: "<html>"},
		"no operator": {desc: `{"privacyNote":"x"}`},
	} {
		t.Run(name, func(t *testing.T) {
			srv := f.serve(t)
			c := New(Sink{URL: srv.URL, Token: "t"})
			now := time.Now()
			c.now = func() time.Time { return now }

			if _, err := c.Descriptor(context.Background()); err == nil {
				t.Fatal("a bad descriptor was accepted")
			}
			c.Descriptor(context.Background())
			if n := f.gets.Load(); n != 1 {
				t.Fatalf("asked %d times inside the failure window", n)
			}
			now = now.Add(failureTTL + time.Second)
			c.Descriptor(context.Background())
			if n := f.gets.Load(); n != 2 {
				t.Errorf("asked %d times after the failure window, want 2", n)
			}
		})
	}
}

func TestSendForwardsTheSubmission(t *testing.T) {
	f := &fakeSink{desc: `{"operator":"Acme"}`}
	srv := f.serve(t)
	c := New(Sink{URL: srv.URL, Token: "t"})

	err := c.Send(context.Background(), Submission{
		Message:  "  more themes please  ",
		Category: "idea",
		Context:  Context{Source: "maison", Version: "1.2.3", Page: "/store", AppID: "jellyfin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(f.lastBody, &got); err != nil {
		t.Fatal(err)
	}
	if got["message"] != "more themes please" || got["category"] != "idea" {
		t.Errorf("body = %s", f.lastBody)
	}
	if r, ok := got["reporter"]; !ok || r != nil {
		t.Errorf("reporter should be present and null: %s", f.lastBody)
	}
	ctx := got["context"].(map[string]any)
	if ctx["appId"] != "jellyfin" || ctx["version"] != "1.2.3" || ctx["page"] != "/store" {
		t.Errorf("context = %v", ctx)
	}
}

func TestSendRefusesWhatTheSinkWouldNotTake(t *testing.T) {
	f := &fakeSink{desc: `{"operator":"Acme","maxLength":5}`}
	srv := f.serve(t)
	c := New(Sink{URL: srv.URL, Token: "t"})

	for _, s := range []Submission{
		{Message: "   "},
		{Message: "too long"},
		{Message: "ok", Category: "rant"},
	} {
		if err := c.Send(context.Background(), s); !errors.Is(err, ErrInvalid) {
			t.Errorf("Send(%+v) = %v, want ErrInvalid", s, err)
		}
	}
	if f.lastBody != nil {
		t.Errorf("an invalid submission reached the sink: %s", f.lastBody)
	}
}

func TestTheSinksOwnErrorIsWhatTheUserSees(t *testing.T) {
	f := &fakeSink{desc: `{"operator":"Acme"}`, postCode: http.StatusInternalServerError, postBody: `{"error":"mail relay down"}`}
	srv := f.serve(t)
	c := New(Sink{URL: srv.URL, Token: "t"})

	err := c.Send(context.Background(), Submission{Message: "hello"})
	if err == nil || !strings.Contains(err.Error(), "mail relay down") {
		t.Errorf("err = %v", err)
	}
}
