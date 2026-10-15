package secretgen

import (
	"crypto/rand"
	"math/big"
	"strings"
	"unicode/utf8"
)

// Policy constrains a credential value an operator types or regenerates. It is
// the app's declaration (x-compose-app credentials[].change.policy) with the
// defaults filled in; Builtin applies on top of whatever the app says.
type Policy struct {
	MinLength int
	MaxLength int
	Charset   string   // CharsetPrintable or CharsetAlnum
	Require   []string // any of ClassLower, ClassUpper, ClassDigit, ClassSymbol
	Forbid    string   // characters refused on top of Builtin
}

// Character sets a policy may name.
const (
	CharsetPrintable = "printable"
	CharsetAlnum     = "alnum"
)

// Character classes a policy may require.
const (
	ClassLower  = "lower"
	ClassUpper  = "upper"
	ClassDigit  = "digit"
	ClassSymbol = "symbol"
)

// Rule ids a value can break. They are what the API returns and what the UI
// translates, so they are part of the contract.
const (
	RuleTooShort  = "too_short"
	RuleTooLong   = "too_long"
	RuleCharset   = "charset"
	RuleForbidden = "forbidden"
	RuleLower     = "require_lower"
	RuleUpper     = "require_upper"
	RuleDigit     = "require_digit"
	RuleSymbol    = "require_symbol"
)

// Builtin is refused in every credential, whatever the app's policy. The value
// is written raw into the app's .env and from there read by `docker compose`
// interpolation (`$`), its quote handling (`'`, `"`, `\`) and comment syntax
// (`#`), and by seed templates. Escaping each of those correctly end to end is
// a larger promise than forbidding six characters a password does not need.
// Whitespace, control characters and anything outside printable ASCII are
// refused by the printable check itself.
const Builtin = "$'\"\\`#"

// symbols is what a generated value draws its symbols from: printable, outside
// Builtin, and harmless in a URL userinfo, a YAML scalar and a shell word.
const symbols = "-_.!@%+=,:"

const (
	defaultMinLength = 8
	defaultMaxLength = 128
	defaultGenLength = 20
)

// Normalize fills a policy's defaults and drops what it cannot honour, so the
// UI and the validator read the same rules.
func (p Policy) Normalize() Policy {
	if p.MinLength <= 0 {
		p.MinLength = defaultMinLength
	}
	if p.MaxLength <= 0 || p.MaxLength > maxSize {
		p.MaxLength = defaultMaxLength
	}
	if p.MaxLength < p.MinLength {
		p.MaxLength = p.MinLength
	}
	p.Charset = strings.ToLower(strings.TrimSpace(p.Charset))
	if p.Charset != CharsetAlnum {
		p.Charset = CharsetPrintable
	}
	var req []string
	seen := map[string]bool{}
	for _, c := range p.Require {
		c = strings.ToLower(strings.TrimSpace(c))
		switch c {
		case ClassLower, ClassUpper, ClassDigit:
		case ClassSymbol:
			if p.Charset == CharsetAlnum {
				continue // unsatisfiable; the charset wins
			}
		default:
			continue
		}
		if !seen[c] {
			seen[c] = true
			req = append(req, c)
		}
	}
	p.Require = req
	return p
}

// Validate returns the rules v breaks, empty when it is acceptable. It never
// echoes v: the result goes to an API response.
func (p Policy) Validate(v string) []string {
	p = p.Normalize()
	var broken []string
	n := utf8.RuneCountInString(v)
	if n < p.MinLength {
		broken = append(broken, RuleTooShort)
	}
	if n > p.MaxLength {
		broken = append(broken, RuleTooLong)
	}
	var lower, upper, digit, symbol, badCharset, forbidden bool
	for _, r := range v {
		switch {
		case r < 0x21 || r > 0x7e:
			badCharset = true
		case strings.ContainsRune(Builtin, r) || strings.ContainsRune(p.Forbid, r):
			forbidden = true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= '0' && r <= '9':
			digit = true
		default:
			symbol = true
			if p.Charset == CharsetAlnum {
				badCharset = true
			}
		}
	}
	if badCharset {
		broken = append(broken, RuleCharset)
	}
	if forbidden {
		broken = append(broken, RuleForbidden)
	}
	has := map[string]bool{ClassLower: lower, ClassUpper: upper, ClassDigit: digit, ClassSymbol: symbol}
	rule := map[string]string{ClassLower: RuleLower, ClassUpper: RuleUpper, ClassDigit: RuleDigit, ClassSymbol: RuleSymbol}
	for _, c := range p.Require {
		if !has[c] {
			broken = append(broken, rule[c])
		}
	}
	return broken
}

// GenerateFor produces a value that satisfies p. genSpec is the secret's own
// generator (`password:24`); its length is kept when the policy allows it, so a
// regenerated password looks like the one the app was installed with.
func GenerateFor(p Policy, genSpec string) (string, error) {
	p = p.Normalize()
	n := defaultGenLength
	kind, arg, ok := strings.Cut(strings.TrimSpace(genSpec), ":")
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "password", "alnum":
		if ok {
			if m, err := size(genSpec, arg, true); err == nil {
				n = m
			}
		}
	}
	if n < p.MinLength {
		n = p.MinLength
	}
	if n > p.MaxLength {
		n = p.MaxLength
	}
	if n < len(p.Require) {
		n = len(p.Require) // MaxLength may then be exceeded; Validate will say so
	}

	pool := filterOut(alphabet, p.Forbid)
	if containsString(p.Require, ClassSymbol) {
		pool += filterOut(symbols, p.Forbid)
	}
	classes := map[string]string{
		ClassLower:  filterOut("abcdefghijklmnopqrstuvwxyz", p.Forbid),
		ClassUpper:  filterOut("ABCDEFGHIJKLMNOPQRSTUVWXYZ", p.Forbid),
		ClassDigit:  filterOut("0123456789", p.Forbid),
		ClassSymbol: filterOut(symbols, p.Forbid),
	}
	if pool == "" {
		return "", errUnsatisfiable
	}

	out := make([]byte, 0, n)
	for _, c := range p.Require {
		set := classes[c]
		if set == "" {
			return "", errUnsatisfiable
		}
		ch, err := pick(set)
		if err != nil {
			return "", err
		}
		out = append(out, ch)
	}
	for len(out) < n {
		ch, err := pick(pool)
		if err != nil {
			return "", err
		}
		out = append(out, ch)
	}
	// Shuffle, so the required classes are not always the leading characters.
	for i := len(out) - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return "", err
		}
		out[i], out[j.Int64()] = out[j.Int64()], out[i]
	}
	return string(out), nil
}

type policyError string

func (e policyError) Error() string { return string(e) }

const errUnsatisfiable = policyError("the password policy cannot be satisfied by a generated value")

func pick(set string) (byte, error) {
	i, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
	if err != nil {
		return 0, err
	}
	return set[i.Int64()], nil
}

func filterOut(set, forbid string) string {
	if forbid == "" {
		return set
	}
	var b strings.Builder
	for _, r := range set {
		if !strings.ContainsRune(forbid, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
