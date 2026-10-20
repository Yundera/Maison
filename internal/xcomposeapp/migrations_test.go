package xcomposeapp

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func parseDataYAML(t *testing.T, src string) (Data, error) {
	t.Helper()
	var ext map[string]any
	if err := yaml.Unmarshal([]byte(src), &ext); err != nil {
		t.Fatal(err)
	}
	return ParseData(ext)
}

// Every app that predates migrations — which is all of them — is data version 1.
func TestParseDataDefaultsToOne(t *testing.T) {
	for _, ext := range []map[string]any{nil, {"title": "Odoo"}} {
		d, err := ParseData(ext)
		if err != nil || d.Version != 1 || len(d.Migrations) != 0 {
			t.Fatalf("ParseData(%v) = %+v, %v; want version 1, no steps", ext, d, err)
		}
	}
}

func TestParseDataReadsTheForms(t *testing.T) {
	d, err := parseDataYAML(t, `
data_version: 4
migrations:
  - id: 2
    name: {en_us: pg-14-to-16, fr_fr: pg-14-vers-16}
    images: [pgautoupgrade/pgautoupgrade:16-alpine]
    script: |
      docker run --rm -e PGAUTO_ONESHOT=yes -v "$APP_DIR/db:/var/lib/postgresql/data" pgautoupgrade/pgautoupgrade:16-alpine
  - id: 4
    blocked:
      en_us: |
        Needs OpenUpgrade.
  - id: 3
    file: scripts/0003-move-config.sh
    images: [nextcloud:28.0.14-apache]
    timeout: 30m
`)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := d.Pending(1)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int
	for _, m := range steps {
		ids = append(ids, m.ID)
	}
	if len(ids) != 3 || ids[0] != 2 || ids[1] != 3 || ids[2] != 4 {
		t.Fatalf("pending from 1 = %v, want [2 3 4] in order whatever the declaration order", ids)
	}
	if steps[0].Label() != "pg-14-to-16" || steps[2].Label() != "step 4" {
		t.Errorf("labels = %q, %q", steps[0].Label(), steps[2].Label())
	}
	if !strings.Contains(steps[0].Script.Script, "PGAUTO_ONESHOT") || len(steps[0].Images) != 1 {
		t.Errorf("script step = %+v", steps[0])
	}
	if steps[1].File != "scripts/0003-move-config.sh" || steps[1].Timeout != "30m" {
		t.Errorf("file step = %+v", steps[1])
	}
}

// A block Maison cannot read must say so: it decides whether an update is
// refused, so it cannot be allowed to degrade into "no migrations".
func TestParseDataRejectsAuthoringErrors(t *testing.T) {
	cases := map[string]string{
		"zero version":   "data_version: 0",
		"not a number":   "data_version: three",
		"gap":            "data_version: 3\nmigrations: [{id: 3, blocked: {en_us: x}}]",
		"beyond target":  "data_version: 2\nmigrations: [{id: 2, blocked: {en_us: x}}, {id: 3, blocked: {en_us: y}}]",
		"id 1":           "data_version: 2\nmigrations: [{id: 1, blocked: {en_us: x}}, {id: 2, blocked: {en_us: y}}]",
		"twice":          "data_version: 2\nmigrations: [{id: 2, blocked: {en_us: x}}, {id: 2, blocked: {en_us: y}}]",
		"no form":        "data_version: 2\nmigrations: [{id: 2, name: {en_us: nothing}}]",
		"script+file":    "data_version: 2\nmigrations: [{id: 2, script: \"true\", file: scripts/b.sh}]",
		"retired image":  "data_version: 2\nmigrations: [{id: 2, image: a:1}]",
		"outside folder": "data_version: 2\nmigrations: [{id: 2, file: b.sh}]",
		"old folder":     "data_version: 2\nmigrations: [{id: 2, file: migrations/b.sh}]",
		"dotdot escape":  "data_version: 2\nmigrations: [{id: 2, file: scripts/../b.sh}]",
		"escaping path":  "data_version: 2\nmigrations: [{id: 2, file: ../../etc/x.sh}]",
		"absolute path":  "data_version: 2\nmigrations: [{id: 2, file: /bin/x.sh}]",
		"bad timeout":    "data_version: 2\nmigrations: [{id: 2, script: \"true\", timeout: soon}]",
		"blocked+script": "data_version: 2\nmigrations: [{id: 2, blocked: {en_us: x}, script: \"true\"}]",
		"blocked+extra":  "data_version: 2\nmigrations: [{id: 2, blocked: {en_us: x}, timeout: 1h}]",
		"steps, no ver":  "migrations: [{id: 2, blocked: {en_us: x}}]",
		"plain blocked":  "data_version: 2\nmigrations: [{id: 2, blocked: by hand}]",
		"plain name":     "data_version: 2\nmigrations: [{id: 2, name: upgrade, script: \"true\"}]",
		"blocked no en":  "data_version: 2\nmigrations: [{id: 2, blocked: {fr_fr: à la main}}]",
	}
	for name, src := range cases {
		if _, err := parseDataYAML(t, src); err == nil {
			t.Errorf("%s: accepted %q", name, src)
		}
	}
}

