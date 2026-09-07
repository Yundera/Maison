package incident

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mailbox is a fake relay. Everything interesting in this package is "who gets told,
// and when", so the seam that has to be fakeable is the send.
type mailbox struct {
	sent []string // subjects
	body []string
	fail error
}

func (m *mailbox) notify(subject, body string) error {
	if m.fail != nil {
		return m.fail
	}
	m.sent = append(m.sent, subject)
	m.body = append(m.body, body)
	return nil
}

// newStore builds a store on a scratch tree with a clock the test drives. The clock is
// a pointer the caller keeps, because most of these invariants are about the passage
// of time between two calls.
func newStore(t *testing.T) (*Store, *mailbox, *time.Time) {
	t.Helper()
	clock := time.Date(2026, 3, 1, 3, 30, 0, 0, time.UTC)
	mb := &mailbox{}
	s := New(filepath.Join(t.TempDir(), "incidents.json"))
	s.Now = func() time.Time { return clock }
	s.Notify = mb.notify
	s.Where = func() string { return "john.nsl.sh" }
	return s, mb, &clock
}

func failing(id string) Report {
	return Report{ID: id, Kind: KindBackupFailed, Severity: Critical,
		Title: "Backups are failing", Detail: "app:beta\n  repository unreachable"}
}

func TestNewSeedsTheFileWhenItIsAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "incidents.json")
	New(path)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("seed did not create %s: %v", path, err)
	}
	if strings.TrimSpace(string(b)) != "{}" {
		t.Errorf("seed should be an empty document, got %q", b)
	}
}

// The file is the only record of what has already been announced. Overwriting one we
// cannot parse would re-announce every incident in it.
func TestNewLeavesAMalformedFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incidents.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(path)
	if got := len(s.Snapshot().Open); got != 0 {
		t.Errorf("a malformed file should load as empty, got %d open", got)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "{not json" {
		t.Errorf("the malformed file was rewritten: %q", b)
	}
}

// This is the invariant the backup scheduler used to carry alone, and the reason this
// package exists: a nightly message becomes noise, then a filter rule, and then the
// failure it was reporting is invisible again.
func TestOnlyTheTransitionIsMailed(t *testing.T) {
	s, mb, clock := newStore(t)

	// Healthy: nothing to say, and nothing was ever said.
	s.Resolve("backup.run")
	s.Deliver()

	// Three failing runs on three consecutive nights.
	for range 3 {
		s.Report(failing("backup.run"))
		s.Deliver()
		*clock = clock.Add(24 * time.Hour)
	}

	// Then it recovers.
	s.Resolve("backup.run")
	s.Deliver()

	if len(mb.sent) != 2 {
		t.Fatalf("want exactly 2 mails (one failing, one recovered), got %d: %q", len(mb.sent), mb.sent)
	}
	if !strings.Contains(mb.sent[0], "Backups are failing") {
		t.Errorf("first mail should announce the failure, got %q", mb.sent[0])
	}
	if !strings.HasPrefix(mb.sent[1], "Resolved:") {
		t.Errorf("second mail should announce the recovery, got %q", mb.sent[1])
	}
}

// Absent prior state is not a recovery. A box whose first ever backup succeeds must
// not mail anybody to say so.
func TestResolvingSomethingThatWasNeverOpenSaysNothing(t *testing.T) {
	s, mb, _ := newStore(t)
	s.Resolve("backup.run")
	s.Resolve("disk.full:sda1")
	s.Deliver()
	if len(mb.sent) != 0 {
		t.Errorf("resolving an unknown incident announced something: %q", mb.sent)
	}
}

