package envinject

import (
	"fmt"
	"regexp"
	"strings"
)

// Var is one KEY=VALUE entry of an app's .env, in file order. EnvFileVars gives
// the same data as an unordered map (enough for ${VAR} expansion); Var keeps the
// order the operator sees when editing the file.
type Var struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// keyRe is what `docker compose` accepts as an .env key, and what the editor is
// allowed to write: a shell-style identifier.
var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ParseEnvFile reads the KEY=VALUE lines of an app's .env in file order. Blanks
// and # comments are skipped (PatchEnvFile preserves them on write); a duplicate
// key keeps its first position and takes the last value, which is what compose
// itself resolves to.
func ParseEnvFile(b []byte) []Var {
	var out []Var
	at := map[string]int{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := cutEnvLine(line)
		if !ok {
			continue
		}
		if i, dup := at[k]; dup {
			out[i].Value = v
			continue
		}
		at[k] = len(out)
		out = append(out, Var{Key: k, Value: v})
	}
	return out
}

// PatchEnvFile rewrites an app's .env to hold exactly vars, editing the existing
// text in place rather than regenerating it: a kept key stays on its own line
// (so the comments and blank lines around it keep their meaning), a dropped key
// loses its line, and a new key is appended. Regenerating the file instead would
// silently discard whatever the operator wrote into it by hand.
//
// Values are written verbatim — .env has no escaping, so a value is whatever
// follows the first '=' up to the end of the line. That is also why a value may
// not contain a newline (it would parse back as a second, bogus entry).
func PatchEnvFile(old []byte, vars []Var) ([]byte, error) {
	if err := ValidateVars(vars); err != nil {
		return nil, err
	}

	want := make(map[string]string, len(vars))
	order := make([]string, 0, len(vars))
	for _, v := range vars {
		want[v.Key] = v.Value
		order = append(order, v.Key)
	}

	var out []string
	written := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(string(old), "\n"), "\n") {
		k, _, ok := cutEnvLine(line)
		if !ok {
			out = append(out, line) // blank or comment — carried through untouched
			continue
		}
		v, keep := want[k]
		if !keep || written[k] {
			continue // removed by the editor, or a duplicate of a line we already wrote
		}
		written[k] = true
		out = append(out, k+"="+v)
	}
	for _, k := range order { // keys the editor added
		if !written[k] {
			out = append(out, k+"="+want[k])
		}
	}

	// Trailing blank lines are an artifact of deleting the last entries, not
	// operator intent; drop them so the file doesn't grow a gap on every save.
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return nil, nil
	}
	return []byte(strings.Join(out, "\n") + "\n"), nil
}

// ValidateVars rejects entries `docker compose` could not read back: a key that
// isn't a shell identifier, a duplicate key (one of the two values would be
// silently dropped), or a value spanning multiple lines.
func ValidateVars(vars []Var) error {
	seen := map[string]bool{}
	for _, v := range vars {
		if !keyRe.MatchString(v.Key) {
			return fmt.Errorf("invalid variable name %q: use letters, digits and _ (not starting with a digit)", v.Key)
		}
		if seen[v.Key] {
			return fmt.Errorf("duplicate variable %q", v.Key)
		}
		seen[v.Key] = true
		if strings.ContainsAny(v.Value, "\r\n") {
			return fmt.Errorf("value of %q must be a single line", v.Key)
		}
	}
	return nil
}

// ValidateEnvFile rejects .env text Maison would read back differently from how
// it was written. It backs the raw editors, where the operator types the file
// itself rather than filling in fields.
//
// It is deliberately stricter than ParseEnvFile. Parsing is forgiving — it skips a
// line it cannot read and takes the last of a duplicated key — which is right when
// reading a file someone else wrote, and wrong when saving one someone is writing
// now: a typo that silently does nothing is worse than a rejected save. Errors
// carry a line number because that is what the editor can point at.
func ValidateEnvFile(raw []byte) error {
	seen := map[string]bool{}
	for i, line := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		k, _, ok := cutEnvLine(line)
		if !ok {
			return fmt.Errorf("line %d: %q is not KEY=VALUE — prefix it with # if it is a note", i+1, t)
		}
		if !keyRe.MatchString(k) {
			return fmt.Errorf("line %d: invalid variable name %q: use letters, digits and _ (not starting with a digit)", i+1, k)
		}
		if seen[k] {
			return fmt.Errorf("line %d: duplicate variable %q", i+1, k)
		}
		seen[k] = true
	}
	return nil
}

// cutEnvLine splits one .env line into key and value, reporting whether it holds
// an entry at all (blanks and # comments do not).
func cutEnvLine(line string) (key, value string, ok bool) {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") {
		return "", "", false
	}
	k, v, ok := strings.Cut(t, "=")
	if !ok {
		return "", "", false
	}
	return strings.TrimSpace(k), strings.TrimSpace(v), true
}
