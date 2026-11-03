package apps

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/config"
)

const inputsBase = `services: {}
x-compose-app:
  title: {en_us: Foundry}
  inputs:
    - env: FOUNDRY_USERNAME
      label: {en_us: Username}
      required: true
    - env: FOUNDRY_PASSWORD
      label: {en_us: Password}
      secret: true
      required: true
    - env: FOUNDRY_LICENSE_KEY
      label: {en_us: Licence key}
      secret: true
`

func TestSetupPendingFollowsRequiredInputs(t *testing.T) {
	r := New(config.Config{DataRoot: t.TempDir()}, nil)
	for env, want := range map[string]bool{
		"": true, // nothing yet
		"FOUNDRY_USERNAME=gm\nFOUNDRY_PASSWORD=\n":   true,  // a required one empty
		"FOUNDRY_USERNAME=gm\nFOUNDRY_PASSWORD=pw\n": false, // the optional licence may stay empty
		"FOUNDRY_USERNAME= \nFOUNDRY_PASSWORD=pw\n":  true,  // blank is empty
	} {
		dir := seedCredentialsApp(t, r, "foundry", inputsBase, "", env)
		_, ca, err := r.mergedApp("foundry")
		if err != nil {
			t.Fatal(err)
		}
		if got := setupPending(dir, ca); got != want {
			t.Errorf(".env %q: setupPending = %v, want %v", env, got, want)
		}
	}
}

func TestSetupPendingWithoutInputs(t *testing.T) {
	r := newSecretsRegistry(t)
	_, ca, _ := r.mergedApp("outline")
	if setupPending(filepath.Join(r.cfg.AppsDir(), "outline"), ca) {
		t.Fatal("an app with no inputs is never pending setup")
	}
}

// A secret input is masked in the Env tab, but not locked: it is the user's value.
func TestSecretInputIsMaskedNotLocked(t *testing.T) {
	r := New(config.Config{DataRoot: t.TempDir()}, nil)
	seedCredentialsApp(t, r, "foundry", inputsBase, "", "FOUNDRY_USERNAME=gm\nFOUNDRY_PASSWORD=pw\nFOUNDRY_LICENSE_KEY=lk\n")
	got, err := r.GetEnv("foundry")
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]EnvEntry{}
	for _, e := range got {
		byKey[e.Key] = e
	}
	if e := byKey["FOUNDRY_LICENSE_KEY"]; !e.Masked || e.Source != "" || e.Value != "" {
		t.Errorf("FOUNDRY_LICENSE_KEY = %+v, want masked, unlocked", e)
	}
	if e := byKey["FOUNDRY_USERNAME"]; e.Masked || e.Value != "gm" {
		t.Errorf("FOUNDRY_USERNAME = %+v, want shown", e)
	}
}

func TestSetInputsRefusesUndeclaredKeys(t *testing.T) {
	r := New(config.Config{DataRoot: t.TempDir()}, nil)
	seedCredentialsApp(t, r, "foundry", inputsBase, "", "")
	for _, values := range []map[string]string{
		{"PUID": "0"},
		{"FOUNDRY_USERNAME": "gm\nPUID=0"},
	} {
		if err := r.SetInputs(context.Background(), "foundry", values); !errors.Is(err, ErrBadInput) {
			t.Errorf("%v: err = %v, want ErrBadInput", values, err)
		}
	}
}

// The form sees plain values, and only whether a secret is set.
func TestInputsNeverServeSecretValues(t *testing.T) {
	r := New(config.Config{DataRoot: t.TempDir()}, nil)
	seedCredentialsApp(t, r, "foundry", inputsBase, "", "FOUNDRY_USERNAME=gm\nFOUNDRY_PASSWORD=pw\n")
	got, err := r.Inputs("foundry")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("inputs = %+v", got)
	}
	if got[0].Value != "gm" || !got[0].Set {
		t.Errorf("username = %+v", got[0])
	}
	if got[1].Value != "" || !got[1].Set {
		t.Errorf("password = %+v, want set with no value", got[1])
	}
	if got[2].Set {
		t.Errorf("licence = %+v, want unset", got[2])
	}
}
