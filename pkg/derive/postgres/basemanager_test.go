package postgres

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/testutil"
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

func failingSeedBlueprint() *blueprint.Blueprint {
	bp := testBlueprint()
	bp.Seed.Command = `psql "$DATABASE_URL" -c "INSERT INTO nonexistent_table (id) VALUES (1);"`
	return bp
}

func testManager(t *testing.T) *BaseManager {
	t.Helper()
	return testManagerWith(t, testBlueprint())
}

func testManagerWith(t *testing.T, bp *blueprint.Blueprint) *BaseManager {
	t.Helper()
	ctx := context.Background()
	url := pgTestURL(t)
	// These tests create and drop plax_base; serialize against the cmd/plax
	// end-to-end test, which uses the same database on the same server.
	testutil.LockPostgres(t, url)
	bm, err := NewBaseManager(ctx, url, ".", bp)
	if err != nil {
		t.Skipf("skipping: cannot connect to Postgres: %v", err)
	}
	t.Cleanup(func() { bm.Close() })
	return bm
}

// setBaseLock flips ALLOW_CONNECTIONS on the base, simulating the states the
// lock lifecycle is supposed to guard against.
func setBaseLock(t *testing.T, bm *BaseManager, allow bool) {
	t.Helper()
	if err := bm.setLock(context.Background(), bm.baseName, allow); err != nil {
		t.Fatalf("set lock(%t): %v", allow, err)
	}
}

// countUsers requires an unlocked dbName — callers unlock the base first.
func countUsers(t *testing.T, ctx context.Context, bm *BaseManager, dbName string) int {
	t.Helper()
	pool, err := pgxpool.New(ctx, bm.dsnForDB(dbName))
	if err != nil {
		t.Fatalf("connect to %s: %v", dbName, err)
	}
	defer pool.Close()
	var n int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM users").Scan(&n); err != nil {
		t.Fatalf("count users in %s: %v", dbName, err)
	}
	return n
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
	if !info.Locked {
		t.Error("base should be locked after create")
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

func TestCreateBase_ExistsUnlocked(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")

	if err := bm.CreateBase(ctx); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}
	setBaseLock(t, bm, true)

	err := bm.CreateBase(ctx)
	if err == nil || !strings.Contains(err.Error(), "not locked") {
		t.Errorf("expected 'not locked' error, got %v", err)
	}

	_ = bm.DropInstanceDB(ctx, "plax_base")
}

func TestSeedBase_Success(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")

	if err := bm.CreateBase(ctx); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}
	if err := bm.SeedBase(ctx); err != nil {
		t.Fatalf("SeedBase: %v", err)
	}

	info, err := bm.BaseStatus(ctx)
	if err != nil {
		t.Fatalf("BaseStatus: %v", err)
	}
	if info.ProvenanceVer != 2 {
		t.Errorf("expected version 2 after seed, got %d", info.ProvenanceVer)
	}
	if !info.Locked {
		t.Error("base should be re-locked after seed")
	}

	setBaseLock(t, bm, true)
	if n := countUsers(t, ctx, bm, "plax_base"); n != 1 {
		t.Errorf("expected 1 seed user, got %d", n)
	}

	_ = bm.DropInstanceDB(ctx, "plax_base")
}

func TestSeedBase_NoBase(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")

	err := bm.SeedBase(ctx)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("expected 'does not exist' error, got %v", err)
	}
}

func TestSeedBase_CommandFails(t *testing.T) {
	bm := testManagerWith(t, failingSeedBlueprint())
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")

	if err := bm.CreateBase(ctx); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}

	err := bm.SeedBase(ctx)
	if err == nil {
		t.Fatal("expected seed command failure")
	}

	info, serr := bm.BaseStatus(ctx)
	if serr != nil {
		t.Fatalf("BaseStatus: %v", serr)
	}
	if !info.Locked {
		t.Error("base should be re-locked after a failed seed")
	}
	if info.ProvenanceVer != 1 {
		t.Errorf("version should stay 1 after failed seed, got %d", info.ProvenanceVer)
	}

	_ = bm.DropInstanceDB(ctx, "plax_base")
}

