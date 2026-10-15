package secretgen

import (
	"slices"
	"strings"
	"testing"
)

func TestPolicyValidate(t *testing.T) {
	p := Policy{MinLength: 8, MaxLength: 16, Require: []string{ClassLower, ClassUpper, ClassDigit}}
	cases := []struct {
		value string
		want  []string
	}{
		{"Abcdef12", nil},
		{"Ab1", []string{RuleTooShort}},
		{"Abcdefgh12345678x", []string{RuleTooLong}},
		{"abcdefgh", []string{RuleUpper, RuleDigit}},
		{"Abcd 1234", []string{RuleCharset}},
		{"Abcdé1234", []string{RuleCharset}},
		{"Abcd$1234", []string{RuleForbidden}},
		{`Abcd"1234`, []string{RuleForbidden}},
		{"Abcd#1234", []string{RuleForbidden}},
		{"Abcd!1234", nil},
	}
	for _, c := range cases {
		if got := p.Validate(c.value); !slices.Equal(got, c.want) {
			t.Errorf("Validate(%q) = %v, want %v", c.value, got, c.want)
		}
	}
}

func TestPolicyAlnumAndForbid(t *testing.T) {
	p := Policy{Charset: CharsetAlnum, Forbid: "0O"}
	if got := p.Validate("abcd!efgh"); !slices.Equal(got, []string{RuleCharset}) {
		t.Errorf("alnum: %v", got)
	}
	if got := p.Validate("abcdOefgh"); !slices.Equal(got, []string{RuleForbidden}) {
		t.Errorf("forbid: %v", got)
	}
}

func TestPolicyNormalize(t *testing.T) {
	p := Policy{Charset: "ALNUM", Require: []string{"symbol", "Digit", "digit", "bogus"}}.Normalize()
	if p.MinLength != 8 || p.MaxLength != 128 || p.Charset != CharsetAlnum {
		t.Fatalf("defaults: %+v", p)
	}
	if !slices.Equal(p.Require, []string{ClassDigit}) {
		t.Fatalf("require = %v, want symbol dropped under alnum and digit deduped", p.Require)
	}
}

func TestGenerateForAlwaysValidates(t *testing.T) {
	policies := []Policy{
		{},
		{MinLength: 12, MaxLength: 12, Require: []string{ClassLower, ClassUpper, ClassDigit, ClassSymbol}},
		{Charset: CharsetAlnum, Require: []string{ClassDigit}, Forbid: "0Ol1I"},
		{MinLength: 30, MaxLength: 40},
	}
	for _, p := range policies {
		for i := 0; i < 300; i++ {
			v, err := GenerateFor(p, "password:24")
			if err != nil {
				t.Fatal(err)
			}
			if broken := p.Validate(v); len(broken) > 0 {
				t.Fatalf("policy %+v: generated %q breaks %v", p, v, broken)
			}
			if strings.ContainsAny(v, Builtin) {
				t.Fatalf("generated %q holds a built-in forbidden character", v)
			}
		}
	}
}

func TestGenerateForKeepsSecretLength(t *testing.T) {
	v, err := GenerateFor(Policy{}, "password:24")
	if err != nil || len(v) != 24 {
		t.Fatalf("got %q, %v; want 24 characters", v, err)
	}
	v, _ = GenerateFor(Policy{MaxLength: 16}, "password:24")
	if len(v) != 16 {
		t.Fatalf("got %d characters, want clamped to 16", len(v))
	}
	v, _ = GenerateFor(Policy{}, "hex:32")
	if len(v) != defaultGenLength {
		t.Fatalf("got %d characters, want the default for a non-password generator", len(v))
	}
}
