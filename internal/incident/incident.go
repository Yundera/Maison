// Package incident is Maison's record of what is currently wrong with the box.
//
// It generalises the one thing Maison could already report — a failing backup — into
// something every subsystem can use. The lesson that shaped it is in the backup
// scheduler's old notifyOutcome: a nightly "backups failed" message becomes noise,
// then a filter rule, and then the failure it was reporting is invisible again. So
// the unit here is NOT an event.
//
// AN INCIDENT IS A STATE. A reporter does not send a notification; it asserts a
// condition and later clears it:
//
//	inc.Report(Report{ID: "disk.full:sda1", Kind: KindDiskFull, ...})  // idempotent
//	inc.Resolve("disk.full:sda1")                                      // no-op if not open
//
// Asserting an already-open incident is free and silent. That is what lets a detector
// run on a five-minute loop and re-state the same truth every time without anybody's
// inbox noticing, and it is what moves the "have I already said this?" question out of
// every reporter and into one place that persists the answer across restarts.
//
// Delivery is deliberately NOT inline. Report and Resolve only mark a transition;
// Deliver drains them into a single mail. One root cause fans out — a disk fills, six
// apps go unhealthy — and seven separate emails about one problem is the failure mode
// this exists to prevent.
package incident

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yundera/maison/internal/notify"
)

// This package imports notify and nothing else of Maison's, on purpose. It is a leaf,
// so config, stackup, installer, appstore and backup can all report into it without an
// import cycle — which is what makes it a *central* register rather than one more
// subsystem with its own mailer.

// Severity is how loud an incident is. Two levels, not five: the only decision it
// drives is how the dashboard paints the badge, and a scale nobody can apply
// consistently is worse than no scale.
type Severity string

const (
	// Warning is something the owner should deal with. Critical is something that is
	// losing them data or access right now.
	Warning  Severity = "warning"
	Critical Severity = "critical"
)

// Valid reports whether s is a severity this build understands.
func (s Severity) Valid() bool { return s == Warning || s == Critical }

// Kinds. A Kind is the *class* of problem; an ID is one instance of it. The dashboard
// looks up "incident_"+Kind for a localised title and the "here is what to do about
// it" line, and mutes are per-Kind, so these strings are wire format: renaming one
// silently un-mutes it and blanks its label on every box in the field.
const (
	KindBackupFailed = "backup.failed"
	KindBackupStale  = "backup.stale"
	KindBackupEngine = "backup.engine"
	KindDiskFull     = "disk.full"
	KindDiskUnseen   = "disk.unseen"
	KindAppUnhealthy = "app.unhealthy"
	KindAppPartial   = "app.partial"
	KindAppCrashLoop = "app.crashloop"
	KindAppInstall   = "app.install"
	KindAppUpdate    = "app.update"
	KindAppStackup   = "app.stackup"
	KindStoreSource  = "store.source"
	KindTest         = "test.notification"
)

// IDs that more than one package has to name. Most IDs are built by the reporter that
// owns them; these two are shared — the schedule asserts the first, and the detectors
// and the upgrade adoption in server.New both have to refer to it.
const (
	IDBackupRun   = "backup.run"
	IDBackupStale = "backup.stale"
)

// Report is what a reporter asserts. Everything else about an incident — when it
// started, how many times it has been seen, whether anyone has been told — is derived
// and owned by the store.
type Report struct {
	// ID is the dedup key and the whole trick of this package. Two calls with the same
	// ID are the same incident, however far apart. Shape it as kind:subject —
	// "backup.run", "disk.full:sda1", "app.unhealthy:nextcloud" — so that one problem
	// with one subject can never open two records.
	ID string `json:"id"`

	// Kind is the class. See the constants above.
	Kind string `json:"kind"`

	Severity Severity `json:"severity"`

	// Title is one line, and it is what lands in the mail's subject. Keep it in the
	// owner's terms ("Backups are failing"), not the subsystem's.
	Title string `json:"title"`

	// Detail is the body: what went wrong, and what to do about it. It is read by
	// someone who did not ask for the mail and cannot see the logs, so an error string
	// on its own is rarely enough.
	Detail string `json:"detail,omitempty"`

	// Args interpolates the dashboard's localised text for this Kind, so the UI can
	// say "Nextcloud is unhealthy" in the user's language rather than echoing the
	// English Title. Optional; the UI falls back to Title.
	Args map[string]string `json:"args,omitempty"`
}

