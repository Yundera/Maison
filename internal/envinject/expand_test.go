package envinject

import (
	"testing"

	"github.com/yundera/maison/internal/config"
)

// testCfg is a deployment whose data root is not the host's, so a renderer that
// leaked one for the other would show up here.
func testCfg() config.Config {
	return config.Config{DataRoot: "/DATA", DataHostPath: "/opt/maison/DATA"}
}

func TestExpandSpeaksComposeDialect(t *testing.T) {
	vars := map[string]string{"SET": "value", "EMPTY": "", "DOMAIN": "example.test"}
	keep := func(name string) string { return "${" + name + "}" }

	cases := []struct{ in, want string }{
		{"${SET}", "value"},
		{"$SET", "value"},
		{"prefix-$SET-suffix", "prefix-value-suffix"},
		// The spelling the whole store is written in, and the one os.Expand read
		// as a variable literally named "APP_NET:-pcs".
		{"${APP_NET:-pcs}", "pcs"},
		{"${DATA_ROOT:-/DATA}/AppData", "/DATA/AppData"},
		{"${SET:-fallback}", "value"},
		// :- falls back on empty; plain - only on unset.
		{"${EMPTY:-fallback}", "fallback"},
		{"${EMPTY-fallback}", ""},
		{"${UNSET-fallback}", "fallback"},
		// A default may itself reference something.
		{"${UNSET:-${DOMAIN}}", "example.test"},
		// $$ is a literal dollar, and a lone $ before punctuation is text.
		{"$$HOME", "$HOME"},
		{"cost: $5", "cost: $5"},
		{"awk '{print $1}'", "awk '{print $1}'"},
		{"${UNRESOLVED}", "${UNRESOLVED}"},
		{"", ""},
		{"$", "$"},
		{"${unclosed", "${unclosed"},
	}
	for _, c := range cases {
		if got := expand(c.in, vars, keep); got != c.want {
			t.Errorf("expand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// RenderStrict must not report a reference that carries a default — the default
// IS the resolution.
func TestRenderStrictAcceptsDefaults(t *testing.T) {
	got, err := RenderStrict("net=${APP_NET:-pcs} root=${DATA_ROOT:-/DATA}", testCfg(), "app", nil, nil)
	if err != nil {
		t.Fatalf("RenderStrict: %v", err)
	}
	// DATA_ROOT is a base variable and resolves to the HOST spelling — an app's
	// compose reads it as a bind-mount source, which the daemon resolves.
	if got != "net=pcs root=/opt/maison/DATA" {
		t.Fatalf("got %q", got)
	}
	if _, err := RenderStrict("${NOPE_NO_DEFAULT}", testCfg(), "app", nil, nil); err == nil {
		t.Fatal("want an error for a reference with no value and no default")
	}
}
