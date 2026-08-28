package engine

import (
	"bufio"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/yundera/maison/internal/config"
)

func newScanner(s string) *bufio.Scanner {
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Split(ScanProgress)
	return sc
}

func baseSpec() Spec {
	return Spec{
		Image:    "kopia/kopia:0.23.1",
		Name:     "maison-engine-jellyfin",
		Hostname: "pcs-test",
		Network:  NetworkNone,
		Args:     []string{"snapshot", "create", "/DATA/AppData/jellyfin"},
	}
}

func argvString(t *testing.T, s Spec) string {
	t.Helper()
	argv, err := Argv(s)
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	return strings.Join(argv, " ")
}

// The bind source must be the HOST's path while every path handed to the engine is
// this container's. On a real PCS the two are pinned identical, so getting it
// backwards is invisible in production and only breaks in dev — which is the worst
// possible place for a bug to hide, and the reason this is asserted exactly.
func TestDataMountTranslatesToTheHostPath(t *testing.T) {
	r := New(config.Config{DataRoot: "/DATA", DataHostPath: "/opt/maison/DATA"})
	s := baseSpec()
	s.Mounts = []Mount{r.DataMount(false)}

	if got, want := argvString(t, s), "-v /opt/maison/DATA:/DATA"; !strings.Contains(got, want) {
		t.Fatalf("argv = %s\nwant it to contain %q", got, want)
	}
	// The engine's own arguments stay in container spelling — they are resolved
	// inside the engine container, not by the daemon.
	if !strings.Contains(argvString(t, s), "/DATA/AppData/jellyfin") {
		t.Error("engine arguments were rewritten to host paths; only the mount source may be")
	}
}

// On a PCS the two paths are pinned equal, so the translation must be a no-op
// rather than doubling the prefix.
func TestDataMountIsANoOpWhenPathsMatch(t *testing.T) {
	r := New(config.Config{DataRoot: "/DATA", DataHostPath: "/DATA"})
	s := baseSpec()
	s.Mounts = []Mount{r.DataMount(true)}

	if got, want := argvString(t, s), "-v /DATA:/DATA:ro"; !strings.Contains(got, want) {
		t.Fatalf("argv = %s\nwant it to contain %q", got, want)
	}
}

// An engine container must never join the deployment's app network: there, a
// container's name is a reverse-DNS-attested identity that the auth-registrar turns
// into an OIDC client_id. The type is closed so this cannot be done by accident, and
// this test is what stops someone reopening it.
func TestArgvRefusesAnyNetworkButNoneOrDefault(t *testing.T) {
	for _, net := range []Network{"pcs", "host", "bridge"} {
		s := baseSpec()
		s.Network = net
		if _, err := Argv(s); err == nil {
			t.Errorf("Argv accepted --network %q", net)
		}
	}
	// The default bridge is spelled by omission, so no --network flag at all.
	s := baseSpec()
	s.Network = NetworkDefault
	if strings.Contains(argvString(t, s), "--network") {
		t.Error("the default network should be left unspecified, not passed explicitly")
	}
}

// Both are the handle on a run whose client has died, and the identity every
// snapshot is filed under. Neither has a safe default.
func TestArgvRequiresNameAndHostname(t *testing.T) {
	noName := baseSpec()
	noName.Name = ""
	if _, err := Argv(noName); err == nil {
		t.Error("Argv accepted a spec with no container name")
	}
	noHost := baseSpec()
	noHost.Hostname = ""
	if _, err := Argv(noHost); err == nil {
		t.Error("Argv accepted a spec with no hostname pin")
	}
}

