package bench

import (
	"context"
	"sync"
	"time"
)

// Status is where a benchmark slot currently stands.
type Status string

const (
	StatusIdle    Status = "idle"
	StatusRunning Status = "running"
	StatusOK      Status = "ok"
	StatusError   Status = "error"
)

// State is what the API reports for one benchmark.
type State[T any] struct {
	Status Status `json:"status"`
	Result *T     `json:"result,omitempty"`
	RanAt  string `json:"ran_at,omitempty"`
	Error  string `json:"error,omitempty"`
}

// cooldown is the minimum gap between two runs of the same benchmark, measured
// from when the previous one started. These endpoints are behind the PCS auth
// gate, so this is about a double-clicked button rather than abuse — but a disk
// benchmark is 256 MiB of IO and running two at once would measure the contention
// between them.
const cooldown = 30 * time.Second

// runTimeout bounds a single run so a wedged network or a stalled device cannot
// leave the slot marked "running" forever.
const runTimeout = 5 * time.Minute

// slot holds one benchmark's in-flight state and last result.
type slot[T any] struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	result    *T
	ranAt     time.Time
	err       string
}

// start launches the benchmark unless one is already running or the cooldown has
// not elapsed, and returns immediately either way.
//
// The run is deliberately asynchronous. A disk benchmark takes tens of seconds and
// a network one can take over a minute; holding an HTTP request open that long
// would be at the mercy of every proxy timeout between the browser and here. The
// caller starts it and polls.
func (s *slot[T]) start(run func(context.Context) (T, error)) State[T] {
	s.mu.Lock()
	if s.running || time.Since(s.startedAt) < cooldown {
		st := s.stateLocked()
		s.mu.Unlock()
		return st
	}
	s.running = true
	s.startedAt = time.Now()
	s.err = ""
	st := s.stateLocked()
	s.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
		defer cancel()
		res, err := run(ctx)

		s.mu.Lock()
		defer s.mu.Unlock()
		s.running = false
		if err != nil {
			s.err = err.Error()
			return
		}
		s.result = &res
		s.ranAt = time.Now()
		s.err = ""
	}()

	return st
}

func (s *slot[T]) state() State[T] {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked()
}

func (s *slot[T]) stateLocked() State[T] {
	st := State[T]{Status: StatusIdle, Result: s.result, Error: s.err}
	if !s.ranAt.IsZero() {
		st.RanAt = s.ranAt.Format(time.RFC3339)
	}
	switch {
	case s.running:
		st.Status = StatusRunning
	case s.err != "":
		st.Status = StatusError
	case s.result != nil:
		st.Status = StatusOK
	}
	return st
}

// Runner owns both benchmark slots.
type Runner struct {
	dir  string
	disk slot[DiskResult]
	net  slot[NetworkResult]
}

// New returns a Runner writing its disk scratch file under dir, and clears any
// scratch file a previous run left behind.
func New(dir string) *Runner {
	CleanScratch(dir)
	return &Runner{dir: dir}
}

// StartDisk begins a disk benchmark and reports the state as of that moment.
func (r *Runner) StartDisk() State[DiskResult] {
	return r.disk.start(func(ctx context.Context) (DiskResult, error) {
		return RunDisk(ctx, r.dir)
	})
}

// StartNetwork begins a link benchmark and reports the state as of that moment.
func (r *Runner) StartNetwork() State[NetworkResult] {
	return r.net.start(RunNetwork)
}

// Results is the combined view both benchmark cards poll.
type Results struct {
	Disk    State[DiskResult]    `json:"disk"`
	Network State[NetworkResult] `json:"network"`
}

// State reports both slots.
func (r *Runner) State() Results {
	return Results{Disk: r.disk.state(), Network: r.net.state()}
}
