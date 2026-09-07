package backupconfig

import "time"

// Per-engine settings and how the layers resolve.
//
// Two things live here. The first is Mode: retention expressed as an *intent* the
// user picked, rather than as four numbers they have to reason about. The second is
// the rule that keeps the nightly host-side script and the user's own choices from
// overwriting each other — a setting is stored only where it was decided, and
// anything not decided is inherited from the layer below.
//
// That rule is the generalisation of what Config.Engine already does, and the reason
// is the same: the deployment re-renders its side of the configuration every night,
// so any field the two sides share is a field the script silently reverts.

// EngineLocal is apps.EngineLocal, restated rather than imported: this package is the
// small persistence layer at the bottom of the backup stack, and pulling the whole app
// registry in for one string would invert that. Engine IDs are permanent once shipped,
// so the two cannot drift.
const EngineLocal = "local"

// Mode is what the user asked retention to do. It is the *only* field that selects,
// which is deliberate: every other field (tiers, counts, ages) has a meaningful zero
// and so cannot distinguish "the user chose none" from "nobody has said". Mode has
// an explicit unset value, so the whole layering below can be a search for the first
// layer whose Mode is set, with no non-zero-wins guesswork of the kind this package's
// doc comment exists to complain about.
type Mode string

const (
	// ModeInherit is the zero value: this layer expresses no opinion, ask the one
	// below. It is what "reset to default" writes — never today's default value,
	// which would look identical now and diverge the first time the fleet's default
	// changes.
	ModeInherit Mode = ""

	// ModeSmart is the tiering consumer backup tools have taught users to expect:
	// one backup for each of the last 7 days, each of the last 4 weeks, each of the
	// last 12 months.
	ModeSmart Mode = "smart"

	// ModeCustom is ModeSmart with the tiers the user typed.
	ModeCustom Mode = "custom"

	// ModeCount keeps the N most recent backups and nothing older.
	ModeCount Mode = "count"

	// ModeAll never expires anything.
	ModeAll Mode = "all"

	// ModeAge keeps everything newer than MaxAgeDays. It is the only model bucket
	// lifecycle rules can express, and the only one a snapshot engine's own policy
	// engine generally cannot — see apps.RetentionModel.
	ModeAge Mode = "age"
)

// Valid reports whether m is a mode this build understands. An unknown mode — from a
// newer Maison that wrote the file, or from a hand edit — resolves as ModeInherit
// rather than as an error: falling back to the layer below keeps backups running,
// and refusing to boot over a settings file does not.
func (m Mode) Valid() bool {
	switch m {
	case ModeInherit, ModeSmart, ModeCustom, ModeCount, ModeAll, ModeAge:
		return true
	}
	return false
}

// set reports whether this layer decided the retention question.
func (m Mode) set() bool { return m.Valid() && m != ModeInherit }

// SmartKeep is what ModeSmart means in tiers.
//
// Latest is 2 rather than 1 because a backup writes two snapshots against one source
// and deletes the first only once the second succeeds; keeping fewer than two would
// let retention evict the consistent snapshot in favour of the torn one it was about
// to replace. See kopia.EnsureRetention, which enforces the same floor independently.
func SmartKeep() Keep { return Keep{Latest: 2, Daily: 7, Weekly: 4, Monthly: 12} }

// KeepAll is the tier value that means "do not expire". No engine has an unlimited
// keyword — kopia's policy takes counts and reads 0 as *delete*, which is the exact
// opposite — so "keep everything" has to be a number large enough that no repository
// reaches it. A million daily backups is 2700 years.
const KeepAll = 1_000_000

