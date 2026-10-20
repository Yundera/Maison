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

func TestParseDataReadsTheThreeForms(t *testing.T) {
	d, err := parseDataYAML(t, `
data_version: 4
migrations:
  - id: 2
    name: {en_us: pg-14-to-16, fr_fr: pg-14-vers-16}
    image: pgautoupgrade/pgautoupgrade:16-alpine
    environment: {PGAUTO_ONESHOT: "yes"}
    volumes: ["/DATA/AppData/${AppID}/db:/var/lib/postgresql/data"]
  - id: 4
    blocked:
      en_us: |
        Needs OpenUpgrade.
  - id: 3
    script: migrations/0003-move-config.sh
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
	if len(steps[0].Env) != 1 || steps[0].Env[0] != "PGAUTO_ONESHOT=yes" {
		t.Errorf("env = %v", steps[0].Env)
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
		"two forms":      "data_version: 2\nmigrations: [{id: 2, image: a:1, script: b.sh}]",
		"script+volumes": "data_version: 2\nmigrations: [{id: 2, script: migrations/b.sh, volumes: [/a:/b]}]",
		"outside folder": "data_version: 2\nmigrations: [{id: 2, script: b.sh}]",
		"dotdot escape":  "data_version: 2\nmigrations: [{id: 2, script: migrations/../b.sh}]",
		"image+images":   "data_version: 2\nmigrations: [{id: 2, image: a:1, images: [b:1]}]",
		"escaping path":  "data_version: 2\nmigrations: [{id: 2, script: ../../etc/x.sh}]",
		"absolute path":  "data_version: 2\nmigrations: [{id: 2, script: /bin/x.sh}]",
		"bad timeout":    "data_version: 2\nmigrations: [{id: 2, image: a:1, timeout: soon}]",
		"blocked+extra":  "data_version: 2\nmigrations: [{id: 2, blocked: {en_us: x}, timeout: 1h}]",
		"steps, no ver":  "migrations: [{id: 2, blocked: {en_us: x}}]",
		"plain blocked":  "data_version: 2\nmigrations: [{id: 2, blocked: by hand}]",
		"plain name":     "data_version: 2\nmigrations: [{id: 2, name: upgrade, image: a:1}]",
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
