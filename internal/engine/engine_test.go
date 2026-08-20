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
