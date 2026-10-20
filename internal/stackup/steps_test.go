package stackup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/xcomposeapp"
)

func step(name, at, run, script string) xcomposeapp.Step {
	return xcomposeapp.Step{Name: name, At: at, Run: run, Script: xcomposeapp.Script{Script: script}}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return strings.Fields(string(b))
}

// A `once` step runs until it succeeds once, and is remembered across converges in
// .init/<name> — including a marker an install made before steps existed.
func TestRunStepsOnceIsRemembered(t *testing.T) {
	fixtureHookBin(t)
	cfg, dir := hookTestApp(t)
	steps := []xcomposeapp.Step{step("seed", xcomposeapp.AtSetup, "", `echo ran >> runs`)}
	for range 2 {
		if err := RunSteps(t.Context(), cfg, "app", dir, xcomposeapp.AtSetup, steps, map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := readLines(t, filepath.Join(dir, "runs")); len(got) != 1 {
		t.Fatalf("ran %d times, want once", len(got))
	}
	if _, err := os.Stat(filepath.Join(dir, InitStateDir, "seed")); err != nil {
		t.Fatalf("no marker: %v", err)
	}

	// An existing marker from an earlier Maison (same directory, same name) holds.
	os.WriteFile(filepath.Join(dir, InitStateDir, "add-admin"), nil, 0o644)
	steps = []xcomposeapp.Step{step("add-admin", xcomposeapp.AtSetup, "once", `echo ran >> admin`)}
	if err := RunSteps(t.Context(), cfg, "app", dir, xcomposeapp.AtSetup, steps, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "admin")); !os.IsNotExist(err) {
		t.Fatal("a step with an existing marker ran again")
	}
}

func TestRunStepsAlwaysRunsEveryTime(t *testing.T) {
	fixtureHookBin(t)
	cfg, dir := hookTestApp(t)
	steps := []xcomposeapp.Step{step("tick", xcomposeapp.AtStarted, "always", `echo ran >> runs`)}
	for range 3 {
		if err := RunSteps(t.Context(), cfg, "app", dir, xcomposeapp.AtStarted, steps, map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := readLines(t, filepath.Join(dir, "runs")); len(got) != 3 {
		t.Fatalf("ran %d times, want 3", len(got))
	}
	if _, err := os.Stat(filepath.Join(dir, InitStateDir, "tick")); !os.IsNotExist(err) {
		t.Fatal("an always step wrote a marker")
	}
}

// Steps run in declared order, and only those declared at the point being run.
func TestRunStepsOrderAndAt(t *testing.T) {
	fixtureHookBin(t)
	cfg, dir := hookTestApp(t)
	steps := []xcomposeapp.Step{
		step("a", xcomposeapp.AtSetup, "always", `echo a >> order`),
		step("b", xcomposeapp.AtStarted, "always", `echo b >> order`),
		step("c", xcomposeapp.AtSetup, "always", `echo c >> order`),
	}
	if err := RunSteps(t.Context(), cfg, "app", dir, xcomposeapp.AtSetup, steps, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(readLines(t, filepath.Join(dir, "order")), ","); got != "a,c" {
		t.Fatalf("setup ran %s, want a,c", got)
	}
}

// At setup the first failure stops the run and records nothing; at started every
// step still runs and the failures come back together.
func TestRunStepsFailureBySetupAndStarted(t *testing.T) {
	fixtureHookBin(t)
	cfg, dir := hookTestApp(t)
	steps := []xcomposeapp.Step{
		step("bad", xcomposeapp.AtSetup, "once", `exit 4`),
		step("after", xcomposeapp.AtSetup, "once", `echo ran >> after`),
	}
	err := RunSteps(t.Context(), cfg, "app", dir, xcomposeapp.AtSetup, steps, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "step bad") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, InitStateDir, "bad")); !os.IsNotExist(err) {
		t.Fatal("a failed once step wrote its marker")
	}
	if _, err := os.Stat(filepath.Join(dir, "after")); !os.IsNotExist(err) {
		t.Fatal("a setup step ran after a failed one")
	}

	steps = []xcomposeapp.Step{
		step("bad", xcomposeapp.AtStarted, "always", `exit 4`),
		step("after", xcomposeapp.AtStarted, "always", `echo ran >> after`),
	}
	err = RunSteps(t.Context(), cfg, "app", dir, xcomposeapp.AtStarted, steps, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "step bad") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "after")); err != nil {
		t.Fatal("a failed started step stopped the ones after it")
	}

	// A malformed step fails like a failed script, not silently.
	err = RunSteps(t.Context(), cfg, "app", dir, xcomposeapp.AtSetup, []xcomposeapp.Step{{Name: "empty", At: xcomposeapp.AtSetup}}, map[string]string{})
	if err == nil {
		t.Fatal("a step with nothing to run was accepted")
	}
}

