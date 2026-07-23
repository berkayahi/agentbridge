package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
)

type schemaQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// SchemaFingerprint is a stable hash of the user-defined SQLite schema.
func SchemaFingerprint(ctx context.Context, db *sql.DB) (string, error) {
	return schemaFingerprint(ctx, db)
}

func schemaFingerprint(ctx context.Context, db schemaQueryer) (string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT type, name, COALESCE(sql, '')
		FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_%'
		ORDER BY type, name`)
	if err != nil {
		return "", fmt.Errorf("read schema for fingerprint: %w", err)
	}
	defer rows.Close()
	hash := sha256.New()
	for rows.Next() {
		var kind, name, definition string
		if err := rows.Scan(&kind, &name, &definition); err != nil {
			return "", fmt.Errorf("scan schema for fingerprint: %w", err)
		}
		definition = normalizeSchemaSQL(definition)
		_, _ = hash.Write([]byte(kind))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(definition))
		_, _ = hash.Write([]byte{0})
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate schema for fingerprint: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