// Incident is a Report plus the store's bookkeeping.
type Incident struct {
	Report

	// Since is when it first opened and Updated when it was last re-asserted. Since
	// survives re-assertion, so "failing for three days" is answerable.
	Since   time.Time `json:"since"`
	Updated time.Time `json:"updated"`

	// Count is how many times it has been asserted. A detector on a loop makes this a
	// rough age; a pushed reporter makes it a real occurrence count.
	Count int `json:"count"`

	// Resolved is when it cleared; nil means still open.
	//
	// A POINTER, and that is load-bearing for the same reason backupconfig.LegacySMTP
	// is one: omitempty does nothing for a struct, so a plain time.Time would ship
	// "resolved":"0001-01-01T00:00:00Z" on every open incident — a date that is truthy
	// in every language the dashboard is written in, and a lie the UI would render.
	// The backup scheduler has a test pinning the same trap on RunState.Ran; here the
	// pointer makes the bad state unrepresentable instead.
	Resolved *time.Time `json:"resolved,omitempty"`

	// Acked means the owner has seen it and does not want the badge any more. It does
	// not resolve the incident and does not stop the recovery notice.
	Acked bool `json:"acked,omitempty"`
}

// Open reports whether this incident is still current.
func (i Incident) Open() bool { return i.Resolved == nil }

// pending is one transition that has happened but has not been mailed.
//
// It carries a copy of the incident rather than a reference to it because the mail
// describes the world at the moment of the transition, and because a resolved incident
// has already left the open list by the time anyone composes anything.
type pending struct {
	Snapshot Incident `json:"incident"`

	// Opened distinguishes the two transitions worth mailing. There are only two.
	Opened bool `json:"opened"`

	// Tries counts delivery attempts, so a relay that is down for an hour does not
	// mean the announcement is lost, and a relay that is misconfigured forever does
	// not mean the queue grows forever. See Deliver.
	Tries int `json:"tries,omitempty"`
}

// document is the whole persisted state.
type document struct {
	Incidents []Incident      `json:"incidents"`
	Muted     map[string]bool `json:"muted,omitempty"`

	// Pending is on disk, not in memory, because the window between a transition and
	// its delivery pass is a window a restart can land in — and an alert lost to a
	// container restart is exactly the silent failure this package exists to prevent.
	Pending []pending `json:"pending,omitempty"`
}

// Retention. Resolved incidents are kept so the settings page can answer "has this
// happened before?", which is most of what makes a one-off alert interpretable.
const (
	keepResolvedFor = 30 * 24 * time.Hour
	maxRecords      = 200

	// maxDetail bounds one field. A failed install's error can carry a whole compose
	// output, and this file is read on every boot.
	maxDetail = 2000
	maxTitle  = 200

	// maxTries is how often a pending transition is re-offered to a failing relay
	// before it is dropped. Three passes of the delivery loop is enough to ride out a
	// restarting mail container and short enough that a permanently broken relay does
	// not accumulate a backlog nobody will ever read.
	maxTries = 3
)

// Store is the persisted register.
type Store struct {
	path string
	mu   sync.Mutex
	cur  document

	// Now, Notify, Mail and Where exist so the interesting logic — when something is
	// announced, and what it says — can be tested without a clock or an SMTP server.
	// Nil means the real thing. This mirrors backup.Scheduler, deliberately.
	Now    func() time.Time
	Notify func(subject, body string) error

	// Mail resolves the transport at send time. A hook rather than a settings store
	// for the reason the scheduler gives for its own: this needs one answer, not a
	// dependency on where the answer is kept — and where it is kept has changed once
	// already.
	Mail func() notify.SMTP

	// Where names the box in the subject line ("… on john.nsl.sh"). Empty or nil
	// degrades to "your server".
	Where func() string

	// OnChange fires when the open set changes, so the dashboard can rebroadcast.
	// Re-asserting an unchanged incident deliberately does NOT fire it.
	OnChange func()
}

