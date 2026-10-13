package secretgen

import (
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestGenerateShapes(t *testing.T) {
	cases := []struct {
		spec  string
		check func(t *testing.T, got string)
	}{
		// hex:32 is the one that matters most: it is `openssl rand -hex 32`, the
		// line every migrated app copied, and it must stay 64 characters.
		{"hex:32", func(t *testing.T, got string) {
			if len(got) != 64 {
				t.Fatalf("len = %d, want 64", len(got))
			}
			if _, err := hex.DecodeString(got); err != nil {
				t.Fatalf("not hex: %v", err)
			}
		}},
		{"hex:1", func(t *testing.T, got string) {
			if len(got) != 2 {
				t.Fatalf("len = %d, want 2", len(got))
			}
		}},
		{"base64:32", func(t *testing.T, got string) {
			b, err := base64.StdEncoding.DecodeString(got)
			if err != nil {
				t.Fatalf("not base64: %v", err)
			}
			if len(b) != 32 {
				t.Fatalf("decoded %d bytes, want 32", len(b))
			}
		}},
		{"alnum:24", func(t *testing.T, got string) {
			if len(got) != 24 {
				t.Fatalf("len = %d, want 24", len(got))
			}
			if strings.Trim(got, alphabet) != "" {
				t.Fatalf("outside the alphabet: %q", got)
			}
		}},
		{"password:16", func(t *testing.T, got string) {
			if len(got) != 16 {
				t.Fatalf("len = %d, want 16", len(got))
			}
		}},
		{"uuid", func(t *testing.T, got string) {
			re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
			if !re.MatchString(got) {
				t.Fatalf("not a v4 uuid: %q", got)
			}
		}},
		{"bcrypt:hunter2", func(t *testing.T, got string) {
			if err := bcrypt.CompareHashAndPassword([]byte(got), []byte("hunter2")); err != nil {
				t.Fatalf("hash does not verify: %v", err)
			}
		}},
		// Case and surrounding space are the store author's typing, not a
		// different generator.
		{" HEX:32 ", func(t *testing.T, got string) {
			if len(got) != 64 {
				t.Fatalf("len = %d, want 64", len(got))
			}
		}},
	}

	for _, c := range cases {
		t.Run(c.spec, func(t *testing.T) {
			got, err := Generate(c.spec)
			if err != nil {
				t.Fatalf("Generate(%q): %v", c.spec, err)
			}
			c.check(t, got)
		})
	}
}

// The whole point of the package is that a value is never empty and never
// repeats — the two ways the shell version failed.
func TestGenerateIsRandomAndNonEmpty(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		got, err := Generate("hex:16")
		if err != nil {
			t.Fatal(err)
		}
		if got == "" {
			t.Fatal("empty value")
		}
		if seen[got] {
			t.Fatalf("repeated value %q", got)
		}
		seen[got] = true
	}
}

func TestGenerateRejects(t *testing.T) {
	for _, spec := range []string{
		"",         // nothing at all
		"openssl",  // a command, not a generator
		"hex",      // no size
		"hex:",     // empty size
		"hex:abc",  // non-numeric size
		"hex:0",    // zero
		"hex:-1",   // negative
		"hex:4097", // over the cap
		"uuid:4",   // takes no argument
		"bcrypt",   // nothing to hash
		"bcrypt:",  // ...which is what an unresolved variable looks like
		"rsa:2048", // unknown kind
	} {
		if got, err := Generate(spec); err == nil {
			t.Errorf("Generate(%q) = %q, want an error", spec, got)
		}
	}
}

func TestParseSpec(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		seeds []string
		gen   string
	}{
		// The form an installed app is moved off APP_DEFAULT_PASSWORD with.
		{"${APP_DEFAULT_PASSWORD}|alnum:32", []string{"APP_DEFAULT_PASSWORD"}, "alnum:32"},
		{" ${A} | ${B} | hex:8 ", []string{"A", "B"}, "hex:8"},
		// Every spec written before the form is returned whole.
		{"hex:32", nil, "hex:32"},
		{"bcrypt:${APP_DEFAULT_PASSWORD}", nil, "bcrypt:${APP_DEFAULT_PASSWORD}"},
		// Not exactly ${NAME} before a `|`: no seeds, so Generate reports it.
		{"foo|alnum:8", nil, "foo|alnum:8"},
		{"$A|alnum:8", nil, "$A|alnum:8"},
		{"${A:-x}|alnum:8", nil, "${A:-x}|alnum:8"},
		{"bcrypt:${A}|alnum:8", nil, "bcrypt:${A}|alnum:8"},
		// A seed with no generator after it is the generator's error to report.
		{"${A}|", []string{"A"}, ""},
	} {
		seeds, gen := ParseSpec(tc.raw)
		if strings.Join(seeds, ",") != strings.Join(tc.seeds, ",") || gen != tc.gen {
			t.Errorf("ParseSpec(%q) = %q, %q; want %q, %q", tc.raw, seeds, gen, tc.seeds, tc.gen)
		}
	}
}
