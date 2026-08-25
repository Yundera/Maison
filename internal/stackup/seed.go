package stackup

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
)

// SeedDir is the app's own copy of the seed tree its store shipped, kept inside
// the app folder.
//
// It is a copy, not a reference back into the store cache, for the same reason
// docker-compose.yml is copied byte-for-byte at install: after that the app
// folder stands on its own (docs/app-model.md). The backup archives it with
// everything else, a re-ensure works on a box whose store list has changed, and
// `docker compose up -d` run by hand in the folder needs nothing else present.
const SeedDir = ".seed"

// TemplateSuffix marks a seed file to render rather than copy. It is stripped
// from the name on the way out: seed/element/config.json.tmpl becomes
// element/config.json.
//
// A suffix rather than sniffing whether a file "looks like text": a store ships
// SQL dumps, icons and shell scripts beside its templates, and every heuristic
// for telling them apart is wrong on somebody's file. Declaring it in the name
// also means the rendered form is visible in a `git diff` of the store.
const TemplateSuffix = ".tmpl"

// reservedSeedTargets are names a seed tree may not write at the top of the app
// folder: they are the app's identity, not its data, and Maison or the store
// owns each of them.
var reservedSeedTargets = map[string]bool{
	"docker-compose.yml":          true,
	"docker-compose.override.yml": true,
	".env":                        true,
	SeedDir:                       true,
}

// EnsureSeed mirrors the app's seed tree into its folder: a path inside
// SeedDir is the same path inside the app directory, so the store declares a
// file by putting it where it goes rather than by naming a destination.
//
// Create-if-absent, on every up — the same contract folders has. An app that
// rewrites its own config keeps its version; an operator who edits one keeps
// theirs; a file that has gone missing comes back. The cost, accepted
// deliberately, is that a seed file deleted on purpose returns on the next
// start.
//
// claimed names targets a `files` entry owns (see EnsureFiles): those are
// skipped here so nothing is written twice in one converge, and so an
// `ensure: always` file is never briefly seeded with a stale render first.
//
// Targets always land inside the app folder, which is already a container-side
// path, so unlike folders/files/init this needs no host↔container mapping at
// all — there is nowhere outside the data root for it to reach.
func EnsureSeed(cfg config.Config, project, dir string, claimed map[string]bool, captures map[string]string) error {
	root := filepath.Join(dir, SeedDir)
	if _, err := os.Stat(root); err != nil {
		return nil // no seed tree — the common case
	}
	envFile := readEnvFile(dir)

	// Two seed entries rendering to the same target (config.json and
	// config.json.tmpl) would make the result depend on walk order.
	written := map[string]string{}

	return filepath.WalkDir(root, func(src string, d fs.DirEntry, err error) error {
		if err != nil || src == root {
			return err
		}
		rel, err := filepath.Rel(root, src)
		if err != nil {
			return err
		}
		// A symlink in a downloaded tree can point anywhere, including out of the
		// app folder; nothing in a store needs one.
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("seed %s: symlinks are not allowed", rel)
		}
		if d.IsDir() {
			return ensureSeedDir(cfg, filepath.Join(dir, rel))
		}

		target := filepath.Join(dir, strings.TrimSuffix(rel, TemplateSuffix))
		if err := checkSeedTarget(dir, target, rel); err != nil {
			return err
		}
		if prev, dup := written[target]; dup {
			return fmt.Errorf("seed %s and %s both write %s", prev, rel,
				strings.TrimSuffix(rel, TemplateSuffix))
		}
		written[target] = rel

		if claimed[target] {
			return nil // a files entry owns this path
		}
		if _, err := os.Lstat(target); err == nil {
			return nil // already there: never clobber the app's or the operator's copy
		}

		content, mode, err := readSeedFile(src, rel, cfg, project, envFile, captures)
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, content, mode); err != nil {
			return fmt.Errorf("seed %s: %w", rel, err)
		}
		// WriteFile applies the umask, so pin the mode we meant.
		if err := os.Chmod(target, mode); err != nil {
			log.Printf("seed %s: chmod: %v", rel, err)
		}
		chownDefault(cfg, target)
		return nil
	})
}

// readSeedFile returns what a seed entry should write and the mode to write it
// with: a template rendered strictly, anything else copied byte-for-byte.
func readSeedFile(src, rel string, cfg config.Config, project string, envFile []byte, captures map[string]string) ([]byte, fs.FileMode, error) {
	raw, err := os.ReadFile(src)
	if err != nil {
		return nil, 0, fmt.Errorf("seed %s: %w", rel, err)
	}
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(src); err == nil && info.Mode().Perm()&0o111 != 0 {
		// The store marked it executable and appstore.extractZip carried that bit
		// through — Guacamole's init script is one.
		mode = 0o755
	}
	if !strings.HasSuffix(rel, TemplateSuffix) {
		return raw, mode, nil
	}
	rendered, err := envinject.RenderStrict(string(raw), cfg, project, envFile, captures)
	if err != nil {
		// The old form of this failure wrote the file anyway, with an empty
		// substitution, and exited 0.
		return nil, 0, fmt.Errorf("seed %s: %w", rel, err)
	}
	return []byte(rendered), mode, nil
}

// ensureSeedDir creates a directory the seed tree implies, and takes ownership
// of it — but only if it is creating it. A directory the app declared under
// `folders` already exists by now, with the owner and mode that declaration
// asked for, and folders stays the one place a directory's identity is stated.
func ensureSeedDir(cfg config.Config, path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(path, DefaultFolderMode); err != nil {
		return err
	}
	if err := os.Chmod(path, DefaultFolderMode); err != nil {
		log.Printf("seed dir %s: chmod: %v", path, err)
	}
	chownDefault(cfg, path)
	return nil
}

// checkSeedTarget rejects a seed entry that would write outside the app folder
// or over the app's identity.
func checkSeedTarget(dir, target, rel string) error {
	clean := filepath.Clean(target)
	if clean != filepath.Clean(dir) && !strings.HasPrefix(clean, filepath.Clean(dir)+string(os.PathSeparator)) {
		return fmt.Errorf("seed %s: escapes the app folder", rel)
	}
	if name, err := filepath.Rel(dir, clean); err == nil {
		if reservedSeedTargets[name] {
			return fmt.Errorf("seed %s: %s belongs to Maison, not to the seed tree", rel, name)
		}
	}
	return nil
}

// chownDefault gives a path the deployment's app identity, best-effort — the
// same treatment and the same reasoning as EnsureFolders: a filesystem that
// cannot chown should log, not block an otherwise healthy start.
func chownDefault(cfg config.Config, path string) {
	uid, err := resolveUID("", cfg.PUID)
	if err != nil {
		return
	}
	gid, err := resolveGID("", cfg.PGID)
	if err != nil {
		return
	}
	chown(path, uid, gid)
}
