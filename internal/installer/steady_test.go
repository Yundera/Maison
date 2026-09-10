package installer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yundera/maison/internal/dockerx"
)

func runningWith(restarts int) dockerx.RunState {
	return dockerx.RunState{State: "running", Restarts: restarts}
}

// The FileBrowser rollback on watch.nsl.sh (2026-09-10): the restored folder could not
// open its own database, so the container died on start, forever, and Docker showed
// it "restarting" between attempts.
func TestSteadyCatchesAContainerThatDiesOnStart(t *testing.T) {
	err := steadyVerdict(
		map[string]dockerx.RunState{"filebrowser": {State: "restarting", Restarts: 1, ExitCode: 1}},
		map[string]dockerx.RunState{"filebrowser": {State: "restarting", Restarts: 9, ExitCode: 1}},
	)
	if err == nil || !strings.Contains(err.Error(), "filebrowser keeps restarting (last exit code 1)") {
		t.Fatalf("err = %v, want the crash loop named with its exit code", err)
	}
}

// A crash loop is running for part of every cycle. A look that lands on that half
// must not read as healthy.
func TestSteadyCatchesACrashLoopSeenWhileRunning(t *testing.T) {
	err := steadyVerdict(
		map[string]dockerx.RunState{"app": runningWith(2)},
		map[string]dockerx.RunState{"app": runningWith(7)},
	)
	if err == nil || !strings.Contains(err.Error(), "app restarted 5 times") {
		t.Fatalf("err = %v, want the restarts between the two looks counted", err)
	}
}

func TestSteadyCatchesAContainerThatStoppedBetweenTheLooks(t *testing.T) {
	err := steadyVerdict(
		map[string]dockerx.RunState{"app": runningWith(0), "db": runningWith(0)},
		map[string]dockerx.RunState{"app": runningWith(0), "db": {State: "exited", ExitCode: 137, OOMKilled: true}},
	)
	if err == nil || !strings.Contains(err.Error(), "db stopped (it ran out of memory)") {
		t.Fatalf("err = %v, want the stopped service named, OOM included", err)
	}
}

// Already stopped at the first look is not the app failing: a one-shot service that
// finished, or a service the failed update created that the rolled-back compose no
// longer declares.
func TestSteadyIgnoresWhatWasAlreadyStopped(t *testing.T) {
	same := map[string]dockerx.RunState{
		"app":                 runningWith(0),
		"migrate":             {State: "exited"},
		"filebrowser-backend": {State: "exited", ExitCode: 143},
	}
	if err := steadyVerdict(same, same); err != nil {
		t.Fatalf("err = %v, want a steady app", err)
	}
}

func TestSteadyNeedsSomethingRunning(t *testing.T) {
	err := steadyVerdict(map[string]dockerx.RunState{}, map[string]dockerx.RunState{})
	if err == nil || !strings.Contains(err.Error(), "none of its containers are running") {
		t.Fatalf("err = %v, want an app with nothing running refused", err)
	}
}

type fakeStater struct {
	looks []map[string]dockerx.RunState
}

func (f *fakeStater) ProjectStates(context.Context, string) (map[string]dockerx.RunState, error) {
	look := f.looks[0]
	if len(f.looks) > 1 {
		f.looks = f.looks[1:]
	}
	return look, nil
}

// The first look here is healthy; only the second shows the restart. One look would
// have passed it.
func TestSteadyComparesTwoLooks(t *testing.T) {
	dx := &fakeStater{looks: []map[string]dockerx.RunState{
		{"app": runningWith(0)},
		{"app": runningWith(3)},
	}}
	if err := Steady(context.Background(), dx, "app", time.Millisecond); err == nil {
		t.Fatal("a restart between the two looks was not caught")
	}
}
