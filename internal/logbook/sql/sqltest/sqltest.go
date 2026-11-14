// Package sqltest gives a test a Logbook of its own: a new PostgreSQL
// database with every Logbook migration applied, reached as the muster
// role, and removed when the test ends.
//
// It needs two things from outside, and a test that asks for a Logbook
// without them is skipped:
//
//	MUSTER_TEST_DATABASE_URL    a PostgreSQL server, as a role that may create databases
//	MUSTER_LOGBOOK_MIGRATIONS   the migrations directory of a Logbook checkout
//
// The migrations belong to Logbook and are not copied into this
// repository.
package sqltest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ClearedStack/muster/internal/lifecycle"
	"github.com/ClearedStack/muster/internal/logbook/memory"
	"github.com/ClearedStack/muster/internal/logbook/sql"
)

const template = "muster_test_logbook_template"

var (
	templateOnce sync.Once
	templateErr  error
	serial       atomic.Int64
)

// Available reports whether a test Logbook can be had.
func Available() bool {
	return os.Getenv("MUSTER_TEST_DATABASE_URL") != "" && os.Getenv("MUSTER_LOGBOOK_MIGRATIONS") != ""
}

// Logbook is one test's database.
type Logbook struct {
	Store *sql.Store
	Admin *pgx.Conn // the owner of the database, for setting a scene and looking at the result
}

func connect(t testing.TB, database string, role string) (*pgx.ConnConfig, *pgx.Conn) {
	t.Helper()
	config, err := pgx.ParseConfig(os.Getenv("MUSTER_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("MUSTER_TEST_DATABASE_URL is not a database address")
	}
	if database != "" {
		config.Database = database
	}
	if role != "" {
		config.RuntimeParams["role"] = role
	}
	config.DefaultQueryExecMode = pgx.QueryExecModeExec
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("the test database did not answer: %v", err)
	}
	return config, conn
}

// makeTemplate applies Logbook's migrations, in order, to a template
// database that every test database is copied from.
//
// Several test programs run at once and share the one server, so the
// template is made under a lock held in the server, and is kept from one
// run to the next. It is remade only when the migrations have changed:
// the template carries a hash of the files it was made from.
func makeTemplate(t testing.TB) {
	templateOnce.Do(func() { templateErr = ensureTemplate(t) })
	if templateErr != nil {
		t.Fatalf("Logbook's migrations could not be applied: %v", templateErr)
	}
}

func ensureTemplate(t testing.TB) error {
	ctx := context.Background()
	dir := os.Getenv("MUSTER_LOGBOOK_MIGRATIONS")
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil || len(files) == 0 {
		return fmt.Errorf("no migrations found in MUSTER_LOGBOOK_MIGRATIONS")
	}
	sort.Strings(files)
	hash := sha256.New()
	texts := make([]string, len(files))
	for i, file := range files {
		text, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		texts[i] = string(text)
		fmt.Fprintf(hash, "%s\n%d\n%s", filepath.Base(file), len(text), text)
	}
	want := hex.EncodeToString(hash.Sum(nil))

	_, admin := connect(t, "", "")
	defer admin.Close(ctx)
	// The same made-up number in every test program.
	const lock = 7_203_114_001
	if _, err := admin.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(lock)); err != nil {
		return err
	}
	defer admin.Exec(ctx, `SELECT pg_advisory_unlock($1)`, int64(lock))

	var have *string
	err = admin.QueryRow(ctx, `SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1`, template).Scan(&have)
	if err == nil && have != nil && *have == want {
		return nil
	}
	if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+template+` WITH (FORCE)`); err != nil {
		return err
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+template); err != nil {
		return err
	}
	_, conn := connect(t, template, "")
	for i, text := range texts {
		// One transaction for each migration, as Logbook's own migrate
		// script applies them.
		if _, err := conn.PgConn().Exec(ctx, "BEGIN;\n"+text+"\n;COMMIT;").ReadAll(); err != nil {
			conn.Close(ctx)
			return fmt.Errorf("%s: %w", filepath.Base(files[i]), err)
		}
	}
	conn.Close(ctx)
	_, err = admin.Exec(ctx, `COMMENT ON DATABASE `+template+` IS '`+want+`'`)
	return err
}

// New gives the test a Logbook of its own, or skips the test when none
// can be had.
func New(t testing.TB, clock func() time.Time) *Logbook {
	t.Helper()
	if !Available() {
		t.Skip("no test Logbook: set MUSTER_TEST_DATABASE_URL and MUSTER_LOGBOOK_MIGRATIONS")
	}
	makeTemplate(t)
	ctx := context.Background()
	name := "muster_test_" + strconv.Itoa(os.Getpid()) + "_" + strconv.FormatInt(serial.Add(1), 10)
	_, server := connect(t, "", "")
	if _, err := server.Exec(ctx, `CREATE DATABASE `+name+` TEMPLATE `+template); err != nil {
		server.Close(ctx)
		t.Fatalf("a test database could not be made: %v", err)
	}
	server.Close(ctx)

	config, admin := connect(t, name, "")
	store, err := sql.Open(address(config, name), clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.Close(ctx)
		admin.Close(ctx)
		_, server := connect(t, "", "")
		defer server.Close(ctx)
		server.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})
	return &Logbook{Store: store, Admin: admin}
}

// address is where the store under test connects: the test server, the
// test's own database, and the muster role, so that the store has
// exactly the rights Logbook gives Muster and no more.
func address(config *pgx.ConnConfig, database string) string {
	u := url.URL{
		Scheme:   "postgres",
		Host:     net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))),
		Path:     "/" + database,
		RawQuery: "role=muster&sslmode=disable",
	}
	if config.Password != "" {
		u.User = url.UserPassword(config.User, config.Password)
	} else {
		u.User = url.User(config.User)
	}
	return u.String()
}

// Seed puts made-up items on record as the owner of the database, the
// way memory.Store.Load does for the stand-in: in the order listed, each
// with its step in the queue, and a check for a live item that has none.
func (l *Logbook) Seed(t testing.TB, seed memory.Seed, at time.Time) {
	t.Helper()
	ctx := context.Background()
	highest := 0
	for _, it := range seed.Items {
		l.exec(t, `INSERT INTO core.item (item_id, state, title, recorded_at) VALUES ($1, $2, $3, $4)`,
			it.ID, string(it.State), "Made-up item "+it.ID, at)
		l.exec(t, `INSERT INTO core.ledger (item_id, actor, to_state, note, recorded_at) VALUES ($1, 'trawl', $2, 'seed', $3)`,
			it.ID, string(it.State), at)
		next, lane := it.Next, it.Lane
		if next == "" && it.State == lifecycle.Live {
			next, lane = "tidegauge", lifecycle.Backfill
		}
		if next != "" {
			l.exec(t, `INSERT INTO core.queue (worker, lane, item_id, queued_at) VALUES ($1, $2, $3, $4)`,
				next, string(lane), it.ID, at)
		}
		if n, _ := strconv.Atoi(it.ID[len("PA-"):]); n > highest {
			highest = n
		}
	}
	if highest > 0 {
		if _, err := l.Admin.Exec(ctx, `SELECT setval('core.item_number', $1)`, highest); err != nil {
			t.Fatal(err)
		}
	}
}

func (l *Logbook) exec(t testing.TB, query string, args ...any) {
	t.Helper()
	if _, err := l.Admin.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("setting the scene: %v", err)
	}
}

// Exec runs a statement as the owner of the database.
func (l *Logbook) Exec(t testing.TB, query string, args ...any) { l.exec(t, query, args...) }
