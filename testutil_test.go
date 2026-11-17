package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"go.mau.fi/util/dbutil"
)

func testDB(t *testing.T) *dbutil.Database {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := dbutil.NewWithDialect(fmt.Sprintf("file:%s?_txlock=immediate", path), "sqlite3-fk-wal")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func testStore(t *testing.T) *Store {
	t.Helper()
	s := &Store{db: testDB(t)}
	if err := s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}
