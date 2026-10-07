package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/installer"
	"github.com/yundera/maison/internal/live"
)

// Settings → Updates: every app's update status in one list, and "update all".
//
// The check is installer.CheckAll. What lives here is the part with state: the last
// report, and a run that updates apps one after another on a background context, so
// it survives the browser tab that started it. Each app of a run goes through exactly
// what the Update tab's button does — installer.ApplyUpdate under the tile's busy
// overlay — so the rollback point, the rollback and the incidents are the ones every
// single update already has.
//
// Strictly sequential, for the reason the nightly backup is: every update takes a
// local rollback point, which is a full copy of the app, and several at once is how
// a data disk fills.

const (
	// updateTimeout is one app's budget inside a run — the same the Update tab's
	// request gives it.
	updateTimeout = 5 * time.Minute
	// updateCheckTimeout bounds a whole check: one conditional GET per store, plus a
	// download for any store that changed.
	updateCheckTimeout = 3 * time.Minute
	// The daily check follows the 03:00 store refresh, so it reads a fresh catalog;
	// the first one waits for the boot refresh and the apps to settle.
	updateCheckHour, updateCheckMinute = 4, 0
	updateCheckBootDelay               = 10 * time.Minute
)

// Run item states.
const (
	runQueued   = "queued"
	runUpdating = "updating"
	runDone     = "done"
	runFailed   = "failed"
)

// UpdateRunItem is one app of a run.
type UpdateRunItem struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Applied    bool   `json:"applied,omitempty"`
	RolledBack bool   `json:"rolled_back,omitempty"`
	Warning    string `json:"warning,omitempty"`
	Error      string `json:"error,omitempty"`
	// NoRollback marks an item refused because its rollback point could not be
	// taken (installer.ErrNoRollback). Nothing was changed; the row offers
	// "Update without backup" for that app alone.
	NoRollback bool `json:"no_rollback,omitempty"`
	// Reason says why (installer.NoRollback*), and for no_room how far off it was, so
	// the row can explain it in plain words rather than relaying the engine's error.
	Reason string `json:"reason,omitempty"`
	Needed int64  `json:"needed,omitempty"`
	Free   int64  `json:"free,omitempty"`
}

// UpdateRun is the current run, or the last one once it has finished. A run's
// results stay on the page until the next run replaces them.
type UpdateRun struct {
	Running  bool            `json:"running"`
	Items    []UpdateRunItem `json:"items"`
	Started  time.Time       `json:"started,omitempty"`
	Finished time.Time       `json:"finished,omitempty"`
}

// UpdatesSnapshot is what GET /api/updates and the "updates" channel carry.
type UpdatesSnapshot struct {
	CheckedAt time.Time             `json:"checked_at,omitempty"`
	Checking  bool                  `json:"checking"`
	Apps      []installer.AppUpdate `json:"apps"`
	Run       UpdateRun             `json:"run"`
}

type updatesState struct {
	mu        sync.Mutex
	checkedAt time.Time
	checking  chan struct{} // non-nil while a check runs; closed when it ends
	rows      []installer.AppUpdate
	run       UpdateRun
}

var (
	errRunInProgress = errors.New("an update run is already in progress")
	// Skipping the rollback point is an owner's decision about one app, made after
	// seeing that app refused — never something "update all" does wholesale.
	errNoBackupNeedsOneApp = errors.New("an update without a backup is chosen for one app at a time")
	errNothingToRun        = errors.New("nothing to update")
)

func (s *Server) updatesSnapshot() any {
	u := &s.updates
	u.mu.Lock()
	defer u.mu.Unlock()
	snap := UpdatesSnapshot{
		CheckedAt: u.checkedAt,
		Checking:  u.checking != nil,
		Apps:      append([]installer.AppUpdate{}, u.rows...),
		Run:       u.run,
	}
	snap.Run.Items = append([]UpdateRunItem{}, u.run.Items...)
	return snap
}

func (s *Server) broadcastUpdates() {
	s.hub.BroadcastLazy(live.ChannelUpdates, s.updatesSnapshot)
}

// checkUpdates re-checks every app. A second call while one is running waits for
// that one rather than starting another: both would sync the same stores.
func (s *Server) checkUpdates(ctx context.Context) {
	if s.apps == nil || s.installer == nil {
		return
	}
	u := &s.updates
	u.mu.Lock()
	if ch := u.checking; ch != nil {
		u.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
		}
		return
	}
	done := make(chan struct{})
	u.checking = done
	u.mu.Unlock()
	s.broadcastUpdates()

	rows, err := s.checkTargets(ctx, nil)

	u.mu.Lock()
	if err == nil {
		u.rows, u.checkedAt = rows, time.Now()
	}
	u.checking = nil
	close(done)
	u.mu.Unlock()
	s.broadcastUpdates()
}

