package xcomposeapp

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// What the user must bring to an app, as opposed to what the box must have
// (requirements.go). Four independent fields, all optional:
//
//   - cost: whether the app (or what it is useless without) has to be paid for.
//   - accounts: third-party accounts the user must hold — plex.tv, foundryvtt.com.
//   - inputs: values only the user can provide, asked at install and, while a
//     required one is still empty, by a "Setup needed" form instead of the app.
//   - audience: technical apps (root shells, host-level tools, agent tooling) are
//     hidden in the store unless the operator asked to see them.
//
// cost and accounts are information only: they never hide an app or gate an
// install. Like every other declaration, a bad value is dropped and recorded on
// App.Problems, never fatal.

// Cost values.
const (
	CostFree     = "free"
	CostFreemium = "freemium"
	CostPaid     = "paid"
)

// Audience values.
const (
	AudienceEveryone  = "everyone"
	AudienceTechnical = "technical"
)

// Account is a third-party account the user must hold for the app to be usable.
type Account struct {
	// Provider names it the way a user would recognise it: "plex.tv".
	Provider string `yaml:"provider" json:"provider"`
	// URL is where to get one; absolute http(s), optional.
	URL string `yaml:"url,omitempty" json:"url,omitempty"`
}

// Input is one value only the user can provide, written into the app's .env
// under Env. A Secret is masked wherever it is shown. A Required input left
// empty marks the installed app as needing setup.
type Input struct {
	Env         string    `yaml:"env" json:"env"`
	Label       Localized `yaml:"label,omitempty" json:"label,omitempty"`
	Description Localized `yaml:"description,omitempty" json:"description,omitempty"`
	Secret      bool      `yaml:"secret,omitempty" json:"secret,omitempty"`
	Required    bool      `yaml:"required,omitempty" json:"required,omitempty"`
}

// Technical reports whether the app declares itself for technical users.
func (a *App) Technical() bool { return a != nil && a.Audience == AudienceTechnical }

// Input returns the declared input for key, if any.
func (a *App) Input(key string) (Input, bool) {
	if a != nil {
		for _, in := range a.Inputs {
			if in.Env == key {
				return in, true
			}
		}
	}
	return Input{}, false
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// checkDisclosure returns ext with cost, accounts, inputs and audience reduced
// to what this build understands, and one problem per value it had to drop.
// ext itself is never modified.
func checkDisclosure(ext map[string]any) (map[string]any, []string) {
	var problems []string
	out, owned := ext, false
	own := func() {
		if !owned {
			owned = true
			out = make(map[string]any, len(ext))
			for k, v := range ext {
				out[k] = v
			}
		}
	}
	oneOf := func(key string, allowed ...string) {
		v, ok := ext[key]
		if !ok || v == nil {
			return
		}
		s, _ := v.(string)
		for _, a := range allowed {
			if s == a {
				return
			}
		}
		problems = append(problems, fmt.Sprintf("%s: unknown value %v (known: %s)", key, v, strings.Join(allowed, ", ")))
		own()
		delete(out, key)
	}
	oneOf("cost", CostFree, CostFreemium, CostPaid)
	oneOf("audience", AudienceEveryone, AudienceTechnical)

	if v, ok := ext["accounts"]; ok && v != nil {
		list, isList := v.([]any)
		if !isList {
			problems = append(problems, "accounts: must be a list, e.g. accounts: [{provider: plex.tv}]")
		}
		var kept []any
		for i, e := range list {
			m, _ := e.(map[string]any)
			provider, _ := m["provider"].(string)
			if strings.TrimSpace(provider) == "" {
				problems = append(problems, fmt.Sprintf("accounts[%d]: provider is required", i))
				continue
			}
			if raw, ok := m["url"]; ok && raw != nil {
				s, _ := raw.(string)
				if u, err := url.Parse(s); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
					problems = append(problems, fmt.Sprintf("accounts[%d].url: must be an absolute http(s) URL", i))
					m = map[string]any{"provider": provider}
				}
			}
			kept = append(kept, m)
		}
		own()
		if len(kept) == 0 {
			delete(out, "accounts")
		} else {
			out["accounts"] = kept
		}
	}

	if v, ok := ext["inputs"]; ok && v != nil {
		list, isList := v.([]any)
		if !isList {
			problems = append(problems, "inputs: must be a list, e.g. inputs: [{env: API_KEY, label: {en_us: …}}]")
		}
		var kept []any
		seen := map[string]bool{}
		for i, e := range list {
			m, _ := e.(map[string]any)
			key, _ := m["env"].(string)
			switch {
			case !envKeyRe.MatchString(key):
				problems = append(problems, fmt.Sprintf("inputs[%d].env: %q is not a variable name (letters, digits and _, not starting with a digit)", i, key))
				continue
			case seen[key]:
				problems = append(problems, fmt.Sprintf("inputs[%d].env: %s is declared twice", i, key))
				continue
			}
			seen[key] = true
			if m["label"] == nil {
				// Kept: the form falls back to the variable name. Losing the
				// prompt over a missing label would be worse than an ugly one.
				problems = append(problems, fmt.Sprintf("inputs[%d].label: required, e.g. label: {en_us: …}", i))
			}
			kept = append(kept, m)
		}
		own()
		if len(kept) == 0 {
			delete(out, "inputs")
		} else {
			out["inputs"] = kept
		}
	}
	return out, problems
}