// $MAISON_OUTPUT reaches the steps after it and the converge's outputs; it is
// never written to the .env.
func TestRunStepsOutputReachesLaterSteps(t *testing.T) {
	fixtureHookBin(t)
	cfg, dir := hookTestApp(t)
	steps := []xcomposeapp.Step{
		step("hash", xcomposeapp.AtSetup, "always", `hash=abc123; echo "HASH=$hash" >> "$MAISON_OUTPUT"`),
		step("use", xcomposeapp.AtSetup, "always", `test "$HASH" = abc123`),
	}
	outputs := map[string]string{}
	if err := RunSteps(t.Context(), cfg, "app", dir, xcomposeapp.AtSetup, steps, outputs); err != nil {
		t.Fatal(err)
	}
	if outputs["HASH"] != "abc123" {
		t.Fatalf("outputs = %v", outputs)
	}
	if _, ok := envinject.EnvFileVars(readEnvFile(dir))["HASH"]; ok {
		t.Fatal("an output was persisted to .env")
	}
}

// $MAISON_ENV adds a key the .env lacks and never overwrites one it has — even an
// empty one, which is the user's or the app's.
func TestRunStepsEnvAddsOnlyMissingKeys(t *testing.T) {
	fixtureHookBin(t)
	cfg, dir := hookTestApp(t)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("KEEP=mine\nBLANK=\n"), 0o644)
	steps := []xcomposeapp.Step{step("env", xcomposeapp.AtSetup, "always",
		`printf 'KEEP=theirs\nBLANK=filled\nNEW=added\n' >> "$MAISON_ENV"`)}
	if err := RunSteps(t.Context(), cfg, "app", dir, xcomposeapp.AtSetup, steps, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	env := envinject.EnvFileVars(readEnvFile(dir))
	if env["KEEP"] != "mine" || env["BLANK"] != "" || env["NEW"] != "added" {
		t.Fatalf(".env = %v", env)
	}
}

// A `file:` entry runs the copy under .scripts/, with the same contract.
func TestRunStepsFileMode(t *testing.T) {
	fixtureHookBin(t)
	cfg, dir := hookTestApp(t)
	os.MkdirAll(filepath.Join(dir, ScriptsStateDir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, ScriptsStateDir, "sub", "go.sh"), []byte("#!/bin/bash\necho \"$AppID\" > from-file\n"), 0o644)
	steps := []xcomposeapp.Step{{Name: "f", At: xcomposeapp.AtSetup, Script: xcomposeapp.Script{File: "scripts/sub/go.sh"}}}
	if err := RunSteps(t.Context(), cfg, "app", dir, xcomposeapp.AtSetup, steps, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if got := readLines(t, filepath.Join(dir, "from-file")); len(got) != 1 || got[0] != "app" {
		t.Fatalf("file step wrote %v", got)
	}
	// A file the app does not ship is an error, not a silent no-op.
	steps[0].Name, steps[0].File = "missing", "scripts/nope.sh"
	if err := RunSteps(t.Context(), cfg, "app", dir, xcomposeapp.AtSetup, steps, map[string]string{}); err == nil {
		t.Fatal("a missing file was accepted")
	}
}

func TestRunScriptTimeout(t *testing.T) {
	fixtureHookBin(t)
	cfg, dir := hookTestApp(t)
	_, err := RunScript(t.Context(), cfg, "app", dir, xcomposeapp.Script{Script: "while :; do :; done", Timeout: "200ms"}, ScriptOpts{})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err = %v, want the timeout named", err)
	}
}

// Strict mode is what catches a failed command substitution; a translated CasaOS
// command runs without it, as it was written to.
func TestRunScriptStrictUnlessLenient(t *testing.T) {
	fixtureHookBin(t)
	cfg, dir := hookTestApp(t)
	body := "x=$(false)\necho after > after"
	if _, err := RunScript(t.Context(), cfg, "app", dir, xcomposeapp.Script{Script: body}, ScriptOpts{}); err == nil {
		t.Fatal("a failed substitution passed in strict mode")
	}
	if _, err := os.Stat(filepath.Join(dir, "after")); !os.IsNotExist(err) {
		t.Fatal("strict mode ran past the failure")
	}
	if _, err := RunScript(t.Context(), cfg, "app", dir, xcomposeapp.Script{Script: body, Lenient: true}, ScriptOpts{}); err != nil {
		t.Fatalf("lenient: %v", err)
	}
}

// End to end through the converge: a setup step's output renders into a file.
func TestConvergeStepOutputRendersFiles(t *testing.T) {
	fixtureHookBin(t)
	cfg, dir := hookTestApp(t)
	spec := Spec{
		Steps: []xcomposeapp.Step{step("token", xcomposeapp.AtSetup, "always", `echo "TOKEN=t0k" >> "$MAISON_OUTPUT"`)},
		Files: []xcomposeapp.File{{Path: "/DATA/AppData/${AppID}/conf", Content: "token=${TOKEN}"}},
	}
	if _, err := converge(context.Background(), cfg, "app", dir, spec, nil, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "conf")); string(b) != "token=t0k" {
		t.Fatalf("conf = %q", b)
	}
}
