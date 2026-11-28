// Package testenv gives tests a real Postgres: one container per test binary,
// and a fresh database per test cloned from a migrated template.
package testenv

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dataanalyst/api/internal/platform/db"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpg "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	once    sync.Once
	baseURL string // connection string without the database name applied
	ctr     *tcpg.PostgresContainer
	initErr error
	counter atomic.Int64
)

const template = "daa_template"

func start() {
	ctx := context.Background()
	ctr, initErr = tcpg.Run(ctx, "postgres:17-alpine",
		tcpg.WithDatabase(template),
		tcpg.WithUsername("daa"),
		tcpg.WithPassword("daa"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if initErr != nil {
		return
	}
	baseURL, initErr = ctr.ConnectionString(ctx, "sslmode=disable")
	if initErr != nil {
		return
	}
	pool, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		initErr = err
		return
	}
	initErr = db.Migrate(ctx, pool)
	pool.Close() // a template database must have no open connections
}

// Postgres returns a pool on a new, migrated, empty database.
func Postgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("postgres test")
	}
	once.Do(start)
	if initErr != nil {
		t.Skipf("postgres unavailable: %v", initErr)
	}
	ctx := context.Background()
	name := fmt.Sprintf("t%d_%d", time.Now().UnixNano(), counter.Add(1))

	admin, err := pgxpool.New(ctx, replaceDB(baseURL, "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, template)); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, replaceDB(baseURL, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// replaceDB swaps the database name in a postgres:// URL.
func replaceDB(url, name string) string {
	// URLs look like postgres://user:pass@host:port/dbname?params
	q := ""
	if i := strings.IndexByte(url, '?'); i >= 0 {
		url, q = url[:i], url[i:]
	}
	slash := strings.LastIndexByte(url, '/')
	return url[:slash+1] + name + q
}