// New loads the register, seeding an empty document when there is none.
//
// It never returns an error, for the reason every other Maison store gives: a
// dashboard that will not start is worse than a register that starts empty. A file
// that exists but does not parse is LEFT EXACTLY AS IT IS — it is the only record of
// what has been announced, and overwriting it would re-announce everything in it.
func New(path string) *Store {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if err := s.seed(); err != nil {
				log.Printf("incident: could not seed %s: %v", path, err)
			}
		} else {
			log.Printf("incident: %s unreadable: %v (starting empty)", path, err)
		}
		return s
	}
	var loaded document
	if json.Unmarshal(b, &loaded) == nil {
		s.cur = sane(loaded, s.now())
	}
	return s
}

// seed writes the empty document, with O_EXCL for the same reason backupconfig does:
// two Maison processes booting against one state directory must not have the second
// blank the file the first is already using.
func (s *Store) seed() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil // someone else got there first, which is the outcome we wanted
		}
		return err
	}
	defer f.Close()
	_, err = f.WriteString("{}\n")
	return err
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) where() string {
	if s.Where != nil {
		if w := s.Where(); w != "" {
			return w
		}
	}
	return "your server"
}

// Report asserts that something is wrong. It is idempotent: the second and every
// later call for an ID that is already open refreshes the record and tells nobody.
//
// IT RETURNS NOTHING, and that is load-bearing. The scheduler's rule — "a broken SMTP
// configuration must never turn a successful backup into a failed one" — used to rely
// on one caller remembering to discard an error. Here no caller can propagate a
// delivery problem even by accident, because there is nothing to propagate.
func (s *Store) Report(r Report) {
	r = saneReport(r)
	if r.ID == "" || r.Kind == "" {
		return // a report with no dedup key is a log line, not an incident
	}

	s.mu.Lock()
	now := s.now()
	changed := false
	if i, ok := s.findOpenLocked(r.ID); ok {
		cur := s.cur.Incidents[i]
		// Count and Updated move on every assertion, but on their own they are not
		// worth a disk write or a broadcast every five minutes for as long as the
		// problem lasts. They ride along on the next write that has a real reason.
		cur.Count++
		cur.Updated = now
		changed = cur.Title != r.Title || cur.Detail != r.Detail || cur.Severity != r.Severity
		cur.Report = r
		s.cur.Incidents[i] = cur
	} else {
		inc := Incident{Report: r, Since: now, Updated: now, Count: 1}
		s.cur.Incidents = append(s.cur.Incidents, inc)
		s.cur.Pending = append(s.cur.Pending, pending{Snapshot: inc, Opened: true})
		changed = true
	}
	if changed {
		s.persistLocked(now)
	}
	s.mu.Unlock()

	if changed {
		s.changed()
	}
}

// Adopt records a condition that is already true and already known, WITHOUT announcing
// it.
//
// It exists for one job: a box upgrading into this package may already have been
// failing for weeks, and the register has never heard of it. Reporting it normally
// would mail the owner about a problem they were told about long ago, by the code this
// package replaced. Adopting it means the badge is right immediately and the recovery
// notice still arrives when it is fixed.
//
// A no-op when the ID is already open, so it is safe to call on every boot.
func (s *Store) Adopt(r Report) {
	r = saneReport(r)
	if r.ID == "" || r.Kind == "" {
		return
	}
	s.mu.Lock()
	if _, ok := s.findOpenLocked(r.ID); ok {
		s.mu.Unlock()
		return
	}
	now := s.now()
	s.cur.Incidents = append(s.cur.Incidents, Incident{Report: r, Since: now, Updated: now, Count: 1})
	s.persistLocked(now)
	s.mu.Unlock()
	s.changed()
}

