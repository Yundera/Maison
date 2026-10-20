package overrideform

import (
	"strings"
	"testing"
)

const base = `
services:
  app:
    image: jellyfin/jellyfin:10.9.6
    restart: unless-stopped
    ports:
      - "8096:8096"
    volumes:
      - /DATA/AppData/jellyfin/config:/config
    environment:
      PUID: "1000"
      TZ: Europe/Paris
  db:
    image: postgres:16
x-casaos:
  main: app
`

// build → apply round-trip with no edits must leave the override alone.
func TestApplyNoEditsKeepsOverride(t *testing.T) {
	override := `
# my notes
services:
  app:
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8096"]
x-compose-app:
  tips: {en_us: read me}
`
	form, err := Build([]byte(base), []byte(override))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Apply([]byte(base), []byte(override), form)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	// Keys the form doesn't model — and the comment above them — survive a save.
	for _, want := range []string{"# my notes", "healthcheck", "x-compose-app", "tips: {en_us: read me}"} {
		if !strings.Contains(got, want) {
			t.Fatalf("a save dropped %q from the override:\n%s", want, got)
		}
	}
}

// A save through the form must not reformat the file around the field it touched:
// a hand-written override keeps its two-space indent, not yaml.Marshal's four.
func TestApplyKeepsTwoSpaceIndent(t *testing.T) {
	override := `services:
  app:
    healthcheck:
      test: ["CMD", "true"]
`
	form, err := Build([]byte(base), []byte(override))
	if err != nil {
		t.Fatal(err)
	}
	form.Services[0].Ports.Value = []string{"8096:8096", "9090:9090"}

	out, err := Apply([]byte(base), []byte(override), form)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "\n  app:\n") {
		t.Fatalf("the file was re-indented by a save:\n%s", out)
	}
}

func TestBuildMergesBaseAndOverride(t *testing.T) {
	override := `
services:
  app:
    image: jellyfin/jellyfin:10.10.0
    ports:
      - "9090:9090"
    environment:
      TZ: UTC
      EXTRA: "1"
`
	form, err := Build([]byte(base), []byte(override))
	if err != nil {
		t.Fatal(err)
	}
	if len(form.Services) != 2 || form.Services[0].Name != "app" || form.Services[1].Name != "db" {
		t.Fatalf("services = %+v, want app then db", form.Services)
	}
	app := form.Services[0]

	if app.Image.Value != "jellyfin/jellyfin:10.10.0" || app.Image.Base != "jellyfin/jellyfin:10.9.6" || !app.Image.Overridden {
		t.Fatalf("image = %+v", app.Image)
	}
	// Untouched by the override: effective value is the store's, not flagged.
	if app.Restart.Value != "unless-stopped" || app.Restart.Overridden {
		t.Fatalf("restart = %+v, want the store's value, unoverridden", app.Restart)
	}
	// Compose concatenates sequences, so the effective ports are base + override.
	if !equal(app.Ports.Value, []string{"8096:8096", "9090:9090"}) {
		t.Fatalf("ports = %v, want the base's then the override's", app.Ports.Value)
	}
	// Mappings merge by key: PUID from the store, TZ overridden, EXTRA added.
	want := []EnvVar{{"PUID", "1000"}, {"TZ", "UTC"}, {"EXTRA", "1"}}
	if !equalEnv(app.Env.Value, want) {
		t.Fatalf("environment = %+v, want %+v", app.Env.Value, want)
	}
}

// A pure append needs no tag — Compose's own concatenation does the work, and the
// override stays as small as possible.
func TestApplyAppendedPortNeedsNoTag(t *testing.T) {
	form, _ := Build([]byte(base), nil)
	form.Services[0].Ports.Value = []string{"8096:8096", "9090:9090"}

	out, err := Apply([]byte(base), nil, form)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if strings.Contains(got, tagOverride) {
		t.Fatalf("an appended port was tagged !override:\n%s", got)
	}
	if strings.Contains(got, "8096:8096") {
		t.Fatalf("an appended port restated the store's:\n%s", got)
	}
	if !strings.Contains(got, "9090:9090") {
		t.Fatalf("the added port is missing:\n%s", got)
	}
}

// Editing a port the store ships cannot be a plain list: Compose would publish
// both. It has to replace the base's outright.
func TestApplyEditedPortUsesOverrideTag(t *testing.T) {
	form, _ := Build([]byte(base), nil)
	form.Services[0].Ports.Value = []string{"8097:8096"}

	out, err := Apply([]byte(base), nil, form)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "ports: "+tagOverride) {
		t.Fatalf("an edited port was not tagged !override:\n%s", got)
	}
	if strings.Contains(got, "8096:8096") {
		t.Fatalf("the store's port survived the edit:\n%s", got)
	}
}

