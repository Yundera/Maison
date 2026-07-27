package stackup

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	osuser "os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// DefaultFolderMode is the permission a declared folder gets when it names none.
const DefaultFolderMode = 0o755

// EnsureFolders creates each declared folder and applies its ownership and mode,
// so an app that drops privileges to PUID:PGID can write to its bind mounts on
// first boot. Paths (and the user/group/mode fields) are interpolated with the
// app's own variables — ${DATA_ROOT}, ${AppID}, ${PUID}, anything in its .env —
// then mapped into Maison's data mount, which is where the container can
// actually create them.
//
// A folder that cannot be resolved (unresolved variable, relative path, outside
// the data root) is a declaration error and fails the up. A folder that resolves
// but cannot be created is also fatal: the app would start with an unwritable
// mount. Ownership and mode are applied best-effort — Maison may not be able to
// chown on every filesystem, and that shouldn't stop an otherwise healthy start.
func EnsureFolders(cfg config.Config, appID string, folders []xcomposeapp.Folder, envFile []byte) error {
	for _, f := range folders {
		rendered := xcomposeapp.Folder{
			Path:      envinject.Render(f.Path, cfg, appID, envFile),
			User:      envinject.Render(f.User, cfg, appID, envFile),
			Group:     envinject.Render(f.Group, cfg, appID, envFile),
			Mode:      envinject.Render(f.Mode, cfg, appID, envFile),
			Recursive: f.Recursive,
		}
		if err := ensure(cfg, rendered); err != nil {
			return fmt.Errorf("folder %q: %w", f.Path, err)
		}
	}
	return nil
}

// ensure creates one fully-interpolated folder and applies its ownership/mode.
func ensure(cfg config.Config, f xcomposeapp.Folder) error {
	dir, err := resolvePath(f.Path, cfg)
	if err != nil {
		return err
	}

	mode := fs.FileMode(DefaultFolderMode)
	if f.Mode != "" {
		// Quoting matters: YAML types a bare 0755 as an octal *int*, and the
		// extension block round-trips through map[string]any on the way here, so the
		// leading zero would already be gone by now.
		m, err := strconv.ParseUint(f.Mode, 8, 32)
		if err != nil {
			return fmt.Errorf("mode %q is not an octal string — quote it, e.g. mode: \"0755\"", f.Mode)
		}
		mode = fs.FileMode(m)
	}
	uid, err := resolveUID(f.User, cfg.PUID)
	if err != nil {
		return err
	}
	gid, err := resolveGID(f.Group, cfg.PGID)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	// MkdirAll applies the umask and does nothing at all when the directory
	// already exists, so pin the mode explicitly afterwards.
	if err := os.Chmod(dir, mode); err != nil {
		log.Printf("folder %s: chmod: %v", dir, err)
	}
	chown(dir, uid, gid)

	if f.Recursive {
		// Reclaim a tree that already exists (a restored backup, a folder an app
		// wrote as root). Ownership only: rewriting the mode of every file below
		// would flip executable bits the app set for itself.
		err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			chown(p, uid, gid)
			return nil
		})
		if err != nil {
			log.Printf("folder %s: recursive chown: %v", dir, err)
		}
	}
	return nil
}

// resolvePath maps a declared folder path into this container's data mount and
// rejects anything Maison has no business creating.
func resolvePath(p string, cfg config.Config) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("no path")
	}
	if strings.Contains(p, "$") {
		return "", fmt.Errorf("unresolved variable in %q", p)
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path must be absolute")
	}
	dir := filepath.Clean(envinject.ContainerPath(p, cfg))
	// Everything an app owns lives under the data root (see docs/app-model.md), and
	// it is the only host directory this container has mounted — a path outside it
	// would silently create a directory inside the container instead.
	if dir != cfg.DataRoot && !strings.HasPrefix(dir, cfg.DataRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("outside the data root (%s)", cfg.DataRoot)
	}
	return dir, nil
}

// chown is best-effort: an unsupported filesystem shouldn't fail a start.
func chown(path string, uid, gid int) {
	if uid < 0 || gid < 0 {
		return
	}
	if err := os.Chown(path, uid, gid); err != nil {
		log.Printf("folder %s: chown %d:%d: %v", path, uid, gid, err)
	}
}

// resolveUID resolves a uid or user name, falling back to the deployment's PUID.
// It returns -1 when neither resolves, which chown treats as "leave ownership be".
func resolveUID(name, fallback string) (int, error) {
	if name == "" {
		name = fallback
	}
	if name == "" {
		return -1, nil
	}
	if id, err := strconv.Atoi(name); err == nil {
		return id, nil
	}
	u, err := osuser.Lookup(name)
	if err != nil {
		return 0, fmt.Errorf("unknown user %q", name)
	}
	return strconv.Atoi(u.Uid)
}

// resolveGID resolves a gid or group name, falling back to the deployment's PGID.
func resolveGID(name, fallback string) (int, error) {
	if name == "" {
		name = fallback
	}
	if name == "" {
		return -1, nil
	}
	if id, err := strconv.Atoi(name); err == nil {
		return id, nil
	}
	g, err := osuser.LookupGroup(name)
	if err != nil {
		return 0, fmt.Errorf("unknown group %q", name)
	}
	return strconv.Atoi(g.Gid)
}