// EngineSettings is one layer's opinion about one engine. Every field is optional;
// an unset field defers to the layer below.
type EngineSettings struct {
	// Mode selects, and carries Keep / Count / MaxAgeDays with it: whichever layer
	// sets Mode supplies the parameter that mode reads. Splitting them across layers
	// would let a custom tier set from one place be interpreted by a count from
	// another.
	Mode Mode `json:"mode,omitempty"`

	// Keep is read only under ModeCustom.
	Keep Keep `json:"keep,omitempty"`

	// Count is read only under ModeCount.
	Count int `json:"count,omitempty"`

	// MaxAgeDays is read only under ModeAge.
	MaxAgeDays int `json:"max_age_days,omitempty"`

	// Schedule and Uninstall are the triggers this engine receives: does the nightly
	// run write here, and does an uninstall archive here.
	//
	// They are what replaced the single "default engine". Engines are independent
	// repositories — Set.List has always said so — and the last thing tying them
	// together was that exactly one of them could be written to. A backup can now land
	// in several at once, and each engine says for itself what it is for.
	//
	// POINTERS, because the three states are all real and all different: on, off, and
	// "nobody has said". A box that has never opened the settings page must keep
	// writing wherever it was provisioned to, and that is the third state — see
	// engineTriggers in the server, which is where the fallback lives. A plain bool
	// would make every unconfigured box read as "off", which is a box that silently
	// stops backing up.
	//
	// The update rollback point is deliberately not here. It is always the local
	// engine because rolling back has to be a rename, which is a property of the
	// mechanism rather than a preference — see server.BackupBeforeUpdate.
	Schedule  *bool `json:"schedule,omitempty"`
	Uninstall *bool `json:"uninstall,omitempty"`

	// UploadLimitMB caps upload bandwidth. Meaningful only to an Offsite engine.
	UploadLimitMB int `json:"upload_limit_mb,omitempty"`

	// Unmanaged suppresses the retention policy Maison would otherwise push into the
	// engine's own repository, for an operator who drives it by hand.
	//
	// It is a stored flag rather than something inferred from what the repository
	// already holds, because Maison reapplies its policy on every run by design — a
	// policy lives in the repository and therefore outlives a Maison reinstall, and a
	// Maison bug can leave a stale one behind. "Reapply every run" and "whatever is
	// in the repository wins" cannot both be true, so the operator says which.
	Unmanaged bool `json:"unmanaged,omitempty"`
}

// Provisioned is the deployment's side of the settings — what the host-side
// self-check script renders onto the box. Maison never writes it; the script never
// writes Maison's state file. One writer per file is what makes the nightly
// re-render safe.
type Provisioned struct {
	Settings EngineSettings

	// Locked names the fields the deployment pins ("mode", "keep", "upload_limit_mb", …).
	// A locked field is rendered disabled with a reason rather than accepted and then
	// silently reverted the next morning.
	//
	// It is advisory here: this package reports it so the UI and the API can honour
	// it. Enforcement belongs where the write happens, not where the value is read.
	Locked []string
}

// Source says which layer decided a value, so the UI can label a number as the
// deployment's default rather than as something the user chose.
type Source string

const (
	SourceDefault     Source = "default"     // compiled in
	SourceProvisioned Source = "provisioned" // rendered by the host-side script
	SourceBox         Source = "box"         // this box's own setting, all engines
	SourceEngine      Source = "engine"      // this box's setting for this engine
)

// Resolved is the answer: what this engine should actually do, with the tiers already
// derived from the mode so no caller has to know what "smart" means.
type Resolved struct {
	Mode   Mode
	Keep   Keep
	Count  int
	MaxAge time.Duration

	UploadLimitMB int
	Unmanaged     bool

	// Schedule and Uninstall are the triggers this engine receives, and Stated is
	// whether anyone actually decided them.
	//
	// Stated is the whole point of resolving them here rather than reading the
	// pointers directly: false means no layer has an opinion about this engine, and
	// the caller must fall back to the engine the box was provisioned with instead of
	// treating the zero value as "receives nothing". That fallback needs to know which
	// engine that is, which this package deliberately does not.
	Schedule  bool
	Uninstall bool
	Stated    bool

	// Source is where Mode came from. UploadLimitMB, Unmanaged and the triggers are
	// resolved separately and may come from a different layer; the UI labels the
	// retention block, which is the part a user can be surprised by.
	Source Source

	// Locked is carried through from Provisioned unchanged.
	Locked []string
}

// Locks reports whether the deployment pinned a named field.
func (r Resolved) Locks(field string) bool {
	for _, f := range r.Locked {
		if f == field {
			return true
		}
	}
	return false
}

