package asset

import "testing"

// The rule the whole feature rests on: what counts as a file beside the compose,
// and what does not. The rejections matter more than the acceptances — a value
// this lets through is joined onto a directory and read.
func TestRel(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want string // "" means: not a compose-relative asset
	}{
		// The ordinary spellings.
		{"icon.png", "icon.png"},
		{"./icon.png", "icon.png"},
		{"assets/screenshot-1.png", "assets/screenshot-1.png"},
		{"./assets/../icon.svg", "icon.svg"},
		{"  icon.png  ", "icon.png"},
		{"icon.PNG", "icon.PNG"}, // the name keeps its case; only the extension check folds it

		// URLs stay URLs — every store declares them today.
		{"https://cdn.example/gh/Store@main/Apps/Demo/icon.png", ""},
		{"http://example.test/icon.png", ""},
		{"//example.test/icon.png", ""}, // scheme-relative: still not a file

		// Escapes. A store's compose is not trusted with a path.
		{"../icon.png", ""},
		{"../../etc/shadow.png", ""},
		{"file.png/../../secret.png", ""},
		{"/etc/hosts.png", ""},
		{`assets\icon.png`, ""},

		// Not an image as far as this package is concerned.
		{"icon.txt", ""},
		{"icon", ""},
		{"icon.png?v=2", ""},
		{"", ""},
		{"   ", ""},
		{".", ""},
		{"..", ""},
	} {
		got, ok := Rel(c.raw)
		if c.want == "" {
			if ok {
				t.Errorf("Rel(%q) = %q, true; want it rejected", c.raw, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("Rel(%q) = %q, %v; want %q, true", c.raw, got, ok, c.want)
		}
	}
}

func TestIsURL(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want bool
	}{
		{"https://example.test/icon.png", true},
		{"http://example.test/icon.png", true},
		{"ftp://example.test/icon.png", true},
		{"//example.test/icon.png", false}, // no scheme, so not one this decides for
		{"icon.png", false},
		{"/api/store/demo/asset/icon.png", false},
		{"", false},
	} {
		if got := IsURL(c.raw); got != c.want {
			t.Errorf("IsURL(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

func TestExt(t *testing.T) {
	for _, c := range []struct{ name, want string }{
		{"icon.png", ".png"},
		{"ICON.PNG", ".png"},
		{"a/b/logo.svg", ".svg"},
		{"icon.jpeg", ".jpeg"},
		{"icon.bmp", ""},
		{"icon", ""},
	} {
		if got := Ext(c.name); got != c.want {
			t.Errorf("Ext(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}