// Resolve clears an incident.
//
// An ID that is not open is a NO-OP, and that is the whole of the "first ever run, and
// it worked: nothing to announce" rule the scheduler used to spell out by hand. A
// recovery notice is only ever sent for a failure somebody was actually told about.
func (s *Store) Resolve(id string) {
	s.mu.Lock()
	i, ok := s.findOpenLocked(id)
	if !ok {
		s.mu.Unlock()
		return
	}
	now := s.now()
	cur := s.cur.Incidents[i]
	cur.Resolved = &now
	cur.Updated = now
	s.cur.Incidents[i] = cur
	s.cur.Pending = append(s.cur.Pending, pending{Snapshot: cur, Opened: false})
	s.persistLocked(now)
	s.mu.Unlock()

	s.changed()
}

// IsOpen reports whether id is currently open. Detectors use it to implement
// hysteresis — open at 90% but do not clear until 85% — without keeping their own
// copy of state that a restart would lose.
func (s *Store) IsOpen(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.findOpenLocked(id)
	return ok
}

// Ack hides an open incident's badge without resolving it. The problem is still there
// and the recovery notice will still be sent.
func (s *Store) Ack(id string) error {
	s.mu.Lock()
	i, ok := s.findOpenLocked(id)
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("no open incident %q", id)
	}
	s.cur.Incidents[i].Acked = true
	err := s.persistLocked(s.now())
	s.mu.Unlock()
	s.changed()
	return err
}

// MuteKind stops a class of incident from being mailed.
//
// It does NOT stop it being recorded, and does not hide it from the settings page. A
// mute that erased the evidence would be indistinguishable from the detector being
// broken, which is the thing an owner most needs to be able to rule out.
func (s *Store) MuteKind(kind string, on bool) error {
	if kind == "" {
		return errors.New("no kind given")
	}
	s.mu.Lock()
	if s.cur.Muted == nil {
		s.cur.Muted = map[string]bool{}
	}
	if on {
		s.cur.Muted[kind] = true
	} else {
		delete(s.cur.Muted, kind)
	}
	err := s.persistLocked(s.now())
	s.mu.Unlock()
	s.changed()
	return err
}

// Snapshot is what the API and the live channel serve.
type Snapshot struct {
	Open   []Incident      `json:"open"`
	Recent []Incident      `json:"recent"`
	Muted  map[string]bool `json:"muted"`

	// MailConfigured tells the dashboard whether anything would actually have been
	// sent. The incident register works on a box with no relay — it is the badge that
	// matters most — but the settings page has to be able to say so.
	MailConfigured bool `json:"mail_configured"`

	Critical int `json:"critical"`
	Warnings int `json:"warnings"`
}

// Snapshot returns the current state, newest first.
func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := Snapshot{Muted: map[string]bool{}, Open: []Incident{}, Recent: []Incident{}}
	for k, v := range s.cur.Muted {
		out.Muted[k] = v
	}
	for _, inc := range s.cur.Incidents {
		if inc.Open() {
			out.Open = append(out.Open, inc)
			if inc.Severity == Critical {
				out.Critical++
			} else {
				out.Warnings++
			}
		} else {
			out.Recent = append(out.Recent, inc)
		}
	}
	sort.SliceStable(out.Open, func(i, j int) bool { return out.Open[i].Since.After(out.Open[j].Since) })
	sort.SliceStable(out.Recent, func(i, j int) bool { return out.Recent[i].Resolved.After(*out.Recent[j].Resolved) })
	if s.Mail != nil {
		out.MailConfigured = s.Mail().Configured()
	}
	return out
}

