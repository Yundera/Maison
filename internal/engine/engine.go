// Package engine runs a backup engine as a throwaway container.
//
// Maison ships no engine binary and installs none on the host. It already holds the
// Docker socket and already shells out to `docker compose` for installs, so running
// kopia (or restic, or whatever comes next) as `docker run --rm kopia/kopia:<tag>`
// costs nothing new: no image bloat for the FOSS install that only ever uses the
// local engine, no host package to keep current, and no multi-arch build stage —
// the registry serves the right platform. Upgrading an engine becomes a tag change
// rather than a Maison release.
//
// Shelling out to the docker CLI rather than driving the API is deliberate and
// matches internal/composecmd: a one-shot container over the API is a five-call
// lifecycle (create, start, attach, wait, remove) with its own leak-on-panic story,
// and a leaked engine container holds an app's files open — which is exactly the
// failure this package's timeout handling exists to prevent.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
)

// Network is the container networking an engine invocation gets. The type is
// closed on purpose — see Argv, which rejects anything else.
type Network string

const (
	// NetworkNone gives the container no networking at all. Correct for a repository
	// on a local filesystem, and what makes the tests hermetic.
	NetworkNone Network = "none"

	// NetworkDefault is Docker's default bridge: outbound only. Correct for an S3 or
	// B2 repository.
	NetworkDefault Network = ""
)

// Mount is one bind mount. HostPath is resolved by the Docker daemon, which lives
// on the host — so it must be the host's spelling of the path, not this container's.
// Use Runner.DataMount rather than building one by hand.
type Mount struct {
	HostPath      string
	ContainerPath string
	ReadOnly      bool
}

// Spec is one engine invocation.
type Spec struct {
	// Image is the pinned engine image, e.g. "kopia/kopia:0.23.1". Never a floating
	// tag: an engine that silently changes under a repository is how a backup format
	// surprise becomes a 3am failure.
	Image string

	// Name is the container's name. It is required, because it is the *only* handle
	// on the container once the client process is gone — see Run's timeout handling.
	Name string

	// Hostname is the identity the engine records against its snapshots. It must be
	// stable across runs and across Maison reinstalls: kopia keys snapshots
	// user@host:path, so a hostname that changes turns every backup into a new source
	// and destroys both incremental backup and per-source retention. A `docker run`
	// container is given a random hostname unless told otherwise, which makes setting
	// this mandatory rather than cosmetic.
	Hostname string

	// User is the uid:gid the engine runs as. Restored files must land owned like the
	// rest of the data; running as root silently fixes ownership upward in a way
	// nothing later undoes.
	User string

	Network Network
	Mounts  []Mount

	// WorkDir is the container's working directory.
	//
	// It is not cosmetic. kopia records its cache location in the repository
	// configuration RELATIVE to that file when the two are siblings, and then resolves
	// it against the process's working directory — so a container left in the image's
	// own WORKDIR looks for the cache at /app/cache and fails with "unable to create
	// cache directory: permission denied" the moment it runs as anything but root.
	// Pointing the container at the engine's own directory makes the two agree.
	WorkDir string

	// Args are the engine's own arguments, appended after the image.
	Args []string

	// Secrets are environment variables passed **by name** on the command line and by
	// value through the child's environment, so the value never appears in argv and
	// therefore never in another user's `ps`. It is still visible to anyone who can
	// `docker inspect` the container — but anyone who can do that already has the
	// socket, which is root-equivalent, so this closes the gap that can be closed and
	// does not pretend to close the other one.
	Secrets map[string]string

	// Timeout bounds the run. Zero means no limit, which is right for a first full
	// upload and wrong for anything taken while an app is stopped.
	Timeout time.Duration
}

// Runner invokes engine containers.
type Runner struct {
	cfg config.Config

	// Docker overrides the docker binary; empty means "docker". Tests set it.
	Docker string
}

// New builds a Runner.
func New(cfg config.Config) *Runner { return &Runner{cfg: cfg} }

func (r *Runner) docker() string {
	if r.Docker != "" {
		return r.Docker
	}
	return "docker"
}

// DataMount is the bind mount that gives an engine the data root.
//
// The source is the *host's* path and the target is this container's, because the
// daemon resolves bind sources on the host while every path Maison hands the engine
// is its own `/DATA/...` spelling. envinject.HostPath is the same translation
// stackup already applies to APP_DIR.
//
// On a real PCS the two are pinned identical, so HostPath is a no-op and a mistake
// here is invisible; it only shows up in dev, where they differ. That asymmetry is
// why this lives in one function with a test rather than being inlined at call
// sites.
func (r *Runner) DataMount(readOnly bool) Mount {
	return Mount{
		HostPath:      envinject.HostPath(r.cfg.DataRoot, r.cfg),
		ContainerPath: r.cfg.DataRoot,
		ReadOnly:      readOnly,
	}
}