// Capabilities under a non-root --user are inert: Docker has no --ambient-cap, so the
// effective set of a process that starts under a non-zero uid is empty. Accepting the
// pair would let a spec read as though it could open a file it cannot — which is the
// shape of the bug that made backing up any app with a bundled database impossible.
// Verified against Docker 28: --user 1000:1000 --cap-add DAC_READ_SEARCH still gets
// EACCES on a 0700 directory owned by another uid.
func TestArgvRefusesCapabilitiesUnderANonRootUser(t *testing.T) {
	s := baseSpec()
	s.User = "1000:1000"
	s.Caps = Caps{Drop: []string{"ALL"}, Add: []string{"DAC_READ_SEARCH"}}
	if _, err := Argv(s); err == nil {
		t.Fatal("Argv accepted --cap-add under a non-root --user, where it does nothing")
	}
	// Dropping capabilities from a non-root container is not the same mistake: it
	// takes away rather than pretending to give, and stays allowed.
	only := baseSpec()
	only.User = "1000:1000"
	only.Caps = Caps{Drop: []string{"ALL"}}
	if _, err := Argv(only); err != nil {
		t.Fatalf("Argv rejected a --cap-drop-only spec: %v", err)
	}
	// Every spelling of uid 0 is root, whatever the gid.
	for _, user := range []string{"0:0", "0", "root:root", ""} {
		ok := baseSpec()
		ok.User = user
		ok.Caps = Caps{Add: []string{"DAC_READ_SEARCH"}}
		if _, err := Argv(ok); err != nil {
			t.Errorf("Argv rejected capabilities under --user %q: %v", user, err)
		}
	}
}

// Drops come before adds, and no-new-privileges is what stops anything the engine
// execs from regaining them.
func TestArgvRendersCapabilitiesAndNoNewPrivileges(t *testing.T) {
	s := baseSpec()
	s.User = "0:0"
	s.Caps = Caps{Drop: []string{"ALL"}, Add: []string{"DAC_READ_SEARCH", "CHOWN"}}
	s.NoNewPrivileges = true

	got := argvString(t, s)
	for _, want := range []string{
		"--user 0:0",
		"--cap-drop ALL --cap-add DAC_READ_SEARCH --cap-add CHOWN",
		"--security-opt no-new-privileges",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("argv = %s\nwant it to contain %q", got, want)
		}
	}
	// A spec that asks for neither must not grow flags it did not ask for.
	bare := argvString(t, baseSpec())
	if strings.Contains(bare, "--cap-") || strings.Contains(bare, "--security-opt") {
		t.Errorf("argv = %s\nwant no capability flags on a spec that set none", bare)
	}
}

// A repository password must not be readable in the process table.
func TestSecretsAppearByNameOnlyInArgv(t *testing.T) {
	s := baseSpec()
	s.Secrets = map[string]string{"KOPIA_PASSWORD": "hunter2"}

	got := argvString(t, s)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("the secret's value leaked into argv: %s", got)
	}
	if !strings.Contains(got, "-e KOPIA_PASSWORD") {
		t.Fatalf("argv = %s\nwant it to pass the variable by name", got)
	}
}

// --rm alone is not enough; the name is what makes an abandoned container findable.
func TestArgvAlwaysRemovesAndNames(t *testing.T) {
	got := argvString(t, baseSpec())
	for _, want := range []string{"run --rm", "--name maison-engine-jellyfin", "--hostname pcs-test"} {
		if !strings.Contains(got, want) {
			t.Errorf("argv = %s\nwant it to contain %q", got, want)
		}
	}
	// The image must come before the engine's own arguments, or docker parses them.
	if i, j := strings.Index(got, "kopia/kopia:0.23.1"), strings.Index(got, "snapshot create"); i > j {
		t.Errorf("engine arguments precede the image: %s", got)
	}
}

// Engines redraw progress with \r and emit no newline until they finish. Splitting
// on newlines alone would deliver nothing until exit — indistinguishable from a hung
// backup for the whole duration of a working one.
func TestScanProgressSplitsOnCarriageReturns(t *testing.T) {
	in := "hashing 1%\rhashing 50%\rhashing 100%\ndone\n"
	sc := newScanner(in)
	var got []string
	for sc.Scan() {
		if s := strings.TrimSpace(sc.Text()); s != "" {
			got = append(got, s)
		}
	}
	want := []string{"hashing 1%", "hashing 50%", "hashing 100%", "done"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %v, want %v", got, want)
	}
}

// A final chunk with no terminator must still be delivered.
func TestScanProgressFlushesTheLastLine(t *testing.T) {
	sc := newScanner("no terminator")
	if !sc.Scan() || sc.Text() != "no terminator" {
		t.Fatalf("last unterminated line was dropped")
	}
}

func skipWithoutDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable")
	}
}

