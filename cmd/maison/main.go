// Command maison is a lightweight, dashboard-only reimagining of CasaOS.
// It serves the embedded Svelte UI plus a REST + WebSocket API and drives the
// host Docker engine over the mounted socket.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yundera/maison/internal/brand"
	"github.com/yundera/maison/internal/appenv"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/server"
	"github.com/yundera/maison/internal/ui"
)

func main() {
	// `maison <command> …` is the CLI (cli.go), which talks to the running server over
	// its admin socket. It branches off before anything below: the server's
	// construction starts samplers and schedulers that a one-shot command must not.
	if len(os.Args) > 1 {
		os.Exit(runCLI(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix(brand.Slug + ": ")

	cfg := config.FromEnv()

	// App scripts spell paths the way the host does; make that spelling resolve
	// here too when the data folder is mounted somewhere else (Unraid, Docker
	// Desktop). Not fatal: only scripts that touch their files from the shell
	// would miss them.
	if err := envinject.EnsureHostPathAlias(cfg); err != nil {
		log.Printf("data path alias: %v", err)
	}

	// Half a feedback sink is a deployment mistake, not a choice: say so, rather than
	// let the feature quietly not appear.
	if (cfg.Feedback.URL == "") != (cfg.Feedback.Token == "") {
		log.Printf("feedback: FEEDBACK_URL and FEEDBACK_TOKEN must be set together — feedback disabled")
	}

	// .env.app states what every app receives (see internal/appenv). Create it with
	// the documented default when the deployment has none, then read it live: it is
	// the deployment's file, and an edit to it must reach the next app start without
	// restarting Maison.
	if err := appenv.Ensure(cfg); err != nil {
		log.Fatalf("app env: %v", err)
	}
	cfg.AppEnv = func() map[string]string { return appenv.Load(cfg) }

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.New(cfg, ui.Dist()),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("%s listening on %s (data root %s)", brand.Version, cfg.Addr, cfg.DataRoot)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Print("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