// checkTargets runs installer.CheckAll over the app list — all of it, or only the
// ids given. Hidden apps are included: they have no tile, but they are installed,
// they run, and they go out of date like any other.
func (s *Server) checkTargets(ctx context.Context, only map[string]bool) ([]installer.AppUpdate, error) {
	ctx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
	defer cancel()
	list, err := s.apps.List(ctx)
	if err != nil {
		log.Printf("updates: listing apps: %v", err)
		return nil, err
	}
	targets := make([]installer.CheckTarget, 0, len(list))
	for _, a := range list {
		if only != nil && !only[a.ID] {
			continue
		}
		targets = append(targets, installer.CheckTarget{
			ID: a.ID, Name: a.Name, Icon: a.Icon, Managed: a.Managed, StaysUp: !a.Stoppable,
		})
	}
	return s.installer.CheckAll(ctx, targets), nil
}

// refreshUpdateRow re-checks one app and replaces its row, after something outside
// a run changed it — the Update tab's button, or a new store reference. Cheaper than
// a whole check, and without it the page would keep offering an update just applied.
func (s *Server) refreshUpdateRow(ctx context.Context, id string) {
	if s.apps == nil || s.installer == nil {
		return
	}
	rows, err := s.checkTargets(ctx, map[string]bool{id: true})
	if err != nil || len(rows) != 1 {
		return
	}
	u := &s.updates
	u.mu.Lock()
	for i := range u.rows {
		if u.rows[i].ID == id {
			u.rows[i] = rows[0]
		}
	}
	u.mu.Unlock()
	s.broadcastUpdates()
}

// runQueue resolves which apps a run updates. With no ids: every app with an update
// available, except the ones that declare themselves not stoppable (StaysUp) —
// updating the dashboard, or the gateway in front of it, can take down the process
// running the queue. Such an app is updated on its own, by naming it alone.
func runQueue(rows []installer.AppUpdate, ids []string) ([]string, error) {
	if len(ids) == 0 {
		var q []string
		for _, r := range rows {
			if r.State == installer.StateAvailable && !r.StaysUp {
				q = append(q, r.ID)
			}
		}
		if len(q) == 0 {
			return nil, errNothingToRun
		}
		return q, nil
	}
	byID := make(map[string]installer.AppUpdate, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	seen := map[string]bool{}
	var q []string
	for _, id := range ids {
		r, ok := byID[id]
		if !ok || seen[id] {
			continue
		}
		// Retrying an app whose last check failed is allowed: the store may be back.
		if r.State != installer.StateAvailable && r.State != installer.StateError {
			continue
		}
		if r.StaysUp && len(ids) > 1 {
			return nil, errors.New(r.ID + " stays running through its update and is updated on its own")
		}
		seen[id] = true
		q = append(q, id)
	}
	if len(q) == 0 {
		return nil, errNothingToRun
	}
	return q, nil
}

// startUpdateRun queues ids and runs them in the background. It returns once the run
// is accepted.
func (s *Server) startUpdateRun(ids []string, opts installer.UpdateOptions) error {
	if opts.NoBackup && len(ids) != 1 {
		return errNoBackupNeedsOneApp
	}
	u := &s.updates
	u.mu.Lock()
	if u.run.Running {
		u.mu.Unlock()
		return errRunInProgress
	}
	q, err := runQueue(u.rows, ids)
	if err != nil {
		u.mu.Unlock()
		return err
	}
	items := make([]UpdateRunItem, len(q))
	for i, id := range q {
		items[i] = UpdateRunItem{ID: id, Status: runQueued}
	}
	u.run = UpdateRun{Running: true, Items: items, Started: time.Now()}
	u.mu.Unlock()
	s.broadcastUpdates()

	go s.runUpdates(q, opts)
	return nil
}

func (s *Server) runUpdates(q []string, opts installer.UpdateOptions) {
	u := &s.updates
	set := func(i int, fn func(*UpdateRunItem)) {
		u.mu.Lock()
		fn(&u.run.Items[i])
		u.mu.Unlock()
		s.broadcastUpdates()
	}

	for i, id := range q {
		set(i, func(it *UpdateRunItem) { it.Status = runUpdating })

		ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
		var res installer.UpdateResult
		err := s.apps.WithBusy(id, func() error {
			var e error
			res, e = s.installer.ApplyUpdate(ctx, id, opts)
			return e
		})
		cancel()
		s.broadcastApps()

		// A failure does not stop the run: the next app's update has nothing to do
		// with this one's, and ApplyUpdate has already put this one back and raised
		// its incident.
		set(i, func(it *UpdateRunItem) {
			it.Applied, it.RolledBack, it.Warning = res.Applied, res.RolledBack, res.Warning
			if err != nil {
				it.Status, it.Error = runFailed, err.Error()
				it.NoRollback = errors.Is(err, installer.ErrNoRollback)
				var nr *installer.NoRollbackError
				if errors.As(err, &nr) {
					it.Reason, it.Needed, it.Free = nr.Reason, nr.Needed, nr.Free
				}
				return
			}
			it.Status = runDone
		})
	}

	u.mu.Lock()
	u.run.Running = false
	u.run.Finished = time.Now()
	u.mu.Unlock()
	s.broadcastUpdates()

	// So the list reflects what the run did, including what it failed to do.
	s.checkUpdates(context.Background())
}

// startDailyUpdateCheck checks once shortly after boot, then every day after the
// store's nightly refresh. It only ever checks: nothing is updated without somebody
// pressing a button.
func (s *Server) startDailyUpdateCheck(ctx context.Context) {
	go func() {
		wait := updateCheckBootDelay
		for {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				s.checkUpdates(ctx)
			}
			wait = untilNextAt(time.Now(), updateCheckHour, updateCheckMinute)
		}
	}()
}

