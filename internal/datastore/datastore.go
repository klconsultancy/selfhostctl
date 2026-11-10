// Package datastore reads the Management cluster's kine datastore directly, without
// anything running on it.
package datastore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "github.com/lib/pq" // kine's Postgres
)

// Revision is kine's latest revision. Every write raises it, and a live cluster
// writes within seconds: its API servers, controllers and kubelets renew their leases.
func Revision(ctx context.Context, dataSource string) (int64, error) {
	if !strings.HasPrefix(dataSource, "postgres://") && !strings.HasPrefix(dataSource, "postgresql://") {
		return 0, errors.New("only a Postgres datastore (postgres://…) can be read")
	}
	db, err := sql.Open("postgres", dataSource)
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	var rev int64
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) FROM kine").Scan(&rev); err != nil {
		return 0, fmt.Errorf("reading kine's revision: %w", err)
	}
	return rev, nil
}
