package apps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/secretgen"
	"github.com/yundera/maison/internal/stackup"
	"github.com/yundera/maison/internal/xcomposeapp"
)

const changeBase = `services: {}
x-compose-app:
  secrets:
    SMB_PASSWORD: ${APP_DEFAULT_PASSWORD}|password:24
    SMB_HASH: bcrypt:${SMB_PASSWORD}
    OTHER: hex:16
  credentials:
    - label: {en_us: SMB}
      username: admin
      password: ${SMB_PASSWORD}
      change:
        apply: env
        policy: {min_length: 10}
    - label: {en_us: Shared}
      username: admin
      password: ${APP_DEFAULT_PASSWORD}
      change: {apply: env}
    - label: {en_us: Composite}
      password: pre-${SMB_PASSWORD}
      change: {apply: env}
    - label: {en_us: Initial}
      username: root
      password: ${SMB_PASSWORD}
    - label: {en_us: Hooked}
      username: admin
      password: ${SMB_PASSWORD}
      change:
        apply: script
        script: echo "$SMB_PASSWORD"
    - label: {en_us: Broken}
      password: ${SMB_PASSWORD}
      change: {apply: script}
`

const changeEnv = "# hand comment\nSMB_PASSWORD=old-password-1\nSMB_HASH=$2a$10$xyz\nOTHER=abcd\n"

// fakeChange swaps the Docker-facing seams for the test's duration.
type fakeChange struct {
	ups     int
	upErr   []error // returned by successive ups; nil once exhausted
	hookErr error
	hookEnv []string
	running bool
}

func withFakeChange(t *testing.T, f *fakeChange) {
	t.Helper()
	oldScript, oldUp, oldRun := credentialScript, credentialUp, credentialAppRunning
	credentialScript = func(_ context.Context, _ config.Config, _, _ string, _ xcomposeapp.Script, o stackup.ScriptOpts) (stackup.ScriptResult, error) {
		f.hookEnv = o.Extra
		return stackup.ScriptResult{}, f.hookErr
	}
	credentialUp = func(context.Context, config.Config, string, string, []string) error {
		f.ups++
		if len(f.upErr) > 0 {
			err := f.upErr[0]
			f.upErr = f.upErr[1:]
			return err
		}
		return nil
	}
	credentialAppRunning = func(*Registry, context.Context, string) bool { return f.running }
	t.Cleanup(func() { credentialScript, credentialUp, credentialAppRunning = oldScript, oldUp, oldRun })
}

func readEnv(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newChangeApp(t *testing.T) (*Registry, string) {
	r := New(config.Config{DataRoot: t.TempDir()}, nil)
	dir := seedCredentialsApp(t, r, "samba", changeBase, "", changeEnv)
	return r, dir
}

func TestRenderedCredentialsChangeInfo(t *testing.T) {
	r, _ := newChangeApp(t)
	got, err := r.RenderedCredentials("samba")
	if err != nil {
		t.Fatal(err)
	}
	byLabel := map[string]RenderedCredential{}
	for _, c := range got {
		byLabel[c.Label.Value()] = c
	}
	if c := byLabel["SMB"]; c.Index != 0 || c.Change == nil || !c.Change.Changeable || c.Change.Policy.MinLength != 10 ||
		!strings.Contains(c.Change.Policy.Forbid, "$") {
		t.Errorf("SMB = %+v / %+v", c, c.Change)
	}
	if c := byLabel["Shared"]; c.Change == nil || c.Change.Changeable || c.Change.Reason != ReasonShared {
		t.Errorf("Shared = %+v", c.Change)
	}
	if c := byLabel["Composite"]; c.Change == nil || c.Change.Changeable || c.Change.Reason != ReasonTemplate {
		t.Errorf("Composite = %+v", c.Change)
	}
	if c := byLabel["Initial"]; c.Change != nil {
		t.Errorf("Initial carries a change: %+v", c.Change)
	}
	if c := byLabel["Broken"]; c.Change != nil {
		t.Errorf("a script change without a script must be dropped: %+v", c.Change)
	}
	if c := byLabel["Hooked"]; c.Index != 4 || c.Change == nil || c.Change.Apply != "script" {
		t.Errorf("Hooked = %+v", c)
	}
}

func TestChangeCredentialEnv(t *testing.T) {
	r, dir := newChangeApp(t)
	f := &fakeChange{}
	withFakeChange(t, f)

	if err := r.ChangeCredential(context.Background(), "samba", 0, "new-password-2"); err != nil {
		t.Fatal(err)
	}
	vars := envinject.EnvFileVars([]byte(readEnv(t, dir)))
	if vars["SMB_PASSWORD"] != "new-password-2" {
		t.Errorf("SMB_PASSWORD = %q", vars["SMB_PASSWORD"])
	}
	if vars["SMB_HASH"] != "" {
		t.Errorf("dependent bcrypt not emptied for regeneration: %q", vars["SMB_HASH"])
	}
	if vars["OTHER"] != "abcd" || !strings.HasPrefix(readEnv(t, dir), "# hand comment\n") {
		t.Errorf("unrelated content changed:\n%s", readEnv(t, dir))
	}
	if f.ups != 1 {
		t.Errorf("ups = %d, want 1", f.ups)
	}
}

func TestChangeCredentialEnvRollsBack(t *testing.T) {
	r, dir := newChangeApp(t)
	f := &fakeChange{upErr: []error{errors.New("container exited: password was new-password-2")}}
	withFakeChange(t, f)

	err := r.ChangeCredential(context.Background(), "samba", 0, "new-password-2")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "new-password-2") || strings.Contains(err.Error(), "old-password-1") {
		t.Errorf("error not redacted: %v", err)
	}
	if got := readEnv(t, dir); got != changeEnv {
		t.Errorf(".env not restored:\n%s", got)
	}
	if f.ups != 2 {
		t.Errorf("ups = %d, want the failed one plus the revert", f.ups)
	}
}

