package envinject

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// EnvFileMode is the mode of every .env Maison writes: an app's own .env and the
// deployment's .env.app.
//
// Both hold secrets in plain text — an app's generated database password, its
// signing key, the box's default password — and AppData is reachable by more than
// Maison: an app that mounts the whole data root, a file share, anything running
// as PUID. Owner-only, and owned by Maison's user (root on a PCS), makes the file
// readable and writable only through Maison or as root. A hand-run
// `docker compose up` in the app folder still works; it needs sudo, which the
// Docker socket already amounts to.
const EnvFileMode os.FileMode = 0o600

// WriteEnvFile replaces an env file atomically at EnvFileMode (see WriteFileAtomic).
// The file is re-created, so it also ends up owned by Maison's user, whoever owned
// the previous one.
func WriteEnvFile(path string, b []byte) error {
	return WriteFileAtomic(path, b, EnvFileMode)
}

// TightenEnvFile brings an existing env file to EnvFileMode, owned by Maison's user
// when Maison runs as root. A missing file is not an error.
//
// It is what reaches a .env Maison did not write itself, or wrote before files
// were owner-only: one an operator created, one a backup restored with its old
// mode. Nothing is touched when the file is already right.
func TightenEnvFile(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return nil // a symlink is the operator's arrangement; leave it
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 && (st.Uid != 0 || st.Gid != 0) {
		if err := os.Lchown(path, 0, 0); err != nil {
			return err
		}
	}
	if fi.Mode().Perm() != EnvFileMode {
		return os.Chmod(path, EnvFileMode)
	}
	return nil
}

// WriteFileAtomic writes b to path through a temp file in the same directory and a
// rename, so a reader — another container, the operator's editor — never sees a
// half-written file, and the file never exists with looser permissions than perm.
func WriteFileAtomic(path string, b []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once renamed
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
