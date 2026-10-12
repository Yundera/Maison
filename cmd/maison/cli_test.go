package main

import (
	"errors"
	"strings"
	"testing"
)

func TestParseCLI(t *testing.T) {
	cases := []struct {
		args         string
		stdin        string
		method, path string
		body         string
	}{
		{"settings get", "", "GET", "/settings", ""},
		{"settings set smtp --owner mesh", `{"host":"smtp"}`, "PUT", "/settings/smtp?owner=mesh", `{"host":"smtp"}`},
		{"settings release feedback", "", "DELETE", "/settings/feedback", ""},
		{"secret set feedback_token", "tok\n", "PUT", "/secrets/feedback_token", "tok\n"},
		{"secret rm feedback_token", "", "DELETE", "/secrets/feedback_token", ""},
		{"stores ensure https://example.com/s.zip", "", "POST", "/stores/ensure", `{"url":"https://example.com/s.zip"}`},
	}
	for _, c := range cases {
		m, p, b, err := parseCLI(strings.Fields(c.args), strings.NewReader(c.stdin))
		if err != nil || m != c.method || p != c.path || string(b) != c.body {
			t.Errorf("%q → %s %s %q, %v; want %s %s %q", c.args, m, p, b, err, c.method, c.path, c.body)
		}
	}
}

func TestParseCLIRefuses(t *testing.T) {
	for _, c := range []struct{ args, stdin string }{
		{"", ""},
		{"help", ""},
		{"settings", ""},
		{"settings set smtp", `{}`},             // no owner
		{"settings set smtp --owner mesh", ""},  // nothing on stdin
		{"settings set smtp --owner mesh", "{"}, // not JSON
		{"secret set x", ""},                    // nothing on stdin
		{"apps rm everything", ""},
	} {
		if _, _, _, err := parseCLI(strings.Fields(c.args), strings.NewReader(c.stdin)); err == nil {
			t.Errorf("%q accepted", c.args)
		}
	}
	if _, _, _, err := parseCLI([]string{"bogus", "cmd"}, strings.NewReader("")); !errors.Is(err, errUsage) {
		t.Errorf("unknown command: err = %v, want errUsage", err)
	}
}
