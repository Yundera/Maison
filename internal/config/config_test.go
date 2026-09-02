package config

import (
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/notify"
)

func TestStateDirDefaultsToMaisonsOwnAppFolder(t *testing.T) {
	c := Config{DataRoot: "/DATA"}
	if got, want := c.StateDir(), filepath.Join("/DATA", "AppData", "maison"); got != want {
		t.Errorf("StateDir() = %q, want %q", got, want)
	}
}

// STATE_DIR moves everything Maison owns — settings, store cache, and the
// .env.app it reads the deployment's variables from.
func TestStateDirOverride(t *testing.T) {
	c := Config{DataRoot: "/DATA", StateDirPath: "/var/lib/maison"}
	if got, want := c.StateDir(), "/var/lib/maison"; got != want {
		t.Errorf("StateDir() = %q, want %q", got, want)
	}
}

// The deployment names its relay and nothing else; who the mail is for comes from
// the same .env.app that already says who owns the box.
func TestProvisionedSMTPFillsAddressesFromTheDeployment(t *testing.T) {
	t.Setenv("SMTP_HOST", "smtp")
	c := FromEnv()
	c.AppEnv = func() map[string]string {
		return map[string]string{"APP_DOMAIN": "john.nsl.sh", "APP_EMAIL": "john@example.com"}
	}
	got := c.ProvisionedSMTP()
	if got.From != "noreply@john.nsl.sh" || got.To != "john@example.com" {
		t.Errorf("addresses = %q -> %q, want noreply@john.nsl.sh -> john@example.com", got.From, got.To)
	}
	if got.Port != 587 {
		t.Errorf("Port = %d, want the submission port by default", got.Port)
	}
	if !got.Configured() {
		t.Error("naming the relay on a deployment that knows its owner did not produce a usable configuration")
	}
}

// Explicit values win over the fallbacks; that is the point of having them.
func TestProvisionedSMTPPrefersItsOwnEnvironment(t *testing.T) {
	t.Setenv("SMTP_HOST", "relay.example.net")
	t.Setenv("SMTP_PORT", "2525")
	t.Setenv("SMTP_FROM", "pcs@example.net")
	t.Setenv("SMTP_TO", "ops@example.net")
	t.Setenv("SMTP_SECURITY", "none")
	c := FromEnv()
	c.AppEnv = func() map[string]string {
		return map[string]string{"APP_DOMAIN": "john.nsl.sh", "APP_EMAIL": "john@example.com"}
	}
	got := c.ProvisionedSMTP()
	want := notify.SMTP{Host: "relay.example.net", Port: 2525, From: "pcs@example.net", To: "ops@example.net", Security: "none"}
	if got != want {
		t.Errorf("ProvisionedSMTP() = %+v, want %+v", got, want)
	}
}

// No relay named means no mail, not a nightly connection refused against a guess.
// A standalone local install is exactly this case.
func TestProvisionedSMTPIsEmptyWithoutAHost(t *testing.T) {
	c := FromEnv()
	c.AppEnv = func() map[string]string {
		return map[string]string{"APP_DOMAIN": "john.nsl.sh", "APP_EMAIL": "john@example.com"}
	}
	if got := c.ProvisionedSMTP(); got.Configured() || got.From != "" || got.To != "" {
		t.Errorf("ProvisionedSMTP() = %+v, want nothing at all", got)
	}
}

// A dashboard that will not start is worse than a mail sent to the default port.
func TestProvisionedSMTPFallsBackOnAnUnusablePort(t *testing.T) {
	for _, p := range []string{"", "not-a-number", "0", "70000", "-1"} {
		t.Setenv("SMTP_HOST", "smtp")
		t.Setenv("SMTP_PORT", p)
		if got := FromEnv().SMTP.Port; got != 587 {
			t.Errorf("SMTP_PORT=%q gave port %d, want 587", p, got)
		}
	}
}
