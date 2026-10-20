package xcomposeapp

import (
	"fmt"
	"strings"
)

// Every string x-compose-app shows to a person is a locale map, and the map must
// carry a non-empty en_us — the fallback for every UI language, and the one value
// server-side text (logs, mail, incidents) uses. A plain string is refused: it
// would leave the maintainer unsure whether the field is translated at all, and
// a map without en_us leaves some reader with nothing to show.
//
// A refused field is DROPPED, never fatal. Parse's error is discarded by every
// caller and costs the app its whole block — folders, secrets, hooks and all — so
// a title typo must cost the title and nothing else. The refusal is recorded on
// App.Problems for the catalog log and `maison lint`.

// localizedTop are the top-level user-facing fields.
var localizedTop = []string{"title", "tagline", "description", "tips"}

// localizedLists are the user-facing fields of each entry of a list field, in a
// fixed order so problems come out the same way every time.
var localizedLists = []struct {
	list string
	keys []string
}{
	{"credentials", []string{"label", "note"}},
	{"links", []string{"name"}},
}

// localizedProblem is the one refusal message, so `maison lint` and the logs say
// the same thing for every field.
func localizedProblem(path string) string {
	return fmt.Sprintf("%s: must be a locale map with a non-empty en_us entry, e.g. %s: {en_us: …}", path, path[strings.LastIndex(path, ".")+1:])
}

// validLocalized reports whether v is a locale map of strings with a non-empty
// en_us.
func validLocalized(v any) bool {
	// A translated x-casaos block (xcasaos.ComposeApp) builds its maps typed.
	if m, ok := v.(map[string]string); ok {
		return strings.TrimSpace(m["en_us"]) != ""
	}
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	for _, e := range m {
		if _, ok := e.(string); !ok {
			return false
		}
	}
	en, _ := m["en_us"].(string)
	return strings.TrimSpace(en) != ""
}

// checkLocalized returns ext with every invalid localized field removed, and one
// problem per removal. ext itself is never modified: it may be a store's parsed
// compose, shared with other readers.
func checkLocalized(ext map[string]any) (map[string]any, []string) {
	var problems []string
	var out map[string]any
	own := func() {
		if out == nil {
			out = make(map[string]any, len(ext))
			for k, v := range ext {
				out[k] = v
			}
		}
	}
	for _, k := range localizedTop {
		if v := ext[k]; v != nil && !validLocalized(v) {
			own()
			delete(out, k)
			problems = append(problems, localizedProblem(k))
		}
	}
	for _, l := range localizedLists {
		list := l.list
		entries, ok := ext[list].([]any)
		if !ok {
			continue
		}
		var copied []any
		for i, e := range entries {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			var entry map[string]any
			for _, k := range l.keys {
				if v := m[k]; v != nil && !validLocalized(v) {
					if entry == nil {
						entry = make(map[string]any, len(m))
						for mk, mv := range m {
							entry[mk] = mv
						}
					}
					delete(entry, k)
					problems = append(problems, localizedProblem(fmt.Sprintf("%s[%d].%s", list, i, k)))
				}
			}
			if entry != nil {
				if copied == nil {
					copied = append([]any(nil), entries...)
				}
				copied[i] = entry
			}
		}
		if copied != nil {
			own()
			out[list] = copied
		}
	}
	if out == nil {
		return ext, problems
	}
	return out, problems
}