// Argv builds the full docker command line for a spec.
//
// It is separate from Run, and pure, because the interesting failures here are
// argv-shaped: a host path that was not translated, a missing hostname pin, a
// container placed on a network it must never join. Those are worth asserting
// exactly, without a daemon.
func Argv(s Spec) ([]string, error) {
	if s.Image == "" {
		return nil, errors.New("engine: no image")
	}
	if s.Name == "" {
		return nil, errors.New("engine: no container name — it is the only handle on a run whose client has died")
	}
	if s.Hostname == "" {
		return nil, errors.New("engine: no hostname — an unpinned hostname makes every backup a new source")
	}
	// Closed set. An engine container must never join the deployment's app network:
	// on a PCS the auth-registrar derives an app's OIDC client_id from a reverse-DNS
	// lookup of the *caller's container name*, so a named container sitting on that
	// network is a claimable identity. An engine needs to reach a repository, never a
	// peer.
	if s.Network != NetworkNone && s.Network != NetworkDefault {
		return nil, fmt.Errorf("engine: unsupported network %q — only %q and the default bridge are allowed", s.Network, NetworkNone)
	}

	argv := []string{"run", "--rm", "--name", s.Name, "--hostname", s.Hostname}
	if s.Network != NetworkDefault {
		argv = append(argv, "--network", string(s.Network))
	}
	if s.User != "" {
		argv = append(argv, "--user", s.User)
	}
	if s.WorkDir != "" {
		argv = append(argv, "-w", s.WorkDir)
	}
	for _, m := range s.Mounts {
		if m.HostPath == "" || m.ContainerPath == "" {
			return nil, fmt.Errorf("engine: incomplete mount %+v", m)
		}
		spec := m.HostPath + ":" + m.ContainerPath
		if m.ReadOnly {
			spec += ":ro"
		}
		argv = append(argv, "-v", spec)
	}
	// Sorted so argv is deterministic and therefore assertable; names only, values go
	// through the environment.
	for _, k := range sortedKeys(s.Secrets) {
		argv = append(argv, "-e", k)
	}
	argv = append(argv, s.Image)
	return append(argv, s.Args...), nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Run executes the spec and returns whatever the engine wrote to stdout — which is
// where engines put their machine-readable result.
//
// Progress goes to stderr and is delivered line by line to onLine as it arrives.
// Both streams are read concurrently: buffering with CombinedOutput would interleave
// the JSON result with progress noise *and* withhold every progress line until the
// command exited, which for a multi-hour upload is the same as having no progress.
func (r *Runner) Run(ctx context.Context, s Spec, onLine func(string)) ([]byte, error) {
	argv, err := Argv(s)
	if err != nil {
		return nil, err
	}

	runCtx := ctx
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, r.docker(), argv...)
	cmd.Env = os.Environ()
	for _, k := range sortedKeys(s.Secrets) {
		cmd.Env = append(cmd.Env, k+"="+s.Secrets[k])
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("engine: start docker: %w", err)
	}

	var tail lastLines
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		sc.Split(ScanProgress)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			tail.add(line)
			if onLine != nil {
				onLine(line)
			}
		}
	}()

	var out bytes.Buffer
	_, copyErr := io.Copy(&out, stdout)
	<-scanned
	waitErr := cmd.Wait()

	// The trap this whole package is shaped around: exec.CommandContext kills the
	// docker *client*, not the container. --rm fires on container exit, so a client
	// killed mid-run leaves the engine running — still holding the app's files open,
	// while Maison believes the backup failed and restarts the app. Removing it by
	// name is the only way to make "the backup failed, the app is up" true.
	if runCtx.Err() != nil {
		r.forceRemove(s.Name)
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return out.Bytes(), fmt.Errorf("engine: timed out after %s: %s", s.Timeout, tail.String())
		}
		return out.Bytes(), fmt.Errorf("engine: cancelled: %w", runCtx.Err())
	}
	if waitErr != nil {
		return out.Bytes(), fmt.Errorf("engine: %s: %w: %s", s.Image, waitErr, tail.String())
	}
	if copyErr != nil {
		return out.Bytes(), fmt.Errorf("engine: reading output: %w", copyErr)
	}
	return out.Bytes(), nil
}

// forceRemove kills and removes a container by name. Best effort: it runs on a
// fresh context because the one that brought us here is already cancelled, and its
// failure is logged rather than returned — it can only ever be secondary to the
// failure that triggered it.
func (r *Runner) forceRemove(name string) {
	if name == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, r.docker(), "rm", "-f", name).CombinedOutput(); err != nil {
		log.Printf("engine: removing abandoned container %s: %v: %s", name, err, bytes.TrimSpace(out))
	}
}

// ScanProgress is a bufio.SplitFunc that breaks on carriage returns as well as
// newlines.
//
// Engines redraw a progress line in place with \r and emit no \n until they finish.
// bufio.ScanLines would therefore deliver nothing at all until the command exited,
// which looks exactly like a hung backup for the entire duration of a working one.
func ScanProgress(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// lastLines keeps the tail of stderr for an error message. Engines are chatty and a
// failure's cause is at the end, so quoting the whole stream would bury it.
type lastLines struct {
	lines []string
}

const tailLines = 8

func (l *lastLines) add(s string) {
	l.lines = append(l.lines, s)
	if len(l.lines) > tailLines {
		l.lines = l.lines[len(l.lines)-tailLines:]
	}
}

func (l *lastLines) String() string { return strings.Join(l.lines, "; ") }