// untilNextAt is the delay from now to the next hour:minute in now's location,
// recomputed per wait so a DST shift cannot drift the check off the wall clock.
func untilNextAt(now time.Time, hour, minute int) time.Duration {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Sub(now)
}

// --- HTTP ------------------------------------------------------------------------

func (s *Server) requireUpdates(w http.ResponseWriter) bool {
	if !s.requireApps(w) {
		return false
	}
	if s.installer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store unavailable"})
		return false
	}
	return true
}

// handleGetUpdates answers the last report. A box that has never checked — the first
// visit inside the boot delay — checks now, so the page does not open empty.
func (s *Server) handleGetUpdates(w http.ResponseWriter, r *http.Request) {
	if !s.requireUpdates(w) {
		return
	}
	s.updates.mu.Lock()
	never := s.updates.checkedAt.IsZero()
	s.updates.mu.Unlock()
	if never {
		s.checkUpdates(r.Context())
	}
	writeJSON(w, http.StatusOK, s.updatesSnapshot())
}

// handleCheckUpdates starts a check and returns; the result arrives on the channel.
func (s *Server) handleCheckUpdates(w http.ResponseWriter, _ *http.Request) {
	if !s.requireUpdates(w) {
		return
	}
	go s.checkUpdates(context.Background())
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// UpdatePreflight is what the confirmation dialog says before a run starts.
type UpdatePreflight struct {
	Apps []string `json:"apps"`
	// NoRollback names the apps whose rollback point will not fit on the data disk,
	// counting the ones taken before them in the same run — each is a full local copy,
	// and the run does not free them. They will be REFUSED, untouched; each can then
	// be updated on its own without a backup (see lifecycle.md).
	NoRollback []string `json:"no_rollback,omitempty"`
}

// handleUpdatesPreflight resolves a run's queue without starting it, and measures
// whether its rollback points fit. ?ids=a,b — none means "update all".
func (s *Server) handleUpdatesPreflight(w http.ResponseWriter, r *http.Request) {
	if !s.requireUpdates(w) {
		return
	}
	var ids []string
	if v := strings.TrimSpace(r.URL.Query().Get("ids")); v != "" {
		ids = strings.Split(v, ",")
	}
	s.updates.mu.Lock()
	q, err := runQueue(s.updates.rows, ids)
	s.updates.mu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, UpdatePreflight{Apps: q, NoRollback: s.noRollback(q)})
}

// noRollback walks the queue in order against the free space left by the rollback
// points before it. Measuring walks each app's folder, which is why it is asked for
// once, when the dialog opens, and never on a poll.
func (s *Server) noRollback(q []string) []string {
	var out []string
	var used int64
	for _, id := range q {
		est, err := s.apps.EstimateBackup(id, apps.EngineLocal, false)
		if err != nil || est.Skipped {
			continue // no rollback point is taken by design, or it cannot be measured
		}
		if est.Free-used < est.Needed {
			out = append(out, id)
			continue
		}
		used += est.Size
	}
	return out
}

func (s *Server) handleRunUpdates(w http.ResponseWriter, r *http.Request) {
	if !s.requireUpdates(w) {
		return
	}
	var body struct {
		IDs []string `json:"ids"`
		// NoBackup: exactly one id, the owner's retry after a refusal.
		NoBackup bool `json:"noBackup"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
	}
	switch err := s.startUpdateRun(body.IDs, installer.UpdateOptions{NoBackup: body.NoBackup}); {
	case errors.Is(err, errRunInProgress):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case err != nil:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
	}
}
