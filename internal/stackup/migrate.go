package stackup

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/xcomposeapp"
)

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
// captures is the converge's outputs: what earlier scripts wrote to $MAISON_OUTPUT
// is exported to each step, and what a step writes there is added for the setup
// steps, seed tree and files that follow.
func RunMigrations(ctx context.Context, cfg config.Config, project, dir string, plan MigratePlan, captures map[string]string, progress MigrateProgress) error {
	if len(plan.Steps) == 0 {
		return nil
	}
	for i, m := range plan.Steps {
		if progress != nil {
			progress(i+1, len(plan.Steps), m)
		}
		started := time.Now()
		err := runMigration(ctx, cfg, project, dir, plan, m, captures)
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

func runMigration(ctx context.Context, cfg config.Config, project, dir string, plan MigratePlan, m xcomposeapp.Migration, captures map[string]string) error {
	if strings.TrimSpace(m.Blocked.Value()) != "" {
		// Refused by the update's plan before anything was pulled; reaching here is a
		// caller that skipped the plan.
		return errors.New("this step is marked as manual and cannot be run")
	}
	res, err := RunScript(ctx, cfg, project, dir, m.Script, ScriptOpts{
		Label:          fmt.Sprintf("migration %d", m.ID),
		DefaultTimeout: xcomposeapp.DefaultMigrationTimeout,
		Extra: []string{
			"MIGRATION_ID=" + strconv.Itoa(m.ID),
			"DATA_VERSION_FROM=" + strconv.Itoa(plan.From),
			"DATA_VERSION_TO=" + strconv.Itoa(plan.To),
		},
		Outputs: captures,
	})
	if err != nil {
		return err
	}
	for k, v := range res.Output {
		captures[k] = v
	}
	return nil
}
