package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// runLint checks store compose files the way Maison will read them, without a
// running server: everything Parse refuses and drops (a user-facing string that is
// not a locale map with en_us), a block it cannot read at all, a data_version /
// migrations declaration an update would refuse, and every script entry — steps,
// migration steps, credential changes — that would fail before running, including
// a `file:` the store app does not ship. It is for a store's maintainers
// and CI — on a box the same refusals only reach the log.
//
// Exit status: 0 when every file is clean, 1 when any problem is found, 2 on usage.
func runLint(files []string, stdout, stderr io.Writer) int {
	if len(files) == 0 {
		fmt.Fprintln(stderr, "usage: maison lint <docker-compose.yml>…")
		return 2
	}
	bad := 0
	for _, path := range files {
		problems := lintFile(path)
		for _, p := range problems {
			fmt.Fprintf(stdout, "%s: %s\n", path, p)
		}
		if len(problems) > 0 {
			bad++
		}
	}
	if bad > 0 {
		fmt.Fprintf(stderr, "maison lint: %d of %d file(s) have problems\n", bad, len(files))
		return 1
	}
	return 0
}

func lintFile(path string) []string {
	f, err := composefile.Load(path)
	if err != nil {
		return []string{"not a readable compose file: " + err.Error()}
	}
	var out []string
	dir := filepath.Dir(path)
	switch a, err := f.App(); {
	case errors.Is(err, xcomposeapp.ErrNoExtension):
		out = append(out, "no x-compose-app (or x-casaos) block")
	case err != nil:
		// On a box this costs the app its whole block: title, URL, folders, secrets, steps.
		out = append(out, "x-compose-app cannot be read, so Maison ignores all of it: "+err.Error())
	default:
		out = append(out, a.Problems...)
		for _, e := range xcomposeapp.ValidateSteps(a.Steps) {
			out = append(out, e.Error())
		}
		for _, e := range xcomposeapp.ValidateRoutes(a.Routes, f.Exposed()) {
			out = append(out, e.Error())
		}
		for _, s := range a.Steps {
			out = append(out, missingFile(dir, "step "+s.Name, s.Script)...)
		}
		out = append(out, lintCredentials(dir, f.XComposeApp, a.Credentials)...)
	}
	if d, err := f.Data(); err != nil {
		out = append(out, err.Error())
	} else {
		for _, m := range d.Migrations {
			out = append(out, missingFile(dir, fmt.Sprintf("migration %d", m.ID), m.Script)...)
		}
	}
	return out
}

// lintCredentials reports a `change` block Maison would drop: on a box the
// credential silently becomes initial-only, so the store is the place to say so.
func lintCredentials(dir string, ext map[string]any, creds []xcomposeapp.Credential) []string {
	raw, _ := ext["credentials"].([]any)
	var out []string
	for i, c := range creds {
		label := fmt.Sprintf("credentials[%d]", i)
		if c.Change != nil {
			out = append(out, missingFile(dir, label+".change", c.Change.Script)...)
			continue
		}
		if i < len(raw) {
			if m, ok := raw[i].(map[string]any); ok && m["change"] != nil {
				out = append(out, label+".change cannot be used, so the credential is initial-only: apply must be env or script, and a script needs a valid script or file")
			}
		}
	}
	return out
}

// missingFile reports a script entry's `file:` the store app does not ship next
// to its compose — Maison copies only what is there.
func missingFile(dir, label string, s xcomposeapp.Script) []string {
	if s.File == "" || s.Validate() != nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(s.File))); err != nil {
		return []string{fmt.Sprintf("%s: file %s is not in the app's folder", label, s.File)}
	}
	return nil
}