// The dedup state has to be on disk. Maison restarting must not re-announce a failure
// the owner has already been told about, nor stay silent about a recovery it never saw
// the failure for.
func TestDedupSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "incidents.json")
	clock := time.Date(2026, 3, 1, 3, 30, 0, 0, time.UTC)

	first := New(path)
	first.Now = func() time.Time { return clock }
	mb1 := &mailbox{}
	first.Notify = mb1.notify
	first.Report(failing("backup.run"))
	first.Deliver()
	if len(mb1.sent) != 1 {
		t.Fatalf("the first failure should have been announced once, got %d", len(mb1.sent))
	}

	// A restart: a brand new store over the same file.
	second := New(path)
	second.Now = func() time.Time { return clock }
	mb2 := &mailbox{}
	second.Notify = mb2.notify

	second.Report(failing("backup.run")) // still failing after the restart
	second.Deliver()
	if len(mb2.sent) != 0 {
		t.Errorf("a restart re-announced a failure already reported: %q", mb2.sent)
	}
	if got := len(second.Snapshot().Open); got != 1 {
		t.Errorf("the open incident did not survive the restart, got %d open", got)
	}

	second.Resolve("backup.run")
	second.Deliver()
	if len(mb2.sent) != 1 {
		t.Fatalf("the recovery should be announced after a restart, got %d: %q", len(mb2.sent), mb2.sent)
	}
}

// One root cause fans out — a disk fills and takes several apps down with it. Seven
// separate emails about one problem is the failure mode the coalescing window exists
// to prevent.
func TestOneRootCauseSendsOneMail(t *testing.T) {
	s, mb, _ := newStore(t)
	s.Report(Report{ID: "disk.full:sda1", Kind: KindDiskFull, Severity: Critical, Title: "Disk almost full"})
	s.Report(Report{ID: "app.unhealthy:nextcloud", Kind: KindAppUnhealthy, Title: "Nextcloud is unhealthy"})
	s.Report(Report{ID: "app.unhealthy:immich", Kind: KindAppUnhealthy, Title: "Immich is unhealthy"})
	s.Deliver()

	if len(mb.sent) != 1 {
		t.Fatalf("three incidents in one window should be one mail, got %d: %q", len(mb.sent), mb.sent)
	}
	if !strings.Contains(mb.sent[0], "3 issues") {
		t.Errorf("the subject should count them, got %q", mb.sent[0])
	}
	for _, want := range []string{"Disk almost full", "Nextcloud is unhealthy", "Immich is unhealthy"} {
		if !strings.Contains(mb.body[0], want) {
			t.Errorf("the body should list %q:\n%s", want, mb.body[0])
		}
	}
}

// A condition that heals before anyone could have read about it is not news. Saying
// "X broke" and "X is fine" in one message trains the reader to skip the next one.
func TestSomethingThatHealsItselfInTheWindowIsNotMailed(t *testing.T) {
	s, mb, clock := newStore(t)
	s.Report(failing("backup.run"))
	*clock = clock.Add(30 * time.Second)
	s.Resolve("backup.run")
	s.Deliver()

	if len(mb.sent) != 0 {
		t.Errorf("a self-healing blip was mailed: %q / %q", mb.sent, mb.body)
	}
	// It still happened, and the register still says so.
	if got := len(s.Snapshot().Recent); got != 1 {
		t.Errorf("the blip should still be recorded, got %d in history", got)
	}
}

// A mute is about the inbox, not about the evidence. A mute that erased the record
// would be indistinguishable from the detector being broken, which is the one thing an
// owner most needs to be able to rule out.
func TestAMutedKindIsRecordedButNotMailed(t *testing.T) {
	s, mb, _ := newStore(t)
	if err := s.MuteKind(KindAppUnhealthy, true); err != nil {
		t.Fatal(err)
	}
	s.Report(Report{ID: "app.unhealthy:jellyfin", Kind: KindAppUnhealthy, Title: "Jellyfin is unhealthy"})
	s.Deliver()

	if len(mb.sent) != 0 {
		t.Errorf("a muted kind was mailed: %q", mb.sent)
	}
	snap := s.Snapshot()
	if len(snap.Open) != 1 {
		t.Fatalf("a muted kind should still be recorded, got %d open", len(snap.Open))
	}
	if !snap.Muted[KindAppUnhealthy] {
		t.Errorf("the snapshot should report the mute so the UI can show it")
	}

	// Unmuting does not retroactively announce what was suppressed.
	if err := s.MuteKind(KindAppUnhealthy, false); err != nil {
		t.Fatal(err)
	}
	s.Deliver()
	if len(mb.sent) != 0 {
		t.Errorf("unmuting replayed a suppressed announcement: %q", mb.sent)
	}
}

