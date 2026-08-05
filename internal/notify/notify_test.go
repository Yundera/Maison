package notify

import (
	"strings"
	"testing"
	"time"
)

var when = time.Date(2026, 3, 1, 3, 30, 0, 0, time.UTC)

// The wire format is the part worth pinning: bare LF is a protocol violation some
// servers reject and others silently mangle.
func TestComposeUsesCRLFThroughout(t *testing.T) {
	msg := string(Compose("a@b", "c@d", "Subject line", "first\nsecond\n", when))
	if strings.Contains(strings.ReplaceAll(msg, "\r\n", ""), "\n") {
		t.Fatalf("message contains a bare LF:\n%q", msg)
	}
	if !strings.Contains(msg, "\r\n\r\nfirst\r\nsecond\r\n") {
		t.Errorf("headers and body are not separated by a blank line:\n%q", msg)
	}
}

// Subjects carry app names, and app names come from a third-party store — so a
// newline in one must not be able to inject a header.
func TestComposeStripsHeaderInjection(t *testing.T) {
	msg := string(Compose("a@b", "c@d", "Backup failed\r\nBcc: attacker@evil", "body", when))
	// The payload is neutralised by being folded onto the Subject line, not by being
	// removed — so the test is that no *new header line* was created, which is the
	// thing that would actually have changed where the mail went.
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") {
			t.Fatalf("a newline in the subject started a new header:\n%q", msg)
		}
	}
	if !strings.Contains(msg, "Subject: Backup failed  Bcc: attacker@evil\r\n") {
		t.Errorf("the injected text should survive as inert subject text:\n%q", msg)
	}
}

func TestComposeSetsTheRequiredHeaders(t *testing.T) {
	msg := string(Compose("from@x", "to@y", "hello", "body", when))
	for _, want := range []string{"From: from@x", "To: to@y", "Subject: hello", "Date: ", "Content-Type: text/plain"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing header %q", want)
		}
	}
}

func TestConfiguredRequiresAServerAndBothAddresses(t *testing.T) {
	for _, c := range []SMTP{
		{},
		{Host: "smtp"},
		{Host: "smtp", From: "a@b"},
		{From: "a@b", To: "c@d"},
	} {
		if c.Configured() {
			t.Errorf("%+v reported itself as configured", c)
		}
	}
	if !(SMTP{Host: "smtp", From: "a@b", To: "c@d"}).Configured() {
		t.Error("a complete configuration was rejected")
	}
}