func TestResetBase_Success(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")

	if err := bm.CreateBase(ctx); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}
	if err := bm.SeedBase(ctx); err != nil {
		t.Fatalf("SeedBase: %v", err)
	}
	if err := bm.ResetBase(ctx); err != nil {
		t.Fatalf("ResetBase: %v", err)
	}

	info, err := bm.BaseStatus(ctx)
	if err != nil {
		t.Fatalf("BaseStatus: %v", err)
	}
	if !info.Exists || info.ProvenanceVer != 1 {
		t.Errorf("got exists=%v ver=%d, expected exists=true ver=1", info.Exists, info.ProvenanceVer)
	}

	setBaseLock(t, bm, true)
	if n := countUsers(t, ctx, bm, "plax_base"); n != 0 {
		t.Errorf("reset base should have no seed data, got %d users", n)
	}

	_ = bm.DropInstanceDB(ctx, "plax_base")
}

func TestResetBase_CleansBaseNext(t *testing.T) {
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

	if err := bm.ResetBase(ctx); err != nil {
		t.Fatalf("ResetBase: %v", err)
	}

	nextExists, _, err := bm.dbExists(ctx, "plax_base_next")
	if err != nil {
		t.Fatalf("dbExists: %v", err)
	}
	if nextExists {
		t.Error("reset should drop orphaned base_next")
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

func TestCloneBase_BaseNotLocked(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")
	_ = bm.DropInstanceDB(ctx, "unlocked_clone")

	if err := bm.CreateBase(ctx); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}
	setBaseLock(t, bm, true)

	err := bm.CloneBase(ctx, "unlocked_clone")
	if err == nil || !strings.Contains(err.Error(), "not locked") {
		t.Errorf("expected 'not locked' error, got %v", err)
	}

	_ = bm.DropInstanceDB(ctx, "unlocked_clone")
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

	_ = bm.DropInstanceDB(ctx, "plax_base")

	err := bm.CloneBase(ctx, "nonexistent_clone")
	if err == nil {
		t.Fatal("expected error when base does not exist")
	}
}

func TestRefreshBase_Success(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")
	_ = bm.DropInstanceDB(ctx, "plax_base_next")

	if err := bm.CreateBase(ctx); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}
	if err := bm.SeedBase(ctx); err != nil {
		t.Fatalf("SeedBase: %v", err)
	}
	if err := bm.RefreshBase(ctx); err != nil {
		t.Fatalf("RefreshBase: %v", err)
	}

	info, err := bm.BaseStatus(ctx)
	if err != nil {
		t.Fatalf("BaseStatus: %v", err)
	}
	if info.ProvenanceVer != 3 {
		t.Errorf("expected version 3 after refresh, got %d", info.ProvenanceVer)
	}
	if info.HasBaseNext {
		t.Error("base_next should not be left behind")
	}
	if !info.Locked {
		t.Error("refreshed base should be locked")
	}

	setBaseLock(t, bm, true)
	if n := countUsers(t, ctx, bm, "plax_base"); n != 1 {
		t.Errorf("refreshed base should have seed data, got %d users", n)
	}

	_ = bm.DropInstanceDB(ctx, "plax_base")
}

func TestRefreshBase_NoBase(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")
	_ = bm.DropInstanceDB(ctx, "plax_base_next")

	if err := bm.RefreshBase(ctx); err != nil {
		t.Fatalf("RefreshBase should delegate to CreateBase: %v", err)
	}

	info, err := bm.BaseStatus(ctx)
	if err != nil {
		t.Fatalf("BaseStatus: %v", err)
	}
	if !info.Exists || info.ProvenanceVer != 1 {
		t.Errorf("got exists=%v ver=%d, expected exists=true ver=1", info.Exists, info.ProvenanceVer)
	}

	_ = bm.DropInstanceDB(ctx, "plax_base")
}

