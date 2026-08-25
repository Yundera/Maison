package stackup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/dockerx"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// fakeRunner records what would have been run, and answers with canned stdout.
type fakeRunner struct {
	specs []dockerx.RunSpec
	out   string
	err   error
}

func (f *fakeRunner) RunOnce(_ context.Context, spec dockerx.RunSpec) (string, error) {
	f.specs = append(f.specs, spec)
	return f.out, f.err
}

// withRunner swaps the daemon connection for the test's fake.
func withRunner(t *testing.T, f *fakeRunner) {
	t.Helper()
	prev := newRunner
	newRunner = func() (Runner, error) { return f, nil }
	t.Cleanup(func() { newRunner = prev })
}

func TestRunInitOnceRunsOnceAndIsRememberedAcrossConverges(t *testing.T) {
	f := &fakeRunner{}
	withRunner(t, f)
	cfg, dir := dataRootApp(t, "filebrowser", "")
	steps := []xcomposeapp.InitStep{{
		Name:    "seed-db",
		Image:   "filebrowser/filebrowser:v2.63.2",
		Command: xcomposeapp.StrList{"config", "init"},
	}}

	for i := 0; i < 3; i++ {
		if err := RunInit(context.Background(), cfg, "filebrowser", dir, xcomposeapp.PhasePreUp, steps, map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	// filebrowser's `config init` aborts if the database is already there — and
	// took the whole hook down with it. Running it once is the point.
	if len(f.specs) != 1 {
		t.Fatalf("ran %d times, want 1", len(f.specs))
	}
	if _, err := os.Stat(filepath.Join(dir, InitStateDir, "seed-db")); err != nil {
		t.Fatalf("no marker recorded: %v", err)
	}
}

func TestRunInitAbsentGuardTracksThePath(t *testing.T) {
	f := &fakeRunner{}
	withRunner(t, f)
	cfg, dir := dataRootApp(t, "filebrowser", "")
	steps := []xcomposeapp.InitStep{{
		Name:  "seed-db",
		Image: "filebrowser/filebrowser:v2.63.2",
		When:  xcomposeapp.WhenAbsentPrefix + "/DATA/AppData/${AppID}/db/database.db",
	}}

	if err := RunInit(context.Background(), cfg, "filebrowser", dir, xcomposeapp.PhasePreUp, steps, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if len(f.specs) != 1 {
		t.Fatalf("ran %d times while the file was missing, want 1", len(f.specs))
	}

	// The guard path is Maison's to evaluate, so it resolves container-side —
	// under the temp data root, not under the host spelling.
	db := filepath.Join(dir, "db/database.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunInit(context.Background(), cfg, "filebrowser", dir, xcomposeapp.PhasePreUp, steps, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if len(f.specs) != 1 {
		t.Fatalf("ran again with the database in place: %d runs", len(f.specs))
	}
}

func TestRunInitAlwaysRunsEveryTime(t *testing.T) {
	f := &fakeRunner{}
	withRunner(t, f)
	cfg, dir := dataRootApp(t, "app", "")
	steps := []xcomposeapp.InitStep{{Name: "x", Image: "busybox", When: xcomposeapp.WhenAlways}}

	for i := 0; i < 3; i++ {
		if err := RunInit(context.Background(), cfg, "app", dir, xcomposeapp.PhasePreUp, steps, map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.specs) != 3 {
		t.Fatalf("ran %d times, want 3", len(f.specs))
	}
}

func TestRunInitCaptureIsTrimmedAndUsable(t *testing.T) {
	f := &fakeRunner{out: "obscured-value\n"}
	withRunner(t, f)
	cfg, dir := dataRootApp(t, "seafile", "APP_DEFAULT_PASSWORD=hunter2\n")
	captures := map[string]string{}

	steps := []xcomposeapp.InitStep{{
		Name:    "obscure-pass",
		Image:   "rclone/rclone:1.73.3",
		Command: xcomposeapp.StrList{"obscure", "${APP_DEFAULT_PASSWORD}"},
		Capture: "RCLONE_PASS",
	}}
	if err := RunInit(context.Background(), cfg, "seafile", dir, xcomposeapp.PhasePreUp, steps, captures); err != nil {
		t.Fatal(err)
	}

	// Trimmed: the tool's trailing newline would be a syntax error in the config
	// file this value is about to be substituted into.
	if captures["RCLONE_PASS"] != "obscured-value" {
		t.Fatalf("capture = %q", captures["RCLONE_PASS"])
	}
	// And the command it ran had the app's password substituted in.
	if got := f.specs[0].Cmd; len(got) != 2 || got[1] != "hunter2" {
		t.Fatalf("command = %v", got)
	}
}

// The two path spellings in one step: a volume source is the host's, because the
// daemon resolves it; the guard path is Maison's, because Maison resolves it.
func TestRunInitVolumeSourcesAreHostPaths(t *testing.T) {
	f := &fakeRunner{}
	withRunner(t, f)
	cfg, dir := dataRootApp(t, "filebrowser", "")

	steps := []xcomposeapp.InitStep{{
		Name:    "seed-db",
		Image:   "filebrowser/filebrowser:v2.63.2",
		User:    "${PUID}:${PGID}",
		Volumes: []string{"/DATA/AppData/${AppID}/db:/db"},
	}}
	if err := RunInit(context.Background(), cfg, "filebrowser", dir, xcomposeapp.PhasePreUp, steps, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	want := "/host/DATA/AppData/filebrowser/db:/db"
	if got := f.specs[0].Binds; len(got) != 1 || got[0] != want {
		t.Fatalf("binds = %v, want [%s]", got, want)
	}
	if got := f.specs[0].User; got != "1000:1000" {
		t.Fatalf("user = %q", got)
	}
}

func TestRunInitPhaseSelects(t *testing.T) {
	f := &fakeRunner{}
	withRunner(t, f)
	cfg, dir := dataRootApp(t, "hubs", "")
	steps := []xcomposeapp.InitStep{
		{Name: "bootstrap", Image: "hubs-seed", When: xcomposeapp.WhenAlways},
		{Name: "seed", Image: "hubs-seed", When: xcomposeapp.WhenAlways,
			Phase: xcomposeapp.PhasePostUp, Network: "hubs-net"},
	}

	if err := RunInit(context.Background(), cfg, "hubs", dir, xcomposeapp.PhasePreUp, steps, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if len(f.specs) != 1 || f.specs[0].Network != "" {
		t.Fatalf("pre_up ran %v", f.specs)
	}
	if err := RunInit(context.Background(), cfg, "hubs", dir, xcomposeapp.PhasePostUp, steps, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if len(f.specs) != 2 || f.specs[1].Network != "hubs-net" {
		t.Fatalf("post_up ran %v", f.specs)
	}
}

func TestRunInitFailsLoudly(t *testing.T) {
	cfg, dir := dataRootApp(t, "app", "")

	t.Run("container failure", func(t *testing.T) {
		withRunner(t, &fakeRunner{err: errors.New("exited 1: boom")})
		steps := []xcomposeapp.InitStep{{Name: "x", Image: "busybox", When: xcomposeapp.WhenAlways}}
		if err := RunInit(context.Background(), cfg, "app", dir, xcomposeapp.PhasePreUp, steps, map[string]string{}); err == nil {
			t.Fatal("want an error — a failed pre_up seeder must not let the stack start")
		}
		// ...and nothing is remembered, so the next up tries again.
		if _, err := os.Stat(filepath.Join(dir, InitStateDir, "x")); err == nil {
			t.Fatal("a failed step was recorded as done")
		}
	})

	t.Run("declaration errors", func(t *testing.T) {
		withRunner(t, &fakeRunner{})
		cases := map[string]xcomposeapp.InitStep{
			"no image":               {Name: "x"},
			"unknown when":           {Name: "x", Image: "busybox", When: "sometimes"},
			"unresolved variable":    {Name: "x", Image: "busybox", When: xcomposeapp.WhenAlways, Command: xcomposeapp.StrList{"${NOPE_NOT_SET}"}},
			"volume without a colon": {Name: "x", Image: "busybox", When: xcomposeapp.WhenAlways, Volumes: []string{"/DATA/AppData/app/db"}},
			"once without a name":    {Image: "", When: xcomposeapp.WhenOnce},
		}
		for name, step := range cases {
			if err := RunInit(context.Background(), cfg, "app", dir, xcomposeapp.PhasePreUp,
				[]xcomposeapp.InitStep{step}, map[string]string{}); err == nil {
				t.Errorf("%s: want a declaration error", name)
			}
		}
	})
}

func TestRunInitNoStepsNeverTouchesDocker(t *testing.T) {
	prev := newRunner
	newRunner = func() (Runner, error) { return nil, errors.New("must not connect") }
	t.Cleanup(func() { newRunner = prev })

	cfg, dir := dataRootApp(t, "app", "")
	if err := RunInit(context.Background(), cfg, "app", dir, xcomposeapp.PhasePreUp, nil, map[string]string{}); err != nil {
		t.Fatalf("an app with no init steps must converge without a daemon: %v", err)
	}
	// A step declared for the other phase must not connect either.
	steps := []xcomposeapp.InitStep{{Name: "x", Image: "busybox", Phase: xcomposeapp.PhasePostUp}}
	if err := RunInit(context.Background(), cfg, "app", dir, xcomposeapp.PhasePreUp, steps, map[string]string{}); err != nil {
		t.Fatalf("no step due in this phase: %v", err)
	}
}
