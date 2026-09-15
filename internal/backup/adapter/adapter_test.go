package adapter

import (
	"encoding/json"
	"os"
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
