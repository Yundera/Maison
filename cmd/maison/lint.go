package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// runLint checks store compose files the way Maison will read them, without a
// running server: everything Parse refuses and drops (a user-facing string that is
// not a locale map with en_us), a block it cannot read at all, and a data_version /
// migrations declaration an update would refuse. It is for a store's maintainers
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
	switch a, err := f.App(); {
	case errors.Is(err, xcomposeapp.ErrNoExtension):
		out = append(out, "no x-compose-app (or x-casaos) block")
	case err != nil:
		// On a box this costs the app its whole block: title, URL, folders, secrets, hooks.
		out = append(out, "x-compose-app cannot be read, so Maison ignores all of it: "+err.Error())
	default:
		out = append(out, a.Problems...)
	}
	if _, err := f.Data(); err != nil {
		out = append(out, err.Error())
	}
	return out
}