// schema_version gates rendering, not the data: a build that cannot draw a newer
// manifest must still know which data it would be starting.
func TestParseDataIgnoresSchemaVersion(t *testing.T) {
	d, err := parseDataYAML(t, "schema_version: 99\ndata_version: 2\nmigrations: [{id: 2, blocked: {en_us: x}}]")
	if err != nil || d.Version != 2 {
		t.Fatalf("got %+v, %v", d, err)
	}
}

// The installed compose is read for its version only: a step written in a format
// this build refuses (a plain-string name, from before locale maps) does not matter.
func TestParseVersionIgnoresTheSteps(t *testing.T) {
	for src, want := range map[string]int{
		"":                  1,
		"title: {en_us: x}": 1,
		"data_version: 2\nmigrations: [{id: 2, name: postgres-13-to-17, script: \"true\"}]": 2,
		"data_version: 3\nmigrations: [{id: 3, blocked: not localized}]":                    3,
	} {
		var ext map[string]any
		if err := yaml.Unmarshal([]byte(src), &ext); err != nil {
			t.Fatal(err)
		}
		if v, err := ParseVersion(ext); v != want || err != nil {
			t.Errorf("%q: %d, %v; want %d", src, v, err, want)
		}
	}
	for _, src := range []string{"data_version: 0", "data_version: three"} {
		var ext map[string]any
		yaml.Unmarshal([]byte(src), &ext)
		if _, err := ParseVersion(ext); err == nil {
			t.Errorf("%q: accepted", src)
		}
	}
}

func TestPendingRefusesNewerData(t *testing.T) {
	d := Data{Version: 2, Migrations: []Migration{{ID: 2, Blocked: Localized{"en_us": "x"}}}}
	if _, err := d.Pending(3); !errors.Is(err, ErrDataNewer) {
		t.Fatalf("err = %v, want ErrDataNewer", err)
	}
	if steps, err := d.Pending(2); err != nil || len(steps) != 0 {
		t.Fatalf("at target: %v, %v", steps, err)
	}
}

// version is display-only and free-form; a bare YAML number must not cost the app
// its whole x-compose-app block.
func TestVersionIsFreeForm(t *testing.T) {
	for src, want := range map[string]string{
		`version: 2.4`:              "2.4",
		`version: "2024.10.1-ls45"`: "2024.10.1-ls45",
		`version: 18`:               "18",
	} {
		var ext map[string]any
		if err := yaml.Unmarshal([]byte(src), &ext); err != nil {
			t.Fatal(err)
		}
		a, err := Parse(ext)
		if err != nil || !strings.EqualFold(a.Version, want) {
			t.Errorf("%s: got %+v, %v; want %q", src, a, err, want)
		}
	}
}
