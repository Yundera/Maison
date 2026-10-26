package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/mem"
	"golang.org/x/sys/unix"

	"github.com/yundera/maison/internal/appenv"
	"github.com/yundera/maison/internal/appstats"
	"github.com/yundera/maison/internal/requirements"
	"github.com/yundera/maison/internal/system"
	"github.com/yundera/maison/internal/usersettings"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// What the box offers apps — its architecture, size and capabilities — for the
// store's requirement checks (internal/requirements).
//
// Detection is cached: the store asks on every listing, and the probes read host
// /proc and ask the Docker daemon. hostTTL is short enough that an operator who
// just installed a GPU driver or forwarded a port sees it on the next store visit,
// and the detector loop refreshes it anyway.
const hostTTL = time.Minute

type hostCache struct {
	mu  sync.Mutex
	at  time.Time
	det requirements.Detected
}

// detectHost runs every probe now.
func (s *Server) detectHost(ctx context.Context) requirements.Detected {
	d := requirements.Detected{Arch: runtime.GOARCH, CPU: appstats.HostCPUs()}
	if vm, err := mem.VirtualMemory(); err == nil {
		d.Memory = int64(vm.Total)
	}
	var st unix.Statfs_t
	if s.cfg.DataRoot != "" && unix.Statfs(s.cfg.DataRoot, &st) == nil {
		d.DiskFree = int64(st.Bavail) * int64(st.Bsize)
	}
	addrs, ok := system.HostAddresses()
	publicIP := requirements.DetectPublicIP(appenv.Load(s.cfg), addrs, ok)
	nvidia := requirements.DetectNvidia(nil, errors.New("no Docker daemon"))
	if s.dx != nil {
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		nvidia = requirements.DetectNvidia(s.dx.Runtimes(qctx))
		cancel()
	}
	d.Probes = requirements.Probes(publicIP, nvidia, requirements.DetectDRI("/sys"))
	return d
}

// host is the box as the requirement checks see it: cached detection under the
// operator's (or the deployment's) overrides, which are read live.
func (s *Server) host(ctx context.Context) requirements.Host {
	s.hostInfo.mu.Lock()
	if s.hostInfo.at.IsZero() || time.Since(s.hostInfo.at) > hostTTL {
		s.hostInfo.det = s.detectHost(ctx)
		s.hostInfo.at = time.Now()
	}
	det := s.hostInfo.det
	s.hostInfo.mu.Unlock()
	return requirements.NewHost(det, s.settings.Get().Capabilities)
}

// refreshHost drops the cached detection, so the next host() probes again.
func (s *Server) refreshHost() {
	s.hostInfo.mu.Lock()
	s.hostInfo.at = time.Time{}
	s.hostInfo.mu.Unlock()
}

// checkRequirements is requirements.Check against this box.
func (s *Server) checkRequirements(ctx context.Context, app *xcomposeapp.App) requirements.Result {
	return requirements.Check(app, s.host(ctx))
}

// capabilitiesView is Settings › Stores › Capabilities: what the box offers, and
// who owns the overrides.
type capabilitiesView struct {
	requirements.Host
	ManagedBy string `json:"managedBy,omitempty"`
}

func (s *Server) handleGetCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") != "" {
		s.refreshHost()
	}
	owner, _ := s.settings.Get().IsManaged("capabilities")
	writeJSON(w, http.StatusOK, capabilitiesView{Host: s.host(r.Context()), ManagedBy: owner})
}

// handlePutCapabilities replaces the overrides (capability → auto|yes|no). 409
// while the deployment manages them.
func (s *Server) handlePutCapabilities(w http.ResponseWriter, r *http.Request) {
	if owner, managed := s.settings.Get().IsManaged("capabilities"); managed {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "capabilities are managed by " + owner})
		return
	}
	var in map[string]string
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	// auto is the absence of an override: store only what overrides.
	for k, v := range in {
		if v == requirements.Auto {
			delete(in, k)
		}
	}
	if len(in) == 0 {
		in = nil
	}
	if err := s.settings.SetCapabilities(in); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, usersettings.ErrManaged) {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	owner, _ := s.settings.Get().IsManaged("capabilities")
	writeJSON(w, http.StatusOK, capabilitiesView{Host: s.host(r.Context()), ManagedBy: owner})
}
