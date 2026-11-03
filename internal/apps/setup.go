package apps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// An app's declared inputs (x-compose-app.inputs) are values only the user can
// give it — a vendor account, an auth key. They are asked for in the install
// dialog, which may be skipped; while a required one is still empty the app is
// "pending setup", and its tile opens a form for them instead of the app.

// ErrBadInput is returned by SetInputs for a key the app does not declare, or a
// value .env cannot hold.
var ErrBadInput = errors.New("invalid input")

// setupPending reports whether a required input of ca has no value in the app's
// .env yet.
func setupPending(dir string, ca *xcomposeapp.App) bool {
	if ca == nil || len(ca.Inputs) == 0 {
		return false
	}
	var vars map[string]string
	for _, in := range ca.Inputs {
		if !in.Required {
			continue
		}
		if vars == nil {
			raw, _ := os.ReadFile(filepath.Join(dir, ".env"))
			vars = envinject.EnvFileVars(raw)
		}
		if strings.TrimSpace(vars[in.Env]) == "" {
			return true
		}
	}
	return false
}

// InputState is one declared input as the setup form shows it: the declaration,
// the current value for a plain input, and only whether a secret one is set —
// a secret value never leaves the server.
type InputState struct {
	xcomposeapp.Input
	Value string `json:"value,omitempty"`
	Set   bool   `json:"set"`
}

// Inputs returns id's declared inputs with what its .env holds for each.
func (r *Registry) Inputs(id string) ([]InputState, error) {
	dir, ca, err := r.mergedApp(id)
	if err != nil {
		return nil, err
	}
	raw, _ := os.ReadFile(filepath.Join(dir, ".env"))
	vars := envinject.EnvFileVars(raw)
	out := make([]InputState, 0, len(ca.Inputs))
	for _, in := range ca.Inputs {
		v := strings.TrimSpace(vars[in.Env])
		st := InputState{Input: in, Set: v != ""}
		if !in.Secret {
			st.Value = v
		}
		out = append(out, st)
	}
	return out, nil
}

// SetInputs writes values for id's declared inputs into its .env and recreates
// the app — the setup form's save. Only declared keys are accepted. A secret
// input sent blank keeps what it holds (the form never receives secret values,
// so blank means "unchanged"); any other blank clears the value.
func (r *Registry) SetInputs(ctx context.Context, id string, values map[string]string) error {
	dir, ca, err := r.mergedApp(id)
	if err != nil {
		return err
	}
	for k, v := range values {
		if _, ok := ca.Input(k); !ok {
			return fmt.Errorf("%w: %s is not one of this app's inputs", ErrBadInput, k)
		}
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("%w: %s may not span several lines", ErrBadInput, k)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	vars := envinject.ParseEnvFile(raw)
	set := func(k, v string) {
		for i := range vars {
			if vars[i].Key == k {
				vars[i].Value = v
				return
			}
		}
		vars = append(vars, envinject.Var{Key: k, Value: v})
	}
	for _, in := range ca.Inputs {
		v, sent := values[in.Env]
		if !sent {
			continue
		}
		v = strings.TrimSpace(v)
		if v == "" && in.Secret {
			continue
		}
		set(in.Env, v)
	}
	return r.SaveSettings(ctx, id, SettingsChange{Env: &vars})
}