// Deliver drains the outbox into at most one mail.
//
// Called on a timer rather than from Report, which is what makes the coalescing
// window real: a disk that fills and takes six apps down with it produces one message
// listing seven things, not seven messages. A couple of minutes' delay on an email
// alert costs nothing; seven emails cost the owner's attention permanently.
func (s *Store) Deliver() {
	s.mu.Lock()
	if len(s.cur.Pending) == 0 {
		s.mu.Unlock()
		return
	}
	pend := s.cur.Pending
	muted := make(map[string]bool, len(s.cur.Muted))
	for k, v := range s.cur.Muted {
		muted[k] = v
	}
	s.cur.Pending = nil
	now := s.now()
	s.persistLocked(now)
	s.mu.Unlock()

	where := s.where()
	opened, resolved := partition(pend, muted)
	if len(opened) == 0 && len(resolved) == 0 {
		return // everything in this batch was muted, or cancelled itself out
	}

	subject, body := digestMail(where, opened, resolved, now)
	if err := s.send(subject, body); err != nil {
		log.Printf("incident: sending the notification: %v", err)
		s.requeue(pend)
	}
}

// Test sends one message through the real delivery path and reports what happened.
//
// It composes with digestMail, so what arrives is exactly the shape of a real alert
// rather than a "hello" that proves only that a socket opened. And it returns the
// transport error to the caller instead of logging it, because this is the one place
// where somebody is standing in front of the dashboard waiting to be told why their
// relay does not work.
//
// It deliberately does NOT touch the register. A test that left a fake incident in the
// owner's history would make the history untrustworthy, which is most of its value.
func (s *Store) Test() error {
	now := s.now()
	inc := Incident{
		Report: Report{
			ID: "test.notification", Kind: KindTest, Severity: Warning,
			Title:  "Test notification",
			Detail: "Nothing is wrong. This is what an alert from your server looks like,\nsent because someone pressed the test button in Settings → Notifications.",
		},
		Since: now, Updated: now, Count: 1,
	}
	subject, body := digestMail(s.where(), []Incident{inc}, nil, now)
	return s.send(subject, body)
}

// partition splits a batch into the two things a mail can say, dropping muted kinds
// and cancelling out anything that opened and closed inside the same window.
//
// A condition that healed itself before anyone could have read about it is not news.
// Saying "X broke" and "X is fine" in one message trains the reader to skip the next
// one, which is the same slow failure as mailing every night.
func partition(pend []pending, muted map[string]bool) (opened, resolved []Incident) {
	seen := map[string]int{}
	for _, p := range pend {
		if p.Opened {
			seen[p.Snapshot.ID]++
		} else {
			seen[p.Snapshot.ID]--
		}
	}
	for _, p := range pend {
		if muted[p.Snapshot.Kind] || seen[p.Snapshot.ID] == 0 {
			continue
		}
		if p.Opened {
			opened = append(opened, p.Snapshot)
		} else {
			resolved = append(resolved, p.Snapshot)
		}
	}
	return opened, resolved
}

// requeue puts a failed batch back for the next pass, giving up on anything that has
// already had its chances. A mail relay that is restarting should not cost the owner
// an alert; one that has been misconfigured for a week should not build a backlog.
func (s *Store) requeue(pend []pending) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keep []pending
	for _, p := range pend {
		p.Tries++
		if p.Tries < maxTries {
			keep = append(keep, p)
		}
	}
	// Ahead of anything that arrived while the send was in flight, so the queue stays
	// in the order the transitions happened.
	s.cur.Pending = append(keep, s.cur.Pending...)
	s.persistLocked(s.now())
}

func (s *Store) send(subject, body string) error {
	if s.Notify != nil {
		return s.Notify(subject, body)
	}
	if s.Mail == nil {
		return errors.New("incident: no mail transport wired")
	}
	return notify.Send(s.Mail(), subject, body)
}

func (s *Store) changed() {
	if s.OnChange != nil {
		s.OnChange()
	}
}

func (s *Store) findOpenLocked(id string) (int, bool) {
	for i, inc := range s.cur.Incidents {
		if inc.ID == id && inc.Open() {
			return i, true
		}
	}
	return 0, false
}

