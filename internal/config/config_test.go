package config

import (
	"testing"
	"time"
)

func env(pairs map[string]string) func(string) string {
	return func(name string) string { return pairs[name] }
}

func base() map[string]string {
	return map[string]string{
		"MUSTER_AUTH":          "kubernetes",
		"MUSTER_OWNERSHIP_MAP": "/made-up/ownership_map.json",
		"MUSTER_SALT_FILE":     "/made-up/salt",
	}
}

func TestDefaults(t *testing.T) {
	c, err := Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Listen: ":8080", Auth: "kubernetes",
		OwnershipMap: "/made-up/ownership_map.json", SaltFile: "/made-up/salt",
		Lease: 60 * time.Second, FeedEvery: 5 * time.Second, StoreTimeout: 5 * time.Second,
		PowBits: 20, DailyCap: 5,
	}
	if c != want {
		t.Errorf("got  %+v\nwant %+v", c, want)
	}
}

func TestOverrides(t *testing.T) {
	e := base()
	e["MUSTER_AUTH"] = "file"
	e["MUSTER_AUTH_FILE"] = "/made-up/callers.json"
	e["MUSTER_LISTEN"] = "127.0.0.1:9000"
	e["MUSTER_LEASE_SECONDS"] = "120"
	e["MUSTER_FEED_SECONDS"] = "2"
	e["MUSTER_STORE_SECONDS"] = "3"
	e["MUSTER_POW_BITS"] = "22"
	e["MUSTER_EDIT_DAILY_CAP"] = "9"
	e["MUSTER_SEED_FILE"] = "/made-up/seed.json"
	c, err := Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:9000" || c.Lease != 2*time.Minute || c.FeedEvery != 2*time.Second ||
		c.StoreTimeout != 3*time.Second || c.PowBits != 22 || c.DailyCap != 9 ||
		c.AuthFile != "/made-up/callers.json" || c.SeedFile != "/made-up/seed.json" {
		t.Errorf("got %+v", c)
	}
}

// A setting that is missing or does not parse stops Muster from
// starting. Nothing is guessed.
func TestRefusals(t *testing.T) {
	cases := map[string]func(map[string]string){
		"no way to identify callers":        func(e map[string]string) { delete(e, "MUSTER_AUTH") },
		"an unknown way to identify":        func(e map[string]string) { e["MUSTER_AUTH"] = "none" },
		"file mode with no file":            func(e map[string]string) { e["MUSTER_AUTH"] = "file" },
		"a caller table in cluster mode":    func(e map[string]string) { e["MUSTER_AUTH_FILE"] = "/made-up/callers.json" },
		"no ownership map":                  func(e map[string]string) { delete(e, "MUSTER_OWNERSHIP_MAP") },
		"no salt":                           func(e map[string]string) { delete(e, "MUSTER_SALT_FILE") },
		"a lease that is not a number":      func(e map[string]string) { e["MUSTER_LEASE_SECONDS"] = "a minute" },
		"a lease that is too short":         func(e map[string]string) { e["MUSTER_LEASE_SECONDS"] = "1" },
		"a lease with a unit":               func(e map[string]string) { e["MUSTER_LEASE_SECONDS"] = "60s" },
		"a feed too slow for the 15 s rule": func(e map[string]string) { e["MUSTER_FEED_SECONDS"] = "15" },
		"a feed that never ticks":           func(e map[string]string) { e["MUSTER_FEED_SECONDS"] = "0" },
		"a puzzle that is too easy":         func(e map[string]string) { e["MUSTER_POW_BITS"] = "0" },
		"a puzzle that is too hard":         func(e map[string]string) { e["MUSTER_POW_BITS"] = "64" },
		"no submissions allowed":            func(e map[string]string) { e["MUSTER_EDIT_DAILY_CAP"] = "0" },
		"a negative cap":                    func(e map[string]string) { e["MUSTER_EDIT_DAILY_CAP"] = "-1" },
		"a store wait that is not a number": func(e map[string]string) { e["MUSTER_STORE_SECONDS"] = "soon" },
		"a seed file as well as a database": func(e map[string]string) {
			e["MUSTER_SEED_FILE"] = "/made-up/seed.json"
			e["MUSTER_DATABASE_URL"] = "postgres://made-up-host/logbook"
		},
	}
	for name, change := range cases {
		e := base()
		change(e)
		if c, err := Load(env(e)); err == nil {
			t.Errorf("%s: accepted as %+v", name, c)
		}
	}
}
