package composefile

import (
	"errors"
	"testing"

	"github.com/yundera/maison/internal/xcomposeapp"
)

func parse(t *testing.T, y string) *File {
	t.Helper()
	f, err := Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// A CasaOS-only compose is translated whole into the native model.
func TestResolveTranslatesXCasaOS(t *testing.T) {
	a, err := parse(t, `
x-casaos:
  store_app_id: jelly
  title: {en_us: Jellyfin}
  icon: https://e.x/i.png
  main: jellyfin
  scheme: https
  hostname: jellyfin-${APP_DOMAIN}
  port_map: "443"
  index: /web
  webui_port: "8096"
  author: Jelly Team
  tips:
    before_install: {en_us: Read me}
    custom: And me
  pre-install-cmd: echo pre
  post-install-cmd: echo post
`).App()
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "jelly" || a.Title.Value() != "Jellyfin" || a.Icon != "https://e.x/i.png" || a.Main != "jellyfin" ||
		a.Developer != "Jelly Team" || a.Tips.Value() != "Read me\n\nAnd me" ||
		a.Hooks.PreInstall != "echo pre" || a.Hooks.PostInstall != "echo post" {
		t.Fatalf("app = %+v", a)
	}
	if got := a.WebURL("box.example"); got != "https://jellyfin-box.example/web" {
		t.Fatalf("url = %q (the scheme's default port must not be spelled out)", got)
	}
	if a.Legacy == nil || a.WebUIContainerPort() != 8096 {
		t.Fatalf("legacy = %+v, want webui_port carried", a.Legacy)
	}
}

// One or the other: beside an x-compose-app block, x-casaos is not read at all.
func TestResolvePrefersXComposeAppWhole(t *testing.T) {
	a, err := parse(t, `
x-casaos:
  title: {en_us: Legacy}
  icon: legacy.png
  pre-install-cmd: echo legacy
x-compose-app:
  webui-host: app-${domain}
`).App()
	if err != nil {
		t.Fatal(err)
	}
	if a.Title.Value() != "" || a.Icon != "" || a.Hooks.PreInstall != "" || a.Legacy != nil {
		t.Fatalf("x-casaos leaked into %+v", a)
	}
}

// The base decides the mode; the override's x-compose-app layers on top in either
// mode, and its x-casaos is never read.
func TestResolveLayersOverride(t *testing.T) {
	base := parse(t, "x-casaos:\n  title: {en_us: Legacy}\n  icon: legacy.png\n  tips: {custom: store tips}\n")
	over := parse(t, "x-casaos:\n  icon: ignored.png\nx-compose-app:\n  store-ref: https://s.example/-/Apps/x\n  tips: mine\n")
	a, err := Resolve(base, over)
	if err != nil {
		t.Fatal(err)
	}
	if a.Title.Value() != "Legacy" || a.Icon != "legacy.png" || a.StoreRef == "" || a.Tips.Value() != "mine" || a.Legacy == nil {
		t.Fatalf("app = %+v", a)
	}

	// No metadata in the base: the override alone still says something.
	a, err = Resolve(parse(t, "services: {}\n"), over)
	if err != nil || a.StoreRef == "" {
		t.Fatalf("app = %+v, err = %v", a, err)
	}
	if _, err := Resolve(parse(t, "services: {}\n"), nil); !errors.Is(err, xcomposeapp.ErrNoExtension) {
		t.Fatalf("err = %v, want ErrNoExtension", err)
	}
}

// A block this build cannot read counts as absent, and the app falls back to its
// x-casaos block whole. A block that is merely malformed does not: falling back
// would hide the author's mistake behind a half-working tile.
func TestResolveVersionAndErrors(t *testing.T) {
	a, err := parse(t, "x-casaos:\n  title: {en_us: Legacy}\nx-compose-app:\n  schema_version: 99\n  title: Future\n").App()
	if err != nil || a.Title.Value() != "Legacy" {
		t.Fatalf("app = %+v, err = %v; want the x-casaos fallback", a, err)
	}
	if _, err := parse(t, "x-casaos:\n  title: {en_us: Legacy}\nx-compose-app:\n  folders: 42\n").App(); err == nil {
		t.Fatal("a malformed x-compose-app must be an error, not a silent x-casaos fallback")
	}
}
