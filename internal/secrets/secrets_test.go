package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return New(filepath.Join(t.TempDir(), FileName))
}

func TestSetGetDeleteRoundTrip(t *testing.T) {
	s := newStore(t)
	if err := s.Set("smtp_pass", "hunter2=with=equals"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("smtp_pass")
	if err != nil || got != "hunter2=with=equals" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := s.Delete("smtp_pass"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("smtp_pass"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after Delete, Get err = %v, want ErrNotFound", err)
	}
	if err := s.Delete("smtp_pass"); err != nil {
		t.Fatalf("deleting a missing secret: %v", err)
	}
}

func TestFileIsPrivateAndKeepsComments(t *testing.T) {
	s := newStore(t)
	if err := os.WriteFile(s.Path(), []byte("# hand note\nold=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("feedback_token", "abc"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(s.Path())
	if !strings.Contains(string(b), "# hand note") || !strings.Contains(string(b), "old=1") {
		t.Fatalf("hand-written content lost:\n%s", b)
	}
}

func TestSetRejectsBadInput(t *testing.T) {
	s := newStore(t)
	for _, c := range []struct{ name, value string }{
		{"Upper", "x"}, {"1digit", "x"}, {"has-dash", "x"}, {"ok", ""}, {"ok", "two\nlines"},
	} {
		if err := s.Set(c.name, c.value); err == nil {
			t.Errorf("Set(%q, %q) accepted", c.name, c.value)
		}
	}
}

func TestResolveIsWholeValueOnly(t *testing.T) {
	s := newStore(t)
	if err := s.Set("smtp_pass", "real"); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"${secret:smtp_pass}":      "real",
		"pre${secret:smtp_pass}":   "pre${secret:smtp_pass}",
		"${secret:smtp_pass} ":     "${secret:smtp_pass} ",
		"plain-password-with-${x}": "plain-password-with-${x}",
		"":                         "",
	}
	for in, want := range cases {
		got, err := s.Resolve(in)
		if err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := s.Resolve(Ref("missing")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing reference: err = %v, want ErrNotFound", err)
	}
}
