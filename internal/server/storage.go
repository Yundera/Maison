package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yundera/maison/internal/cleanup"
	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/diskuse"
	"github.com/yundera/maison/internal/dockerx"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/system"
)

// Settings › Resources › Storage: where the disk went, and giving some of it back.
//
// Three figures make up the page, each from a different source because no single
// one can answer the question alone:
//
//   - the filesystem the data root lives on (statfs — instant, exact, but blind to
//     what filled it);
//   - the data root itself, walked like `du` (internal/diskuse — slow, so run in
//     the background and cached);
//   - Docker's own accounting (`docker system df`), for the images, containers,
//     volumes and build cache that live outside the data root.
//
// "System" is the filesystem's used bytes less the data root's. On a PCS that is
// honest because /DATA is a directory on the root disk (internal/system); on a box
// where the data root is its own disk the split means nothing, and the page is told
// so rather than shown a "system" figure that is really filesystem overhead.
//
// The removal side is internal/cleanup; see its package doc for why it is not
// `docker system prune --all`.

// dfTTL is how long Docker's disk accounting is reused. The daemon sizes every
// volume to answer, so the page must not ask on every render.
const dfTTL = 30 * time.Second

// dfTimeout bounds one `docker system df`. On a box with many volumes or a big
// build cache it has been seen to take over a minute, which is why no request
// ever waits for it: the page is served what is cached, and polls while a refresh
// runs.
const dfTimeout = 3 * time.Minute

// cleanupTimeout bounds one cleanup run. Removing a few dozen images is seconds;
// a build cache on a slow disk can be minutes.
const cleanupTimeout = 15 * time.Minute

type storageState struct {
	scanner *diskuse.Scanner

	dfMu   sync.Mutex
	dfAt   time.Time
	df     *storageDocker
	dfErr  string
	dfBusy bool

	runMu   sync.Mutex // held for the whole of a cleanup run
	stateMu sync.Mutex // guards run
	run     CleanupRun
}

// storageDocker is Docker's share of the disk, summarised.
type storageDocker struct {
	ImagesBytes      int64 `json:"images_bytes"`
	ImagesCount      int   `json:"images_count"`
	ContainersBytes  int64 `json:"containers_bytes"`
	ContainersCount  int   `json:"containers_count"`
	VolumesBytes     int64 `json:"volumes_bytes"`
	VolumesCount     int   `json:"volumes_count"`
	BuildCacheBytes  int64 `json:"build_cache_bytes"`
	TotalBytes       int64 `json:"total_bytes"`
	ReclaimableCache int64 `json:"reclaimable_cache_bytes"`
}

// storageResponse is GET /api/system/storage.
type storageResponse struct {
	DataRoot string `json:"data_root"`
	// Size/Used/Avail describe the filesystem under the data root.
	SizeBytes  uint64 `json:"size_bytes"`
	UsedBytes  uint64 `json:"used_bytes"`
	AvailBytes uint64 `json:"avail_bytes"`
	// DataOwnFilesystem is true when the data root is a disk of its own, so
	// "used − data" is not the system's share of anything.
	DataOwnFilesystem bool `json:"data_own_filesystem"`

	Data      diskuse.Result `json:"data"`
	Docker    *storageDocker `json:"docker"`
	DockerErr string         `json:"docker_error,omitempty"`
	// DockerPending is true while Docker's figures are being (re)measured; the
	// client polls until it clears.
	DockerPending bool `json:"docker_pending"`
}

// CleanupRun is the last cleanup, or the one running.
type CleanupRun struct {
	Status     string    `json:"status"` // idle | running | done | error
	Kind       string    `json:"kind,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`

	Images     int `json:"images"`
	Networks   int `json:"networks"`
	Containers int `json:"containers"`
	// BuildCacheBytes is what the daemon says the build-cache prune gave back.
	BuildCacheBytes uint64 `json:"build_cache_bytes"`
	// FreedBytes is the filesystem's free space after less before — measured, not
	// estimated, so it counts shared layers correctly. Zero when the data root is a
	// disk of its own, where Docker's space is not what statfs sees.
	FreedBytes int64    `json:"freed_bytes"`
	Errors     []string `json:"errors,omitempty"`
}

// cleanupResponse is GET /api/system/cleanup.
type cleanupResponse struct {
	Plan cleanup.Plan `json:"plan"`
	// ReclaimableCacheBytes is the build cache no running build holds.
	ReclaimableCacheBytes int64      `json:"reclaimable_cache_bytes"`
	Run                   CleanupRun `json:"run"`
	// Blocked names why a cleanup cannot start right now, empty when it can.
	Blocked string `json:"blocked,omitempty"`
}