// The end-to-end shape, against a tiny image: stdout is returned, stderr is streamed
// line by line, and the secret reaches the container without passing through argv.
func TestRunStreamsStderrAndReturnsStdout(t *testing.T) {
	skipWithoutDocker(t)
	r := New(config.Config{DataRoot: "/DATA", DataHostPath: "/DATA"})

	var lines []string
	out, err := r.Run(context.Background(), Spec{
		Image:    "alpine:3.20",
		Name:     "maison-engine-test-stream",
		Hostname: "pcs-test",
		Network:  NetworkNone,
		Secrets:  map[string]string{"ENGINE_TEST_SECRET": "hunter2"},
		Args: []string{"sh", "-c",
			`printf 'progress a\rprogress b\n' >&2; printf '{"secret":"%s"}' "$ENGINE_TEST_SECRET"`},
		Timeout: 2 * time.Minute,
	}, func(l string) { lines = append(lines, l) })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := string(out); !strings.Contains(got, `{"secret":"hunter2"}`) {
		t.Errorf("stdout = %q, want the engine's result, with the secret delivered via the environment", got)
	}
	if len(lines) != 2 || lines[0] != "progress a" || lines[1] != "progress b" {
		t.Errorf("streamed stderr = %v, want both progress lines split on the carriage return", lines)
	}
}

// A failing engine must surface what it said, not just an exit status.
func TestRunReportsStderrOnFailure(t *testing.T) {
	skipWithoutDocker(t)
	r := New(config.Config{DataRoot: "/DATA", DataHostPath: "/DATA"})

	_, err := r.Run(context.Background(), Spec{
		Image:    "alpine:3.20",
		Name:     "maison-engine-test-fail",
		Hostname: "pcs-test",
		Network:  NetworkNone,
		Args:     []string{"sh", "-c", "echo 'repository is not connected' >&2; exit 3"},
		Timeout:  2 * time.Minute,
	}, nil)
	if err == nil {
		t.Fatal("Run should have failed")
	}
	if !strings.Contains(err.Error(), "repository is not connected") {
		t.Errorf("error = %v, want it to quote the engine's own message", err)
	}
}

