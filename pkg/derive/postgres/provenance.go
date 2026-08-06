package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ProvenanceRow struct {
	Version     int       `json:"version"`
	Source      string    `json:"source"`
	CreatedAt   time.Time `json:"created_at"`
	SeedCommand string    `json:"seed_command"`
	SchemaHash  string    `json:"schema_hash"`
}

const provenanceDDL = `
CREATE TABLE IF NOT EXISTS _plax_provenance (
    version       INTEGER NOT NULL,
    source        TEXT NOT NULL DEFAULT 'base',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    seed_command  TEXT NOT NULL DEFAULT '',
    schema_hash   TEXT NOT NULL DEFAULT ''
)
`

func CreateProvenance(ctx context.Context, pool *pgxpool.Pool, row ProvenanceRow) error {
	if _, err := pool.Exec(ctx, provenanceDDL); err != nil {
		return fmt.Errorf("provenance: create table: %w", err)
	}

	var exists bool
	err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM _plax_provenance)`).Scan(&exists)
	if err != nil {
		return fmt.Errorf("provenance: check existence: %w", err)
	}

	if exists {
		_, err = pool.Exec(ctx, `
			UPDATE _plax_provenance
			SET version = $1, source = $2, created_at = NOW(),
			    seed_command = $3, schema_hash = $4
		`, row.Version, row.Source, row.SeedCommand, row.SchemaHash)
	} else {
		_, err = pool.Exec(ctx, `
			INSERT INTO _plax_provenance (version, source, seed_command, schema_hash)
			VALUES ($1, $2, $3, $4)
		`, row.Version, row.Source, row.SeedCommand, row.SchemaHash)
	}
	if err != nil {
		return fmt.Errorf("provenance: write row: %w", err)
	}

	return nil
}

func ReadProvenance(ctx context.Context, pool *pgxpool.Pool) (*ProvenanceRow, error) {
	var exists bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = '_plax_provenance'
		)
	`).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("provenance: check table: %w", err)
	}
	if !exists {
		return nil, nil
	}

	row := &ProvenanceRow{}
	err = pool.QueryRow(ctx, `
		SELECT version, source, created_at, seed_command, schema_hash
		FROM _plax_provenance
		LIMIT 1
	`).Scan(&row.Version, &row.Source, &row.CreatedAt, &row.SeedCommand, &row.SchemaHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("provenance: read row: %w", err)
	}

	return row, nil
}

func ComputeSchemaHash(migrationsDir string) (string, error) {
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("schema hash: read dir: %w", err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}

	if len(names) == 0 {
		return "", nil
	}

	sort.Strings(names)

	h := sha256.New()
	for i, n := range names {
		if i > 0 {
			h.Write([]byte("\n"))
		}
		h.Write([]byte(n))
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