func newStorageState(dataRoot, stateDir string) *storageState {
	return &storageState{
		scanner: diskuse.New(dataRoot, "AppData", filepath.Join(stateDir, "diskuse.json")),
		run:     CleanupRun{Status: "idle"},
	}
}

// ── usage ────────────────────────────────────────────────────────────────────

func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	resp := storageResponse{DataRoot: s.cfg.DataRoot, Data: s.storage.scanner.State()}
	var st unix.Statfs_t
	if err := unix.Statfs(s.cfg.DataRoot, &st); err == nil {
		bs := uint64(st.Bsize)
		resp.SizeBytes = st.Blocks * bs
		resp.UsedBytes = (st.Blocks - st.Bfree) * bs
		resp.AvailBytes = st.Bavail * bs
	}
	resp.DataOwnFilesystem = s.dataOwnFilesystem()
	resp.Docker, resp.DockerErr, resp.DockerPending = s.dockerUsage()
	writeJSON(w, http.StatusOK, resp)
}

// handleStorageScan starts a walk of the data root. Asynchronous, like the
// benchmarks: the client polls GET /api/system/storage.
func (s *Server) handleStorageScan(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusAccepted, s.storage.scanner.Start())
}

// dataOwnFilesystem reports whether the host mounts the data root as a filesystem
// of its own. Unknown — the host's mount table unreadable — counts as no, which is
// what a PCS is.
func (s *Server) dataOwnFilesystem() bool {
	for _, m := range system.ReadFilesystems(s.cfg.DataRoot).Mounts {
		if m.LocalPath == s.cfg.DataRoot {
			return m.Mountpoint != "" && m.Mountpoint != "/" && m.Mountpoint == s.cfg.DataHostPath
		}
	}
	return false
}

// dockerUsage returns Docker's figures as last measured, starting a fresh
// measurement in the background when they are stale. It never waits for one.
func (s *Server) dockerUsage() (*storageDocker, string, bool) {
	if s.dx == nil {
		return nil, "docker unavailable", false
	}
	st := s.storage
	st.dfMu.Lock()
	defer st.dfMu.Unlock()
	if time.Since(st.dfAt) >= dfTTL && !st.dfBusy {
		st.dfBusy = true
		go s.measureDocker()
	}
	return st.df, st.dfErr, st.dfBusy
}

func (s *Server) measureDocker() {
	ctx, cancel := context.WithTimeout(context.Background(), dfTimeout)
	defer cancel()
	du, err := s.dx.DiskUsage(ctx)

	st := s.storage
	st.dfMu.Lock()
	defer st.dfMu.Unlock()
	st.dfBusy, st.dfAt = false, time.Now()
	if err != nil {
		// The previous figures stay: stale numbers beat none, and the error says why
		// they did not move.
		st.dfErr = err.Error()
		return
	}
	out := &storageDocker{
		ImagesBytes:      du.LayersSize,
		ImagesCount:      len(du.Images),
		ContainersCount:  len(du.Containers),
		VolumesBytes:     du.VolumesBytes,
		VolumesCount:     du.VolumesCount,
		BuildCacheBytes:  du.BuildCacheBytes,
		ReclaimableCache: du.BuildCacheReclaimable,
	}
	for _, c := range du.Containers {
		out.ContainersBytes += c.SizeRw
	}
	out.TotalBytes = out.ImagesBytes + out.ContainersBytes + out.VolumesBytes + out.BuildCacheBytes
	st.df, st.dfErr = out, ""
}

func (st *storageState) invalidateDocker() {
	st.dfMu.Lock()
	st.dfAt = time.Time{}
	st.dfMu.Unlock()
}

// ── cleanup ──────────────────────────────────────────────────────────────────

