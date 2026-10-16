// Command nairasim runs the simulator (spec/sim-v1.md).
//
// Settings, all optional:
//
//	PORT          where to listen (default 7070)
//	SIM_SCENARIO  a scenario file to load at start (default: every default)
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Temitayo-spec/naira-sim/internal/api"
	"github.com/Temitayo-spec/naira-sim/internal/scenario"
	"github.com/Temitayo-spec/naira-sim/internal/sim"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	port := os.Getenv("PORT")
	if port == "" {
		port = "7070"
	}

	sc := scenario.Default()
	if path := os.Getenv("SIM_SCENARIO"); path != "" {
		raw, err := os.ReadFile(path)
		if err == nil {
			sc, err = scenario.Parse(raw)
		}
		if err != nil {
			log.Error("can't load the scenario", "path", path, "error", err.Error())
			os.Exit(1)
		}
	}

	s := sim.New(sc, api.HTTPSender{})
	defer s.Close()
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           api.New(s),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	log.Info("naira-sim listening", "port", port, "clock", sc.Clock, "start", sc.Start, "webhook", sc.Webhook.URL)
	log.Info("this is a simulator: it never moves real money; keep it off the open internet")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server failed", "error", err.Error())
		os.Exit(1)
	}
}
