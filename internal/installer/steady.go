package installer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yundera/maison/internal/dockerx"
)

// SteadyWindow is how long Steady watches an app: long enough for Docker's restart
// back-off to show a container that dies on start more than once, short enough that
// the update request it runs inside is not left hanging.
const SteadyWindow = 30 * time.Second

// ProjectStater is the one Docker question Steady asks. *dockerx.Client answers it.
type ProjectStater interface {
	ProjectStates(ctx context.Context, project string) (map[string]dockerx.RunState, error)
}

// Steady watches an app that has just been started and says why it is not staying up,
// or returns nil when it is.
//
// It takes two looks `window` apart rather than one, for the reason the crash-loop
// detector does (internal/server/detect.go): a crash-looping container is "running"
// for part of every cycle, so a single look can land on the good half. Docker's
// restart counter moving between the two is the tell.
func Steady(ctx context.Context, dx ProjectStater, project string, window time.Duration) error {
	before, err := dx.ProjectStates(ctx, project)
	if err != nil {
		return fmt.Errorf("could not read its containers: %w", err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(window):
	}
	after, err := dx.ProjectStates(ctx, project)
	if err != nil {
		return fmt.Errorf("could not read its containers: %w", err)
	}
	return steadyVerdict(before, after)
}

// steadyVerdict compares two looks at one app's containers.
//
// What counts against it: a container restarting, one whose restart counter moved, and
// one that was running at the first look and is not at the second. What does not: a
// container already stopped at the first look. That is a one-shot service that
// finished, or a service a failed update created that the rolled-back compose no longer
// declares — neither is the app failing to run.
func steadyVerdict(before, after map[string]dockerx.RunState) error {
	names := make([]string, 0, len(after))
	for name := range after {
		names = append(names, name)
	}
	sort.Strings(names)

	var problems []string
	running := 0
	for _, name := range names {
		now := after[name]
		then, seen := before[name]
		restarted := seen && now.Restarts > then.Restarts
		switch {
		case now.State == "restarting":
			problems = append(problems, fmt.Sprintf("%s keeps restarting (%s)", name, lastExit(now)))
		case now.State == "running":
			running++
			if restarted {
				problems = append(problems, fmt.Sprintf("%s restarted %d times (%s)", name, now.Restarts-then.Restarts, lastExit(now)))
			}
		case restarted || (seen && then.State == "running"):
			problems = append(problems, fmt.Sprintf("%s stopped (%s)", name, lastExit(now)))
		}
	}
	if len(problems) == 0 && running == 0 {
		problems = append(problems, "none of its containers are running")
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
}

// lastExit names why a container last stopped. Running out of memory is named on its
// own: from the logs it looks like the application crashing, and it is not.
func lastExit(st dockerx.RunState) string {
	if st.OOMKilled {
		return "it ran out of memory"
	}
	return fmt.Sprintf("last exit code %d", st.ExitCode)
}