// Effective resolves the layers for one engine, most specific first:
//
//	engine override  →  box-wide setting  →  provisioned default  →  compiled default
//
// Only the retention block travels together (Mode plus the one parameter that mode
// reads); UploadLimitMB and Unmanaged are resolved field by field, because they
// answer questions that have nothing to do with each other.
func (c Config) Effective(engineID string, prov Provisioned) Resolved {
	layers := []struct {
		src Source
		s   EngineSettings
	}{
		{SourceEngine, c.Engines[engineID]},
		{SourceBox, c.boxSettings()},
		{SourceProvisioned, prov.Settings},
		{SourceDefault, EngineSettings{Mode: ModeSmart}},
	}

	out := Resolved{Locked: prov.Locked}

	for _, l := range layers {
		if !l.s.Mode.set() {
			continue
		}
		out.Source = l.src
		out.Mode = l.s.Mode
		switch l.s.Mode {
		case ModeSmart:
			out.Keep = SmartKeep()
		case ModeCustom:
			out.Keep = sanitizeKeep(l.s.Keep)
		case ModeCount:
			// Exactly what an engine's own "keep the newest N" is: no tiers at all.
			out.Count = max(l.s.Count, 1)
			out.Keep = Keep{Latest: out.Count}
		case ModeAll:
			out.Keep = Keep{Latest: KeepAll, Daily: KeepAll, Weekly: KeepAll, Monthly: KeepAll, Annual: KeepAll}
		case ModeAge:
			out.MaxAge = time.Duration(max(l.s.MaxAgeDays, 1)) * 24 * time.Hour
			// Tiers are deliberately wide open rather than zero. An age is not
			// expressible as a tier, so a caller that pushes Keep into an engine's own
			// policy must push "keep everything" and leave the age to the planner —
			// pushing zeros would delete the whole history the age was meant to keep.
			out.Keep = Keep{Latest: KeepAll, Daily: KeepAll, Weekly: KeepAll, Monthly: KeepAll, Annual: KeepAll}
		}
		break
	}

	// Tier retention is unsound on the local engine, and for a physical reason rather
	// than a matter of taste: a local archive is a FULL SECOND COPY of the app folder
	// (apps.Caps.NeedsLocalSpace), so 7 daily + 4 weekly + 12 monthly is twenty-three
	// complete copies of every app, on the same disk as the app. A repository stores
	// history incrementally and can afford exactly that; this cannot — it is not a
	// retention policy, it is how a PCS fills its disk.
	//
	// So a tier mode resolved for the local engine becomes the count its own Latest
	// tier already names, which is 2 under the smart preset: the copy being replaced
	// and the one before it. An explicit count, age, or keep-everything is left alone
	// — each is bounded by something the user chose on purpose.
	if engineID == EngineLocal && (out.Mode == ModeSmart || out.Mode == ModeCustom) {
		out.Mode = ModeCount
		out.Count = max(out.Keep.Latest, 1)
		out.Keep = Keep{Latest: out.Count}
	}

	// Each trigger resolves on its own, most specific layer that states it. They do not
	// travel together the way the retention block does: "back this engine up nightly"
	// and "archive uninstalls here" are unrelated answers, and a deployment that pins
	// one must not thereby decide the other.
	for _, l := range layers {
		if l.s.Schedule != nil {
			out.Schedule, out.Stated = *l.s.Schedule, true
			break
		}
	}
	for _, l := range layers {
		if l.s.Uninstall != nil {
			out.Uninstall, out.Stated = *l.s.Uninstall, true
			break
		}
	}

	for _, l := range layers {
		if l.s.UploadLimitMB > 0 {
			out.UploadLimitMB = l.s.UploadLimitMB
			break
		}
	}
	for _, l := range layers {
		if l.s.Unmanaged {
			out.Unmanaged = true
			break
		}
	}
	return out
}

// boxSettings is the box-wide layer, built from the flat fields that predate the
// per-engine map. They are what the settings page writes when the user edits
// retention without singling out an engine.
func (c Config) boxSettings() EngineSettings {
	return EngineSettings{Mode: c.Mode, Keep: c.Keep, Count: c.Count, MaxAgeDays: c.MaxAgeDays}
}

// sanitizeKeep clamps a tier set that would otherwise keep nothing. The floor is on
// Latest alone: a user who genuinely wants "only the most recent" says so through
// ModeCount, and every other tier being zero is a legitimate way to say "no weeklies".
func sanitizeKeep(k Keep) Keep {
	if k.Latest < 1 {
		k.Latest = 1
	}
	k.Daily = max(k.Daily, 0)
	k.Weekly = max(k.Weekly, 0)
	k.Monthly = max(k.Monthly, 0)
	k.Annual = max(k.Annual, 0)
	return k
}