func TestRefreshBase_BaseNextExists(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")
	_ = bm.DropInstanceDB(ctx, "plax_base_next")

	if err := bm.CreateBase(ctx); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}
	// An unlocked, unstamped base_next is an interrupted staging — not
	// resumable.
	if _, err := bm.pool.Exec(ctx, "CREATE DATABASE plax_base_next"); err != nil {
		t.Fatalf("create base_next: %v", err)
	}

	err := bm.RefreshBase(ctx)
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("expected 'interrupted' error, got %v", err)
	}

	_ = bm.DropInstanceDB(ctx, "plax_base_next")
	_ = bm.DropInstanceDB(ctx, "plax_base")
}

func TestRefreshBase_ResumeDeferred(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")
	_ = bm.DropInstanceDB(ctx, "plax_base_next")

	if err := bm.CreateBase(ctx); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}

	// Stage base_next exactly as a refresh would leave it after a deferred
	// swap: migrated, stamped, locked.
	if _, err := bm.pool.Exec(ctx, "CREATE DATABASE plax_base_next"); err != nil {
		t.Fatalf("create base_next: %v", err)
	}
	if err := bm.runMigrate(ctx, "plax_base_next"); err != nil {
		t.Fatalf("migrate base_next: %v", err)
	}
	nextPool, err := pgxpool.New(ctx, bm.dsnForDB("plax_base_next"))
	if err != nil {
		t.Fatalf("connect to base_next: %v", err)
	}
	if err := CreateProvenance(ctx, nextPool, ProvenanceRow{
		Version:     42,
		Source:      "base",
		SeedCommand: bm.bp.Seed.Command,
		SchemaHash:  "staged",
	}); err != nil {
		t.Fatalf("stamp base_next: %v", err)
	}
	nextPool.Close()
	if err := bm.setLock(ctx, "plax_base_next", false); err != nil {
		t.Fatalf("lock base_next: %v", err)
	}

	if err := bm.RefreshBase(ctx); err != nil {
		t.Fatalf("RefreshBase should resume the staged swap: %v", err)
	}

	info, err := bm.BaseStatus(ctx)
	if err != nil {
		t.Fatalf("BaseStatus: %v", err)
	}
	if info.ProvenanceVer != 42 {
		t.Errorf("expected version 42 from staged base_next, got %d", info.ProvenanceVer)
	}
	if info.HasBaseNext {
		t.Error("base_next should be consumed by the swap")
	}
	if !info.Locked {
		t.Error("swapped base should be locked")
	}

	_ = bm.DropInstanceDB(ctx, "plax_base")
}

func TestRefreshBase_SeedFails(t *testing.T) {
	bm := testManagerWith(t, failingSeedBlueprint())
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")
	_ = bm.DropInstanceDB(ctx, "plax_base_next")

	if err := bm.CreateBase(ctx); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}

	err := bm.RefreshBase(ctx)
	if err == nil {
		t.Fatal("expected refresh to fail on seed")
	}

	info, serr := bm.BaseStatus(ctx)
	if serr != nil {
		t.Fatalf("BaseStatus: %v", serr)
	}
	if !info.Exists || info.ProvenanceVer != 1 {
		t.Errorf("old base should be intact: exists=%v ver=%d", info.Exists, info.ProvenanceVer)
	}
	if !info.Locked {
		t.Error("old base should still be locked")
	}
	if info.HasBaseNext {
		t.Error("failed refresh should drop base_next")
	}

	_ = bm.DropInstanceDB(ctx, "plax_base")
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