// persistLocked writes the whole document through a temporary, with the lock held
// across the write — the discipline backupconfig documents and usersettings does not
// have. Two concurrent callers must not land their file writes in the opposite order
// to their in-memory updates, and an interrupted write must not truncate the record of
// what has already been announced.
func (s *Store) persistLocked(now time.Time) error {
	s.cur = sane(s.cur, now)
	b, err := json.MarshalIndent(s.cur, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".partial"
	// 0600, like backup.json and unlike the rest of Maison's state: incident detail is
	// error text from anywhere in the system, and a failed install's error can carry a
	// URL with a token in it.
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// sane bounds the document. It runs on read as well as on write, so a file left behind
// by a box that ran for a year does not have to be opened before it is trimmed.
func sane(d document, now time.Time) document {
	kept := make([]Incident, 0, len(d.Incidents))
	for _, inc := range d.Incidents {
		if !inc.Open() && now.Sub(*inc.Resolved) > keepResolvedFor {
			continue
		}
		if !inc.Severity.Valid() {
			inc.Severity = Warning
		}
		kept = append(kept, inc)
	}
	// Trim the oldest resolved first, and never an open one: the open set is the
	// current truth about the box, and a cap that could drop from it would make the
	// dashboard lie.
	if over := len(kept) - maxRecords; over > 0 {
		sort.SliceStable(kept, func(i, j int) bool {
			if kept[i].Open() != kept[j].Open() {
				return kept[i].Open()
			}
			return kept[i].Updated.After(kept[j].Updated)
		})
		if len(kept) > maxRecords {
			kept = kept[:maxRecords]
		}
	}
	d.Incidents = kept
	if len(d.Muted) == 0 {
		d.Muted = nil
	}
	return d
}

func saneReport(r Report) Report {
	r.ID = strings.TrimSpace(r.ID)
	r.Kind = strings.TrimSpace(r.Kind)
	r.Title = truncate(strings.TrimSpace(r.Title), maxTitle)
	r.Detail = truncate(strings.TrimSpace(r.Detail), maxDetail)
	if !r.Severity.Valid() {
		r.Severity = Warning
	}
	if r.Title == "" {
		r.Title = r.Kind
	}
	return r
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// digestMail writes the message. Pure, so what it says can be tested without an SMTP
// server anywhere — the same split internal/notify makes between Compose and Send.
func digestMail(where string, opened, resolved []Incident, now time.Time) (subject, body string) {
	var b strings.Builder

	switch {
	case len(opened) == 1 && len(resolved) == 0:
		subject = opened[0].Title + " on " + where
	case len(opened) == 0 && len(resolved) == 1:
		subject = "Resolved: " + resolved[0].Title + " on " + where
	case len(opened) == 0:
		subject = fmt.Sprintf("%d issues resolved on %s", len(resolved), where)
	default:
		subject = fmt.Sprintf("%d issues on %s", len(opened), where)
	}

	if len(opened) > 0 {
		if len(opened) == 1 {
			b.WriteString("Something needs your attention on " + where + ".\n\n")
		} else {
			fmt.Fprintf(&b, "%d things need your attention on %s.\n\n", len(opened), where)
		}
		for _, inc := range opened {
			writeEntry(&b, inc)
		}
	}

	if len(resolved) > 0 {
		if len(opened) > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Cleared:\n\n")
		for _, inc := range resolved {
			fmt.Fprintf(&b, "  %s\n", inc.Title)
			if d := inc.Resolved.Sub(inc.Since); d > time.Minute {
				fmt.Fprintf(&b, "    Lasted %s.\n", roughly(d))
			}
		}
	}

	b.WriteString("\nOpen the dashboard and go to Settings → Notifications to see the full list.\n")
	b.WriteString("You are told once when something starts going wrong and once when it clears,\n")
	b.WriteString("never on a schedule.\n")
	return subject, b.String()
}

func writeEntry(b *strings.Builder, inc Incident) {
	fmt.Fprintf(b, "  %s", inc.Title)
	if inc.Severity == Critical {
		b.WriteString("  [critical]")
	}
	b.WriteString("\n")
	for _, line := range strings.Split(inc.Detail, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fmt.Fprintf(b, "    %s\n", line)
	}
	b.WriteString("\n")
}

// roughly renders a duration the way someone reading an email would say it. Go's own
// formatting gives "51h14m3.2s", which is not an answer to "how long was this broken".
func roughly(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}
