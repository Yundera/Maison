package stackup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/config"
)

// fixtureHookBin points hookBinDir at a directory containing only the named
// commands, so a test can assert what a hook can and cannot reach without
// depending on what the machine running the test happens to have installed.
func fixtureHookBin(t *testing.T, allow ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range allow {
		real, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("no %s on PATH", name)
		}
		if err := os.Symlink(real, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	prev := hookBinDir
	hookBinDir = dir
	t.Cleanup(func() { hookBinDir = prev })
}

func hookTestApp(t *testing.T) (config.Config, string) {
	t.Helper()
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no /bin/bash")
	}
	cfg := testConfig(t)
	cfg.DataHostPath = "/hostroot"
	dir := filepath.Join(cfg.DataRoot, "AppData", "app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return cfg, dir
}

// A command on the curated list works; the hook is not otherwise constrained.
func TestRunHookAllowsCuratedCommands(t *testing.T) {
	fixtureHookBin(t, "cat")
	cfg, dir := hookTestApp(t)
	if err := RunHook(t.Context(), cfg, "app", dir, "cat /dev/null"); err != nil {
		t.Fatalf("curated command rejected: %v", err)
	}
}

// A command the machine HAS but the curated list withholds is rejected. This is
// the case that matters most: sysctl, ip, mount and adduser are all present in
// the runtime image and all silently act on the wrong namespace.
func TestRunHookRejectsWithheldCommand(t *testing.T) {
	fixtureHookBin(t, "cat")
	cfg, dir := hookTestApp(t)
	err := RunHook(t.Context(), cfg, "app", dir, "sysctl -w net.ipv4.ip_forward=1")
	if err == nil {
		t.Fatal("withheld command was allowed")
	}
	if !strings.Contains(err.Error(), "sysctl") {
		t.Errorf("error does not name the command: %v", err)
	}
	// The message is the documentation: it must carry the recipe, not just a refusal.
	if !strings.Contains(err.Error(), "--privileged") {
		t.Errorf("error does not carry the sanctioned recipe: %v", err)
	}
}

// The regression this whole mechanism exists for: a missing command inside a
// command substitution leaves the substitution empty and the hook exits 0. Two
// shipped apps wrote empty secrets this way and installed green.
func TestRunHookRejectsInsideCommandSubstitution(t *testing.T) {
	fixtureHookBin(t, "cat")
	cfg, dir := hookTestApp(t)
	script := `SECRET="$(openssl rand -hex 32)"
printf 'secret=%s\n' "$SECRET" > /dev/null
exit 0`
	err := RunHook(t.Context(), cfg, "app", dir, script)
	if err == nil {
		t.Fatal("hook exited 0 with an empty secret and was accepted")
	}
	if !strings.Contains(err.Error(), "openssl") {
		t.Errorf("error does not name the command: %v", err)
	}
}

// Same, one level deeper: the rejection is recorded by appending to a file, so
// it survives the subshell that discards the handler's exit status.
func TestRunHookRejectsInsideSubshell(t *testing.T) {
	fixtureHookBin(t, "cat")
	cfg, dir := hookTestApp(t)
	if err := RunHook(t.Context(), cfg, "app", dir, "( ip link show ) || true\nexit 0"); err == nil {
		t.Fatal("rejection inside a subshell was lost")
	}
}

// Every rejected command is reported, once each, so an author fixing a hook
// sees the whole list rather than discovering them one install at a time.
func TestRunHookReportsEachRejectionOnce(t *testing.T) {
	fixtureHookBin(t, "cat")
	cfg, dir := hookTestApp(t)
	err := RunHook(t.Context(), cfg, "app", dir, "openssl x; curl y; openssl z\nexit 0")
	if err == nil {
		t.Fatal("rejections were lost")
	}
	msg := err.Error()
	if !strings.Contains(msg, "openssl") || !strings.Contains(msg, "curl") {
		t.Errorf("not every rejected command reported: %v", err)
	}
	if strings.Count(msg[:strings.Index(msg, ", which app hooks")], "openssl") != 1 {
		t.Errorf("duplicate rejection reported: %v", err)
	}
}

// With no curated directory present — the binary running outside its image, as
// in local development — hooks fall back to the ordinary PATH rather than
// failing every command.
func TestHookPathFallsBackWhenUncurated(t *testing.T) {
	prev := hookBinDir
	hookBinDir = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { hookBinDir = prev })
	if got := hookPath(); got != hookFallbackPath {
		t.Errorf("hookPath() = %q, want the fallback", got)
	}
}

func TestRejectedCommandsDedupes(t *testing.T) {
	f := filepath.Join(t.TempDir(), "rejected")
	if err := os.WriteFile(f, []byte("openssl\ncurl\nopenssl\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := rejectedCommands(f)
	want := []string{"openssl", "curl"}
	if len(got) != len(want) {
		t.Fatalf("rejectedCommands() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rejectedCommands() = %v, want %v", got, want)
		}
	}
}
