package postgres

import (
	"context"
	"testing"

	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testBlueprint() *blueprint.Blueprint {
	return &blueprint.Blueprint{
		Seed: blueprint.SeedConfig{
			Migrate: "psql \"$DATABASE_URL\" -f testdata/migrations/001_create_users.sql",
			Command: "bash testdata/seed.sh",
			Workdir: ".",
		},
	}
}

func testManager(t *testing.T) *BaseManager {
	t.Helper()
	ctx := context.Background()
	url := pgTestURL(t)
	bp := testBlueprint()
	bm, err := NewBaseManager(ctx, url, ".", bp)
	if err != nil {
		t.Skipf("skipping: cannot connect to Postgres: %v", err)
	}
	t.Cleanup(func() { bm.Close() })
	return bm
}

func TestCreateBase_Success(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")

	if err := bm.CreateBase(ctx); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}

	info, err := bm.BaseStatus(ctx)
	if err != nil {
		t.Fatalf("BaseStatus: %v", err)
	}
	if !info.Exists {
		t.Error("base should exist")
	}
	if info.ProvenanceVer != 1 {
		t.Errorf("expected version 1, got %d", info.ProvenanceVer)
	}

	_ = bm.DropInstanceDB(ctx, "plax_base")
}

func TestCreateBase_Idempotent(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")

	if err := bm.CreateBase(ctx); err != nil {
		t.Fatalf("first CreateBase: %v", err)
	}
	if err := bm.CreateBase(ctx); err != nil {
		t.Fatalf("second CreateBase should be no-op: %v", err)
	}

	_ = bm.DropInstanceDB(ctx, "plax_base")
}

func TestCloneBase_Success(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")
	_ = bm.DropInstanceDB(ctx, "test_clone")

	if err := bm.CreateBase(ctx); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}

	if err := bm.CloneBase(ctx, "test_clone"); err != nil {
		t.Fatalf("CloneBase: %v", err)
	}

	cloneDSN := bm.dsnForDB("test_clone")
	clonePool, err := pgxpool.New(ctx, cloneDSN)
	if err != nil {
		t.Fatalf("connect to clone: %v", err)
	}
	defer clonePool.Close()
	prov, err := ReadProvenance(ctx, clonePool)
	if err != nil {
		t.Fatalf("read clone provenance: %v", err)
	}
	if prov == nil {
		t.Fatal("expected provenance row in clone")
	}
	if prov.Source != "test_clone" {
		t.Errorf("expected source=test_clone, got %s", prov.Source)
	}

	_ = bm.DropInstanceDB(ctx, "test_clone")
	_ = bm.DropInstanceDB(ctx, "plax_base")
}

func TestCloneBase_AlreadyExists(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")
	_ = bm.DropInstanceDB(ctx, "dup_clone")

	if err := bm.CreateBase(ctx); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}
	if err := bm.CloneBase(ctx, "dup_clone"); err != nil {
		t.Fatalf("first clone: %v", err)
	}

	err := bm.CloneBase(ctx, "dup_clone")
	if err == nil {
		t.Fatal("expected error for duplicate clone")
	}

	_ = bm.DropInstanceDB(ctx, "dup_clone")
	_ = bm.DropInstanceDB(ctx, "plax_base")
}

func TestCloneBase_NoBase(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	err := bm.CloneBase(ctx, "nonexistent_clone")
	if err == nil {
		t.Fatal("expected error when base does not exist")
	}
}

func TestDropInstanceDB_Success(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")
	_ = bm.DropInstanceDB(ctx, "drop_test")

	if err := bm.CreateBase(ctx); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}
	if err := bm.CloneBase(ctx, "drop_test"); err != nil {
		t.Fatalf("CloneBase: %v", err)
	}

	if err := bm.DropInstanceDB(ctx, "drop_test"); err != nil {
		t.Fatalf("DropInstanceDB: %v", err)
	}

	exists, _, err := bm.dbExists(ctx, "drop_test")
	if err != nil {
		t.Fatalf("dbExists: %v", err)
	}
	if exists {
		t.Error("database should be dropped")
	}

	_ = bm.DropInstanceDB(ctx, "plax_base")
}

func TestDropInstanceDB_NoDB(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	if err := bm.DropInstanceDB(ctx, "nonexistent_db"); err != nil {
		t.Fatalf("DropInstanceDB should be no-op: %v", err)
	}
}

func TestBaseStatus_Exists(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")

	if err := bm.CreateBase(ctx); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}

	info, err := bm.BaseStatus(ctx)
	if err != nil {
		t.Fatalf("BaseStatus: %v", err)
	}
	if !info.Exists || info.ProvenanceVer != 1 {
		t.Errorf("got exists=%v ver=%d", info.Exists, info.ProvenanceVer)
	}

	_ = bm.DropInstanceDB(ctx, "plax_base")
}

func TestBaseStatus_NotExists(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")

	info, err := bm.BaseStatus(ctx)
	if err != nil {
		t.Fatalf("BaseStatus: %v", err)
	}
	if info.Exists {
		t.Error("expected Exists=false")
	}

	_ = bm.DropInstanceDB(ctx, "plax_base")
}

func TestBaseStatus_BaseNext(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")
	_ = bm.DropInstanceDB(ctx, "plax_base_next")

	if err := bm.CreateBase(ctx); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}

	if _, err := bm.pool.Exec(ctx, "CREATE DATABASE plax_base_next"); err != nil {
		t.Fatalf("create base_next: %v", err)
	}

	info, err := bm.BaseStatus(ctx)
	if err != nil {
		t.Fatalf("BaseStatus: %v", err)
	}
	if !info.HasBaseNext {
		t.Error("expected HasBaseNext=true")
	}

	_ = bm.DropInstanceDB(ctx, "plax_base_next")
	_ = bm.DropInstanceDB(ctx, "plax_base")
}
