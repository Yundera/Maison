// Package adapter is the engine-agnostic backup provider.
//
// It speaks the Maison backup adapter protocol — argv in, NDJSON on stdout, documented
// exit codes — to a `maison-engine` binary inside an engine container. It knows no
// engine: everything engine-specific lives in the adapter image, which is versioned and
// shipped independently of Maison.
//
// See docs/backup.md § The engine adapter, and the protocol specification in the
// maison-kopia-engine repository.
package adapter

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/engine"
)

// DescriptorFile is what the host side writes beside the rest of an engine's
// configuration to say that an adapter exists for it.
const DescriptorFile = "adapter.json"

// Protocol is the protocol version this build speaks. An adapter announcing anything
// else is refused rather than guessed at — an adapter speaking an unknown dialect is
// exactly the case where "try it and see" costs a backup.
const Protocol = "v0"

// Descriptor is one adapter, as the host side declares it.
//
// It lives in the engine's own directory, next to repository.config and state.json,
// because that is already the contract: the host provisions, Maison reads what it
// finds. An engine with no descriptor does not exist as far as Maison is concerned,
// which is what keeps "which engines are there" out of Maison's build.
//
// **The image is named here and nowhere else.** That is the line that keeps an adapter
// from being a remote-execution surface: what Maison runs as root with app data in
// scope is named by the deployment, never by a user. Which repository an engine points
// at remains an ordinary user setting.
type Descriptor struct {
	// EngineID is permanent and is recorded on every backup this engine writes.
	EngineID string `json:"engineId"`

	Image string `json:"image"`

	// Container is the resident container to exec into. Empty means one-shot, which is
	// slower by six or seven seconds a command and is not the deployed shape.
	Container string `json:"container,omitempty"`

	// Entrypoint is the adapter binary inside the image. `docker exec` does not apply
	// the image's own ENTRYPOINT, so it has to be named.
	Entrypoint string `json:"entrypoint,omitempty"`

	// Hostname is the identity the engine files backups under, and the value the
	// resident container must have been created with.
	//
	// Maison compares it against the container before using it. It is READ from the
	// host side rather than computed, because two sides computing an identity
	// independently is how one repository ends up holding two lineages that never see
	// each other — invisible until a restore comes back empty.
	Hostname string `json:"hostname,omitempty"`

	// Network is "none" for a repository on a local filesystem, which must not be given
	// networking, and "default" for one that has to reach a bucket. It describes the
	// container the host deployed; Maison only restates it for the one-shot path.
	Network string `json:"network,omitempty"`
}

const defaultEntrypoint = "/usr/local/bin/maison-engine"

func (d Descriptor) entrypoint() string {
	if d.Entrypoint != "" {
		return d.Entrypoint
	}
	return defaultEntrypoint
}

func (d Descriptor) network() engine.Network {
	if d.Network == string(engine.NetworkNone) {
		return engine.NetworkNone
	}
	return engine.NetworkDefault
}

func (d Descriptor) validate() error {
	if d.EngineID == "" {
		return fmt.Errorf("descriptor names no engineId")
	}
	if !apps.ValidProjectName(d.EngineID) {
		// The id becomes a directory name and a recorded field on every backup.
		return fmt.Errorf("engineId %q is not a usable identifier", d.EngineID)
	}
	if d.Image == "" {
		return fmt.Errorf("descriptor for %q names no image", d.EngineID)
	}
	return nil
}

// Discover reads every adapter descriptor under the shared backup directory.
//
// A directory with no descriptor is skipped in silence: it is the ordinary state of a
// box whose host side has not run, or of an engine Maison implements itself. A
// descriptor that is present but unusable is logged and skipped — an engine that cannot
// be constructed must not stop the others being registered, and must not stop Maison
// booting.
func Discover(cfg config.Config) []Descriptor {
	root := filepath.Join(cfg.SharedDir(), "backup")
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []Descriptor
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		path := filepath.Join(root, d.Name(), DescriptorFile)
		desc, err := ReadDescriptor(path)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("backup: ignoring %s: %v", path, err)
			}
			continue
		}
		// The directory is the engine's own, so a descriptor claiming a different id
		// would be read out of one engine's configuration and write into another's.
		if desc.EngineID != d.Name() {
			log.Printf("backup: ignoring %s: it claims engine %q but sits in %q", path, desc.EngineID, d.Name())
			continue
		}
		out = append(out, desc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EngineID < out[j].EngineID })
	return out
}

// ReadDescriptor reads one descriptor. os.IsNotExist is reported unwrapped so Discover
// can tell "no adapter here" from "an adapter that will not load".
func ReadDescriptor(path string) (Descriptor, error) {
	var d Descriptor
	b, err := os.ReadFile(path)
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return d, fmt.Errorf("unreadable adapter descriptor: %w", err)
	}
	if err := d.validate(); err != nil {
		return d, err
	}
	return d, nil
}
