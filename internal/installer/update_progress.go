package installer

// Update phases, in the order ApplyUpdate runs them. The tile and the Updates page
// label the progress bar by phase, so each is a step an owner can recognise — and
// the one that takes minutes (backup) carries real byte counts.
const (
	UpdatePhaseCheck    = "check"    // measuring room for the rollback point
	UpdatePhasePull     = "pull"     // pulling the new version's images
	UpdatePhaseBackup   = "backup"   // taking the rollback point
	UpdatePhaseStop     = "stop"     // stopping the old version
	UpdatePhaseApply    = "apply"    // writing the new compose and seed tree
	UpdatePhaseStart    = "start"    // bringing the new version up
	UpdatePhaseRollback = "rollback" // putting the old version back
)

// UpdateState is a snapshot of one in-flight update, overlaid onto the app list so
// the tile shows a progress bar for the step running now instead of a bare busy
// overlay. Only the phase that is running is described: Pct is that step's own
// progress, not an average across the update.
type UpdateState struct {
	ID      string  `json:"id"`
	Phase   string  `json:"phase"`
	Message string  `json:"message,omitempty"`
	Pct     float64 `json:"pct"`
	// Byte counts, rate and seconds left — backup phase only, and only once the
	// copy engine can say. Zero means unknown and is omitted.
	Done  int64   `json:"done,omitempty"`
	Total int64   `json:"total,omitempty"`
	Rate  float64 `json:"rate,omitempty"`
	ETA   int     `json:"eta,omitempty"`
}

// Updates returns every update in flight.
func (in *Installer) Updates() []UpdateState {
	in.mu.Lock()
	defer in.mu.Unlock()
	out := make([]UpdateState, 0, len(in.updates))
	for _, st := range in.updates {
		out = append(out, *st)
	}
	return out
}

// setUpdate records the step an update is on and rebroadcasts the tiles.
func (in *Installer) setUpdate(project string, st UpdateState) {
	st.ID = project
	in.mu.Lock()
	if in.updates == nil {
		in.updates = map[string]*UpdateState{}
	}
	in.updates[project] = &st
	in.mu.Unlock()
	in.notify()
}

// endUpdate drops an update's progress. The outcome itself — success, rollback,
// refusal — is reported by ApplyUpdate's result and incidents, not by the bar.
func (in *Installer) endUpdate(project string) {
	in.mu.Lock()
	_, existed := in.updates[project]
	delete(in.updates, project)
	in.mu.Unlock()
	if existed {
		in.notify()
	}
}
