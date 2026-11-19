//go:build linux

package ghostlink

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var errUsageStateUnavailable = errors.New("GhostLink usage state database is unavailable")

func readUsageStateDatabase(ctx context.Context, path string) (UsageSnapshot, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return UsageSnapshot{}, errUsageStateUnavailable
		}
		return UsageSnapshot{}, fmt.Errorf("stat GhostLink usage state database: %w", err)
	}
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro&_query_only=1&_busy_timeout=5000")
	if err != nil {
		return UsageSnapshot{}, fmt.Errorf("open GhostLink usage state database: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		return UsageSnapshot{}, fmt.Errorf("ping GhostLink usage state database: %w", err)
	}

	var storeID string
	if err := db.QueryRowContext(ctx,
		"SELECT value FROM store_meta WHERE key = 'store_id'",
	).Scan(&storeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UsageSnapshot{}, errors.New("GhostLink usage state database has no store identity")
		}
		return UsageSnapshot{}, fmt.Errorf("read GhostLink usage store identity: %w", err)
	}
	rows, err := db.QueryContext(ctx,
		"SELECT client_id, upload_bytes, download_bytes, updated_at_unix_nano FROM user_usage ORDER BY client_id",
	)
	if err != nil {
		return UsageSnapshot{}, fmt.Errorf("query GhostLink usage state: %w", err)
	}
	defer rows.Close()
	var users []UsageUser
	for rows.Next() {
		var user UsageUser
		var upload, download, updated int64
		if err := rows.Scan(&user.ClientID, &upload, &download, &updated); err != nil {
			return UsageSnapshot{}, fmt.Errorf("scan GhostLink usage state: %w", err)
		}
		if upload < 0 || download < 0 {
			return UsageSnapshot{}, errors.New("GhostLink usage state contains a negative committed counter")
		}
		user.UploadBytes = uint64(upload)
		user.DownloadBytes = uint64(download)
		user.UpdatedAtUnixNano = updated
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return UsageSnapshot{}, fmt.Errorf("iterate GhostLink usage state: %w", err)
	}
	return UsageSnapshot{
		StoreID:             storeID,
		GeneratedAtUnixNano: time.Now().UnixNano(),
		Users:               users,
	}, nil
}
