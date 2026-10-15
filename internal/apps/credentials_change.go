package apps

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/secretgen"
	"github.com/yundera/maison/internal/secrets"
	"github.com/yundera/maison/internal/stackup"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// Why a declared change cannot be offered. The UI translates them.
const (
	// ReasonTemplate: the password is not exactly one ${VAR}, so there is no
	// single value Maison could replace.
	ReasonTemplate = "template"
	// ReasonShared: the variable is not one of the app's own `secrets` — in
	// practice ${APP_DEFAULT_PASSWORD}, which is box-wide and re-synced from
	// .env.app on every up, so a per-app change would not hold.
	ReasonShared = "shared"
)

// CredentialChangeInfo is what the UI needs to offer a change: how it applies,
// the rules a new value must meet, and whether it can be offered at all.
type CredentialChangeInfo struct {
	Apply      string     `json:"apply"`
	Changeable bool       `json:"changeable"`
	Reason     string     `json:"reason,omitempty"`
	Policy     PolicyView `json:"policy"`
}

// PolicyView is a normalized secretgen.Policy as the UI reads it, with the
// characters Maison refuses in every credential spelled out.
type PolicyView struct {
	MinLength int      `json:"min_length"`
	MaxLength int      `json:"max_length"`
	Charset   string   `json:"charset"`
	Require   []string `json:"require,omitempty"`
	Forbid    string   `json:"forbid"`
}

// ErrNotChangeable reports a credential that declares no usable change.
var ErrNotChangeable = errors.New("this credential cannot be changed from Maison")

// ErrAppNotRunning reports a hook-applied change on a stopped app: the hook
// changes the password inside the running app, so there is nothing to tell.
var ErrAppNotRunning = errors.New("start the app first: the new password is set inside the running app")

// PolicyError lists the rules a proposed value breaks (secretgen.Rule*).
type PolicyError struct{ Rules []string }

func (e *PolicyError) Error() string {
	return "the password does not meet the app's rules: " + strings.Join(e.Rules, ", ")
}

// credentialReadyTimeout bounds the wait for a recreated app to come back.
const credentialReadyTimeout = 90 * time.Second

var wholeVarRe = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// credentialSecret is the variable a credential's password is, when it is
// exactly one of the app's own secrets.
func credentialSecret(ca *xcomposeapp.App, c xcomposeapp.Credential) (name, reason string) {
	m := wholeVarRe.FindStringSubmatch(strings.TrimSpace(c.Password))
	if m == nil {
		return "", ReasonTemplate
	}
	if _, ok := ca.Secrets[m[1]]; !ok {
		return "", ReasonShared
	}
	return m[1], ""
}

func credentialPolicy(c xcomposeapp.Credential) secretgen.Policy {
	p := c.Change.Policy
	return secretgen.Policy{
		MinLength: p.MinLength,
		MaxLength: p.MaxLength,
		Charset:   p.Charset,
		Require:   p.Require,
		Forbid:    p.Forbid,
	}.Normalize()
}

// changeInfo describes c's change for the UI, or nil when it declares none.
func changeInfo(ca *xcomposeapp.App, c xcomposeapp.Credential) *CredentialChangeInfo {
	if c.Change == nil {
		return nil
	}
	_, reason := credentialSecret(ca, c)
	p := credentialPolicy(c)
	return &CredentialChangeInfo{
		Apply:      c.Change.Apply,
		Changeable: reason == "",
		Reason:     reason,
		Policy: PolicyView{
			MinLength: p.MinLength,
			MaxLength: p.MaxLength,
			Charset:   p.Charset,
			Require:   p.Require,
			Forbid:    secretgen.Builtin + p.Forbid,
		},
	}
}

// changeTarget is a credential resolved for a change.
type changeTarget struct {
	dir    string
	ca     *xcomposeapp.App
	cred   xcomposeapp.Credential
	secret string
	policy secretgen.Policy
}

func (r *Registry) changeTarget(id string, index int) (changeTarget, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return changeTarget{}, os.ErrNotExist
	}
	dir, ca, err := r.mergedApp(id)
	if err != nil {
		return changeTarget{}, err
	}
	if ca == nil || index < 0 || index >= len(ca.Credentials) {
		return changeTarget{}, os.ErrNotExist
	}
	c := ca.Credentials[index]
	if c.Change == nil {
		return changeTarget{}, ErrNotChangeable
	}
	name, reason := credentialSecret(ca, c)
	if reason != "" {
		return changeTarget{}, fmt.Errorf("%w (%s)", ErrNotChangeable, reason)
	}
	return changeTarget{dir: dir, ca: ca, cred: c, secret: name, policy: credentialPolicy(c)}, nil
}

// GenerateCredential returns a fresh value for credential index of app id that
// meets its policy. It writes nothing: the operator sees it, then saves it.
func (r *Registry) GenerateCredential(id string, index int) (string, error) {
	t, err := r.changeTarget(id, index)
	if err != nil {
		return "", err
	}
	_, gen := secretgen.ParseSpec(t.ca.Secrets[t.secret])
	return secretgen.GenerateFor(t.policy, gen)
}

