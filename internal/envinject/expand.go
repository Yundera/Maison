package envinject

import "strings"

// expand resolves ${VAR} references in s the way `docker compose` does, which is
// the only dialect a store author is writing in.
//
// os.Expand — what this replaces — understands `$VAR` and `${VAR}` and nothing
// else, so `${APP_NET:-pcs}` reached it as a variable literally named
// "APP_NET:-pcs" and resolved to nothing. That spelling is not exotic: it is the
// store's own convention (`${DATA_ROOT:-/DATA}`, `${APP_NET:-pcs}`) precisely so
// a bare `docker compose up` works outside Maison, and it appears in most of the
// store's compose files.
//
// Supported:
//
//	$NAME              ${NAME}
//	${NAME:-default}   default when NAME is unset OR empty
//	${NAME-default}    default only when NAME is unset
//	$$                 a literal $
//
// A default may itself contain references, and is expanded in turn. missing is
// called for a reference that resolves to nothing and has no default; what it
// returns is substituted, which is how Render leaves `${NAME}` in place while
// RenderStrict records an error and substitutes "".
func expand(s string, vars map[string]string, missing func(name string) string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(s) {
			b.WriteByte('$')
			break
		}
		switch next := s[i+1]; {
		case next == '$':
			b.WriteByte('$')
			i += 2
		case next == '{':
			end := closingBrace(s, i+1)
			if end < 0 {
				// No closing brace: not a reference, so it is text.
				b.WriteByte('$')
				i++
				continue
			}
			b.WriteString(resolveRef(s[i+2:end], vars, missing))
			i = end + 1
		case isNameStart(next):
			j := i + 1
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			b.WriteString(resolveRef(s[i+1:j], vars, missing))
			i = j
		default:
			// `$` followed by anything else is a literal dollar — a price, a
			// regex, an awk field.
			b.WriteByte('$')
			i++
		}
	}
	return b.String()
}

// resolveRef resolves the inside of a reference: a name, optionally followed by
// a `-` or `:-` default.
func resolveRef(ref string, vars map[string]string, missing func(name string) string) string {
	name, def, hasDefault := ref, "", false
	emptyCounts := false
	if i := strings.IndexAny(ref, ":-"); i > 0 {
		switch {
		case strings.HasPrefix(ref[i:], ":-"):
			name, def, hasDefault, emptyCounts = ref[:i], ref[i+2:], true, true
		case ref[i] == '-':
			name, def, hasDefault = ref[:i], ref[i+1:], true
		}
	}

	v, ok := vars[name]
	if ok && (v != "" || !emptyCounts) {
		return v
	}
	if hasDefault {
		return expand(def, vars, missing) // a default may itself reference something
	}
	return missing(name)
}

// closingBrace finds the '}' that closes the '{' at open, counting nested
// references so a default can itself be one: ${A:-${B}} closes at the last brace,
// not the first.
func closingBrace(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}
