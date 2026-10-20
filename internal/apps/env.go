package apps

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/yundera/maison/internal/appenv"
	"github.com/yundera/maison/internal/envinject"
)

// EnvEntry is one .env row as the editor sees it: the variable, plus who sets it
// (appenv.Source*; empty for the operator's own). A maison or deployment row is
// put back by the next up whatever the editor saves, so the editor locks it.
//
// Masked rows come back with an empty Value: the dashboard never receives a
// secret's value, and a client echoing the row back unchanged has said "keep it"
// without having seen it (see mergeProtectedEnv).
type EnvEntry struct {
	envinject.Var
	Source string `json:"source,omitempty"`
	Masked bool   `json:"masked,omitempty"`
}

// secretNameRe is the fallback for a key the app does not declare: a name that
// reads like a credential is masked, but not locked — with nothing declared there
// is no Security tab, and the editor is the only way the operator can change it.
var secretNameRe = regexp.MustCompile(`(?i)(PASSWORD|PASSWD|PASSPHRASE|SECRET|TOKEN|APIKEY|API_KEY|_KEY$|HASH|SALT)`)

// envPolicy is what the editor may show and change of one app's .env.
type envPolicy struct {
	sources map[string]string // appenv.Source* per key; absent = the operator's own
	masked  map[string]bool   // value never leaves the server
}

// locked reports whether a save keeps k's value on disk whatever it was sent.
func (p envPolicy) locked(k string) bool {
	switch p.sources[k] {
	case appenv.SourceMaison, appenv.SourceDeployment, appenv.SourceSeeded, appenv.SourceSecret:
		return true
	}
	return false
}

// envPolicyFor classifies id's .env keys. Declared secrets and the variable each
// declared credential's password is are locked and masked: generated once and
// part of the app's data, they change through credentials[].change, which also
// regenerates what derives from them. The seeded default password is locked and
// masked for the same reason. Any other key whose name reads like a credential is
// masked only.
func (r *Registry) envPolicyFor(id string, keys []string) envPolicy {
	p := envPolicy{sources: appenv.KeySources(r.cfg), masked: map[string]bool{}}
	if _, ca, err := r.mergedApp(id); err == nil && ca != nil {
		for k := range ca.Secrets {
			p.sources[k] = appenv.SourceSecret
		}
		for _, c := range ca.Credentials {
			if m := wholeVarRe.FindStringSubmatch(strings.TrimSpace(c.Password)); m != nil {
				if s := p.sources[m[1]]; s == "" || s == appenv.SourceDeployment {
					p.sources[m[1]] = appenv.SourceSecret
				}
			}
		}
	}
	for k, s := range p.sources {
		if s == appenv.SourceSecret || s == appenv.SourceSeeded {
			p.masked[k] = true
		}
	}
	for _, k := range keys {
		if secretNameRe.MatchString(k) {
			p.masked[k] = true
		}
	}
	return p
}

// GetEnv reads a managed app's .env as an ordered key/value list — the app's
// persistent variable record, prefilled at install from the base interpolation
// vars (PUID, DATA_ROOT, REF_*, …) and hand-editable since. A managed app whose
// .env was deleted reads as empty rather than failing. Secret values are blanked
// (EnvEntry.Masked).
func (r *Registry) GetEnv(id string) ([]EnvEntry, error) {
	dir := filepath.Join(r.cfg.AppsDir(), id)
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err != nil {
		return nil, err // only managed apps have an editable .env
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	vars := envinject.ParseEnvFile(raw)
	p := r.envPolicyFor(id, envKeys(vars))
	out := []EnvEntry{}
	for _, v := range vars {
		e := EnvEntry{Var: v, Source: p.sources[v.Key]}
		if p.masked[v.Key] {
			e.Value, e.Masked = "", true
		}
		out = append(out, e)
	}
	return out, nil
}

// SetEnv rewrites a managed app's .env to hold exactly vars and recreates the
// stack. The recreate is the point: `docker compose` reads .env when it brings a
// project up, so an edit that isn't followed by an up-d leaves the running
// containers on the old values and the UI lying about what took effect.
//
// The file is patched rather than regenerated (see envinject.PatchEnvFile), so
// comments and line order the operator added by hand survive a save.
func (r *Registry) SetEnv(ctx context.Context, id string, vars []envinject.Var) error {
	return r.SaveSettings(ctx, id, SettingsChange{Env: &vars})
}

// protectEnv is the server half of the editor's locks, applied to every .env
// save: the UI's read-only rows are only a courtesy, and a client that drops a
// secret row would otherwise delete it — after which the next up generates a new
// value the app's database was never set up with.
func (r *Registry) protectEnv(id string, old []byte, in []envinject.Var) []envinject.Var {
	oldVars := envinject.ParseEnvFile(old)
	return mergeProtectedEnv(r.envPolicyFor(id, append(envKeys(oldVars), envKeys(in)...)), oldVars, in)
}

// mergeProtectedEnv returns in with p applied against the file's current vars:
//
//   - a locked key keeps its value on disk, whether it was sent changed, sent
//     blank or left out — and a locked key not on disk is not created here;
//   - a masked, unlocked key sent blank keeps its value on disk, because a masked
//     row is sent to the client blank and echoed back so. Like a reference in the
//     secret store (internal/secrets), the blank is its own mask. A non-blank value
//     is a real edit, and leaving the row out deletes the key as usual.
//
// Order follows in; a locked key the client left out is appended, and
// PatchEnvFile keeps it on its own line either way.
func mergeProtectedEnv(p envPolicy, old, in []envinject.Var) []envinject.Var {
	have := map[string]string{}
	for _, v := range old {
		have[v.Key] = v.Value
	}
	out := make([]envinject.Var, 0, len(in))
	sent := map[string]bool{}
	for _, v := range in {
		cur, onDisk := have[v.Key]
		switch {
		case p.locked(v.Key):
			if !onDisk {
				continue
			}
			v.Value = cur
		case p.masked[v.Key] && v.Value == "" && onDisk:
			v.Value = cur
		}
		sent[v.Key] = true
		out = append(out, v)
	}
	for _, v := range old {
		if p.locked(v.Key) && !sent[v.Key] {
			out = append(out, v)
		}
	}
	return out
}

// redactEnv replaces every masked value of id's .env in s with a mask — for
// output that interpolates the .env, like the resolved effective config.
func (r *Registry) redactEnv(id, s string) string {
	raw, err := os.ReadFile(filepath.Join(r.cfg.AppsDir(), id, ".env"))
	if err != nil {
		return s
	}
	vars := envinject.ParseEnvFile(raw)
	p := r.envPolicyFor(id, envKeys(vars))
	var values []string
	for _, v := range vars {
		if p.masked[v.Key] {
			values = append(values, v.Value)
		}
	}
	return redactValues(s, values)
}

// redactMask stands for a hidden value in output.
const redactMask = "••••"

// redactValues masks every occurrence of values in s, longest first so a value
// containing another is masked whole. Values shorter than 4 characters are left:
// masking every "1" or "yes" in a config would hide nothing and garble the rest.
func redactValues(s string, values []string) string {
	vs := make([]string, 0, len(values))
	for _, v := range values {
		if len(v) >= 4 {
			vs = append(vs, v)
		}
	}
	sort.Slice(vs, func(i, j int) bool { return len(vs[i]) > len(vs[j]) })
	for _, v := range vs {
		s = strings.ReplaceAll(s, v, redactMask)
	}
	return s
}

func envKeys(vars []envinject.Var) []string {
	keys := make([]string, len(vars))
	for i, v := range vars {
		keys[i] = v.Key
	}
	return keys
}
