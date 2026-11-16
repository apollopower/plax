package postgres

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/apollopower/plax/pkg/testutil"
	"github.com/jackc/pgx/v5/pgxpool"
)

func pgTestURL(t *testing.T) string {
	t.Helper()
	return testutil.PostgresURL(t)
}

func testPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	cfg := testDatabaseConfig(t, pgTestURL(t), "")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(&connectionError{cause: err})
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatal(&connectionError{cause: err})
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func testDB(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	testutil.LockPostgres(t, pgTestURL(t))
	pool := testPool(t, ctx)
	if _, err := pool.Exec(ctx, "DROP DATABASE IF EXISTS "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name); err != nil {
			t.Error(err)
		}
	})
}

func testDatabaseConfig(t *testing.T, dsn, name string) *pgxpool.Config {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(&connectionError{cause: err})
	}
	if name != "" {
		cfg.ConnConfig.Database = name
	}
	cfg.ConnConfig.ConnectTimeout = 10 * time.Second
	return cfg
}

func TestPostgres_TestDatabaseURLPreservesOptions(t *testing.T) {
	cfg := testDatabaseConfig(t, "postgres://user:encoded%2Fsecret@127.0.0.1:5433/original?sslmode=disable&search_path=foo/bar&application_name=fixture&password=query%2Fsecret", "owned")
	if cfg.ConnConfig.Database != "owned" || cfg.ConnConfig.User != "user" || cfg.ConnConfig.Password != "query/secret" ||
		cfg.ConnConfig.Port != 5433 || cfg.ConnConfig.RuntimeParams["search_path"] != "foo/bar" ||
		cfg.ConnConfig.RuntimeParams["application_name"] != "fixture" || cfg.ConnConfig.TLSConfig != nil {
		t.Fatal("fixture selection lost connection options/credentials")
	}
	cfg = testDatabaseConfig(t, "postgres://user:encoded%2Fsecret@127.0.0.1/original?sslmode=disable", "owned")
	if cfg.ConnConfig.Password != "encoded/secret" {
		t.Fatal("fixture selection lost encoded userinfo")
	}
}

func TestPostgres_CreateProvenanceRoundTrip(t *testing.T) {
	ctx := context.Background()
	testDB(t, ctx, "prov_test_roundtrip")
	pool := testPool(t, ctx)

	_, err := pool.Exec(ctx, "CREATE DATABASE prov_test_roundtrip")
	if err != nil {
		t.Fatalf("create db: %v", err)
	}

	dbPool, err := pgxpool.NewWithConfig(ctx, testDatabaseConfig(t, pgTestURL(t), "prov_test_roundtrip"))
	if err != nil {
		t.Fatal(&connectionError{cause: err})
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

func TestPostgres_CreateProvenanceIdempotent(t *testing.T) {
	ctx := context.Background()
	testDB(t, ctx, "prov_test_idem")
	pool := testPool(t, ctx)

	_, err := pool.Exec(ctx, "CREATE DATABASE prov_test_idem")
	if err != nil {
		t.Fatalf("create db: %v", err)
	}

	dbPool, err := pgxpool.NewWithConfig(ctx, testDatabaseConfig(t, pgTestURL(t), "prov_test_idem"))
	if err != nil {
		t.Fatal(&connectionError{cause: err})
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

func TestPostgres_ReadProvenanceNoTable(t *testing.T) {
	ctx := context.Background()
	testDB(t, ctx, "prov_test_notable")
	pool := testPool(t, ctx)

	_, err := pool.Exec(ctx, "CREATE DATABASE prov_test_notable")
	if err != nil {
		t.Fatalf("create db: %v", err)
	}

	dbPool, err := pgxpool.NewWithConfig(ctx, testDatabaseConfig(t, pgTestURL(t), "prov_test_notable"))
	if err != nil {
		t.Fatal(&connectionError{cause: err})
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

func TestPostgres_ComputeSchemaHashSuccess(t *testing.T) {
	dir := filepath.Join("testdata", "migrations")
	hash, err := ComputeSchemaHash(dir)
	if err != nil {
		t.Fatalf("ComputeSchemaHash: %v", err)
	}
	if hash == "" {
		t.Error("expected non-empty hash")
	}
}

func TestPostgres_ComputeSchemaHashNoDir(t *testing.T) {
	hash, err := ComputeSchemaHash("testdata/nonexistent")
	if err != nil {
		t.Fatalf("ComputeSchemaHash: %v", err)
	}
	if hash != "" {
		t.Errorf("expected empty string, got %q", hash)
	}
}