// Same rule for environment: changing or adding is a key merge, but *removing* a
// store variable needs the whole mapping replaced.
func TestApplyEnv(t *testing.T) {
	form, _ := Build([]byte(base), nil)
	form.Services[0].Env.Value = []EnvVar{{"PUID", "1000"}, {"TZ", "UTC"}, {"NEW", "x"}}

	out, err := Apply([]byte(base), nil, form)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if strings.Contains(got, tagOverride) {
		t.Fatalf("a key merge was tagged !override:\n%s", got)
	}
	if strings.Contains(got, "PUID") { // unchanged from the store — nothing to write
		t.Fatalf("an unchanged variable was written to the override:\n%s", got)
	}
	if !strings.Contains(got, "TZ: UTC") || !strings.Contains(got, "NEW:") {
		t.Fatalf("changed/added variables are missing:\n%s", got)
	}

	form, _ = Build([]byte(base), nil)
	form.Services[0].Env.Value = []EnvVar{{"TZ", "Europe/Paris"}} // PUID dropped
	out, err = Apply([]byte(base), nil, form)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); !strings.Contains(got, "environment: "+tagOverride) {
		t.Fatalf("removing a store variable was not tagged !override:\n%s", got)
	}
}

// Clearing a field means "inherit from the store": the key leaves the override,
// and an override with nothing left in it is removed entirely.
func TestApplyResetToBaseEmptiesOverride(t *testing.T) {
	override := `
services:
  app:
    image: jellyfin/jellyfin:10.10.0
`
	form, err := Build([]byte(base), []byte(override))
	if err != nil {
		t.Fatal(err)
	}
	form.Services[0].Image.Value = "" // reset

	out, err := Apply([]byte(base), []byte(override), form)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Fatalf("a fully-reset override was not removed:\n%s", out)
	}
}

// Restating the store's own value is a no-op, not an override.
func TestApplyValueEqualToBaseWritesNothing(t *testing.T) {
	form, _ := Build([]byte(base), nil)
	form.Services[0].Restart.Value = "unless-stopped"
	form.Services[0].Ports.Value = []string{"8096:8096"}

	out, err := Apply([]byte(base), nil, form)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Fatalf("restating the store's values produced an override:\n%s", out)
	}
}

// A construct the form can't represent is shown but never rewritten — the YAML
// view is the only way to edit it.
func TestComplexFieldsAreReadOnly(t *testing.T) {
	complexBase := `
services:
  app:
    image: app:1
    command: ["serve", "--port", "80"]
    ports:
      - target: 80
        published: 8080
`
	form, err := Build([]byte(complexBase), nil)
	if err != nil {
		t.Fatal(err)
	}
	app := form.Services[0]
	if !app.Command.Complex || !app.Ports.Complex {
		t.Fatalf("long-syntax fields not flagged complex: command=%+v ports=%+v", app.Command, app.Ports)
	}
	// A field the form won't edit must still be *shown*, or the UI renders a blank
	// box where a long-syntax port lives.
	if !strings.Contains(app.Ports.Raw, "published: 8080") {
		t.Fatalf("complex ports carry no raw YAML to display: %q", app.Ports.Raw)
	}
	if !strings.Contains(app.Command.Raw, "serve") {
		t.Fatalf("complex command carries no raw YAML to display: %q", app.Command.Raw)
	}

	// Even if a client sends edits for them, Apply must not touch them.
	form.Services[0].Command.Value = "serve"
	form.Services[0].Ports.Value = []string{"9999:80"}
	out, err := Apply([]byte(complexBase), nil, form)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Fatalf("a complex field was rewritten by the form:\n%s", out)
	}
}

// The opening URL is read from both files' x-compose-app and written back next to
// whatever else the override keeps there.
func TestWebUIRoundTrip(t *testing.T) {
	b := base + `x-compose-app:
  webui-port: "8096"
`
	override := `# my notes
x-compose-app:
  tips: {en_us: read me}
`
	form, err := Build([]byte(b), []byte(override))
	if err != nil {
		t.Fatal(err)
	}
	if form.WebUI == nil || form.WebUI.Port.Base != "8096" || form.WebUI.Host.Overridden {
		t.Fatalf("webui not read from the base: %+v", form.WebUI)
	}
	form.WebUI.Host.Value = "media-${APP_DOMAIN}"

	out, err := Apply([]byte(b), []byte(override), form)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{"# my notes", "tips: {en_us: read me}", "webui-host: media-${APP_DOMAIN}"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q after a webui save:\n%s", want, got)
		}
	}
	if strings.Contains(got, "webui-port") {
		t.Fatalf("an inherited field was written:\n%s", got)
	}

	// Clearing it again empties the block it created, and the file with it.
	again, err := Build([]byte(b), []byte(`x-compose-app:
  webui-host: x
`))
	if err != nil {
		t.Fatal(err)
	}
	again.WebUI.Host.Value = ""
	out, err = Apply([]byte(b), []byte("x-compose-app:\n  webui-host: x\n"), again)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Fatalf("a cleared webui should leave no override, got:\n%s", out)
	}
}

// A form posted without the opening URL (an older UI) must not clear it.
func TestApplyWithoutWebUIKeepsIt(t *testing.T) {
	override := "x-compose-app:\n  webui-host: x\n"
	form, err := Build([]byte(base), []byte(override))
	if err != nil {
		t.Fatal(err)
	}
	form.WebUI = nil
	out, err := Apply([]byte(base), []byte(override), form)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "webui-host: x") {
		t.Fatalf("webui dropped by a form without it:\n%s", out)
	}
}
