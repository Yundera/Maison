package server

import (
	"log"

	"github.com/yundera/maison/internal/feedback"
	"github.com/yundera/maison/internal/notify"
	"github.com/yundera/maison/internal/onboarding"
	"github.com/yundera/maison/internal/secrets"
	"github.com/yundera/maison/internal/usersettings"
)

// Secret names the server itself writes, when a user types a value into a form.
// A deployment managing the same setting names its own (it stores the secret first,
// then references it), so these only ever hold a user's value.
const (
	userSMTPPass = "smtp_pass"
)

// maskedLiteral stands in for a secret the API must not return but that is not (yet)
// a reference — a password a box carried in settings.json from before secrets.env.
// A client that echoes it back unchanged means "keep it", like an echoed reference.
const maskedLiteral = "********"

// mailTransport is the SMTP transport every mail Maison sends goes through: the
// settings resolved over the deployment's (EffectiveSMTP), the password looked up
// in the secret store, and the addresses filled from the deployment's identity
// where neither layer named one.
//
// Resolved on every send rather than captured, so a relay changed in the settings —
// by the user or by the deployment — takes effect without a restart.
func (s *Server) mailTransport() notify.SMTP {
	out := s.settings.EffectiveSMTP(s.cfg.ProvisionedSMTP())
	if out.Host == "" {
		return notify.SMTP{}
	}
	pass, err := s.secrets.Resolve(out.Pass)
	if err != nil {
		log.Printf("mail: %v — sending without a password", err)
		pass = ""
	}
	out.Pass = pass
	from, to := s.cfg.MailIdentity()
	if out.From == "" {
		out.From = from
	}
	if out.To == "" {
		out.To = to
	}
	return out
}

// feedbackClient is the client for the feedback sink as configured right now.
//
// The sink is a managed setting the deployment can set, change and release while
// Maison runs, so the client is rebuilt whenever the resolved URL or token differs
// from the one it was built for — and kept otherwise, because it carries the
// sink's cached descriptor. nil is the feature switched off.
//
// TRANSITION: while the setting is absent, FEEDBACK_URL/FEEDBACK_TOKEN from the env
// still configure it, so this image runs unchanged under a template that has not
// moved to the admin socket yet. That fallback goes once every template has.
func (s *Server) feedbackClient() *feedback.Client {
	sink := s.cfg.Feedback
	if fb := s.settings.Get().Feedback; fb != nil {
		token, err := s.secrets.Resolve(fb.Token)
		if err != nil {
			log.Printf("feedback: %v", err)
			return nil
		}
		sink = feedback.Sink{URL: fb.URL, Token: token}
	}
	s.feedbackMu.Lock()
	defer s.feedbackMu.Unlock()
	if s.feedbackC == nil || sink != s.feedbackSink {
		s.feedbackSink = sink
		s.feedbackC = feedback.New(sink)
	}
	return s.feedbackC
}

// onboardingCheck is the deployment's onboarding check as configured right now, with
// its token resolved from the secret store. The zero Check — no setting, or a value
// from before the check existed ({"url": …}, which decodes to an empty CheckURL) — is
// the feature switched off.
//
// Built per navigation rather than cached: the setting and its secret can change
// while Maison runs, and the check itself holds no state worth keeping (its HTTP
// client is shared). A token that does not resolve switches the gate off — fail open,
// like every other failure of the check.
func (s *Server) onboardingCheck() onboarding.Check {
	ob := s.settings.Get().Onboarding
	if ob == nil || ob.CheckURL == "" {
		return onboarding.Check{}
	}
	token, err := s.secrets.Resolve(ob.Token)
	if err != nil {
		log.Printf("onboarding: %v", err)
		return onboarding.Check{}
	}
	return onboarding.Check{URL: ob.CheckURL, Token: token}
}

// publicSettings is the settings document as the API returns it: no secret value,
// ever. A reference is returned as is (it is its own mask); a literal a box still
// carries is replaced by maskedLiteral.
func publicSettings(st usersettings.Settings) usersettings.Settings {
	if st.SMTP != nil {
		c := *st.SMTP
		c.Pass = maskSecret(c.Pass)
		st.SMTP = &c
	}
	if st.Feedback != nil {
		c := *st.Feedback
		c.Token = maskSecret(c.Token)
		st.Feedback = &c
	}
	if st.Onboarding != nil {
		c := *st.Onboarding
		c.Token = maskSecret(c.Token)
		st.Onboarding = &c
	}
	return st
}

func maskSecret(v string) string {
	if v == "" {
		return ""
	}
	if _, ok := secrets.IsRef(v); ok {
		return v
	}
	return maskedLiteral
}

// prepareUserEdit turns a settings edit from the dashboard into what is stored.
//
// The deployment-only settings are dropped: feedback and onboarding are never the
// user's to set, managed or not. A typed SMTP password goes to the secret store
// and is replaced by its reference; an echoed reference or mask keeps the password
// already stored; an emptied one deletes it. Managed fields are left for the store
// to drop (usersettings.Set), and their secrets are not touched here.
func (s *Server) prepareUserEdit(in *usersettings.Settings) error {
	// Routing has its own endpoint, which recreates every app, and capabilities
	// has its own, which validates; SeenHost is Maison's own record. None rides the
	// generic PUT.
	in.Feedback, in.Onboarding, in.Managed, in.Routing, in.SeenHost, in.Capabilities = nil, nil, nil, nil, "", nil
	cur := s.settings.Get()
	if in.SMTP == nil {
		return nil
	}
	if _, managed := cur.IsManaged("smtp"); managed {
		return nil
	}
	curPass := ""
	if cur.SMTP != nil {
		curPass = cur.SMTP.Pass
	}
	switch p := in.SMTP.Pass; {
	case p == maskedLiteral || p == curPass:
		in.SMTP.Pass = curPass
	case p == "":
		if _, ok := secrets.IsRef(curPass); ok {
			if err := s.secrets.Delete(userSMTPPass); err != nil {
				return err
			}
		}
	default:
		if _, ok := secrets.IsRef(p); ok {
			// A reference the client made up, rather than one it was given: refuse to
			// point the setting at a secret it has never stored.
			in.SMTP.Pass = curPass
			break
		}
		if err := s.secrets.Set(userSMTPPass, p); err != nil {
			return err
		}
		in.SMTP.Pass = secrets.Ref(userSMTPPass)
	}
	return nil
}

// adoptLiteralSecrets moves a password a box still keeps in settings.json — from
// before secrets.env existed — into the secret store, once, at boot.
//
// Only an unmanaged value: a managed one is the deployment's, which re-asserts it
// with its own reference on the next self-check. A failure is logged and retried
// on the next boot; the literal keeps working meanwhile, and the API masks it.
func (s *Server) adoptLiteralSecrets() {
	cur := s.settings.Get()
	if cur.SMTP == nil || cur.SMTP.Pass == "" {
		return
	}
	if _, managed := cur.IsManaged("smtp"); managed {
		return
	}
	if _, ok := secrets.IsRef(cur.SMTP.Pass); ok {
		return
	}
	if err := s.secrets.Set(userSMTPPass, cur.SMTP.Pass); err != nil {
		log.Printf("settings: moving the SMTP password into %s: %v", secrets.FileName, err)
		return
	}
	next := *cur.SMTP
	next.Pass = secrets.Ref(userSMTPPass)
	if err := s.settings.Set(usersettings.Settings{SMTP: &next}); err != nil {
		log.Printf("settings: referencing the moved SMTP password: %v", err)
		return
	}
	log.Printf("settings: moved the SMTP password into %s", secrets.FileName)
}
