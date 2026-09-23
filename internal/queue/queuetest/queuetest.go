// Package queuetest is for tests that use the shared test database.
package queuetest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// lockKey is the advisory lock every database test holds. It is not the
// queue's schema lock: that one is transaction-scoped and is taken inside
// Open, which runs while this one is held.
const lockKey = 0x666f7267652d74 // "forge-t"

// Exclusive gives the calling test the database to itself until it ends.
//
// `go test ./...` runs packages in parallel, and the queue's and the worker's
// tests share one database: one package's TRUNCATE or DROP TABLE landed in
// the middle of the other's test, and failed it with "no such job" or
// "relation jobs does not exist". A session-level advisory lock, held on a
// connection of its own until the test's cleanup closes it, serialises the
// tests that touch the database without serialising the build.
func Exclusive(t testing.TB, dsn string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(ctx) })
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(lockKey)); err != nil {
		t.Fatalf("queuetest: take the database lock: %v", err)
	}
}
