// Package kopia implements the kopia backup engine.
//
// Kopia runs as a throwaway container (internal/engine); Maison ships no binary and
// installs none on the host. Maison also never fetches storage credentials: a
// self-check script on the host renders them into
// ${DATA_ROOT}/AppDataShared/backup/kopia/ and connects the repository, and this
// package only reads what it finds there. An absent or unusable configuration is the
// normal "not configured" state of a box whose host side has not run — it degrades,
// it does not error.
//
// See docs/backup.md, which is authoritative for the design.
package kopia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backupconfig"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/engine"
)

// DefaultImage is the pinned engine image. Never a floating tag: an engine that
// changes under a live repository turns a format surprise into a 3am failure.
const DefaultImage = "kopia/kopia:0.23.1"

// ID is this engine's permanent identifier. It is recorded on every backup it
// writes and is how those backups are found again after the user switches engines,
// so it can never change.
const ID = "kopia"

// Tag keys Maison stamps on every snapshot.
//
// They are how (app, stamp) — the identity the API and the frontend use — survives
// into a repository that has no notion of either. Note the asymmetry: tags are
// *written* as "key:value" but come *back* from --json under a "tag:" prefix, so a
// filter built from the read spelling silently matches nothing.
const (
	tagApp   = "maison-app"
	tagStamp = "maison-stamp"
	tagPass  = "maison-pass"

	jsonTagPrefix = "tag:"
)

// userDataApp is the reserved tag value for the user-data set, which is not an app
// and has no compose project.
//
// A leading underscore is unrepresentable in a real app name (projectRe requires an
// alphanumeric first character), so this cannot collide with one — the guard makes
// the reservation for us rather than us having to police it.
const userDataApp = "_userdata"

// Provider is the kopia engine.
type Provider struct {
	cfg    config.Config
	runner *engine.Runner
	image  string

	mu       sync.Mutex
	cached   Status
	cachedAt time.Time
}

// Status is what Maison knows about the repository.
type Status struct {
	Connected bool   `json:"connected"`
	Type      string `json:"type,omitempty"`   // "filesystem", "s3", "b2", …
	Host      string `json:"host,omitempty"`   // the identity snapshots are filed under
	User      string `json:"user,omitempty"`   // together with Host, what snapshots are keyed by
	Detail    string `json:"detail,omitempty"` // why it is not connected, for the UI

	// Label is what to call this repository on screen, from the host-written state
	// file. Empty means nobody named it and the UI should fall back to describing the
	// engine — see repoState.
	Label string `json:"label,omitempty"`
}

// New builds the engine. It performs no I/O: a Provider is constructed on every
// boot, including on boxes that have no repository.
func New(cfg config.Config) *Provider {
	return &Provider{cfg: cfg, runner: engine.New(cfg), image: DefaultImage}
}

// WithImage overrides the pinned image, so a deployment can move engine version
// without a Maison release.
func (p *Provider) WithImage(ref string) *Provider {
	if ref != "" {
		p.image = ref
	}
	return p
}

func (p *Provider) ID() string { return ID }

func (p *Provider) Caps() apps.Caps {
	return apps.Caps{
		Offsite: true,
		// Restoring is a download, never a rename.
		InstantRestore: false,
		// Snapshots stream straight to the repository, so a backup needs no free disk
		// proportional to the app. That is what lets an app occupying most of its disk
		// be backed up at all.
		NeedsLocalSpace: false,
		// And what lets it be restored again: kopia writes over the live folder.
		InPlaceRestore: true,
		// Retention is kopia's own policy engine; Maison configures the tiers rather
		// than deleting snapshots itself.
		Retention: true,
	}
}

// dir is where the host-side script leaves this engine's configuration. The path is
// container-side, and the engine container mounts the data root at the same place,
// so the same string is valid on both sides.
func (p *Provider) dir() string { return p.cfg.BackupEngineDir(ID) }

func (p *Provider) configFile() string   { return filepath.Join(p.dir(), "repository.config") }
func (p *Provider) passwordFile() string { return filepath.Join(p.dir(), "repository.password") }

// credentialsFile holds the storage credentials as KEY=VALUE lines, written and
// rotated by the host-side ensure-backup-config.sh. See credentials().
func (p *Provider) credentialsFile() string { return filepath.Join(p.dir(), "credentials.env") }

// stateFile is what the host side knows and the engine cannot be asked: what to call
// this repository, and whether the storage behind it still accepts writes.
func (p *Provider) stateFile() string { return filepath.Join(p.dir(), "state.json") }

// repoState is the host-written description of the provisioned space.
//
// Label is the name a user should see. It lives here rather than in this package
// because "kopia" is the engine and the label describes the *space* it points at: a
// PCS provisioned by Yundera says so, and a self-hoster pointing the same engine at
// their own bucket must not be told they are using someone's branded service. An
// absent label is the ordinary self-hosted case, not a defect.
type repoState struct {
	Label    string `json:"label"`
	Writable *bool  `json:"writable"`
	Status   string `json:"status"`
}