func TestChangeCredentialPolicy(t *testing.T) {
	r, dir := newChangeApp(t)
	withFakeChange(t, &fakeChange{})

	err := r.ChangeCredential(context.Background(), "samba", 0, "sh$rt")
	var pe *PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a PolicyError", err)
	}
	if strings.Contains(err.Error(), "sh$rt") {
		t.Errorf("policy error echoes the value: %v", err)
	}
	want := map[string]bool{secretgen.RuleTooShort: true, secretgen.RuleForbidden: true}
	for _, rule := range pe.Rules {
		delete(want, rule)
	}
	if len(want) > 0 {
		t.Errorf("rules = %v, missing %v", pe.Rules, want)
	}
	if readEnv(t, dir) != changeEnv {
		t.Error(".env written on a refused value")
	}
}

func TestChangeCredentialRefusals(t *testing.T) {
	r, _ := newChangeApp(t)
	withFakeChange(t, &fakeChange{})
	ctx := context.Background()

	for _, idx := range []int{1, 2, 3, 5} {
		if err := r.ChangeCredential(ctx, "samba", idx, "new-password-2"); !errors.Is(err, ErrNotChangeable) {
			t.Errorf("index %d: err = %v, want ErrNotChangeable", idx, err)
		}
	}
	if err := r.ChangeCredential(ctx, "samba", 9, "new-password-2"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("out of range: %v", err)
	}
	if err := r.ChangeCredential(ctx, "..", 0, "new-password-2"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("bad id: %v", err)
	}
}

func TestChangeCredentialHook(t *testing.T) {
	r, dir := newChangeApp(t)
	f := &fakeChange{running: true}
	withFakeChange(t, f)

	if err := r.ChangeCredential(context.Background(), "samba", 4, "new-password-2"); err != nil {
		t.Fatal(err)
	}
	env := strings.Join(f.hookEnv, "\n")
	for _, want := range []string{"SMB_PASSWORD=new-password-2", "OLD_SMB_PASSWORD=old-password-1"} {
		if !strings.Contains(env, want) {
			t.Errorf("hook env lacks %q: %v", want, f.hookEnv)
		}
	}
	if envinject.EnvFileVars([]byte(readEnv(t, dir)))["SMB_PASSWORD"] != "new-password-2" {
		t.Error(".env not updated after the hook")
	}
}

func TestChangeCredentialHookFailureChangesNothing(t *testing.T) {
	r, dir := newChangeApp(t)
	f := &fakeChange{running: true, hookErr: errors.New("exit status 1: new-password-2 rejected (was old-password-1)")}
	withFakeChange(t, f)

	err := r.ChangeCredential(context.Background(), "samba", 4, "new-password-2")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "new-password-2") || strings.Contains(err.Error(), "old-password-1") {
		t.Errorf("error not redacted: %v", err)
	}
	if readEnv(t, dir) != changeEnv || f.ups != 0 {
		t.Errorf(".env written or app recreated after a failed hook (ups=%d)", f.ups)
	}
}

func TestChangeCredentialHookNeedsRunningApp(t *testing.T) {
	r, _ := newChangeApp(t)
	withFakeChange(t, &fakeChange{running: false})
	if err := r.ChangeCredential(context.Background(), "samba", 4, "new-password-2"); !errors.Is(err, ErrAppNotRunning) {
		t.Fatalf("err = %v, want ErrAppNotRunning", err)
	}
}

func TestChangeCredentialHookKeepsNewValueWhenRestartFails(t *testing.T) {
	r, dir := newChangeApp(t)
	withFakeChange(t, &fakeChange{running: true, upErr: []error{errors.New("boom")}})
	if err := r.ChangeCredential(context.Background(), "samba", 4, "new-password-2"); err == nil {
		t.Fatal("want an error")
	}
	if envinject.EnvFileVars([]byte(readEnv(t, dir)))["SMB_PASSWORD"] != "new-password-2" {
		t.Error("hook mode must keep the new value: the app already holds it")
	}
}

func TestGenerateCredentialMeetsPolicy(t *testing.T) {
	r, dir := newChangeApp(t)
	v, err := r.GenerateCredential("samba", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 24 {
		t.Errorf("len = %d, want the secret's own password:24", len(v))
	}
	if readEnv(t, dir) != changeEnv {
		t.Error("generate must not write")
	}
}

// The real script runner sees the extra variables over the app's .env.
func TestRunScriptExtraOverridesEnv(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("X=old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := stackup.RunScript(context.Background(), cfg, "app", dir,
		xcomposeapp.Script{Script: `test "$X" = new && test "$OLD_X" = old`},
		stackup.ScriptOpts{Extra: []string{"X=new", "OLD_X=old"}})
	if err != nil {
		t.Fatalf("script did not see the overridden value: %v", err)
	}
}