// A relay that is restarting should not cost the owner an alert; one that has been
// misconfigured for a week should not build a backlog nobody will read.
func TestABrokenRelayRetriesThenGivesUp(t *testing.T) {
	s, mb, _ := newStore(t)
	mb.fail = errors.New("smtp refused")

	s.Report(failing("backup.run"))
	s.Deliver() // try 1
	s.Deliver() // try 2

	mb.fail = nil
	s.Deliver() // try 3 — the relay is back, and the alert survived
	if len(mb.sent) != 1 {
		t.Fatalf("a recovered relay should still deliver the queued alert, got %d: %q", len(mb.sent), mb.sent)
	}

	// And a relay that never comes back stops accumulating.
	mb2 := &mailbox{fail: errors.New("smtp refused")}
	s.Notify = mb2.notify
	s.Report(Report{ID: "disk.full:sda1", Kind: KindDiskFull, Title: "Disk almost full"})
	for range 5 {
		s.Deliver()
	}
	mb2.fail = nil
	s.Deliver()
	if len(mb2.sent) != 0 {
		t.Errorf("a permanently failed alert should have been dropped, not queued forever: %q", mb2.sent)
	}
}

// Report returns nothing on purpose. The scheduler's rule — a broken SMTP
// configuration must never turn a successful backup into a failed one — used to depend
// on one caller remembering to discard an error. Now no caller can propagate one.
func TestAReporterCannotSeeADeliveryFailure(t *testing.T) {
	s, mb, _ := newStore(t)
	mb.fail = errors.New("smtp refused")
	// The compiler is most of this test: if Report ever grows an error return, this
	// stops building and whoever changed it has to think about why.
	s.Report(failing("backup.run"))
	s.Resolve("backup.run")
	s.Deliver()
	if got := len(s.Snapshot().Recent); got != 1 {
		t.Errorf("the register should be correct regardless of the relay, got %d in history", got)
	}
}

// Asserting the same problem on a five-minute loop for a week must not write the file
// or wake the dashboard 2000 times.
func TestReAssertingAnUnchangedIncidentIsSilent(t *testing.T) {
	s, _, clock := newStore(t)
	changes := 0
	s.OnChange = func() { changes++ }

	s.Report(failing("backup.run"))
	if changes != 1 {
		t.Fatalf("opening an incident should notify once, got %d", changes)
	}
	for range 10 {
		*clock = clock.Add(5 * time.Minute)
		s.Report(failing("backup.run"))
	}
	if changes != 1 {
		t.Errorf("re-asserting an unchanged incident notified %d times, want 1", changes)
	}

	// A changed detail is worth a broadcast: the UI is showing the old text.
	r := failing("backup.run")
	r.Detail = "app:beta\n  disk full"
	s.Report(r)
	if changes != 2 {
		t.Errorf("a changed detail should notify, got %d", changes)
	}
	if got := s.Snapshot().Open[0].Count; got != 12 {
		t.Errorf("Count should track every assertion, got %d want 12", got)
	}
}

// A zero time.Time marshals as year 0001, which is truthy in every language the
// dashboard is written in. The backup scheduler already has a test pinning this exact
// trap; an incident's Resolved field is the same shape of lie.
func TestAnOpenIncidentDoesNotClaimAResolutionTime(t *testing.T) {
	s, _, _ := newStore(t)
	s.Report(failing("backup.run"))

	b, err := json.Marshal(s.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "0001-01-01") {
		t.Errorf("an open incident serialised a zero timestamp:\n%s", b)
	}
	if !s.Snapshot().Open[0].Open() {
		t.Error("Open() disagreed with the open list")
	}
}

func TestResolvedIncidentsAreTrimmedByAge(t *testing.T) {
	s, _, clock := newStore(t)
	s.Report(failing("backup.run"))
	s.Resolve("backup.run")

	*clock = clock.Add(keepResolvedFor + 24*time.Hour)
	// Any write runs the trim.
	s.Report(Report{ID: "disk.full:sda1", Kind: KindDiskFull, Title: "Disk almost full"})

	if got := len(s.Snapshot().Recent); got != 0 {
		t.Errorf("a month-old resolved incident should have been trimmed, got %d", got)
	}
	if got := len(s.Snapshot().Open); got != 1 {
		t.Errorf("the trim took an open incident with it, got %d open", got)
	}
}