func (s *Server) handleGetCleanup(w http.ResponseWriter, r *http.Request) {
	if s.dx == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "docker unavailable"})
		return
	}
	plan, err := s.cleanupPlan(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	resp := cleanupResponse{Plan: plan, Run: s.cleanupRun(), Blocked: s.cleanupBlocked(r.Context())}
	if df, _, _ := s.dockerUsage(); df != nil {
		resp.ReclaimableCacheBytes = df.ReclaimableCache
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRunCleanup starts the routine clean: unused images, the build cache,
// compose's leftover networks. It answers at once; the client polls GET.
func (s *Server) handleRunCleanup(w http.ResponseWriter, r *http.Request) {
	s.startCleanup(w, r, "clean", func(ctx context.Context, run *CleanupRun) {
		s.cleanRoutine(ctx, run)
	})
}

// handleRemoveOrphans removes the orphans the operator ticked, by key.
func (s *Server) handleRemoveOrphans(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Keys []string `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Keys) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no orphans selected"})
		return
	}
	s.startCleanup(w, r, "orphans", func(ctx context.Context, run *CleanupRun) {
		s.removeOrphans(ctx, run, body.Keys)
	})
}

func (s *Server) startCleanup(w http.ResponseWriter, r *http.Request, kind string, fn func(context.Context, *CleanupRun)) {
	if s.dx == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "docker unavailable"})
		return
	}
	if why := s.cleanupBlocked(r.Context()); why != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": why})
		return
	}
	st := s.storage
	if !st.runMu.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a cleanup is already running"})
		return
	}
	run := CleanupRun{Status: "running", Kind: kind, StartedAt: time.Now()}
	st.setRun(run)
	go func() {
		defer st.runMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		before := s.availBytes()
		fn(ctx, &run)
		if !s.dataOwnFilesystem() {
			if d := int64(s.availBytes()) - int64(before); d > 0 {
				run.FreedBytes = d
			}
		}
		run.Status, run.FinishedAt = "done", time.Now()
		if len(run.Errors) > 0 && run.Images+run.Networks+run.Containers == 0 && run.BuildCacheBytes == 0 {
			run.Status = "error"
		}
		st.setRun(run)
		st.invalidateDocker()
		if s.apps != nil {
			s.apps.InvalidateContainers()
			s.broadcastApps()
		}
		log.Printf("cleanup %s: %d images, %d networks, %d containers, %d bytes freed, %d errors",
			kind, run.Images, run.Networks, run.Containers, run.FreedBytes, len(run.Errors))
	}()
	writeJSON(w, http.StatusAccepted, run)
}

func (st *storageState) setRun(r CleanupRun) {
	st.stateMu.Lock()
	st.run = r
	st.stateMu.Unlock()
}

func (s *Server) cleanupRun() CleanupRun {
	s.storage.stateMu.Lock()
	defer s.storage.stateMu.Unlock()
	return s.storage.run
}

func (s *Server) availBytes() uint64 {
	var st unix.Statfs_t
	if unix.Statfs(s.cfg.DataRoot, &st) != nil {
		return 0
	}
	return st.Bavail * uint64(st.Bsize)
}

// cleanupBlocked names what makes a cleanup unsafe right now, or "".
//
// An update pulls the new version's images BEFORE it writes the new compose, so for
// a moment those images are named by nothing and used by nothing — exactly what the
// routine clean removes. Rather than a subtler rule, nothing is cleaned while any app
// operation is in flight. The daemon refusing to remove an image a container uses
// covers the rest.
func (s *Server) cleanupBlocked(ctx context.Context) string {
	if s.apps != nil {
		for _, a := range s.listApps(ctx) {
			if a.Installing || a.Busy || a.Uninstalling || a.BackingUp {
				name := a.Name
				if name == "" {
					name = a.ID
				}
				return name + " is being installed, updated or backed up — try again once it has finished"
			}
		}
	}
	if s.backupSched != nil && s.backupSched.State().Running {
		return "a backup is running — try again once it has finished"
	}
	s.updates.mu.Lock()
	updating := s.updates.run.Running
	s.updates.mu.Unlock()
	if updating {
		return "apps are being updated — try again once that has finished"
	}
	return ""
}

// cleanupPlan builds the plan from the box as it is now.
func (s *Server) cleanupPlan(ctx context.Context) (cleanup.Plan, error) {
	images, err := s.dx.Images(ctx)
	if err != nil {
		return cleanup.Plan{}, err
	}
	containers, err := s.dx.Containers(ctx)
	if err != nil {
		return cleanup.Plan{}, err
	}
	networks, err := s.dx.Networks(ctx)
	if err != nil {
		return cleanup.Plan{}, err
	}
	protected := map[string]bool{}
	if s.apps != nil {
		list, _ := s.apps.List(ctx)
		for _, a := range list {
			if a.Protected {
				protected[a.ID] = true
			}
		}
	}
	keep := map[string]bool{"pcs": true}
	if s.cfg.AppEnv != nil {
		if n := s.cfg.AppEnv()["APP_NET"]; n != "" {
			keep[n] = true
		}
	}
	return cleanup.Build(cleanup.Input{
		Images:       images,
		Containers:   containers,
		Networks:     networks,
		Referenced:   s.referencedImages(containers),
		Protected:    protected,
		KeepNetworks: keep,
		FolderGone:   s.folderGone,
		Self:         selfContainerID(),
	}), nil
}

// folderGone reports a compose working directory as gone only when it lies under
// the data root — the one part of the host this process can see — and the data root
// itself is there. A missing data mount must read as "cannot tell", or every app on
// the box would be offered as an orphan.
func (s *Server) folderGone(hostDir string) bool {
	root := filepath.Clean(s.cfg.DataRoot)
	local := filepath.Clean(envinject.ContainerPath(hostDir, s.cfg))
	if local == root || !strings.HasPrefix(local, root+string(filepath.Separator)) {
		return false
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) == 0 {
		return false
	}
	_, err := os.Stat(local)
	return errors.Is(err, fs.ErrNotExist)
}

// referencedImages gathers the image of every service of every installed app: the
// managed ones under the apps folder, and every compose project Docker knows of
// whose files are readable from here. Variables are resolved from each project's
// own .env, as compose would.
func (s *Server) referencedImages(containers []dockerx.ContainerInfo) []string {
	type project struct {
		dir   string
		files []string
	}
	projects := map[string]*project{}
	add := func(dir string, files ...string) {
		p := projects[dir]
		if p == nil {
			p = &project{dir: dir}
			projects[dir] = p
		}
		p.files = append(p.files, files...)
	}
	if entries, err := os.ReadDir(s.cfg.AppsDir()); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(s.cfg.AppsDir(), e.Name())
			add(dir, filepath.Join(dir, "docker-compose.yml"), filepath.Join(dir, "docker-compose.override.yml"))
		}
	}
	for _, c := range containers {
		if c.WorkingDir == "" {
			continue
		}
		dir := envinject.ContainerPath(c.WorkingDir, s.cfg)
		var files []string
		for _, f := range strings.Split(c.ConfigFiles, ",") {
			if f = strings.TrimSpace(f); f != "" {
				files = append(files, envinject.ContainerPath(f, s.cfg))
			}
		}
		add(dir, files...)
	}

	var refs []string
	for _, p := range projects {
		env := map[string]string{}
		if raw, err := os.ReadFile(filepath.Join(p.dir, ".env")); err == nil {
			for _, v := range envinject.ParseEnvFile(raw) {
				env[v.Key] = v.Value
			}
		}
		seen := map[string]bool{}
		for _, f := range p.files {
			if seen[f] {
				continue
			}
			seen[f] = true
			cf, err := composefile.Load(f)
			if err != nil {
				continue
			}
			for _, svc := range cf.Services {
				if svc.Image != "" {
					refs = append(refs, cleanup.ExpandRef(svc.Image, env))
				}
			}
		}
	}
	return refs
}

func (s *Server) cleanRoutine(ctx context.Context, run *CleanupRun) {
	plan, err := s.cleanupPlan(ctx)
	if err != nil {
		run.Errors = append(run.Errors, err.Error())
		return
	}
	for _, im := range plan.Images {
		refs := im.Tags
		if len(refs) == 0 {
			refs = []string{im.ID}
		}
		ok := true
		for _, ref := range refs {
			if err := s.dx.RemoveImage(ctx, ref); err != nil {
				run.Errors = append(run.Errors, ref+": "+err.Error())
				ok = false
			}
		}
		if ok {
			run.Images++
		}
	}
	for _, n := range plan.Networks {
		if err := s.dx.RemoveNetwork(ctx, n.ID); err != nil {
			run.Errors = append(run.Errors, "network "+n.Name+": "+err.Error())
			continue
		}
		run.Networks++
	}
	freed, err := s.dx.PruneBuildCache(ctx)
	if err != nil {
		run.Errors = append(run.Errors, "build cache: "+err.Error())
	}
	run.BuildCacheBytes = freed
}

// removeOrphans removes the orphans named by keys — each only if it is still an
// orphan in a plan built now, not in the one the page was showing.
func (s *Server) removeOrphans(ctx context.Context, run *CleanupRun, keys []string) {
	plan, err := s.cleanupPlan(ctx)
	if err != nil {
		run.Errors = append(run.Errors, err.Error())
		return
	}
	byKey := map[string]cleanup.Orphan{}
	for _, o := range plan.Orphans {
		byKey[o.Key] = o
	}
	for _, k := range keys {
		o, ok := byKey[k]
		if !ok {
			run.Errors = append(run.Errors, k+": no longer an orphan, left alone")
			continue
		}
		for _, c := range o.Containers {
			if err := s.dx.RemoveContainer(ctx, c.ID); err != nil {
				run.Errors = append(run.Errors, c.Name+": "+err.Error())
				continue
			}
			run.Containers++
		}
	}
}

// containerIDPattern finds a container id in a mountinfo line: Docker bind-mounts
// /etc/hostname, /etc/hosts and /etc/resolv.conf out of /var/lib/docker/containers/<id>/.
var containerIDPattern = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

var (
	selfOnce sync.Once
	selfID   string
)

// selfContainerID is this process's own container id, or "" outside a container.
// The hostname cannot be used: Maison's compose sets `hostname: maison`.
func selfContainerID() string {
	selfOnce.Do(func() {
		f, err := os.Open("/proc/self/mountinfo")
		if err != nil {
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			if m := containerIDPattern.FindStringSubmatch(sc.Text()); m != nil {
				selfID = m[1]
				return
			}
		}
	})
	return selfID
}
