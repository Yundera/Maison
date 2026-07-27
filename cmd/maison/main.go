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
	"github.com/yundera/maison/internal/server"
	"github.com/yundera/maison/internal/ui"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix(brand.Slug + ": ")

	cfg := config.FromEnv()

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
		log.Printf("listening on %s (data root %s)", cfg.Addr, cfg.DataRoot)
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