// The cap must never drop from the open set: that is the current truth about the box,
// and a dashboard that under-reports it is worse than one that shows nothing.
func TestTheRecordCapNeverDropsAnOpenIncident(t *testing.T) {
	s, _, clock := newStore(t)
	for i := range maxRecords + 50 {
		id := "app.unhealthy:app" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		s.Report(Report{ID: id, Kind: KindAppUnhealthy, Title: "unhealthy"})
		s.Resolve(id)
		*clock = clock.Add(time.Minute)
	}
	s.Report(failing("backup.run"))

	snap := s.Snapshot()
	if len(snap.Open) != 1 {
		t.Fatalf("the open incident was trimmed away, got %d open", len(snap.Open))
	}
	if total := len(snap.Open) + len(snap.Recent); total > maxRecords {
		t.Errorf("the register grew past its cap: %d records", total)
	}
}

// The mail has to answer "is anything actually broken" on its own, for someone reading
// it on a phone with no access to the dashboard.
func TestDigestMailNamesWhatBrokeAndWhatCleared(t *testing.T) {
	now := time.Date(2026, 3, 1, 3, 30, 0, 0, time.UTC)
	opened := []Incident{{
		Report:  Report{ID: "disk.full:sda1", Kind: KindDiskFull, Severity: Critical, Title: "Disk almost full", Detail: "/DATA is 97% full."},
		Since:   now,
		Updated: now,
	}}
	resolved := []Incident{{
		Report:   Report{ID: "backup.run", Kind: KindBackupFailed, Title: "Backups are failing"},
		Since:    now.Add(-50 * time.Hour),
		Resolved: &now,
	}}

	subject, body := digestMail("john.nsl.sh", opened, resolved, now)
	if !strings.Contains(subject, "john.nsl.sh") {
		t.Errorf("the subject should name the box, got %q", subject)
	}
	for _, want := range []string{"Disk almost full", "/DATA is 97% full.", "[critical]", "Backups are failing", "2 days"} {
		if !strings.Contains(body, want) {
			t.Errorf("the body is missing %q:\n%s", want, body)
		}
	}

	// A box with no domain still has to produce a sentence.
	subject, _ = digestMail("your server", opened, nil, now)
	if !strings.Contains(subject, "your server") {
		t.Errorf("an unnamed box should degrade gracefully, got %q", subject)
	}
}

// Detail is error text from anywhere in the system, and this file is read on every
// boot.
func TestAnEnormousDetailIsBounded(t *testing.T) {
	s, _, _ := newStore(t)
	r := failing("backup.run")
	r.Detail = strings.Repeat("x", 50_000)
	s.Report(r)
	if got := len(s.Snapshot().Open[0].Detail); got > maxDetail+4 {
		t.Errorf("detail was not bounded: %d bytes", got)
	}
}

func TestAckHidesTheBadgeWithoutResolving(t *testing.T) {
	s, mb, _ := newStore(t)
	s.Report(failing("backup.run"))
	s.Deliver()
	if err := s.Ack("backup.run"); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if len(snap.Open) != 1 || !snap.Open[0].Acked {
		t.Fatalf("ack should mark the incident, not close it: %+v", snap.Open)
	}
	// The problem is still there, so the recovery notice must still come.
	s.Resolve("backup.run")
	s.Deliver()
	if len(mb.sent) != 2 {
		t.Errorf("an acked incident should still announce its recovery, got %q", mb.sent)
	}
	if err := s.Ack("nothing.here"); err == nil {
		t.Error("acking an unknown incident should be an error")
	}
}

func TestIsOpenLetsADetectorImplementHysteresis(t *testing.T) {
	s, _, _ := newStore(t)
	if s.IsOpen("disk.full:sda1") {
		t.Fatal("nothing has been reported yet")
	}
	s.Report(Report{ID: "disk.full:sda1", Kind: KindDiskFull, Title: "Disk almost full"})
	if !s.IsOpen("disk.full:sda1") {
		t.Error("a reported incident should read as open")
	}
	s.Resolve("disk.full:sda1")
	if s.IsOpen("disk.full:sda1") {
		t.Error("a resolved incident should not read as open")
	}
}
