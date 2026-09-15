package adapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/yundera/maison/internal/apps"
)

// The adapter protocol's wire types, and the exit codes that carry the states an error
// string cannot.
//
// They are declared here rather than imported from the adapter's own module
// deliberately: the protocol is at v0 and expected to churn, and a shared module would
// make every change a cross-repo version bump on both sides at once. Two declarations
// of a dozen fields is the cheaper cost until it settles.

const (
	typeProgress = "progress"
	typeLog      = "log"
	typeResult   = "result"
)

// Exit codes. 10 and 11 must stay distinct from 1: collapsing 10 turns an unprovisioned
// box into a red page, and collapsing 11 turns a capability gap into a fault.
const (
	exitNotConfigured = 10
	exitNotSupported  = 11
	exitNotWritable   = 12
)

// ErrNotWritable is a repository that is reachable but refuses writes — a storage space
// suspended for quota. Reads and restores keep working.
var ErrNotWritable = errors.New("the backup repository is not accepting writes")

type line struct {
	Type    string   `json:"type"`
	Message string   `json:"message,omitempty"`
	Level   string   `json:"level,omitempty"`
	Pct     *float64 `json:"pct,omitempty"`
	Done    int64    `json:"done,omitempty"`
	Total   int64    `json:"total,omitempty"`

	Result json.RawMessage `json:"result,omitempty"`
}

type wireCaps struct {
	EngineID       string `json:"engineId"`
	EngineVersion  string `json:"engineVersion"`
	AdapterVersion string `json:"adapterVersion"`
	Protocol       string `json:"protocol"`

	Offsite           bool `json:"offsite"`
	Encrypted         bool `json:"encrypted"`
	KeyEscrow         bool `json:"keyEscrow"`
	InstantRestore    bool `json:"instantRestore"`
	NeedsLocalSpace   bool `json:"needsLocalSpace"`
	InPlaceRestore    bool `json:"inPlaceRestore"`
	IncrementalPasses bool `json:"incrementalPasses"`
	ConsumesSource    bool `json:"consumesSource"`
	Retention         bool `json:"retention"`

	RetentionModel string `json:"retentionModel"`
}

func (c wireCaps) caps() apps.Caps {
	return apps.Caps{
		Offsite:         c.Offsite,
		InstantRestore:  c.InstantRestore,
		NeedsLocalSpace: c.NeedsLocalSpace,
		InPlaceRestore:  c.InPlaceRestore,
		Retention:       c.Retention,
		ConsumesSource:  c.ConsumesSource,
		Encrypted:       c.Encrypted,
		KeyEscrow:       c.KeyEscrow,
	}
}

type wireStatus struct {
	Configured bool   `json:"configured"`
	Connected  bool   `json:"connected"`
	Identity   string `json:"identity"`
	Detail     string `json:"detail"`
}

type wireBackup struct {
	SourceID  string    `json:"sourceId"`
	Stamp     string    `json:"stamp"`
	CreatedAt time.Time `json:"createdAt"`
	Size      int64     `json:"size"`
}

// backup validates the stamp before it becomes an apps.Backup.
//
// The value came back from a repository and is untrusted; re-parsing it here is what
// lets the traversal guards elsewhere stay exactly as strict as they are while backups
// live somewhere other than on this disk. A stamp that does not parse is dropped rather
// than surfaced.
func (b wireBackup) backup(app, engineID string) (apps.Backup, bool) {
	out, ok := apps.ParseBackupName(app, b.Stamp)
	if !ok {
		return apps.Backup{}, false
	}
	out.Tier = apps.TierRemote
	out.Engine = engineID
	out.Size = b.Size
	return out, true
}

type wireEntry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
}

// classify turns an adapter's exit code into the error the rest of Maison branches on.
//
// A failure that is not one of the named states keeps the engine's own message: the
// runner has already appended the tail of stderr, which is the only thing that makes a
// repository failure diagnosable.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		// Not the engine reporting a state — the runner failed to reach it at all.
		return err
	}
	switch ee.ExitCode() {
	case exitNotConfigured:
		return apps.ErrNotConfigured
	case exitNotSupported:
		return apps.ErrNotSupported
	case exitNotWritable:
		return ErrNotWritable
	default:
		return err
	}
}

// decode reads the NDJSON a verb produced, feeding progress to emit and returning the
// single result line's payload.
//
// A line that is not JSON is skipped rather than failing the verb: the contract says
// stdout is NDJSON, but an engine that prints one stray line should not turn a
// completed backup into a failure.
func decode(out []byte, emit func(apps.Event)) (json.RawMessage, error) {
	var result json.RawMessage
	for _, raw := range strings.Split(string(out), "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		var l line
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			continue
		}
		switch l.Type {
		case typeProgress:
			if emit == nil {
				continue
			}
			ev := apps.Event{Message: l.Message, Done: l.Done, Total: l.Total, Pct: apps.PctUnknown}
			if l.Pct != nil {
				ev.Pct = *l.Pct
			}
			emit(ev)
		case typeLog:
			logLine(l)
		case typeResult:
			if result != nil {
				return nil, fmt.Errorf("the engine reported two results")
			}
			result = l.Result
		}
	}
	return result, nil
}

// unmarshalResult decodes a verb's payload, treating an absent result as a protocol
// failure — a verb that is supposed to answer and did not is not an empty answer.
func unmarshalResult(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return fmt.Errorf("the engine produced no result")
	}
	return json.Unmarshal(raw, v)
}
