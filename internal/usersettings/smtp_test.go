package usersettings

import (
	"testing"

	"github.com/yundera/maison/internal/notify"
)

var prov = notify.SMTP{Host: "smtp", Port: 587, User: "pcs", Pass: "s3cret", From: "noreply@john.nsl.sh", To: "john@example.com"}

// Nothing set here is the ordinary case: the deployment's relay, untouched.
func TestEffectiveSMTPInheritsTheDeployment(t *testing.T) {
	if got := (Settings{}).EffectiveSMTP(prov); got != prov {
		t.Errorf("EffectiveSMTP() = %+v, want the provisioned transport %+v", got, prov)
	}
}

// Changing where the mail goes is the choice a user actually makes, and it must not
// drag the relay's credentials or its sender along with it.
func TestEffectiveSMTPResolvesTheRecipientOnItsOwn(t *testing.T) {
	c := Settings{SMTP: &notify.SMTP{To: "someone@else.net"}}
	got := c.EffectiveSMTP(prov)
	if got.To != "someone@else.net" {
		t.Errorf("To = %q, want the box's own recipient", got.To)
	}
	if got.Host != prov.Host || got.User != prov.User || got.Pass != prov.Pass || got.From != prov.From {
		t.Errorf("changing the recipient disturbed the transport: %+v", got)
	}
}

// The whole transport travels together: a host set here with the deployment's
// credentials inherited would be a login sent to the wrong server.
func TestEffectiveSMTPTakesTheWholeTransportWithTheHost(t *testing.T) {
	c := Settings{SMTP: &notify.SMTP{Host: "mine.example.net", Port: 25, Security: "none"}}
	got := c.EffectiveSMTP(prov)
	if got.Host != "mine.example.net" || got.Port != 25 || got.Security != "none" {
		t.Errorf("transport = %+v, want the box's own", got)
	}
	if got.User != "" || got.Pass != "" {
		t.Errorf("credentials leaked to another server: user=%q pass set=%v", got.User, got.Pass != "")
	}
	// The addresses were not part of that decision.
	if got.From != prov.From || got.To != prov.To {
		t.Errorf("addresses = %q -> %q, want the deployment's", got.From, got.To)
	}
}

// A box with no deployment behind it — a standalone install — can still configure
// mail entirely by itself.
func TestEffectiveSMTPStandsAloneWithoutADeployment(t *testing.T) {
	c := Settings{SMTP: &notify.SMTP{Host: "mine", Port: 587, From: "a@b", To: "c@d"}}
	if got := c.EffectiveSMTP(notify.SMTP{}); !got.Configured() || got != *c.SMTP {
		t.Errorf("EffectiveSMTP() = %+v, want the box's own configuration", got)
	}
}
