package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/config"
)

// writeDescriptor renders an adapter.json the way the host side would.
func writeDescriptor(t *testing.T, cfg config.Config, engine string, d map[string]any) {
	t.Helper()
	dir := cfg.BackupEngineDir(engine)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DescriptorFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Discovery is the whole of "which engines does this box have". A directory with no
// descriptor is not an engine, and that is the ordinary state of a box whose host side
// has not run — not an error.
func TestDiscoverFindsOnlyDeclaredAdapters(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}

	// An engine directory with configuration but no descriptor: present, not adapted.
	if err := os.MkdirAll(cfg.BackupEngineDir("kopia"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Discover(cfg); len(got) != 0 {
		t.Fatalf("Discover = %v, want nothing: a directory is not a declaration", got)
	}

	writeDescriptor(t, cfg, "kopia", map[string]any{
		"engineId": "kopia", "image": "ghcr.io/yundera/maison-kopia-engine:0.1.0",
		"container": "kopia-engine", "hostname": "pcs-test",
	})
	got := Discover(cfg)
	if len(got) != 1 || got[0].EngineID != "kopia" {
		t.Fatalf("Discover = %+v, want one kopia adapter", got)
	}
	if got[0].Container != "kopia-engine" || got[0].Hostname != "pcs-test" {
		t.Errorf("descriptor lost fields: %+v", got[0])
	}
}

// A descriptor that cannot be used must not stop the others being registered, and must
// not stop Maison booting.
func TestDiscoverSkipsUnusableDescriptors(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}

	writeDescriptor(t, cfg, "good", map[string]any{"engineId": "good", "image": "img:1"})
	// No image: nothing to run.
	writeDescriptor(t, cfg, "noimage", map[string]any{"engineId": "noimage"})
	// Claims an engine other than the directory it sits in — it would be read out of
	// one engine's configuration and write into another's.
	writeDescriptor(t, cfg, "liar", map[string]any{"engineId": "elsewhere", "image": "img:1"})

	dir := cfg.BackupEngineDir("broken")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DescriptorFile), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := Discover(cfg)
	if len(got) != 1 || got[0].EngineID != "good" {
		t.Fatalf("Discover = %+v, want only the usable one", got)
	}
}

// Discovery reads both layouts for as long as the engine directory is moving out of
// AppDataShared and into the engine's own app folder — the host side moves the files
// in a release of its own, so a box can be in either state, or briefly in both.
//
// The app folder wins when both hold a descriptor, matching config.BackupEngineDir.
// If the two disagreed, an engine would be constructed from one directory and then
// read its repository configuration out of the other.
func TestDiscoverReadsBothLayoutsAndPrefersTheAppFolder(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}

	writeDescriptorAt(t, filepath.Join(cfg.SharedDir(), "backup", "restic"),
		map[string]any{"engineId": "restic", "image": "restic:legacy"})
	if got := Discover(cfg); len(got) != 1 || got[0].Image != "restic:legacy" {
		t.Fatalf("Discover = %+v, want the engine still in the shared directory", got)
	}

	writeDescriptorAt(t, cfg.EngineDir("restic"),
		map[string]any{"engineId": "restic", "image": "restic:moved"})
	got := Discover(cfg)
	if len(got) != 1 {
		t.Fatalf("Discover = %+v, want one engine and not the same one twice", got)
	}
	if got[0].Image != "restic:moved" {
		t.Errorf("Discover found %q, want the app folder to win over the shared directory", got[0].Image)
	}
}

// writeDescriptorAt renders an adapter.json into a named directory, for the tests that
// have to be explicit about which of the two layouts they are writing.
func writeDescriptorAt(t *testing.T, dir string, d map[string]any) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DescriptorFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The entrypoint has to be named: `docker exec` does not apply the image's own.
func TestDescriptorDefaults(t *testing.T) {
	d := Descriptor{EngineID: "kopia", Image: "img:1"}
	if d.entrypoint() != defaultEntrypoint {
		t.Errorf("entrypoint = %q, want the default", d.entrypoint())
	}
	if got := (Descriptor{Entrypoint: "/bin/other"}).entrypoint(); got != "/bin/other" {
		t.Errorf("entrypoint = %q, want the declared one", got)
	}
	// A repository on a local filesystem needs no network and must not be given one.
	if got := (Descriptor{Network: "none"}).network(); got != "none" {
		t.Errorf("network = %q, want none", got)
	}
	if got := (Descriptor{}).network(); got != "" {
		t.Errorf("network = %q, want the default bridge", got)
	}
}

