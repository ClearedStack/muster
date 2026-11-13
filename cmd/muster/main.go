// Command muster serves the five worker calls against an in-memory
// stand-in for Logbook.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/clearedstack/muster/internal/api"
	"github.com/clearedstack/muster/internal/logbook"
	"github.com/clearedstack/muster/internal/queue"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("muster: %v", err)
	}
}

func run() error {
	addr := os.Getenv("MUSTER_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	ttl := 60 * time.Second
	if v := os.Getenv("MUSTER_LEASE_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 3600 {
			return errors.New("MUSTER_LEASE_SECONDS must be a whole number from 1 to 3600")
		}
		ttl = time.Duration(n) * time.Second
	}

	book := logbook.NewMemory()
	if path := os.Getenv("MUSTER_SEED"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return errors.New("the file named by MUSTER_SEED could not be read")
		}
		var seeds []logbook.Seed
		if err := json.Unmarshal(raw, &seeds); err != nil {
			return errors.New("the file named by MUSTER_SEED is not a JSON list of items")
		}
		if err := book.Load(seeds); err != nil {
			return err
		}
		log.Printf("muster: loaded %d made-up items", len(seeds))
	}

	srv := &api.Server{
		Gate:    &queue.Gatekeeper{Book: book, TTL: ttl},
		Now:     time.Now,
		Timeout: 5 * time.Second,
	}
	hs := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- hs.ListenAndServe() }()
	log.Printf("muster: in-memory stand-in for Logbook; nothing is kept after a restart")
	log.Printf("muster: listening, lease length %s", ttl)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hs.Shutdown(shut)
}
