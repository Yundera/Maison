package xcomposeapp

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func parseBlock(t *testing.T, src string) *App {
	t.Helper()
	var ext map[string]any
	if err := yaml.Unmarshal([]byte(src), &ext); err != nil {
		t.Fatal(err)
	}
	a, err := Parse(ext)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return a
}

func TestLocalizedFieldsAcceptOnlyMapsWithEnUS(t *testing.T) {
	cases := []struct {
		name, src string
		ok        bool
	}{
		{"map with en_us", "title: {en_us: Jellyfin, fr_fr: Jellyfin}", true},
		{"plain string", "title: Jellyfin", false},
		{"block scalar", "title: |\n  Jellyfin", false},
		{"map without en_us", "title: {fr_fr: Jellyfin}", false},
		{"empty en_us", "title: {en_us: '  '}", false},
		{"nested value", "title: {en_us: {x: y}}", false},
		{"list", "title: [Jellyfin]", false},
		{"absent", "icon: icon.png", true},
		{"null", "title:", true},
	}
	for _, c := range cases {
		a := parseBlock(t, c.src)
		if got := len(a.Problems) == 0; got != c.ok {
			t.Errorf("%s: problems = %v, want ok=%v", c.name, a.Problems, c.ok)
		}
		if !c.ok && a.Title != nil {
			t.Errorf("%s: refused title kept as %v", c.name, a.Title)
		}
	}
}

// A refused string costs that string, never the rest of the block: Parse's error
// is discarded by every caller and would take folders, secrets and hooks with it.
func TestRefusedStringKeepsTheRestOfTheBlock(t *testing.T) {
	a := parseBlock(t, `
title: Plain
tagline: {en_us: Fine}
tips: plain tips
webui-host: app-${domain}
folders: [/DATA/AppData/app/config]
secrets: {KEY: hex:32}
hooks: {pre_up: "echo hi"}
credentials:
  - label: Admin
    username: admin
    password: ${KEY}
    note: {en_us: Keep it safe.}
links:
  - name: Docs
    url: https://docs.example
`)
	want := []string{"title", "tips", "credentials[0].label", "links[0].name"}
	if len(a.Problems) != len(want) {
		t.Fatalf("problems = %v, want one each for %v", a.Problems, want)
	}
	for i, p := range want {
		if !strings.HasPrefix(a.Problems[i], p+": ") {
			t.Errorf("problem %d = %q, want it about %s", i, a.Problems[i], p)
		}
	}
	if a.Title != nil || a.Tips != nil || a.Tagline.Value() != "Fine" {
		t.Errorf("title=%v tips=%v tagline=%v", a.Title, a.Tips, a.Tagline)
	}
	if a.WebUIHost == "" || len(a.Folders) != 1 || a.Secrets["KEY"] == "" || a.Hooks.PreUp == "" {
		t.Errorf("the rest of the block was lost: %+v", a)
	}
	if len(a.Credentials) != 1 || a.Credentials[0].Label != nil || a.Credentials[0].Username != "admin" ||
		a.Credentials[0].Note.Value() != "Keep it safe." {
		t.Errorf("credential = %+v", a.Credentials)
	}
	if len(a.Links) != 1 || a.Links[0].Name != nil || a.Links[0].URL == "" {
		t.Errorf("links = %+v", a.Links)
	}
}

func TestParseDoesNotMutateItsInput(t *testing.T) {
	var ext map[string]any
	if err := yaml.Unmarshal([]byte("title: Plain\ncredentials: [{label: Admin, username: a}]"), &ext); err != nil {
		t.Fatal(err)
	}
	before, _ := yaml.Marshal(ext)
	if _, err := Parse(ext); err != nil {
		t.Fatal(err)
	}
	after, _ := yaml.Marshal(ext)
	if string(before) != string(after) {
		t.Fatalf("Parse modified its input:\n%s\n->\n%s", before, after)
	}
}

func TestLocalizedIn(t *testing.T) {
	l := Localized{"en_us": "Hello", "fr_fr": "Bonjour"}
	if l.In("fr_fr") != "Bonjour" || l.In("de_de") != "Hello" || l.Value() != "Hello" {
		t.Fatalf("In = %q / %q / %q", l.In("fr_fr"), l.In("de_de"), l.Value())
	}
	if (Localized(nil)).In("fr_fr") != "" {
		t.Fatal("nil map must read as empty")
	}
	got := l.Map(func(s string) string {
		if s == "Bonjour" {
			return ""
		}
		return strings.ToUpper(s)
	})
	if !reflect.DeepEqual(got, Localized{"en_us": "HELLO"}) {
		t.Fatalf("Map = %v", got)
	}
}
