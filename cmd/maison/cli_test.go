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
		{"backup approve kopia", "", "POST", "/backup/engines/kopia/approve", ""},
		{"backup status", "", "GET", "/backup/status", ""},
		// One line, the line ending dropped and nothing else: the key reaches the server
		// exactly as typed.
		{"backup init kopia", "correct horse battery staple\n", "POST", "/backup/engines/kopia/init", `{"confirmed":true,"key":"correct horse battery staple"}`},
		{"backup init kopia", "a \"quoted\" pw  \r\n", "POST", "/backup/engines/kopia/init", `{"confirmed":true,"key":"a \"quoted\" pw  "}`},
		{"backup recover kopia", "the old key\n", "POST", "/backup/engines/kopia/recover", `{"key":"the old key"}`},
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
		{"backup init kopia", ""},               // nothing on stdin
		{"backup init kopia", "two\nlines\n"},   // not one line
		{"backup init", "pw\n"},                 // no engine
		{"backup recover kopia extra", "pw\n"},  // too many arguments
		{"backup status kopia", ""},             // status takes no engine
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

func TestTimeoutFor(t *testing.T) {
	if timeoutFor("/backup/engines/kopia/recover") != recoverTimeout ||
		timeoutFor("/backup/engines/kopia/init") != initTimeout ||
		timeoutFor("/settings") != callTimeout {
		t.Error("init and recover must outlast the engine verbs they wait for")
	}
}