// The invariant the whole package is shaped around: when a run is cut short, the
// container must be gone afterwards. Killing the docker client alone leaves it
// running — still holding the app's files — while Maison restarts the app and
// reports that the backup merely failed.
func TestRunLeavesNoContainerBehindOnTimeout(t *testing.T) {
	skipWithoutDocker(t)
	r := New(config.Config{DataRoot: "/DATA", DataHostPath: "/DATA"})
	const name = "maison-engine-test-timeout"
	_ = exec.Command("docker", "rm", "-f", name).Run()

	start := time.Now()
	_, err := r.Run(context.Background(), Spec{
		Image:    "alpine:3.20",
		Name:     name,
		Hostname: "pcs-test",
		Network:  NetworkNone,
		Args:     []string{"sleep", "120"},
		Timeout:  5 * time.Second,
	}, nil)
	if err == nil {
		t.Fatal("Run should have timed out")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want it to name the timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 60*time.Second {
		t.Errorf("Run took %s; the timeout did not bound it", elapsed)
	}

	// Give the daemon a moment to finish the forced removal, then assert it is gone.
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, _ := exec.Command("docker", "ps", "-a", "--filter", "name="+name, "--format", "{{.Names}}").Output()
		if strings.TrimSpace(string(out)) == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %s survived the timeout: %q", name, strings.TrimSpace(string(out)))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Cancelling the caller's context must be distinguishable from hitting the timeout,
// since one is an operator stopping work and the other is a repository hanging.
func TestRunReportsCancellationDistinctly(t *testing.T) {
	skipWithoutDocker(t)
	r := New(config.Config{DataRoot: "/DATA", DataHostPath: "/DATA"})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(3 * time.Second); cancel() }()

	_, err := r.Run(ctx, Spec{
		Image:    "alpine:3.20",
		Name:     "maison-engine-test-cancel",
		Hostname: "pcs-test",
		Network:  NetworkNone,
		Args:     []string{"sleep", "120"},
		Timeout:  5 * time.Minute,
	}, nil)
	if err == nil {
		t.Fatal("Run should have been cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
}

// Env exists to beat environment an image bakes into itself, so the values have to
// reach argv — unlike Secrets, which appear by name only.
func TestEnvAppearsByValueAndSecretsDoNot(t *testing.T) {
	argv, err := Argv(Spec{
		Image:    "kopia/kopia:0.23.1",
		Name:     "n",
		Hostname: "h",
		Env:      map[string]string{"KOPIA_CACHE_DIRECTORY": "/DATA/AppDataShared/backup/kopia/cache"},
		Secrets:  map[string]string{"KOPIA_PASSWORD": "hunter2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "-e KOPIA_CACHE_DIRECTORY=/DATA/AppDataShared/backup/kopia/cache") {
		t.Errorf("Env value missing from argv: %v", argv)
	}
	if strings.Contains(joined, "hunter2") {
		t.Errorf("secret VALUE leaked into argv: %v", argv)
	}
	if !strings.Contains(joined, "-e KOPIA_PASSWORD") {
		t.Errorf("secret name missing from argv: %v", argv)
	}
}

func execArgvString(t *testing.T, s Spec) string {
	t.Helper()
	argv, err := ExecArgv(s)
	if err != nil {
		t.Fatalf("ExecArgv: %v", err)
	}
	return strings.Join(argv, " ")
}

func residentSpec() Spec {
	s := baseSpec()
	s.Container = "maison-engine"
	s.Entrypoint = "/bin/kopia"
	return s
}

// Each of these is a handle without which a cancelled exec cannot be cleaned up, or a
// command that cannot be assembled at all. None may be inferred silently.
func TestExecArgvRequiresItsHandles(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Spec)
		want string
	}{
		{"no container", func(s *Spec) { s.Container = "" }, "container"},
		{"no entrypoint", func(s *Spec) { s.Entrypoint = "" }, "entrypoint"},
		{"no name", func(s *Spec) { s.Name = "" }, "name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := residentSpec()
			tc.mut(&s)
			_, err := ExecArgv(s)
			if err == nil {
				t.Fatalf("ExecArgv should have refused a spec with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name the missing %s", err, tc.want)
			}
		})
	}
}

// The same rule as the one-shot path: a repository password must not be readable in
// the process table, whichever way the engine is reached.
func TestExecArgvPassesSecretsByNameOnly(t *testing.T) {
	s := residentSpec()
	s.Secrets = map[string]string{"KOPIA_PASSWORD": "hunter2"}

	got := execArgvString(t, s)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("the secret's value leaked into argv: %s", got)
	}
	if !strings.Contains(got, "-e KOPIA_PASSWORD") {
		t.Fatalf("argv = %s\nwant it to pass the variable by name", got)
	}
}

// The pid file is the exec-mode equivalent of --name, and it is only a handle on the
// *engine* if the shell hands its own process over rather than forking a child. Both
// halves are asserted: the pid is recorded first, and `exec` replaces the shell.
func TestExecArgvRecordsTheEnginesOwnPid(t *testing.T) {
	got := execArgvString(t, residentSpec())

	wrote := strings.Index(got, "echo $$ > '/run/maison-ops/maison-engine-jellyfin.pid'")
	handed := strings.Index(got, "exec '/bin/kopia'")
	if wrote < 0 {
		t.Fatalf("argv = %s\nwant it to record the pid under the run's name", got)
	}
	if handed < 0 {
		t.Fatalf("argv = %s\nwant the shell to exec the engine, so the recorded pid is the engine's", got)
	}
	if wrote > handed {
		t.Errorf("argv = %s\nwant the pid recorded before the shell is replaced", got)
	}
	// Failing to record it must abort: an engine nothing can cancel is worse than one
	// that never started.
	if !strings.Contains(got, "exit 97") {
		t.Errorf("argv = %s\nwant a run with no cancellation handle to refuse to start", got)
	}
}

// App names and snapshot stamps reach this shell from a repository, which the rest of
// the package treats as untrusted input. They must arrive as one word each.
func TestExecArgvQuotesEngineArguments(t *testing.T) {
	s := residentSpec()
	s.Args = []string{"snapshot", "create", "/DATA/AppData/od'; rm -rf /; #"}

	got := execArgvString(t, s)
	if strings.Contains(got, "rm -rf /;") && !strings.Contains(got, `'\''`) {
		t.Fatalf("argv = %s\nwant the quote in the argument escaped rather than closing the word", got)
	}
	if !strings.Contains(got, `'/DATA/AppData/od'\''; rm -rf /; #'`) {
		t.Errorf("argv = %s\nwant the whole argument to survive as a single quoted word", got)
	}
}

// Retrying a command that writes is only safe when nothing ran. That distinction is
// the daemon refusing to exec at all, and it must not be confused with the engine
// itself failing — which a retry would repeat.
func TestResidentGoneOnlyMatchesTheDaemonRefusing(t *testing.T) {
	for _, s := range []string{
		"Error response from daemon: No such container: maison-engine",
		"Error response from daemon: Container maison-engine is not running",
		"Error response from daemon: Container abc is paused",
	} {
		if !residentGone(s) {
			t.Errorf("residentGone(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"ERROR error connecting to repository: repository is not connected",
		"ERROR unable to open snapshot: no such snapshot",
		"",
	} {
		if residentGone(s) {
			t.Errorf("residentGone(%q) = true, want false — the engine failed, a retry would repeat it", s)
		}
	}
}

// A box with no resident engine is the ordinary case, not an error: the command must
// still run, in a one-shot container.
func TestRunFallsBackWhenThereIsNoResidentContainer(t *testing.T) {
	skipWithoutDocker(t)
	r := New(config.Config{DataRoot: "/DATA", DataHostPath: "/DATA"})

	out, err := r.Run(context.Background(), Spec{
		Image:      "alpine:3.20",
		Name:       "maison-engine-test-fallback",
		Hostname:   "pcs-test",
		Network:    NetworkNone,
		Container:  "maison-engine-test-does-not-exist",
		Entrypoint: "/bin/sh",
		Args:       []string{"sh", "-c", `printf 'fell-back'`},
		Timeout:    2 * time.Minute,
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := string(out); !strings.Contains(got, "fell-back") {
		t.Errorf("stdout = %q, want the command to have run anyway", got)
	}
}

// startResident brings up a throwaway container to exec into, and removes it after.
func startResident(t *testing.T, name, hostname string) {
	t.Helper()
	_ = exec.Command("docker", "rm", "-f", name).Run()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm",
		"--name", name, "--hostname", hostname, "--network", "none",
		"alpine:3.20", "sleep", "600").CombinedOutput()
	if err != nil {
		t.Skipf("could not start a resident container: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
}

// The exec-mode half of the invariant this package is shaped around. Cancelling kills
// the docker client; without the pid file the engine would keep running inside the
// resident container, holding the app's files, while Maison restarts the app and
// reports a merely failed backup.
func TestRunLeavesNoEngineBehindOnTimeoutInExecMode(t *testing.T) {
	skipWithoutDocker(t)
	const name = "maison-engine-test-resident"
	startResident(t, name, "pcs-test")
	r := New(config.Config{DataRoot: "/DATA", DataHostPath: "/DATA"})

	_, err := r.Run(context.Background(), Spec{
		Image:      "alpine:3.20",
		Name:       "maison-engine-test-exec-timeout",
		Hostname:   "pcs-test",
		Network:    NetworkNone,
		Container:  name,
		Entrypoint: "/bin/sleep",
		Args:       []string{"120"},
		Timeout:    5 * time.Second,
	}, nil)
	if err == nil {
		t.Fatal("Run should have timed out")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ps, psErr := exec.CommandContext(ctx, "docker", "exec", name, "ps").CombinedOutput()
	if psErr != nil {
		t.Fatalf("listing processes in %s: %v: %s", name, psErr, ps)
	}
	if strings.Contains(string(ps), "sleep 120") {
		t.Errorf("the engine outlived its run:\n%s", ps)
	}
}

// A resident container whose hostname disagrees with the repository's would file every
// snapshot under a second identity — one repository, two lineages, noticed only when a
// restore comes back empty. Latency is never worth that, so the run goes one-shot.
func TestResidentContainerWithTheWrongHostnameIsNotUsed(t *testing.T) {
	skipWithoutDocker(t)
	const name = "maison-engine-test-wrong-host"
	startResident(t, name, "some-other-box")
	r := New(config.Config{DataRoot: "/DATA", DataHostPath: "/DATA"})

	if r.residentUsable(context.Background(), Spec{Container: name, Hostname: "pcs-test"}) {
		t.Fatal("a container pinned to another hostname must not be used")
	}
	if !r.residentUsable(context.Background(), Spec{Container: name, Hostname: "some-other-box"}) {
		t.Error("the matching hostname should have been accepted")
	}
}
