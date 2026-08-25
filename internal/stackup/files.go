package stackup

import (
	"bytes"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// DefaultFileMode is the permission a declared file gets when it names none.
const DefaultFileMode = 0o644

// EnsureFiles writes the app's declared files.
//
// This is the escape hatch beside the seed tree, and it exists for the two
// things a folder of files cannot say:
//
//   - ensure: always — the file is re-rendered on every up, so a config that
//     embeds ${APP_DOMAIN} follows a domain change instead of freezing at
//     install time. That is the bug this closes: Tuwunel and Outline wrote
//     their domain into a config once, in a hook, and nothing ever revisited it.
//   - a non-default owner or mode.
//
// Everything else belongs in the seed tree, where it needs no declaration at
// all.
//
// A declaration error — an unresolved variable, a relative path, a path outside
// the data root, both or neither of from/content — fails the up, exactly as it
// does for folders.
func EnsureFiles(cfg config.Config, project, dir string, files []xcomposeapp.File, captures map[string]string) error {
	if len(files) == 0 {
		return nil
	}
	envFile := readEnvFile(dir)
	for _, f := range files {
		if err := ensureFile(cfg, project, dir, f, envFile, captures); err != nil {
			return fmt.Errorf("file %q: %w", f.Path, err)
		}
	}
	return nil
}

// ClaimedPaths resolves the targets of a file list, so EnsureSeed can leave them
// alone. A path that does not resolve is skipped here rather than reported:
// EnsureFiles runs moments later and reports it properly.
func ClaimedPaths(cfg config.Config, project, dir string, files []xcomposeapp.File) map[string]bool {
	out := make(map[string]bool, len(files))
	envFile := readEnvFile(dir)
	for _, f := range files {
		if path, err := resolvePath(envinject.Render(f.Path, cfg, project, envFile), cfg); err == nil {
			out[path] = true
		}
	}
	return out
}

func ensureFile(cfg config.Config, project, dir string, f xcomposeapp.File, envFile []byte, captures map[string]string) error {
	if (f.From == "") == (f.Content == "") {
		return fmt.Errorf("needs exactly one of from: or content:")
	}
	// Interpolated then checked the same way a folder path is, so the two report
	// the same three declaration errors in the same words.
	path, err := resolvePath(envinject.Render(f.Path, cfg, project, envFile), cfg)
	if err != nil {
		return err
	}

	ensure := f.Ensure
	if ensure == "" {
		ensure = xcomposeapp.EnsureOnce
	}
	switch ensure {
	case xcomposeapp.EnsureOnce, xcomposeapp.EnsureAlways:
	default:
		return fmt.Errorf("ensure: %q is not %q or %q", f.Ensure, xcomposeapp.EnsureOnce, xcomposeapp.EnsureAlways)
	}
	_, statErr := os.Lstat(path)
	exists := statErr == nil
	if exists && ensure == xcomposeapp.EnsureOnce {
		return nil
	}

	content, mode, err := fileContent(cfg, project, dir, f, envFile, captures)
	if err != nil {
		return err
	}
	uid, err := resolveUID(envinject.Render(f.User, cfg, project, envFile), cfg.PUID)
	if err != nil {
		return err
	}
	gid, err := resolveGID(envinject.Render(f.Group, cfg, project, envFile), cfg.PGID)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), DefaultFolderMode); err != nil {
		return err
	}
	// An always-file that already says what it should say is left alone, so a
	// re-converge doesn't churn its mtime — the same reasoning EnsureVars applies
	// to an unchanged .env.
	if old, err := os.ReadFile(path); err != nil || !bytes.Equal(old, content) {
		if err := os.WriteFile(path, content, mode); err != nil {
			return err
		}
	}
	if err := os.Chmod(path, mode); err != nil {
		log.Printf("file %s: chmod: %v", path, err)
	}
	chown(path, uid, gid)
	return nil
}

// fileContent renders what the file should hold, and the mode to write it with.
func fileContent(cfg config.Config, project, dir string, f xcomposeapp.File, envFile []byte, captures map[string]string) ([]byte, fs.FileMode, error) {
	mode := fs.FileMode(DefaultFileMode)
	if f.Mode != "" {
		m, err := strconv.ParseUint(envinject.Render(f.Mode, cfg, project, envFile), 8, 32)
		if err != nil {
			// The same trap folders documents: YAML reads a bare 0644 as an octal
			// int and the leading zero is gone before Maison sees it.
			return nil, 0, fmt.Errorf("mode %q is not an octal string — quote it, e.g. mode: \"0644\"", f.Mode)
		}
		mode = fs.FileMode(m)
	}

	if f.Content != "" {
		// An inline body is always a template: there is no reason to write a
		// literal ${VAR} into a config file.
		rendered, err := envinject.RenderStrict(f.Content, cfg, project, envFile, captures)
		if err != nil {
			return nil, 0, err
		}
		return []byte(rendered), mode, nil
	}

	src := filepath.Join(dir, SeedDir, filepath.Clean("/"+f.From))
	content, srcMode, err := readSeedFile(src, f.From, cfg, project, envFile, captures)
	if err != nil {
		return nil, 0, err
	}
	if f.Mode == "" {
		// Nothing declared, so carry the source's exec bit like the seed tree does.
		mode = srcMode
	}
	return content, mode, nil
}
