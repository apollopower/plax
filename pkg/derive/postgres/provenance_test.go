package postgres

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func pgTestURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PLAX_TEST_POSTGRES_URL")
	if url == "" {
		url = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
	}
	return url
}

func testPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, pgTestURL(t))
	if err != nil {
		t.Skipf("skipping: cannot connect to Postgres (%s): %v", pgTestURL(t), err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func testDB(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	pool := testPool(t, ctx)
	_, _ = pool.Exec(ctx, "DROP DATABASE IF EXISTS "+name)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name)
	})
}

func TestCreateProvenance_RoundTrip(t *testing.T) {
	ctx := context.Background()
	testDB(t, ctx, "prov_test_roundtrip")
	pool := testPool(t, ctx)

	_, err := pool.Exec(ctx, "CREATE DATABASE prov_test_roundtrip")
	if err != nil {
		t.Fatalf("create db: %v", err)
	}

	dsn := pgTestURL(t)
	idx := -1
	for i := len(dsn) - 1; i >= 0; i-- {
		if dsn[i] == '/' {
			idx = i
			break
		}
	}
	dbDSN := dsn[:idx+1] + "prov_test_roundtrip"

	dbPool, err := pgxpool.New(ctx, dbDSN)
	if err != nil {
		t.Fatalf("connect to test db: %v", err)
	}
	defer dbPool.Close()

	row := ProvenanceRow{
		Version:     1,
		Source:      "base",
		SeedCommand: "bun run db fixtures",
		SchemaHash:  "abc123",
	}
	if err := CreateProvenance(ctx, dbPool, row); err != nil {
		t.Fatalf("CreateProvenance: %v", err)
	}

	got, err := ReadProvenance(ctx, dbPool)
	if err != nil {
		t.Fatalf("ReadProvenance: %v", err)
	}
	if got == nil {
		t.Fatal("expected row, got nil")
	}
	if got.Version != 1 || got.Source != "base" || got.SchemaHash != "abc123" {
		t.Errorf("got %+v, expected version=1 source=base schema=abc123", got)
	}
}

func TestCreateProvenance_Idempotent(t *testing.T) {
	ctx := context.Background()
	testDB(t, ctx, "prov_test_idem")
	pool := testPool(t, ctx)

	_, err := pool.Exec(ctx, "CREATE DATABASE prov_test_idem")
	if err != nil {
		t.Fatalf("create db: %v", err)
	}

	dsn := pgTestURL(t)
	idx := -1
	for i := len(dsn) - 1; i >= 0; i-- {
		if dsn[i] == '/' {
			idx = i
			break
		}
	}
	dbDSN := dsn[:idx+1] + "prov_test_idem"

	dbPool, err := pgxpool.New(ctx, dbDSN)
	if err != nil {
		t.Fatalf("connect to test db: %v", err)
	}
	defer dbPool.Close()

	row1 := ProvenanceRow{Version: 1, Source: "base", SeedCommand: "cmd1", SchemaHash: "h1"}
	if err := CreateProvenance(ctx, dbPool, row1); err != nil {
		t.Fatalf("first CreateProvenance: %v", err)
	}

	row2 := ProvenanceRow{Version: 2, Source: "base", SeedCommand: "cmd2", SchemaHash: "h2"}
	if err := CreateProvenance(ctx, dbPool, row2); err != nil {
		t.Fatalf("second CreateProvenance: %v", err)
	}

	got, err := ReadProvenance(ctx, dbPool)
	if err != nil {
		t.Fatalf("ReadProvenance: %v", err)
	}
	if got.Version != 2 || got.SeedCommand != "cmd2" {
		t.Errorf("got version=%d cmd=%s, expected version=2 cmd=cmd2", got.Version, got.SeedCommand)
	}
}

func TestReadProvenance_NoTable(t *testing.T) {
	ctx := context.Background()
	testDB(t, ctx, "prov_test_notable")
	pool := testPool(t, ctx)

	_, err := pool.Exec(ctx, "CREATE DATABASE prov_test_notable")
	if err != nil {
		t.Fatalf("create db: %v", err)
	}

	dsn := pgTestURL(t)
	idx := -1
	for i := len(dsn) - 1; i >= 0; i-- {
		if dsn[i] == '/' {
			idx = i
			break
		}
	}
	dbDSN := dsn[:idx+1] + "prov_test_notable"

	dbPool, err := pgxpool.New(ctx, dbDSN)
	if err != nil {
		t.Fatalf("connect to test db: %v", err)
	}
	defer dbPool.Close()

	got, err := ReadProvenance(ctx, dbPool)
	if err != nil {
		t.Fatalf("ReadProvenance: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestComputeSchemaHash_Success(t *testing.T) {
	dir := filepath.Join("testdata", "migrations")
	hash, err := ComputeSchemaHash(dir)
	if err != nil {
		t.Fatalf("ComputeSchemaHash: %v", err)
	}
	if hash == "" {
		t.Error("expected non-empty hash")
	}
}

func TestComputeSchemaHash_NoDir(t *testing.T) {
	hash, err := ComputeSchemaHash("testdata/nonexistent")
	if err != nil {
		t.Fatalf("ComputeSchemaHash: %v", err)
	}
	if hash != "" {
		t.Errorf("expected empty string, got %q", hash)
	}
}
