package appstore

import (
	"os"
	"path/filepath"
	"testing"
)

// The store reads an app's catalog entry from x-compose-app, Maison's own
// definition; x-casaos only for a compose that carries no x-compose-app, and never
// mixed with it. A compose with neither is not a store app.
func TestParseStoreReadsXComposeAppAndFallsBackWhole(t *testing.T) {
	root := t.TempDir()
	write := func(app, compose string) {
		dir := filepath.Join(root, "Apps", app)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("native", `services: {app: {image: n:1}}
x-compose-app:
  title: {en_us: Native, fr_fr: Natif}
  tagline: {en_us: One line}
  description: {en_us: Long}
  category: Media
  developer: Dev
  min_memory: 512
  parent: host
  icon: https://cdn.example/native.png
  screenshots: [https://cdn.example/1.png]
`)
	write("legacy", `services: {app: {image: l:1}}
x-casaos:
  title: {en_us: Legacy}
  tagline: {en_us: Old line}
  author: Someone
  min_memory: 256
  category: Tools
`)
	write("both", `services: {app: {image: b:1}}
x-casaos:
  title: {en_us: From CasaOS}
  category: Ignored
  developer: Ignored
x-compose-app:
  title: {en_us: Both}
`)
	write("bare", "services: {app: {image: x:1}}\n")

	apps, _, _ := parseStore(root, "https://store.example", "")
	got := map[string]*CatalogApp{}
	for _, a := range apps {
		got[a.ID] = a
	}
	if len(got) != 3 || got["bare"] != nil {
		t.Fatalf("catalog = %v, want native, legacy and both only", got)
	}
	n := got["native"]
	if n.Name != "Native" || n.Tagline.Value() != "One line" || n.Description.Value() != "Long" || n.Category != "Media" ||
		n.Developer != "Dev" || n.MinMemory != 512 || n.Parent != "host" ||
		n.Icon != "https://cdn.example/native.png" || len(n.Screenshots) != 1 {
		t.Fatalf("native = %+v", n)
	}
	l := got["legacy"]
	if l.Name != "Legacy" || l.Tagline.Value() != "Old line" || l.Developer != "Someone" || l.MinMemory != 256 || l.Category != "Tools" {
		t.Fatalf("legacy = %+v", l)
	}
	b := got["both"]
	if b.Name != "Both" || b.Category != "" || b.Developer != "" {
		t.Fatalf("both = %+v, want x-casaos ignored", b)
	}
}
