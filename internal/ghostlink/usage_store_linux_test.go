//go:build linux

package ghostlink

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestReadUsageStateDatabaseExcludesReservedAndRecoveredBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		"CREATE TABLE store_meta (key TEXT PRIMARY KEY NOT NULL, value TEXT NOT NULL)",
		"INSERT INTO store_meta VALUES ('store_id', 'store:read-test')",
		`CREATE TABLE user_usage (
			client_id TEXT PRIMARY KEY NOT NULL,
			upload_bytes INTEGER NOT NULL,
			download_bytes INTEGER NOT NULL,
			reserved_bytes INTEGER NOT NULL,
			recovered_bytes INTEGER NOT NULL,
			updated_at_unix_nano INTEGER NOT NULL
		)`,
		`INSERT INTO user_usage VALUES ('alice-device', 10, 20, 999, 777, 123)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	snapshot, err := readUsageStateDatabase(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.StoreID != "store:read-test" || len(snapshot.Users) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	user := snapshot.Users[0]
	if user.ClientID != "alice-device" || user.UploadBytes != 10 || user.DownloadBytes != 20 {
		t.Fatalf("committed usage = %+v", user)
	}
}
