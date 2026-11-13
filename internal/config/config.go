// Package config reads Muster's settings from the environment.
//
// A setting that is missing where one is required, or that does not
// parse, stops Muster from starting. Nothing is guessed.
package config

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Config is everything Muster is told at start.
type Config struct {
	Listen string // MUSTER_LISTEN: address to listen on

	Auth     string // MUSTER_AUTH: "kubernetes" or "file"
	AuthFile string // MUSTER_AUTH_FILE: the caller table, when Auth is "file"

	OwnershipMap string // MUSTER_OWNERSHIP_MAP: path of ownership_map.json
	SaltFile     string // MUSTER_SALT_FILE: path of the secret the daily salt is made from
	SeedFile     string // MUSTER_SEED_FILE: made-up items for the stand-in; optional

	Lease        time.Duration // MUSTER_LEASE_SECONDS: how long a lease protects a step
	FeedEvery    time.Duration // MUSTER_FEED_SECONDS: how often the feed sends a batch
	StoreTimeout time.Duration // MUSTER_STORE_SECONDS: how long a call may wait for Logbook

	PowBits  int // MUSTER_POW_BITS: zero bits the proof of work must show
	DailyCap int // MUSTER_EDIT_DAILY_CAP: submissions kept per source per day
}

// Auth modes.
const (
	AuthKubernetes = "kubernetes"
	AuthFile       = "file"
)

// Load reads the settings through getenv, which has the shape of
// os.Getenv.
func Load(getenv func(string) string) (Config, error) {
	c := Config{
		Listen:       or(getenv("MUSTER_LISTEN"), ":8080"),
		Auth:         getenv("MUSTER_AUTH"),
		AuthFile:     getenv("MUSTER_AUTH_FILE"),
		OwnershipMap: getenv("MUSTER_OWNERSHIP_MAP"),
		SaltFile:     getenv("MUSTER_SALT_FILE"),
		SeedFile:     getenv("MUSTER_SEED_FILE"),
	}
	var err error
	if c.Lease, err = seconds(getenv, "MUSTER_LEASE_SECONDS", 60, 5, 3600); err != nil {
		return Config{}, err
	}
	// The feed is stale after 15 seconds of silence, so a batch must go
	// out comfortably more often than that.
	if c.FeedEvery, err = seconds(getenv, "MUSTER_FEED_SECONDS", 5, 1, 10); err != nil {
		return Config{}, err
	}
	if c.StoreTimeout, err = seconds(getenv, "MUSTER_STORE_SECONDS", 5, 1, 30); err != nil {
		return Config{}, err
	}
	if c.PowBits, err = number(getenv, "MUSTER_POW_BITS", 20, 8, 32); err != nil {
		return Config{}, err
	}
	if c.DailyCap, err = number(getenv, "MUSTER_EDIT_DAILY_CAP", 5, 1, 1000); err != nil {
		return Config{}, err
	}

	switch c.Auth {
	case AuthKubernetes:
		if c.AuthFile != "" {
			return Config{}, errors.New("MUSTER_AUTH_FILE is set, and MUSTER_AUTH is not \"file\"")
		}
	case AuthFile:
		if c.AuthFile == "" {
			return Config{}, errors.New("MUSTER_AUTH is \"file\", and MUSTER_AUTH_FILE is not set")
		}
	case "":
		return Config{}, errors.New("MUSTER_AUTH is not set; it must be \"kubernetes\" or \"file\"")
	default:
		return Config{}, errors.New("MUSTER_AUTH must be \"kubernetes\" or \"file\"")
	}
	if c.OwnershipMap == "" {
		return Config{}, errors.New("MUSTER_OWNERSHIP_MAP is not set")
	}
	if c.SaltFile == "" {
		return Config{}, errors.New("MUSTER_SALT_FILE is not set")
	}
	return c, nil
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func number(getenv func(string) string, name string, fallback, low, high int) (int, error) {
	raw := getenv(name)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < low || n > high {
		return 0, fmt.Errorf("%s must be a whole number from %d to %d", name, low, high)
	}
	return n, nil
}

func seconds(getenv func(string) string, name string, fallback, low, high int) (time.Duration, error) {
	n, err := number(getenv, name, fallback, low, high)
	return time.Duration(n) * time.Second, err
}