// Progress is fed through as it arrives; the single result line is what comes back.
func TestDecodeSeparatesProgressFromTheResult(t *testing.T) {
	out := []byte(`{"type":"progress","message":"estimating","pct":-1}
{"type":"progress","message":"uploading","pct":42.5,"done":10,"total":100}
this line is not JSON and must not fail the verb
{"type":"log","level":"warn","message":"cache rebuilt"}
{"type":"result","result":{"configured":true,"connected":true,"identity":"pcs@abc"}}`)

	var events []apps.Event
	raw, err := decode(out, func(e apps.Event) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d progress events, want 2: %+v", len(events), events)
	}
	if events[0].Pct != apps.PctUnknown {
		t.Errorf("pct = %v, want unknown", events[0].Pct)
	}
	if events[1].Pct != 42.5 || events[1].Done != 10 || events[1].Total != 100 {
		t.Errorf("event = %+v, want the reported figures", events[1])
	}

	var st wireStatus
	if err := unmarshalResult(raw, &st); err != nil {
		t.Fatal(err)
	}
	if !st.Configured || !st.Connected || st.Identity != "pcs@abc" {
		t.Errorf("status = %+v", st)
	}
}

// A verb that is supposed to answer and did not is a protocol failure, not an empty
// answer — an empty list would read as "this engine holds no backups".
func TestAnAbsentResultIsAnError(t *testing.T) {
	raw, err := decode([]byte(`{"type":"progress","message":"working","pct":-1}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var list []wireBackup
	if err := unmarshalResult(raw, &list); err == nil {
		t.Error("an absent result decoded as an empty answer")
	}
}

// A stamp that came back from a repository is untrusted input and is re-validated
// before it becomes a Backup that feeds path construction.
func TestAnUnusableStampIsDropped(t *testing.T) {
	if _, ok := (wireBackup{Stamp: "../../etc/passwd"}).backup("jellyfin", "kopia"); ok {
		t.Error("a traversal stamp was accepted")
	}
	b, ok := (wireBackup{Stamp: "2026-02-03_120000", Size: 42}).backup("jellyfin", "kopia")
	if !ok {
		t.Fatal("a valid stamp was rejected")
	}
	if b.Tier != apps.TierRemote || b.Engine != "kopia" || b.Size != 42 {
		t.Errorf("backup = %+v, want tier/engine/size filled in", b)
	}
}

// The user-data set must never be returned as an app: it has no tile, no folder and no
// per-app page for the global list to link it to.
func TestAppOfRejectsWhatIsNotAnApp(t *testing.T) {
	for _, id := range []string{userDataID, "app:" + apps.UserDataApp, "app:../x", "jellyfin", "app:"} {
		if app, ok := appOf(id); ok {
			t.Errorf("appOf(%q) = %q, want a refusal", id, app)
		}
	}
	if app, ok := appOf("app:jellyfin"); !ok || app != "jellyfin" {
		t.Errorf("appOf(app:jellyfin) = %q, %v", app, ok)
	}
}

// The rebuilt box arrives as a status, not as a failure: configured=false and exit 0,
// with needsRecovery set. Losing the field in the decode would turn "every backup you
// own is here and needs your key" into the words for a fresh box.
func TestStatusCarriesNeedsRecovery(t *testing.T) {
	raw, err := decode([]byte(`{"type":"result","result":{"configured":false,"connected":false,"detail":"storage holds a repository this box has no key for","needsRecovery":true}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var st wireStatus
	if err := unmarshalResult(raw, &st); err != nil {
		t.Fatal(err)
	}
	if !st.NeedsRecovery || st.Configured || st.Connected {
		t.Errorf("status = %+v, want an unconfigured engine asking for its key", st)
	}

	// And an adapter that predates the field says nothing, which must read as "no".
	raw, _ = decode([]byte(`{"type":"result","result":{"configured":true,"connected":true}}`), nil)
	st = wireStatus{}
	if err := unmarshalResult(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.NeedsRecovery {
		t.Error("an old adapter's status read as needing recovery")
	}
}

// Caps.Recover is what decides whether the page offers the key form at all, so an
// adapter that predates the verb must come out false and a new one true.
func TestCapsCarryRecover(t *testing.T) {
	raw, err := decode([]byte(`{"type":"result","result":{"engineId":"kopia","protocol":"`+Protocol+`","offsite":true,"recover":true}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var wc wireCaps
	if err := unmarshalResult(raw, &wc); err != nil {
		t.Fatal(err)
	}
	if c := wc.caps(); !c.Recover || !c.Offsite {
		t.Errorf("caps = %+v, want recover carried through", c)
	}
	if c := (wireCaps{Offsite: true}).caps(); c.Recover {
		t.Error("an adapter that never said recover was taken to support it")
	}
}

// Exit 14 is the user's typo, and has to arrive as a sentinel the route can turn into
// a 400 — not as a generic failure that would read as a broken engine.
func TestExit14IsTheWrongKey(t *testing.T) {
	err := exec.Command("sh", "-c", "exit 14").Run()
	if err == nil {
		t.Skip("no shell to produce an exit code with")
	}
	if got := classify(err); !errors.Is(got, ErrWrongKey) || !errors.Is(got, apps.ErrWrongKey) {
		t.Errorf("classify(exit 14) = %v, want the wrong-key sentinel", got)
	}
	// The neighbours keep their meaning.
	err = exec.Command("sh", "-c", "exit 10").Run()
	if got := classify(err); !errors.Is(got, apps.ErrNotConfigured) {
		t.Errorf("classify(exit 10) = %v, want not configured", got)
	}
}

// The help link is the host's to name, read from the same state file as the label.
func TestReadStateCarriesTheRecoveryHelpURL(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	p := New(cfg, Descriptor{EngineID: "kopia", Image: "img:1"})
	if err := os.MkdirAll(p.dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"label":"Yundera Backup Storage","recoveryHelpUrl":"https://app.example/dashboard/backup"}`
	if err := os.WriteFile(filepath.Join(p.dir(), "state.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	st := p.readState()
	if st.Label != "Yundera Backup Storage" || st.RecoveryHelpURL != "https://app.example/dashboard/backup" {
		t.Errorf("state = %+v", st)
	}
}

// The candidate is the user's secret in a file: written whole, readable by its owner
// alone, and never half there for the adapter to read.
func TestWriteSecretIsPrivateAndWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), candidateFile)
	if err := writeSecret(path, "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "correct horse battery staple" {
		t.Errorf("candidate = %q", b)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("candidate mode = %v, want 0600", fi.Mode().Perm())
	}
	// No temporary left beside it.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("found %d files, want only the candidate", len(entries))
	}
}

// An empty key never reaches the engine, and leaves nothing on disk.
func TestRecoverRefusesAnEmptyKeyWithoutWritingIt(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	p := New(cfg, Descriptor{EngineID: "kopia", Image: "img:1"})
	if err := os.MkdirAll(p.dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Recover(context.Background(), "  \n"); !errors.Is(err, ErrWrongKey) {
		t.Errorf("Recover(blank) = %v, want the wrong-key sentinel", err)
	}
	if _, err := os.Stat(filepath.Join(p.dir(), candidateFile)); !os.IsNotExist(err) {
		t.Error("a blank key left a candidate file behind")
	}
}

// The secret block names a file inside the engine directory and nothing else: what
// Maison shows and mails must be what that directory holds.
func TestDescriptorSecretIsAPlainFileName(t *testing.T) {
	for _, tc := range []struct {
		file string
		ok   bool
	}{{"repository.password", true}, {"", false}, {"../escape", false}, {"sub/key", false}, {"..", false}, {".", false}} {
		d := Descriptor{EngineID: "kopia", Image: "img:1", Secret: &SecretDescriptor{File: tc.file}}
		if err := d.validate(); (err == nil) != tc.ok {
			t.Errorf("secret file %q: validate = %v, want ok=%v", tc.file, err, tc.ok)
		}
	}
}

// A descriptor that declares its secret hands it over verbatim; one written before the
// block existed declares nothing, and the caller falls back.
func TestProviderReportsTheDeclaredSecret(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	if _, ok := New(cfg, Descriptor{EngineID: "kopia", Image: "img:1"}).Secret(); ok {
		t.Error("a descriptor without a secret block declared one")
	}
	p := New(cfg, Descriptor{EngineID: "kopia", Image: "img:1",
		Secret: &SecretDescriptor{Label: "Kopia repository password", File: "repository.password", Escrow: true}})
	spec, ok := p.Secret()
	if !ok || spec.Label != "Kopia repository password" || spec.File != "repository.password" || !spec.Escrow {
		t.Errorf("Secret = %+v, %v", spec, ok)
	}
}

// Caps.ChangeSecret decides whether the page offers the change form, so an adapter that
// predates the verb must come out false.
func TestCapsCarryChangeSecret(t *testing.T) {
	if c := (wireCaps{ChangeSecret: true}).caps(); !c.ChangeSecret {
		t.Error("changeSecret was not carried through")
	}
	if c := (wireCaps{Recover: true}).caps(); c.ChangeSecret {
		t.Error("an adapter that never said changeSecret was taken to support it")
	}
}

// A blank key never reaches the engine directory.
func TestChangeSecretRefusesAnEmptyKeyWithoutWritingIt(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	p := New(cfg, Descriptor{EngineID: "kopia", Image: "img:1"})
	if err := os.MkdirAll(p.dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := p.ChangeSecret(context.Background(), " \n"); !errors.Is(err, ErrWrongKey) {
		t.Errorf("ChangeSecret(blank) = %v, want the wrong-key sentinel", err)
	}
	if _, err := os.Stat(filepath.Join(p.dir(), nextFile)); !os.IsNotExist(err) {
		t.Error("a blank key left a .next file behind")
	}
}
