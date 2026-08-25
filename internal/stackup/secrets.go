package stackup

import (
	"fmt"
	"path/filepath"
	"sort"

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
func EnsureSecrets(cfg config.Config, project, dir string, secrets xcomposeapp.StringMap, captures map[string]string) error {
	if len(secrets) == 0 {
		return nil
	}
	envPath := filepath.Join(dir, ".env")
	envFile := readEnvFile(dir)
	have := envinject.EnvFileVars(envFile)

	out := map[string]string{}
	for _, key := range sortedKeys(secrets) {
		if have[key] != "" {
			continue // already generated (or pinned by the operator)
		}
		spec, err := envinject.RenderStrict(secrets[key], cfg, project, envFile, captures)
		if err != nil {
			return fmt.Errorf("secret %s: %w", key, err)
		}
		value, err := secretgen.Generate(spec)
		if err != nil {
			// Deliberately reports the spec, never the value.
			return fmt.Errorf("secret %s: %w", key, err)
		}
		out[key] = value
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
