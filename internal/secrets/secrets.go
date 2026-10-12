// Package secrets keeps the box's secret values in one file, apart from the
// settings that use them.
//
// The file is ${STATE_DIR}/secrets.env, mode 0600, one name=value per line — the
// same plain format as every other .env Maison handles (envinject.ParseEnvFile), so
// an operator can read and fix it by hand. Settings never hold a secret themselves:
// they hold a REFERENCE, the whole string `${secret:<name>}`, and the value is
// looked up at the moment it is used (see Resolve).
//
// That split is what lets the settings document be shown, logged and sent to the
// dashboard without leaking anything: the reference is its own mask. The API
// returns it as is, and a client that echoes it back unchanged has said "keep the
// secret I already have" without ever having seen it.
//
// A reference is a WHOLE value or nothing. There is no interpolation inside a
// longer string, so a real password that happens to contain `${` is never read as
// a reference — it is only a reference if it is exactly one.
//
// Nothing here is reachable from the network: values are written by the user typing
// a password into a settings form (the server stores it here and saves the
// reference) or by the deployment through the container-local admin socket.
package secrets

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/yundera/maison/internal/envinject"
)

// FileName is the store's file inside the state directory.
const FileName = "secrets.env"

// nameRe is what a secret may be called: short, lower-case, and a valid .env key.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// refRe matches a value that is exactly one reference.
var refRe = regexp.MustCompile(`^\$\{secret:([a-z][a-z0-9_]{0,63})\}$`)

// ErrNotFound is a reference to a secret the store does not hold.
var ErrNotFound = errors.New("secret not found")

// ValidName reports whether name may be used for a secret.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// Ref is the reference that stands for secret name inside a setting.
func Ref(name string) string { return "${secret:" + name + "}" }

// IsRef reports whether v is a reference, and to which secret.
func IsRef(v string) (string, bool) {
	m := refRe.FindStringSubmatch(v)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Store is the file-backed secret store. Safe for concurrent use.
type Store struct {
	path string
	mu   sync.Mutex
}

// New returns the store kept at path. Nothing is read until it is used: the file is
// small and read on every lookup, so an operator's hand edit takes effect without a
// restart, exactly like .env.app.
func New(path string) *Store { return &Store{path: path} }

// Path is where the store keeps its file.
func (s *Store) Path() string { return s.path }

// Get returns the value of secret name.
func (s *Store) Get(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vars, err := s.read()
	if err != nil {
		return "", err
	}
	for _, v := range vars {
		if v.Key == name {
			return v.Value, nil
		}
	}
	return "", ErrNotFound
}

// Has reports whether secret name is set.
func (s *Store) Has(name string) bool {
	_, err := s.Get(name)
	return err == nil
}

// Resolve turns a setting's value into what is actually used: the secret's value
// when v is a reference, v itself otherwise. A reference to a missing secret is an
// error rather than an empty string, so a caller can tell "no password" from "the
// password went missing".
func (s *Store) Resolve(v string) (string, error) {
	name, ok := IsRef(v)
	if !ok {
		return v, nil
	}
	val, err := s.Get(name)
	if err != nil {
		return "", fmt.Errorf("%s: %w", v, err)
	}
	return val, nil
}

// Set stores value under name, replacing any previous value.
func (s *Store) Set(name, value string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid secret name %q: use a-z, 0-9 and _, starting with a letter", name)
	}
	if value == "" {
		return errors.New("a secret cannot be empty — delete it instead")
	}
	if strings.ContainsAny(value, "\r\n") {
		return errors.New("a secret must be a single line")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	vars, err := s.read()
	if err != nil {
		return err
	}
	found := false
	for i := range vars {
		if vars[i].Key == name {
			vars[i].Value, found = value, true
		}
	}
	if !found {
		vars = append(vars, envinject.Var{Key: name, Value: value})
	}
	return s.write(vars)
}

// Delete removes secret name. Deleting a secret that is not there is not an error:
// the caller wanted it gone, and it is.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	vars, err := s.read()
	if err != nil {
		return err
	}
	kept := vars[:0]
	for _, v := range vars {
		if v.Key != name {
			kept = append(kept, v)
		}
	}
	if len(kept) == len(vars) {
		return nil
	}
	return s.write(kept)
}

// read loads the file; a missing file is an empty store.
func (s *Store) read() ([]envinject.Var, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return envinject.ParseEnvFile(b), nil
}

// write replaces the file atomically, keeping any comments an operator added
// (envinject.PatchEnvFile edits in place). Temp file + rename in the same
// directory, created 0600 from the start so the values are never readable for an
// instant by anyone else.
func (s *Store) write(vars []envinject.Var) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	old, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	out, err := envinject.PatchEnvFile(old, vars)
	if err != nil {
		return err
	}
	return WriteFileAtomic(s.path, out, 0o600)
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