// ChangeCredential replaces the password of credential index of app id.
//
// The new value goes into the secret's line in .env; every other secret derived
// from it (`bcrypt:${X}`) is emptied so the up regenerates it from the new value.
// How the app learns the value is the credential's declaration:
//
//   - env:  write .env, recreate, wait until ready. If the app does not come
//     back, the previous .env is restored and the app recreated on it.
//   - hook: run the declared hook against the running app first, with the new
//     value in the environment; only then write .env and recreate. A failed hook
//     changes nothing. Once it has succeeded the app holds the new password, so a
//     failed recreate keeps the new .env rather than reverting to a password the
//     app no longer accepts.
//
// Neither value is ever logged or returned; errors are redacted of both.
func (r *Registry) ChangeCredential(ctx context.Context, id string, index int, value string) error {
	t, err := r.changeTarget(id, index)
	if err != nil {
		return err
	}
	if rules := t.policy.Validate(value); len(rules) > 0 {
		return &PolicyError{Rules: rules}
	}

	release, err := r.ReserveUpdate(id)
	if err != nil {
		return err
	}
	defer release()
	r.enter(id)
	defer r.leave(id)
	if r.RestoreInterrupted(id) {
		return fmt.Errorf("%s: an interrupted restore left its data incomplete", id)
	}

	envPath := filepath.Join(t.dir, ".env")
	before, err := os.ReadFile(envPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	old := envinject.EnvFileVars(before)[t.secret]
	if value == old {
		return nil
	}
	redact := func(err error) error { return redactErr(err, value, old) }

	vars := map[string]string{t.secret: value}
	for _, k := range dependentSecrets(t.ca.Secrets, t.secret) {
		vars[k] = "" // EnsureSecrets fills an empty value on the up
	}
	next, err := patchEnv(before, vars)
	if err != nil {
		return redact(err)
	}

	label := t.cred.Label.Value()
	if t.cred.Change.Apply == xcomposeapp.ApplyHook {
		if !credentialAppRunning(r, ctx, id) {
			return ErrAppNotRunning
		}
		extra := []string{
			"COMPOSE_PROJECT_NAME=" + id,
			t.secret + "=" + value,
			"OLD_" + t.secret + "=" + old,
		}
		if err := r.runChangeHook(ctx, id, t.dir, t.cred.Change.Hook, extra); err != nil {
			log.Printf("credential change for %s (%s): hook failed", id, label)
			return redact(fmt.Errorf("the change hook failed, nothing was changed: %w", err))
		}
	}

	if err := secrets.WriteFileAtomic(envPath, next, 0o644); err != nil {
		return redact(err)
	}
	err = r.upAndWait(ctx, id, t.dir)
	if err == nil {
		log.Printf("credential changed for %s (%s)", id, label)
		return nil
	}
	if t.cred.Change.Apply == xcomposeapp.ApplyHook {
		log.Printf("credential change for %s (%s): password changed, restart failed", id, label)
		return redact(fmt.Errorf("the password was changed, but the app did not restart cleanly: %w", err))
	}

	// env: put the app back on the password it had.
	log.Printf("credential change for %s (%s): app did not come back, reverting", id, label)
	if werr := secrets.WriteFileAtomic(envPath, before, 0o644); werr != nil {
		return redact(fmt.Errorf("the app did not start with the new password (%v), and restoring the previous one failed: %w", err, werr))
	}
	if uerr := r.upAndWait(ctx, id, t.dir); uerr != nil {
		return redact(fmt.Errorf("the app did not start with the new password (%v); the previous one was restored but the app did not come back either: %w", err, uerr))
	}
	return redact(fmt.Errorf("the app did not start with the new password; the previous one was restored: %w", err))
}

// dependentSecrets are the secrets whose generator reads name — a bcrypt of it,
// typically. Seeds (`${X}|gen`) are not dependents: a seed is read once, to move
// an installed app off a shared value, and then the secret is its own.
func dependentSecrets(all xcomposeapp.StringMap, name string) []string {
	ref := "${" + name + "}"
	var out []string
	for k, spec := range all {
		if k == name {
			continue
		}
		if _, gen := secretgen.ParseSpec(spec); strings.Contains(gen, ref) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// patchEnv sets vars in an .env's text the way envinject.EnsureVars does —
// in place, missing keys appended sorted, nothing removed — without writing it.
func patchEnv(raw []byte, vars map[string]string) ([]byte, error) {
	out := []envinject.Var{}
	have := map[string]bool{}
	for _, v := range envinject.ParseEnvFile(raw) {
		if val, ok := vars[v.Key]; ok {
			v.Value = val
			have[v.Key] = true
		}
		out = append(out, v)
	}
	var missing []string
	for k := range vars {
		if !have[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	for _, k := range missing {
		out = append(out, envinject.Var{Key: k, Value: vars[k]})
	}
	b, err := envinject.PatchEnvFile(raw, out)
	if err != nil {
		return nil, err
	}
	if b == nil {
		b = []byte{}
	}
	return b, nil
}

// redactErr replaces every occurrence of the given values in err's text. A hook's
// or an up's error carries its full output, and a hook that echoes its input
// would otherwise hand the password to the API response and the log.
func redactErr(err error, values ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	redacted := msg
	for _, v := range values {
		if len(v) >= 3 {
			redacted = strings.ReplaceAll(redacted, v, "***")
		}
	}
	if redacted == msg {
		return err // keep the chain (errors.Is) when there was nothing to hide
	}
	return errors.New(redacted)
}

// runChangeHook, upAndWait and the running check go through these seams so
// tests can drive a change without a Docker daemon.
var (
	credentialHook       = stackup.RunHookWith
	credentialUp         = stackup.Up
	credentialAppRunning = (*Registry).isRunning
)

func (r *Registry) runChangeHook(ctx context.Context, id, dir, script string, extra []string) error {
	return credentialHook(ctx, r.cfg, id, dir, script, extra)
}

func (r *Registry) upAndWait(ctx context.Context, id, dir string) error {
	if err := credentialUp(ctx, r.cfg, id, dir, r.composeFiles(dir)); err != nil {
		return err
	}
	if r.dx == nil {
		return nil
	}
	return r.dx.WaitProjectReady(ctx, id, credentialReadyTimeout)
}
