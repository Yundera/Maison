package stackup

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// MigrateDir holds the store app's migrations/ folder inside the app folder,
// refreshed by every update from the same store sync as the compose — so a compose
// and the scripts it names are always from the same commit.
const MigrateDir = ".migrate"

// MigratePlan is what an update converts the app's data with: the version the data
// is at, the version the new compose expects, and the steps between, in order.
type MigratePlan struct {
	From, To int
	Steps    []xcomposeapp.Migration
}

// MigrateProgress is told about each step as it starts — i counts from 1.
type MigrateProgress func(i, n int, m xcomposeapp.Migration)

// RunMigrations runs the plan's steps once each, in order, writing the marker after
// every step that succeeds. See docs/migrations.md.
//
// Exit 0 means applied; anything else fails the step, and with it the update —
// whose backup, restored, puts the data and the marker back together. There is no
// "skipped": a step with nothing to do checks the data and exits 0.
//
// captures is the converge's: what secrets and variables resolved so far, so a step
// can reference a value the new version introduced.
func RunMigrations(ctx context.Context, cfg config.Config, project, dir string, plan MigratePlan, captures map[string]string, progress MigrateProgress) error {
	if len(plan.Steps) == 0 {
		return nil
	}
	var runner Runner
	for i, m := range plan.Steps {
		if progress != nil {
			progress(i+1, len(plan.Steps), m)
		}
		started := time.Now()
		err := runMigration(ctx, cfg, project, dir, plan, m, captures, &runner)
		if err != nil {
			return fmt.Errorf("data migration %d (%s): %w", m.ID, m.Label(), err)
		}
		if err := WriteDataVersion(dir, m.ID); err != nil {
			return fmt.Errorf("data migration %d (%s) ran, but recording it failed: %w", m.ID, m.Label(), err)
		}
		log.Printf("%s: data migration %d (%s) applied in %s", project, m.ID, m.Label(), time.Since(started).Round(time.Second))
	}
	return nil
}

func runMigration(ctx context.Context, cfg config.Config, project, dir string, plan MigratePlan, m xcomposeapp.Migration, captures map[string]string, runner *Runner) error {
	timeout := xcomposeapp.DefaultMigrationTimeout
	if m.Timeout != "" {
		d, err := time.ParseDuration(m.Timeout)
		if err != nil {
			return fmt.Errorf("timeout: %w", err)
		}
		timeout = d
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch {
	case m.Image != "":
		if *runner == nil {
			r, err := newRunner()
			if err != nil {
				return fmt.Errorf("connect to docker: %w", err)
			}
			*runner = r
		}
		envFile := readEnvFile(dir)
		spec, err := buildRunSpec(cfg, func(s string) (string, error) {
			return envinject.RenderStrict(s, cfg, project, envFile, captures)
		}, runSurface{
			Image: m.Image, User: m.User, Network: m.Network,
			Entrypoint: m.Entrypoint, Command: m.Command, Env: m.Env, Volumes: m.Volumes,
		})
		if err != nil {
			return err
		}
		_, err = (*runner).RunOnce(ctx, spec)
		return timedOut(ctx, err, timeout)

	case m.Script != "":
		if !filepath.IsLocal(m.Script) {
			return fmt.Errorf("script %q is not a path inside the app's folder", m.Script)
		}
		body, err := os.ReadFile(filepath.Join(dir, MigrateDir, m.Script))
		if err != nil {
			return fmt.Errorf("script: %w", err)
		}
		err = runHook(ctx, cfg, project, dir, string(body), []string{
			"MIGRATION_ID=" + strconv.Itoa(m.ID),
			"DATA_VERSION_FROM=" + strconv.Itoa(plan.From),
			"DATA_VERSION_TO=" + strconv.Itoa(plan.To),
		})
		return timedOut(ctx, err, timeout)

	case strings.TrimSpace(m.Blocked) != "":
		// Refused by the update's plan before anything was pulled; reaching here is a
		// caller that skipped the plan.
		return errors.New("this step is marked as manual and cannot be run")
	}
	return errors.New("the step has no image, script or blocked")
}

// timedOut names the step's own deadline when that is what ended it, rather than
// leaving a bare "signal: killed" or "context deadline exceeded".
func timedOut(ctx context.Context, err error, timeout time.Duration) error {
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("did not finish within its %s timeout: %w", timeout, err)
	}
	return err
}
