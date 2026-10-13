package stackup

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/secretgen"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// EnsureSecrets and EnsureVariables put an app's declared values into its own
// .env, which is where compose reads them and where every later mechanism —
// the seed renderer, `files`, a hand-run `docker compose up` in the app folder —
// picks them up without knowing they were generated.
//
// That is the whole design: a generated secret is an ordinary .env variable the
// moment it is written, so nothing downstream needs a second resolution path.

// EnsureSecrets generates each declared secret that the app's .env does not
// already carry, and writes it there.
//
// Generate-once is the contract. A secret that encrypts data at rest (Outline's
// SECRET_KEY encrypts its authentications table) cannot be regenerated behind an
// app's back: the app would come up with a key that cannot read its own rows.
// So a key already present with a non-empty value is left exactly as it is —
// including one an operator typed in themselves, which is the supported way to
// pin a value. Rotation is an explicit act, not a side effect of a restart.
//
// The bcrypt spec's argument is a template, rendered strictly before hashing:
// bcrypt of an unresolved ${VAR} is a hash of the literal string, which would
// authenticate nobody and report nothing.
//
// A spec may name seeds before its generator — `${APP_DEFAULT_PASSWORD}|alnum:32`
// (see secretgen.ParseSpec). A seed is looked up in prior, the app's .env as it
// stood BEFORE this operation brought it in line with .env.app, and the first
// non-empty one becomes the secret instead of a generated value. That is what
// moves an installed app off a shared password without changing it: its database
// was initialised with the old value, and an update keeps it. A fresh install has
// no prior .env, so it gets the generator — even though Sync has by now written
// APP_DEFAULT_PASSWORD into its .env, which is exactly why the current file is not
// what a seed reads. A seed with no value is not an error; that is the fresh case.
func EnsureSecrets(cfg config.Config, project, dir string, secrets xcomposeapp.StringMap, prior, captures map[string]string) error {
	if len(secrets) == 0 {
		return nil
	}
	envPath := filepath.Join(dir, ".env")
	envFile := readEnvFile(dir)
	have := envinject.EnvFileVars(envFile)

	// A bcrypt spec hashes a template, and the natural thing for it to hash is a
	// sibling — `ADMIN_HASH: bcrypt:${ADMIN_PASSWORD}` beside `ADMIN_PASSWORD:
	// password:24`. So the hashes go last, and what this pass has already
	// produced overlays what they render against; on a fresh install the
	// sibling is not in the .env yet.
	keys := sortedKeys(secrets)
	sort.SliceStable(keys, func(i, j int) bool {
		return !isBcrypt(secrets[keys[i]]) && isBcrypt(secrets[keys[j]])
	})

	out := map[string]string{}
	overlay := map[string]string{}
	for k, v := range captures {
		overlay[k] = v
	}
next:
	for _, key := range keys {
		if have[key] != "" {
			continue // already generated (or pinned by the operator)
		}
		seeds, gen := secretgen.ParseSpec(secrets[key])
		for _, s := range seeds {
			if v := prior[s]; v != "" {
				out[key], overlay[key] = v, v
				continue next
			}
		}
		spec, err := envinject.RenderStrict(gen, cfg, project, envFile, overlay)
		if err != nil {
			return fmt.Errorf("secret %s: %w", key, err)
		}
		value, err := secretgen.Generate(spec)
		if err != nil {
			// Deliberately reports the spec, never the value.
			return fmt.Errorf("secret %s: %w", key, err)
		}
		out[key], overlay[key] = value, value
	}
	if len(out) == 0 {
		return nil
	}
	if err := envinject.EnsureVars(envPath, out); err != nil {
		return fmt.Errorf("write secrets to .env: %w", err)
	}
	return nil
}

// EnsureVariables renders each declared variable and refreshes it in the app's
// .env on every up.
//
// Unlike a secret, a variable is derived rather than remembered: it is a
// template over what the deployment currently is, so re-rendering is the point.
// It follows the same key-by-key path .env.app does (envinject.EnsureVars), so
// an unrelated line an operator added to the file is untouched.
func EnsureVariables(cfg config.Config, project, dir string, vars xcomposeapp.StringMap, captures map[string]string) error {
	if len(vars) == 0 {
		return nil
	}
	envFile := readEnvFile(dir)

	out := map[string]string{}
	for _, key := range sortedKeys(vars) {
		value, err := envinject.RenderStrict(vars[key], cfg, project, envFile, captures)
		if err != nil {
			return fmt.Errorf("variable %s: %w", key, err)
		}
		out[key] = value
	}
	if err := envinject.EnsureVars(filepath.Join(dir, ".env"), out); err != nil {
		return fmt.Errorf("write variables to .env: %w", err)
	}
	return nil
}

// sortedKeys gives a map a stable order, so a fresh .env is deterministic and a
// failure reports the same key every time rather than whichever Go's map order
// reached first.
func sortedKeys(m xcomposeapp.StringMap) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// isBcrypt reports whether a raw spec hashes a template (see EnsureSecrets).
func isBcrypt(raw string) bool {
	_, gen := secretgen.ParseSpec(raw)
	return strings.HasPrefix(strings.ToLower(gen), "bcrypt:")
}
