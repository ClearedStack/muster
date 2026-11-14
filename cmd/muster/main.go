// Command muster hands out work to the workers, records every result and
// change of state, and receives edit proposals.
//
// It writes nothing to disk and needs no privileges. All of its settings
// come from the environment; see the config package.
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/ClearedStack/muster/internal/auth"
	"github.com/ClearedStack/muster/internal/config"
	"github.com/ClearedStack/muster/internal/feed"
	"github.com/ClearedStack/muster/internal/health"
	"github.com/ClearedStack/muster/internal/httpapi"
	"github.com/ClearedStack/muster/internal/intake"
	"github.com/ClearedStack/muster/internal/logbook"
	"github.com/ClearedStack/muster/internal/logbook/memory"
	"github.com/ClearedStack/muster/internal/logbook/sql"
	"github.com/ClearedStack/muster/internal/queue"
)

// memoryLimit is the heap the runtime aims to stay under, leaving room
// inside the 48 MiB budget for everything that is not heap. A GOMEMLIMIT
// in the environment takes its place.
const memoryLimit = 36 << 20

const maxSmallFile = 1 << 20

func main() {
	logger := log.New(os.Stdout, "muster: ", log.LstdFlags|log.LUTC)
	if err := run(logger); err != nil {
		logger.Printf("stopped: %v", err)
		os.Exit(1)
	}
}

func run(logger *log.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(memoryLimit)
	}

	var who auth.Authenticator
	switch cfg.Auth {
	case config.AuthKubernetes:
		if who, err = auth.InCluster(); err != nil {
			return err
		}
	case config.AuthFile:
		data, err := readSmall(cfg.AuthFile)
		if err != nil {
			return errors.New("the caller table could not be read")
		}
		if who, err = auth.ParseTable(data); err != nil {
			return err
		}
	}

	data, err := readSmall(cfg.OwnershipMap)
	if err != nil {
		return errors.New("the ownership map could not be read")
	}
	owners, err := intake.ParseOwnership(data)
	if err != nil {
		return err
	}
	secret, err := readSmall(cfg.SaltFile)
	if err != nil {
		return errors.New("the salt secret could not be read")
	}
	if len(secret) < intake.MinSecretBytes {
		return errors.New("the salt secret is too short")
	}

	var store logbook.Store
	if cfg.DatabaseURL != "" {
		db, err := sql.Open(cfg.DatabaseURL, time.Now)
		if err != nil {
			return err
		}
		defer func() {
			closing, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			db.Close(closing)
		}()
		store = db
		logger.Print("using Logbook")
	} else {
		standIn := memory.New()
		store = standIn
		logger.Print("using the in-memory stand-in for Logbook; nothing is kept when this process stops")
		if cfg.SeedFile != "" {
			data, err := readSmall(cfg.SeedFile)
			if err != nil {
				return errors.New("the seed could not be read")
			}
			seed, err := memory.ParseSeed(data)
			if err != nil {
				return err
			}
			if err := standIn.Load(seed); err != nil {
				return err
			}
			logger.Printf("loaded %d seed items", len(seed.Items))
		}
	}

	hub := &feed.Hub{Store: store, Clock: time.Now, Timeout: cfg.StoreTimeout}
	server := httpapi.New(httpapi.Server{
		Auth:         who,
		Queue:        &queue.Service{Store: store, Clock: time.Now, LeaseFor: cfg.Lease, Notify: hub},
		Health:       &health.Service{Store: store, Clock: time.Now, Notify: hub},
		Intake:       &intake.Service{Store: store, Clock: time.Now, Notify: hub, Owners: owners, Secret: secret, Difficulty: cfg.PowBits, DailyCap: cfg.DailyCap},
		Feed:         hub,
		StoreTimeout: cfg.StoreTimeout,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return errors.New("could not listen on the configured address")
	}
	srv := &http.Server{
		Handler:           server,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
		ErrorLog:          log.New(discard{}, "", 0),
	}

	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		hub.Run(ctx, cfg.FeedEvery)
	}()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()
	logger.Print("ready")

	select {
	case err := <-served:
		return errors.Join(errors.New("the HTTP server stopped"), scrub(err))
	case <-ctx.Done():
	}
	logger.Print("stopping")
	<-feedDone // every feed subscriber has been let go
	closing, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(closing); err != nil {
		return errors.New("did not stop cleanly in time")
	}
	return nil
}

func readSmall(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxSmallFile {
		return nil, errors.New("file too large")
	}
	return os.ReadFile(path)
}

// scrub keeps an error from another system out of the log, where it
// might name an address.
func scrub(err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return errors.New("a network error")
}

// discard drops what the HTTP server would log by itself, which can
// include the addresses of callers.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
