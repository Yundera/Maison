package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/engine"
)

// call runs one verb and decodes its result.
//
// Everything about HOW it runs — the container, its capabilities, its network, its
// mounts — is the engine stack's declaration and is only restated here for the one-shot
// path. What this adds is the verb, its flags, and the engine directory.
//
// **No secrets are passed.** The adapter reads the repository password and the storage
// credentials from its own directory, which it has mounted. That keeps Maison out of the
// repository-password path entirely except for escrow, which reads the file directly
// because it has to work when the engine does not.
func (p *Provider) call(ctx context.Context, timeout time.Duration, emit func(apps.Event), result any, verb string, args ...string) error {
	full := append([]string{verb}, args...)
	full = append(full, "--repo-dir="+p.dir())

	spec := engine.Spec{
		Image:           p.desc.Image,
		Name:            invocationName(p.desc.EngineID, verb),
		Hostname:        p.hostname(),
		User:            "0:0",
		Caps:            engineCaps,
		NoNewPrivileges: true,
		Network:         p.desc.network(),
		Mounts:          []engine.Mount{p.runner.DataMount(false)},
		Args:            full,
		Timeout:         timeout,
		Container:       p.desc.Container,
		Entrypoint:      p.desc.entrypoint(),
	}

	// Progress arrives on stdout as NDJSON, not on stderr, so the runner's line callback
	// is left nil and the whole of stdout is decoded below. That is the difference from
	// an engine driven directly: an adapter frames its own progress.
	out, runErr := p.runner.Run(ctx, spec, nil)

	// Decoded even on failure: an engine that reported progress and then failed has
	// still told the user how far it got, and the result line may carry detail.
	raw, decErr := decode(out, emit)
	if runErr != nil {
		return classify(runErr)
	}
	if decErr != nil {
		return decErr
	}
	if result == nil {
		return nil
	}
	return unmarshalResult(raw, result)
}

// hostname is the identity the engine files backups under, read from the descriptor
// rather than computed.
//
// It is what engine.Runner compares against the resident container before using it: a
// container disagreeing would open a second lineage inside one repository, invisible
// until a restore came back empty. The fallback is obviously synthetic, so a deployment
// missing its pin is recognisable rather than silently divergent — and a mismatch costs
// a slower invocation, never a misfiled one.
func (p *Provider) hostname() string {
	if p.desc.Hostname != "" {
		return p.desc.Hostname
	}
	return "maison-unpinned"
}

// invocationName is unique per call, because it is both the one-shot container's name
// and the pid file the resident path cancels through.
func invocationName(engineID, verb string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, engineID+"-"+verb)
	return "maison-" + safe + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func logLine(l line) {
	level := l.Level
	if level == "" {
		level = "info"
	}
	log.Printf("backup engine [%s]: %s", level, l.Message)
}

// writeExcludes renders the caller's exclusion patterns where the engine container can
// read them, and returns a cleanup.
//
// It has to live under the data root: that is the only thing the engine container
// mounts. Maison's own state directory is inside it and is already Maison's to write.
//
// Patterns go through a file rather than repeated flags because a pattern is whatever
// an app author wrote, and an argv built from untrusted text is an argv worth not
// building.
// An EMPTY rule set still produces a file: the caller asking for exclusions to be
// installed and having none is a real request, and it is how a rule an app author has
// removed stops applying.
func (p *Provider) writeExcludes(rules []string) (path string, cleanup func(), err error) {
	dir := filepath.Join(p.cfg.StateDir(), ".engine")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", func() {}, err
	}
	f, err := os.CreateTemp(dir, "exclude-*.txt")
	if err != nil {
		return "", func() {}, err
	}
	name := f.Name()
	var body string
	if len(rules) > 0 {
		body = strings.Join(rules, "\n") + "\n"
	}
	_, werr := f.WriteString(body)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(name)
		return "", func() {}, fmt.Errorf("writing backup exclusions: %w", firstErr(werr, cerr))
	}
	// Readable by the engine, which runs as root — the mode matters only so a later
	// non-root reader is not surprised.
	_ = os.Chmod(name, 0o644)
	return name, func() { _ = os.Remove(name) }, nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// keepJSON renders retention tiers for --keep. One value rather than five flags, so a
// caller that meant to clear a tier cannot silently leave the previous one in place.
func keepJSON(k struct{ Latest, Daily, Weekly, Monthly, Annual int }) (string, error) {
	b, err := json.Marshal(map[string]int{
		"latest": k.Latest, "daily": k.Daily, "weekly": k.Weekly,
		"monthly": k.Monthly, "annual": k.Annual,
	})
	return string(b), err
}