func (p *Provider) readState() repoState {
	var st repoState
	b, err := os.ReadFile(p.stateFile())
	if err != nil {
		return st
	}
	if err := json.Unmarshal(b, &st); err != nil {
		log.Printf("kopia: unreadable %s: %v", p.stateFile(), err)
		return repoState{}
	}
	return st
}

// repoConfig is the part of kopia's own config file Maison reads. The identity
// fields are written there by `repository connect --override-hostname/--username`,
// which is why Maison never has to pass a hostname of its own — and must not, since
// two sides computing it independently is how a repository ends up split into two
// lineages that never see each other.
type repoConfig struct {
	Hostname string `json:"hostname"`
	Username string `json:"username"`
	Storage  struct {
		Type string `json:"type"`
	} `json:"storage"`
}

func (p *Provider) readConfig() (repoConfig, error) {
	var rc repoConfig
	b, err := os.ReadFile(p.configFile())
	if err != nil {
		if os.IsNotExist(err) {
			return rc, apps.ErrNotConfigured
		}
		return rc, err
	}
	if err := json.Unmarshal(b, &rc); err != nil {
		return rc, fmt.Errorf("kopia: unreadable repository config: %w", err)
	}
	return rc, nil
}

func (p *Provider) password() (string, error) {
	b, err := os.ReadFile(p.passwordFile())
	if err != nil {
		if os.IsNotExist(err) {
			return "", apps.ErrNotConfigured
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// credentials are the storage credentials, read fresh on every invocation.
//
// They are NOT in repository.config, and that is deliberate on the host side: kopia
// persists whatever it was given at connect time, so installing a rotated key that
// way means rewriting the whole configuration file — including the identity that
// every snapshot is filed under. The host script therefore blanks the persisted
// fields and rotates this file alone, which kopia accepts as the credential source
// for every ordinary operation.
//
// Reading it per run rather than at construction is what makes a rotation take effect
// on the next backup instead of the next restart. An absent file is not an error: a
// filesystem repository has no credentials, and a repository whose configuration
// still carries its own needs none from here.
func (p *Provider) credentials() (map[string]string, error) {
	b, err := os.ReadFile(p.credentialsFile())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

// Status reports whether the repository is usable, cached briefly because both the
// settings page and every app's Backups tab ask, and answering costs a container
// start.
func (p *Provider) Status(ctx context.Context) Status {
	p.mu.Lock()
	if time.Since(p.cachedAt) < 30*time.Second {
		defer p.mu.Unlock()
		return p.cached
	}
	p.mu.Unlock()

	st := p.probe(ctx)

	p.mu.Lock()
	p.cached, p.cachedAt = st, time.Now()
	p.mu.Unlock()
	return st
}

func (p *Provider) probe(ctx context.Context) Status {
	// Read before the configuration check: a box that has been issued a space but has
	// not connected yet should still be able to say whose space it is, so the settings
	// page names it rather than falling back to "kopia" while it is being set up.
	label := p.readState().Label

	rc, err := p.readConfig()
	if err != nil {
		return Status{Detail: notConfiguredDetail(err), Label: label}
	}
	out, err := p.run(ctx, nil, 2*time.Minute, "repository", "status", "--json")
	if err != nil {
		return Status{Detail: err.Error(), Host: rc.Hostname, User: rc.Username, Type: rc.Storage.Type, Label: label}
	}
	_ = out
	return Status{Connected: true, Type: rc.Storage.Type, Host: rc.Hostname, User: rc.Username, Label: label}
}

func notConfiguredDetail(err error) string {
	if errors.Is(err, apps.ErrNotConfigured) {
		return "no repository configured on this box"
	}
	return err.Error()
}

// Prepare makes the engine ready to run — currently, makes sure the image is
// present. It is called when the engine is selected and at boot, so that a first
// pull happens while someone is watching rather than silently delaying the first
// scheduled backup.
func (p *Provider) Prepare(ctx context.Context, emit func(apps.Event)) error {
	if _, err := p.readConfig(); err != nil {
		return err
	}
	// `docker run` would pull anyway; doing it here is what moves the wait somewhere
	// visible. Errors are returned rather than swallowed for the same reason.
	if _, err := p.runner.Run(ctx, engine.Spec{
		Image:    p.image,
		Name:     "maison-engine-prepare",
		Hostname: "maison-prepare",
		Network:  engine.NetworkDefault,
		Args:     []string{"--version"},
		Timeout:  10 * time.Minute,
	}, func(line string) { emitLine(emit, line) }); err != nil {
		return fmt.Errorf("kopia: engine image unavailable: %w", err)
	}
	return nil
}

// engineEnv overrides the paths the kopia image bakes into its own environment.
//
// The image ships KOPIA_CACHE_DIRECTORY=/app/cache and KOPIA_LOG_DIR=/app/logs, and
// those outrank --cache-directory and --log-dir. Both point inside the container, so
// under --rm every run would start from a cold cache and discard its log — and against
// a remote repository a cold cache means refetching indexes before any real work can
// start. Both belong beside the rest of the engine's state, where the user-data set
// already excludes them by pattern.
func (p *Provider) engineEnv() map[string]string {
	return map[string]string{
		"KOPIA_CACHE_DIRECTORY": filepath.Join(p.dir(), "cache"),
		"KOPIA_LOG_DIR":         filepath.Join(p.dir(), "logs"),
	}
}

// run invokes kopia. Every invocation carries --config-file and the password through
// the environment; nothing else is global.
func (p *Provider) run(ctx context.Context, emit func(apps.Event), timeout time.Duration, args ...string) ([]byte, error) {
	rc, err := p.readConfig()
	if err != nil {
		return nil, err
	}
	pw, err := p.password()
	if err != nil {
		return nil, err
	}
	secrets := map[string]string{"KOPIA_PASSWORD": pw}
	creds, err := p.credentials()
	if err != nil {
		return nil, err
	}
	for k, v := range creds {
		secrets[k] = v
	}
	return p.runner.Run(ctx, p.spec(rc, secrets, args, timeout), func(line string) { emitLine(emit, line) })
}

// engineCaps is what the engine keeps of root.
//
// DAC_READ_SEARCH and DAC_OVERRIDE are the two sides of the same requirement: read
// every file an app owns, and write back into directories it does not. CHOWN, FOWNER
// and FSETID are what let a restore put the recorded owner, mode and setuid bits back
// — which is what makes a restored database start rather than come back as a
// directory postgres refuses.
//
// Everything else goes. The engine binds no port, changes uid never, and reaches at
// most one repository; a capability nobody can name a use for is one an image
// compromise inherits for free.
var engineCaps = engine.Caps{
	Drop: []string{"ALL"},
	Add:  []string{"DAC_READ_SEARCH", "DAC_OVERRIDE", "CHOWN", "FOWNER", "FSETID"},
}

// spec builds one engine invocation.
//
// It is separate from run, and takes everything it needs as arguments, so the
// privilege decisions below can be asserted without a daemon or a repository.
func (p *Provider) spec(rc repoConfig, secrets map[string]string, args []string, timeout time.Duration) engine.Spec {
	// A filesystem repository needs no networking at all, which also keeps the tests
	// hermetic. Anything else reaches a bucket over the default bridge — never a
	// network Maison's peers are on.
	net := engine.NetworkDefault
	if rc.Storage.Type == "filesystem" || rc.Storage.Type == "" {
		net = engine.NetworkNone
	}
	return engine.Spec{
		Image:    p.image,
		Name:     containerName(args),
		Hostname: p.hostname(rc),
		// Root, narrowed by engineCaps — PUID:PGID cannot read an app's own private
		// data directories, and the backup an uninstall depends on fails outright. The
		// full reasoning, and why capabilities are not an alternative, is on
		// engine.Spec.User.
		User:            "0:0",
		Caps:            engineCaps,
		NoNewPrivileges: true,
		Network:         net,
		Env:             p.engineEnv(),
		Mounts:          []engine.Mount{p.runner.DataMount(false)},
		Secrets:         secrets,
		Args:            append(args, "--config-file="+p.configFile()),
		Timeout:         timeout,
	}
}

// hostname is the identity snapshots are filed under.
//
// It comes from the repository config, written there by the host-side connect. The
// fallback exists only for a hand-written dev config: it is derived from the data
// path so it is stable for a given box, and it is obviously synthetic so that a real
// deployment missing its pin is recognisable rather than silently divergent.
func (p *Provider) hostname(rc repoConfig) string {
	if rc.Hostname != "" {
		return rc.Hostname
	}
	return "maison-unpinned"
}

// containerName keeps a run findable after its client dies. Docker names allow only
// [a-zA-Z0-9][a-zA-Z0-9_.-]*, so the verb is sanitised rather than trusted.
func containerName(args []string) string {
	verb := "run"
	if len(args) > 0 {
		verb = args[0]
	}
	if len(args) > 1 {
		verb += "-" + args[1]
	}
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, verb)
	return "maison-kopia-" + safe + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// emitLine lives in progress.go, with the rest of the output parsing.

// --- sources -----------------------------------------------------------------

// Source is one thing kopia snapshots. Apps and user data are genuinely different —
// an app has a compose project and containers to stop, user data has neither — and
// modelling user data as a pseudo-app would push a name through guards written for
// project names.
type Source struct {
	App string // compose project, or userDataApp for the user-data set
}

// AppSource is the source for one app.
func AppSource(app string) Source { return Source{App: app} }

// UserDataSource is the source for everything under the data root that is not an
// app: Documents, Downloads, Media, and whatever else the user drops there.
func UserDataSource() Source { return Source{App: userDataApp} }

func (s Source) isUserData() bool { return s.App == userDataApp }

func (p *Provider) sourcePath(s Source) string {
	if s.isUserData() {
		return p.cfg.DataRoot
	}
	return filepath.Join(p.cfg.AppsDir(), s.App)
}

// EnsurePolicy configures retention and exclusions for a source. It is idempotent
// and cheap, and is reapplied on every run rather than only at setup: kopia policies
// live in the repository, so they survive a Maison reinstall — and a Maison bug can
// leave a stale one behind.
func (p *Provider) EnsurePolicy(ctx context.Context, s Source, keep Retention) error {
	path := p.sourcePath(s)
	if _, err := p.run(ctx, nil, 5*time.Minute, append([]string{"policy", "set", path}, keep.args()...)...); err != nil {
		return err
	}
	if !s.isUserData() {
		return nil
	}

	// Exclusions are reset and re-added in TWO invocations, and they must stay that
	// way: kopia applies --clear-ignore *after* --add-ignore regardless of the order
	// they appear on the command line, so combining them yields a source with no
	// ignore rules at all. Verified against kopia 0.23.1 — and it fails silently, the
	// backup simply succeeds while carrying everything the rules were meant to keep
	// out. TestUserDataExclusionsAreAnchoredCorrectly is what catches a regression.
	if _, err := p.run(ctx, nil, 5*time.Minute, "policy", "set", path, "--clear-ignore"); err != nil {
		return err
	}

	// The exclusions themselves, and why each one is there, are on UserDataExclusions.
	args := []string{"policy", "set", path}
	for _, ig := range UserDataExclusions {
		args = append(args, "--add-ignore", ig)
	}
	_, err := p.run(ctx, nil, 5*time.Minute, args...)
	return err
}

// UserDataExclusions is what the user-data set leaves out, and it is exported because
// the page that offers a restore has to be able to say so: a restore that does not
// bring something back is only diagnosable if the exclusions are visible. One list, so
// what is shown can never drift from what is applied.
//
//   - The app tree has its own per-app sources; backing it up here too would store
//     everything twice — and it is the reason an in-place restore is done entry by
//     entry, since a delete-extra aimed at the data root would remove it.
//   - Cache and logs are matched by pattern rather than by a fixed list, so the next
//     engine someone adds does not silently ship its multi-gigabyte cache offsite every
//     night for data that is rebuilt on demand.
//
// AppDataShared is deliberately *not* excluded: on a box running two engines each
// engine's backup then carries the other's configuration, so recovering either returns
// the rest. The password riding along is harmless — reading it requires the password
// already — it is merely useless.
var UserDataExclusions = []string{"/AppData/", "**/cache/", "**/logs/"}

// Retention is the tiered (GFS) policy. Because each source accumulates snapshots
// over time, this maps directly onto kopia's own policy engine instead of having to
// be reimplemented — a direct dividend of every backup of one app sharing a source
// path.
type Retention struct {
	Latest, Daily, Weekly, Monthly, Annual int
}

// DefaultRetention is the shape consumer backup tools have taught users to expect:
// a week of dailies, a month of weeklies, a year of monthlies.
func DefaultRetention() Retention {
	return Retention{Latest: 2, Daily: 7, Weekly: 4, Monthly: 12, Annual: 0}
}

// EnsureRetention applies the operator's tiers to one app's source.
//
// Latest has a floor of 2 regardless of what is configured: a backup writes two
// snapshots against the same source and deletes the first only after the second
// succeeds, so keeping fewer than two would let retention evict the consistent
// snapshot in favour of the torn one it was about to replace.
func (p *Provider) EnsureRetention(ctx context.Context, app string, keep backupconfig.Keep) error {
	latest := keep.Latest
	if latest < 2 {
		latest = 2
	}
	return p.EnsurePolicy(ctx, AppSource(app), Retention{
		Latest:  latest,
		Daily:   keep.Daily,
		Weekly:  keep.Weekly,
		Monthly: keep.Monthly,
		Annual:  keep.Annual,
	})
}

func (r Retention) args() []string {
	return []string{
		"--keep-latest", strconv.Itoa(r.Latest),
		"--keep-hourly", "0",
		"--keep-daily", strconv.Itoa(r.Daily),
		"--keep-weekly", strconv.Itoa(r.Weekly),
		"--keep-monthly", strconv.Itoa(r.Monthly),
		"--keep-annual", strconv.Itoa(r.Annual),
	}
}

// BackupUserData snapshots everything under the data root that is not an app.
//
// One pass, because there is nothing to stop and therefore nothing a second pass
// would buy. The snapshot has no consistency guarantee — a file being written while
// the engine reads it is captured mid-write — which is normal and accepted for
// documents and media, and is exactly why apps get the stop treatment and this does
// not. The corollary is that a database must never live here.
func (p *Provider) BackupUserData(ctx context.Context, stamp string, emit func(apps.Event)) (string, error) {
	src := UserDataSource()
	// Reapplied every run rather than only at setup: policies live in the repository,
	// so they outlive a Maison reinstall — and a Maison bug can leave a stale one.
	if err := p.EnsurePolicy(ctx, src, DefaultRetention()); err != nil {
		return "", err
	}
	// emit is passed on rather than dropped, which it was for as long as this existed.
	// This is the single largest thing the box backs up — the terabytes are here, not
	// in the apps — and it is the one target with no tile to fall back on, so a run
	// reporting nothing for it was a run that appeared to hang for hours.
	if err := p.SnapshotSource(ctx, src, stamp, 2, emit); err != nil {
		return "", err
	}
	b, err := p.CommitSource(ctx, src, stamp, emit)
	if err != nil {
		return "", err
	}
	return b.Name, nil
}

// ListUserData returns the user-data set's snapshots, newest first.
//
// The set is one source with many snapshots over time, so this is the same read as an
// app's — the reserved App name is what tells them apart on the way out.
func (p *Provider) ListUserData(ctx context.Context) ([]apps.Backup, error) {
	return p.ListSource(ctx, UserDataSource())
}

// RestoreUserData puts the user-data set back.
//
// **In place, this deliberately does not restore the data root as one target.** It
// restores each of the snapshot's top-level entries into its own path. The reason is
// `--delete-extra`, which is what makes a restore a restore rather than a merge: aimed
// at the data root it would delete everything there that the snapshot does not
// contain — and `AppData/` is excluded from this snapshot by policy, so it would
// delete every app's data on the box. Per-entry, `AppData` is never a target and
// cannot be reached. Verified against kopia 0.23.1; TestUserDataRestoreNeverTargetsTheDataRoot
// is what catches a regression.
//
// Two consequences worth stating plainly, because they are the semantics the UI has to
// describe:
//
//   - Within each restored entry the result is exact: files created since the snapshot
//     are removed, deletions are undone, modifications reverted.
//   - A top-level entry that exists on disk but not in the snapshot is left alone. It
//     did not exist when the snapshot was taken, so a true restore would remove it —
//     but silently deleting a whole tree the user made since is a worse surprise than
//     leaving it, and nothing forces the choice to be made here.
//   - AppDataShared/ is skipped in place, though it is in the snapshot. See inPlaceSkip.
func (p *Provider) RestoreUserData(ctx context.Context, stamp string, opts apps.UserDataRestoreOpts, emit func(apps.Event)) error {
	id, err := p.snapshotID(ctx, userDataApp, stamp)
	if err != nil {
		return err
	}

	// Restoring into a fresh directory is the simple case: there is nothing there to
	// delete, so one call does it and --delete-extra would be meaningless.
	if opts.Dest != "" {
		if !filepath.IsAbs(opts.Dest) {
			return fmt.Errorf("kopia: restore destination must be an absolute path: %s", opts.Dest)
		}
		if err := os.MkdirAll(opts.Dest, 0o755); err != nil {
			return err
		}
		_, err = p.run(ctx, emit, 0, "restore", id, opts.Dest, "--progress")
		return err
	}

	entries, err := p.entries(ctx, id)
	if err != nil {
		return err
	}
	wanted, err := selectEntries(entries, opts.Entries)
	if err != nil {
		return err
	}
	if len(wanted) == 0 {
		return fmt.Errorf("kopia: snapshot %s holds nothing to restore", stamp)
	}

	for _, e := range wanted {
		if inPlaceSkip[e.name] {
			// Skipped only when restoring over the live tree. See inPlaceSkip.
			if emit != nil {
				emit(apps.Event{Message: "Leaving " + e.name + " as it is"})
			}
			continue
		}
		dst := filepath.Join(p.cfg.DataRoot, e.name)
		args := []string{"restore", id + "/" + e.name, dst, "--progress"}
		// Only a directory can hold extra files. Aimed at a plain file --delete-extra has
		// nothing to act on, and overwriting is kopia's default.
		if e.dir {
			args = append(args, "--delete-extra")
		}
		if emit != nil {
			emit(apps.Event{Message: "Restoring " + e.name})
		}
		if _, err := p.run(ctx, emit, 0, args...); err != nil {
			return fmt.Errorf("restoring %s: %w", e.name, err)
		}
	}
	return nil
}

// inPlaceSkip is what an in-place restore leaves alone even though the snapshot holds
// it.
//
// AppDataShared/ carries the backup engines' own configuration — endpoint, password,
// cache — and it is in the set deliberately, so that recovering *any* engine's backup
// returns the credentials for the others. That is exactly right for restoring onto a
// fresh box, and exactly wrong for restoring over a live one: the files being replaced
// are the ones the engine performing the restore is reading, and an older repository
// config swapped in mid-restore points the engine at a repository that is not the one
// it is currently reading from.
//
// It is not excluded from the *snapshot* — that would break disaster recovery, which is
// the reason it is included — only from the in-place restore, which is the one case
// where the live copy is more current than the backed-up one by definition.
var inPlaceSkip = map[string]bool{"AppDataShared": true}

// entry is one top-level member of a snapshot.
type entry struct {
	name string
	dir  bool
}

// entries lists a snapshot's top-level members.
//
// `kopia ls` has no --json, so this parses the long form. Fields are mode, size, date,
// time, zone, object id, name — split with a limit so a name containing spaces stays
// whole, and a trailing "/" is what marks a directory. Verified against kopia 0.23.1.
func (p *Provider) entries(ctx context.Context, id string) ([]entry, error) {
	out, err := p.run(ctx, nil, 5*time.Minute, "ls", "-l", id)
	if err != nil {
		return nil, err
	}
	var list []entry
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		// The columns are padded to align, so the separator is a *run* of spaces of
		// unknown width — splitting on single spaces yields empty fields and a name made
		// of column fragments. Anchor on the object id instead, which is the field before
		// the name: everything after it is the name, spaces and all.
		oid := fields[5]
		at := strings.Index(line, oid)
		if at < 0 {
			continue
		}
		name := strings.TrimSpace(line[at+len(oid):])
		dir := strings.HasSuffix(name, "/")
		name = strings.TrimSuffix(name, "/")
		// A name that is not a plain member of the data root has no business becoming a
		// path. The snapshot came from a repository, and this is the value that reaches
		// filepath.Join below.
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
			continue
		}
		list = append(list, entry{name: name, dir: dir})
	}
	return list, nil
}

// selectEntries narrows the snapshot's members to what the caller asked for, refusing
// a name the snapshot does not have — so "restore just Documents" cannot become
// "restore whatever path you name".
func selectEntries(have []entry, want []string) ([]entry, error) {
	if len(want) == 0 {
		return have, nil
	}
	byName := map[string]entry{}
	for _, e := range have {
		byName[e.name] = e
	}
	out := make([]entry, 0, len(want))
	for _, w := range want {
		e, ok := byName[w]
		if !ok {
			return nil, fmt.Errorf("kopia: this backup has no %q to restore", w)
		}
		out = append(out, e)
	}
	return out, nil
}

// --- apps.Provider -----------------------------------------------------------

func (p *Provider) Snapshot(ctx context.Context, app, stamp string, opts apps.SnapshotOpts, emit func(apps.Event)) error {
	return p.SnapshotSource(ctx, AppSource(app), stamp, opts.Pass, emit)
}

// SnapshotSource captures one source. Both passes of a backup write the same
// (app, stamp) and differ only in their pass tag, so the pass-1 snapshot — taken
// while the app was still writing and therefore possibly torn — can be told apart
// from the consistent one and removed.
func (p *Provider) SnapshotSource(ctx context.Context, s Source, stamp string, pass int, emit func(apps.Event)) error {
	if _, ok := apps.ParseBackupName(s.App, stamp); !ok {
		return fmt.Errorf("kopia: not a backup stamp: %s", stamp)
	}
	if pass < 1 {
		pass = 1
	}
	_, err := p.run(ctx, emit, 0,
		"snapshot", "create", p.sourcePath(s),
		"--progress",
		"--tags", tagApp+":"+s.App,
		"--tags", tagStamp+":"+stamp,
		"--tags", tagPass+":"+strconv.Itoa(pass),
	)
	return err
}

// Commit drops the torn first-pass snapshot, leaving the consistent one as the
// backup.
//
// Content is shared between the two, so removing the manifest frees nothing and
// loses nothing — the point is that a user browsing snapshots can never restore the
// inconsistent one. A failure here is logged rather than returned: the real backup
// exists, and refusing to commit it because a cleanup failed would be the worse
// outcome. The stale pass-1 snapshot is invisible to List and is swept later.
func (p *Provider) Commit(ctx context.Context, app, stamp string, opts apps.SnapshotOpts, emit func(apps.Event)) (apps.Backup, error) {
	return p.CommitSource(ctx, AppSource(app), stamp, emit)
}

func (p *Provider) CommitSource(ctx context.Context, s Source, stamp string, emit func(apps.Event)) (apps.Backup, error) {
	snaps, err := p.snapshots(ctx, s.App)
	if err != nil {
		return apps.Backup{}, err
	}
	var committed *snapshot
	for i := range snaps {
		sn := &snaps[i]
		if sn.tag(tagStamp) != stamp {
			continue
		}
		switch sn.tag(tagPass) {
		case "1":
			if err := p.deleteByID(ctx, sn.ID); err != nil {
				log.Printf("kopia: dropping first-pass snapshot %s: %v", sn.ID, err)
			}
		default:
			committed = sn
		}
	}
	if committed == nil {
		return apps.Backup{}, fmt.Errorf("kopia: no snapshot to commit for %s/%s", s.App, stamp)
	}
	b, ok := committed.backup(s.App)
	if !ok {
		return apps.Backup{}, fmt.Errorf("kopia: committed snapshot %s has an unusable stamp", committed.ID)
	}
	return b, nil
}

// Abort removes every snapshot carrying this stamp, so an interrupted backup leaves
// nothing a later List could offer for restore.
func (p *Provider) Abort(ctx context.Context, app, stamp string) error {
	return p.AbortSource(ctx, AppSource(app), stamp)
}

func (p *Provider) AbortSource(ctx context.Context, s Source, stamp string) error {
	snaps, err := p.snapshots(ctx, s.App)
	if err != nil {
		if errors.Is(err, apps.ErrNotConfigured) {
			return nil // nothing was written, so nothing to undo
		}
		return err
	}
	var errs []error
	for _, sn := range snaps {
		if sn.tag(tagStamp) == stamp {
			if err := p.deleteByID(ctx, sn.ID); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (p *Provider) List(ctx context.Context, app string) ([]apps.Backup, error) {
	return p.ListSource(ctx, AppSource(app))
}

// ListSource returns the committed backups of one source, newest first.
//
// Every stamp is re-validated through apps.ParseBackupName before it becomes a
// Backup. That value came back from a repository and is untrusted; validating it
// here is what lets the traversal guard elsewhere stay exactly as strict as it is
// while backups live somewhere other than on disk. A snapshot whose stamp does not
// parse is dropped rather than surfaced.
func (p *Provider) ListSource(ctx context.Context, s Source) ([]apps.Backup, error) {
	snaps, err := p.snapshots(ctx, s.App)
	if err != nil {
		return nil, err
	}
	var out []apps.Backup
	for _, sn := range snaps {
		if sn.tag(tagPass) == "1" {
			continue // torn, never restorable
		}
		if b, ok := sn.backup(s.App); ok {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stamp > out[j].Stamp })
	return out, nil
}

// ListAll returns every app's snapshots in one query, grouped by the app tag rather
// than derived from any local state — that is what lets a rebuilt box list what it
// can restore before anything is installed on it.
//
// One query is the point: the alternative is a subprocess per app, and this backs the
// page that lists every app at once.
//
// The user-data set is excluded: it is a source, not an app, and it has no tile, no
// folder and no per-app page for the global list to link it to.
//
// App names are validated through apps.ValidProjectName and stamps through
// ParseBackupName (inside snapshot.backup), because both arrive from a repository. An
// unparseable tag is dropped rather than surfaced.
func (p *Provider) ListAll(ctx context.Context) (map[string][]apps.Backup, error) {
	snaps, err := p.allSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]apps.Backup{}
	for _, sn := range snaps {
		app := sn.tag(tagApp)
		if app == "" || app == userDataApp || !apps.ValidProjectName(app) {
			continue
		}
		if sn.tag(tagPass) == "1" {
			continue // torn, never restorable — the rule ListSource applies
		}
		if b, ok := sn.backup(app); ok {
			out[app] = append(out[app], b)
		}
	}
	for app := range out {
		list := out[app]
		sort.Slice(list, func(i, j int) bool { return list[i].Stamp > list[j].Stamp })
	}
	return out, nil
}

func (p *Provider) Delete(ctx context.Context, app, stamp string) error {
	return p.AbortSource(ctx, AppSource(app), stamp)
}

// Materialize downloads a backup to .backups/<app>/<stamp> so the ordinary restore
// path can swap it in. It needs room for a full copy of the app; the caller is
// responsible for having checked.
func (p *Provider) Materialize(ctx context.Context, app, stamp string, emit func(apps.Event)) error {
	snaps, err := p.snapshots(ctx, app)
	if err != nil {
		return err
	}
	id := ""
	for _, sn := range snaps {
		if sn.tag(tagStamp) == stamp && sn.tag(tagPass) != "1" {
			id = sn.ID
		}
	}
	if id == "" {
		return fmt.Errorf("backup not found: %s", stamp)
	}
	// Built from the validated stamp via AppBackupDir, exactly as a local archive's
	// path is — the name never reaches a path unvalidated.
	dst := filepath.Join(apps.AppBackupDir(p.cfg.BackupsDir(), app), stamp)
	if err := p.mkdirForEngine(filepath.Dir(dst)); err != nil {
		return err
	}
	_, err = p.run(ctx, emit, 0, "restore", id, dst, "--progress")
	return err
}

// mkdirForEngine creates a directory the engine container writes into, owned by the
// data user.
//
// This is the one place kopia writes to the data disk rather than to the repository.
// The engine runs as root now (see spec), so the chown is no longer what makes the
// write succeed — it is what keeps .backups/<app> looking like the rest of the data
// disk instead of a root-owned island the user cannot manage from an app. It also
// repairs a directory an older Maison left behind.
func (p *Provider) mkdirForEngine(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	uid, uerr := strconv.Atoi(p.cfg.PUID)
	gid, gerr := strconv.Atoi(p.cfg.PGID)
	if uerr != nil || gerr != nil {
		// No usable ids to hand it to. Left as it is rather than guessed at: on a box
		// where Maison and the engine run as the same user this is already correct, and
		// guessing an owner for a directory holding backups is worse than not trying.
		return nil
	}
	if err := os.Chown(dir, uid, gid); err != nil {
		return fmt.Errorf("hand %s to the engine user: %w", dir, err)
	}
	return nil
}

// RestoreInPlace writes a backup straight over the app's folder.
//
// --delete-extra is what makes this a restore rather than a merge: without it, files
// the app created after the backup would survive it, and the result would be a state
// that never existed. Overwriting is kopia's default, so only the deletion has to be
// asked for.
func (p *Provider) RestoreInPlace(ctx context.Context, app, stamp, dst string, emit func(apps.Event)) error {
	id, err := p.snapshotID(ctx, app, stamp)
	if err != nil {
		return err
	}
	_, err = p.run(ctx, emit, 0, "restore", id, dst, "--progress", "--delete-extra")
	return err
}

// snapshotID resolves (app, stamp) to the engine's own identifier.
//
// The value handed to kopia is always one kopia itself returned, never the name the
// caller supplied — so a crafted name cannot reach the command line.
func (p *Provider) snapshotID(ctx context.Context, app, stamp string) (string, error) {
	snaps, err := p.snapshots(ctx, app)
	if err != nil {
		return "", err
	}
	for _, sn := range snaps {
		if sn.tag(tagStamp) == stamp && sn.tag(tagPass) != "1" {
			return sn.ID, nil
		}
	}
	return "", fmt.Errorf("backup not found: %s", stamp)
}

func (p *Provider) deleteByID(ctx context.Context, id string) error {
	_, err := p.run(ctx, nil, 10*time.Minute, "snapshot", "delete", id, "--delete")
	return err
}

// --- snapshot listing --------------------------------------------------------

type snapshot struct {
	ID     string `json:"id"`
	Source struct {
		Host     string `json:"host"`
		UserName string `json:"userName"`
		Path     string `json:"path"`
	} `json:"source"`
	StartTime time.Time `json:"startTime"`
	RootEntry struct {
		Summ struct {
			Size int64 `json:"size"`
		} `json:"summ"`
	} `json:"rootEntry"`
	Tags map[string]string `json:"tags"`
}

// tag reads one of Maison's tags. The JSON spelling carries a "tag:" prefix that the
// command-line spelling does not, which is exactly the sort of asymmetry that makes
// a filter silently match nothing.
func (s snapshot) tag(key string) string { return s.Tags[jsonTagPrefix+key] }

func (s snapshot) backup(app string) (apps.Backup, bool) {
	b, ok := apps.ParseBackupName(app, s.tag(tagStamp))
	if !ok {
		return apps.Backup{}, false
	}
	b.Tier = apps.TierRemote
	b.Engine = ID
	b.Size = s.RootEntry.Summ.Size
	return b, true
}

func (p *Provider) snapshots(ctx context.Context, app string) ([]snapshot, error) {
	return p.listSnapshots(ctx, "--tags", tagApp+":"+app)
}

// allSnapshots is the same read with no app filter, for enumerating which apps the
// repository knows about.
func (p *Provider) allSnapshots(ctx context.Context) ([]snapshot, error) {
	return p.listSnapshots(ctx)
}

func (p *Provider) listSnapshots(ctx context.Context, filter ...string) ([]snapshot, error) {
	out, err := p.run(ctx, nil, 5*time.Minute,
		append([]string{"snapshot", "list", "--all", "--json"}, filter...)...)
	if err != nil {
		return nil, err
	}
	var snaps []snapshot
	if err := json.Unmarshal(out, &snaps); err != nil {
		return nil, fmt.Errorf("kopia: unreadable snapshot list: %w", err)
	}
	return snaps, nil
}