func TestDropInstanceDB_ActiveConnections(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")
	_ = bm.DropInstanceDB(ctx, "conn_test")

	if err := bm.CreateBase(ctx); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}
	if err := bm.CloneBase(ctx, "conn_test"); err != nil {
		t.Fatalf("CloneBase: %v", err)
	}

	held, err := pgxpool.New(ctx, bm.dsnForDB("conn_test"))
	if err != nil {
		t.Fatalf("connect to clone: %v", err)
	}
	defer held.Close()
	if err := held.Ping(ctx); err != nil {
		t.Fatalf("ping clone: %v", err)
	}

	if err := bm.DropInstanceDB(ctx, "conn_test"); err != nil {
		t.Fatalf("DropInstanceDB should terminate held connections: %v", err)
	}

	_ = bm.DropInstanceDB(ctx, "plax_base")
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
	if !info.Exists || !info.Locked || info.ProvenanceVer != 1 {
		t.Errorf("got exists=%v locked=%v ver=%d", info.Exists, info.Locked, info.ProvenanceVer)
	}
	if info.CreatedAt.IsZero() {
		t.Error("expected non-zero created_at")
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

// TestEndToEnd_RefreshWhileCloning exercises the swap retry path: clones in
// flight hold the template lock, so the swap's DROP must back off and retry
// until they finish. Clones that collide with the brief provenance-read
// unlock window fail with a recoverable error and are tolerated.
func TestEndToEnd_RefreshWhileCloning(t *testing.T) {
	bm := testManager(t)
	ctx := context.Background()

	_ = bm.DropInstanceDB(ctx, "plax_base")
	_ = bm.DropInstanceDB(ctx, "plax_base_next")
	for i := 0; i < 10; i++ {
		_ = bm.DropInstanceDB(ctx, fmt.Sprintf("clone_%d", i))
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for i := 0; i < 10; i++ {
			_ = bm.DropInstanceDB(ctx, fmt.Sprintf("clone_%d", i))
		}
		_ = bm.DropInstanceDB(ctx, "plax_base_next")
		_ = bm.DropInstanceDB(ctx, "plax_base")
	})

	if err := bm.CreateBase(ctx); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}
	if err := bm.SeedBase(ctx); err != nil {
		t.Fatalf("SeedBase: %v", err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	versions := map[string]int{}
	var cloneErrs []error

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			name := fmt.Sprintf("clone_%d", i)
			if err := bm.CloneBase(ctx, name); err != nil {
				mu.Lock()
				cloneErrs = append(cloneErrs, err)
				mu.Unlock()
			} else {
				pool, err := pgxpool.New(ctx, bm.dsnForDB(name))
				if err == nil {
					prov, _ := ReadProvenance(ctx, pool)
					pool.Close()
					if prov != nil {
						mu.Lock()
						versions[name] = prov.Version
						mu.Unlock()
					}
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	time.Sleep(100 * time.Millisecond)
	if err := bm.RefreshBase(ctx); err != nil {
		t.Fatalf("RefreshBase: %v", err)
	}
	wg.Wait()

	info, err := bm.BaseStatus(ctx)
	if err != nil {
		t.Fatalf("BaseStatus: %v", err)
	}
	if info.ProvenanceVer != 3 {
		t.Errorf("expected refreshed base at version 3, got %d", info.ProvenanceVer)
	}

	if len(versions) == 0 {
		t.Fatalf("no clones succeeded; errors: %v", cloneErrs)
	}
	for name, v := range versions {
		if v != 2 && v != 3 {
			t.Errorf("clone %s has unexpected version %d (want pre- or post-refresh)", name, v)
		}
	}
	t.Logf("%d clones succeeded, %d failed recoverably: %v", len(versions), len(cloneErrs), cloneErrs)
}

func TestDSNForDB_QueryParamSlash(t *testing.T) {
	bm := testManager(t)
	// Simulate a DSN with slashes in query params.
	bm.dsn = "postgres://user:pass@localhost:5432/postgres?search_path=foo/bar"

	got := bm.dsnForDB("plax_test")
	if !strings.Contains(got, "plax_test") {
		t.Errorf("dsnForDB result should contain target db name, got %s", got)
	}
	if !strings.Contains(got, "search_path=foo/bar") {
		t.Errorf("dsnForDB result should preserve query params, got %s", got)
	}
	if strings.Contains(got, "/postgres?") {
		t.Errorf("dsnForDB should replace db path, got %s", got)
	}
}
