package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrDeferredSwap marks a refresh whose staging completed but whose swap
// could not finish because the old base stayed in use. The staged base_next
// is left in place; a later refresh resumes the swap. The CLI maps this to
// exit code 2.
var ErrDeferredSwap = errors.New("swap deferred")

type BaseManager struct {
	pool     *pgxpool.Pool
	bp       *blueprint.Blueprint
	repoRoot string
	baseName string
	nextName string
	dsn      string
}

type BaseInfo struct {
	Exists        bool      `json:"exists"`
	Locked        bool      `json:"locked"`
	ProvenanceVer int       `json:"provenance_version"`
	CreatedAt     time.Time `json:"created_at"`
	HasBaseNext   bool      `json:"has_base_next"`
}

func NewBaseManager(ctx context.Context, connString string, repoRoot string, bp *blueprint.Blueprint) (*BaseManager, error) {
	// dsnForDB rewrites the database name by string surgery, which only
	// works on URL-form DSNs. Reject key=value DSNs up front instead of
	// silently connecting every per-database pool to the maintenance DB.
	if !strings.HasPrefix(connString, "postgres://") && !strings.HasPrefix(connString, "postgresql://") {
		return nil, fmt.Errorf("postgres: connString must be URL form (postgres://user:pass@host:port/dbname)")
	}

	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}

	bm := &BaseManager{
		pool:     pool,
		bp:       bp,
		repoRoot: repoRoot,
		baseName: "plax_base",
		nextName: "plax_base_next",
		dsn:      connString,
	}

	if prefix := os.Getenv("PLAX_BASE_NAME"); prefix != "" {
		bm.baseName = prefix
		bm.nextName = prefix + "_next"
	}

	return bm, nil
}

func (bm *BaseManager) Close() {
	bm.pool.Close()
}

func (bm *BaseManager) migrationsDir() string {
	if bm.bp.Seed.MigrationsDir != "" {
		return filepath.Join(bm.repoRoot, bm.bp.Seed.MigrationsDir)
	}
	return filepath.Join(bm.repoRoot, "src", "db", "migrations")
}

func (bm *BaseManager) CreateBase(ctx context.Context) error {
	exists, locked, err := bm.dbExists(ctx, bm.baseName)
	if err != nil {
		return err
	}
	if exists {
		if locked {
			return nil
		}
		return fmt.Errorf("base exists but is not locked — run 'plax base reset' to recreate")
	}

	if _, err := bm.pool.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", bm.baseName)); err != nil {
		return fmt.Errorf("create base: %w", err)
	}

	basePool, err := pgxpool.New(ctx, bm.dsnForDB(bm.baseName))
	if err != nil {
		return errors.Join(fmt.Errorf("create base: connect to %s: %w", bm.baseName, err), bm.cleanupDB(ctx, bm.baseName))
	}

	if err := bm.runMigrate(ctx, bm.baseName); err != nil {
		basePool.Close()
		return errors.Join(err, bm.cleanupDB(ctx, bm.baseName))
	}

	schemaHash, err := ComputeSchemaHash(bm.migrationsDir())
	if err != nil {
		basePool.Close()
		return errors.Join(err, bm.cleanupDB(ctx, bm.baseName))
	}

	if err := CreateProvenance(ctx, basePool, ProvenanceRow{
		Version:     1,
		Source:      "base",
		SeedCommand: bm.bp.Seed.Command,
		SchemaHash:  schemaHash,
	}); err != nil {
		basePool.Close()
		return errors.Join(err, bm.cleanupDB(ctx, bm.baseName))
	}
	basePool.Close()

	if err := bm.setLock(ctx, bm.baseName, false); err != nil {
		return errors.Join(fmt.Errorf("create base: lock: %w", err), bm.cleanupDB(ctx, bm.baseName))
	}

	return nil
}

func (bm *BaseManager) SeedBase(ctx context.Context) error {
	exists, _, err := bm.dbExists(ctx, bm.baseName)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("base does not exist — run 'plax base create' first")
	}

	if err := bm.setLock(ctx, bm.baseName, true); err != nil {
		return fmt.Errorf("seed: unlock base: %w", err)
	}

	basePool, err := pgxpool.New(ctx, bm.dsnForDB(bm.baseName))
	if err != nil {
		return errors.Join(fmt.Errorf("seed: connect to %s: %w", bm.baseName, err), bm.relock(ctx))
	}

	if err := bm.runSeed(ctx, bm.baseName); err != nil {
		basePool.Close()
		return errors.Join(err, bm.relock(ctx))
	}

	prov, err := ReadProvenance(ctx, basePool)
	if err != nil {
		basePool.Close()
		return errors.Join(fmt.Errorf("seed: read provenance: %w", err), bm.relock(ctx))
	}
	newVer := 1
	hash := ""
	if prov != nil {
		newVer = prov.Version + 1
		hash = prov.SchemaHash
	}

	if err := CreateProvenance(ctx, basePool, ProvenanceRow{
		Version:     newVer,
		Source:      "base",
		SeedCommand: bm.bp.Seed.Command,
		SchemaHash:  hash,
	}); err != nil {
		basePool.Close()
		return errors.Join(fmt.Errorf("seed: update provenance: %w", err), bm.relock(ctx))
	}
	basePool.Close()

	return bm.relock(ctx)
}

func (bm *BaseManager) ResetBase(ctx context.Context) error {
	// Drop base_next as well as the base: it is the documented recovery path
	// for an interrupted refresh, so reset must clear the wedge.
	for _, name := range []string{bm.baseName, bm.nextName} {
		exists, _, err := bm.dbExists(ctx, name)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if err := bm.terminateConnections(ctx, name); err != nil {
			return fmt.Errorf("reset: terminate connections to %s: %w", name, err)
		}
		if err := bm.dropDB(ctx, name); err != nil {
			return fmt.Errorf("reset: drop %s: %w", name, err)
		}
	}
	return bm.CreateBase(ctx)
}

func (bm *BaseManager) CloneBase(ctx context.Context, targetDB string) error {
	exists, locked, err := bm.dbExists(ctx, bm.baseName)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("base database does not exist — run 'plax base create'")
	}
	if !locked {
		return fmt.Errorf("base is not locked; clones are unsafe")
	}

	cloneExists, _, err := bm.dbExists(ctx, targetDB)
	if err != nil {
		return err
	}
	if cloneExists {
		return fmt.Errorf("database %q already exists", targetDB)
	}

	_, err = bm.pool.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", targetDB, bm.baseName))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55006" {
			return fmt.Errorf("clone: base has active connections — is it locked?")
		}
		return fmt.Errorf("clone: create %s: %w", targetDB, err)
	}

	// The target database now exists. Any failure below must drop it, or an
	// untracked database would block the next clone of the same name.
	// WithoutCancel: cleanup must run even when ctx caused the failure.
	complete := false
	defer func() {
		if !complete {
			_ = bm.DropInstanceDB(context.WithoutCancel(ctx), targetDB)
		}
	}()

	// Clones are born with ALLOW_CONNECTIONS true (datallowconn is not
	// inherited from the template), so they are ready for instance use.
	clonePool, err := pgxpool.New(ctx, bm.dsnForDB(targetDB))
	if err != nil {
		return fmt.Errorf("clone: connect to %s: %w", targetDB, err)
	}
	defer clonePool.Close()

	prov, err := ReadProvenance(ctx, clonePool)
	if err != nil {
		return fmt.Errorf("clone: read provenance: %w", err)
	}
	if prov != nil {
		prov.Source = targetDB
		if err := CreateProvenance(ctx, clonePool, *prov); err != nil {
			return fmt.Errorf("clone: update source: %w", err)
		}
	}

	complete = true
	return nil
}

func (bm *BaseManager) RefreshBase(ctx context.Context) error {
	exists, _, err := bm.dbExists(ctx, bm.baseName)
	if err != nil {
		return err
	}
	if !exists {
		return bm.CreateBase(ctx)
	}

	nextExists, _, err := bm.dbExists(ctx, bm.nextName)
	if err != nil {
		return err
	}
	if nextExists {
		// A staged, locked, provenance-stamped base_next is a deferred swap
		// from a previous refresh — resume it. Anything else is an
		// interrupted staging that cannot be resumed safely.
		ready, err := bm.baseNextReady(ctx)
		if err != nil {
			return err
		}
		if !ready {
			return fmt.Errorf("'%s' exists but is incomplete — a previous refresh was interrupted. Run 'plax base reset' to clean up", bm.nextName)
		}
		return bm.swapBase(ctx)
	}

	if _, err := bm.pool.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", bm.nextName)); err != nil {
		return fmt.Errorf("refresh: create base_next: %w", err)
	}

	nextPool, err := pgxpool.New(ctx, bm.dsnForDB(bm.nextName))
	if err != nil {
		return errors.Join(fmt.Errorf("refresh: connect to %s: %w", bm.nextName, err), bm.cleanupDB(ctx, bm.nextName))
	}

	if err := bm.runMigrate(ctx, bm.nextName); err != nil {
		nextPool.Close()
		return errors.Join(err, bm.cleanupDB(ctx, bm.nextName))
	}

	schemaHash, err := ComputeSchemaHash(bm.migrationsDir())
	if err != nil {
		nextPool.Close()
		return errors.Join(err, bm.cleanupDB(ctx, bm.nextName))
	}

	oldProv, err := bm.readProvenance(ctx, bm.baseName)
	if err != nil {
		nextPool.Close()
		return errors.Join(fmt.Errorf("refresh: read old provenance: %w", err), bm.cleanupDB(ctx, bm.nextName))
	}
	newVer := 1
	if oldProv != nil {
		newVer = oldProv.Version + 1
	}

	if err := CreateProvenance(ctx, nextPool, ProvenanceRow{
		Version:     newVer,
		Source:      "base",
		SeedCommand: bm.bp.Seed.Command,
		SchemaHash:  schemaHash,
	}); err != nil {
		nextPool.Close()
		return errors.Join(err, bm.cleanupDB(ctx, bm.nextName))
	}

	if err := bm.runSeed(ctx, bm.nextName); err != nil {
		nextPool.Close()
		return errors.Join(err, bm.cleanupDB(ctx, bm.nextName))
	}

	if err := bm.setLock(ctx, bm.nextName, false); err != nil {
		nextPool.Close()
		return errors.Join(fmt.Errorf("refresh: lock base_next: %w", err), bm.cleanupDB(ctx, bm.nextName))
	}
	// The swap renames base_next, which Postgres refuses while any
	// connection to it is open — the pool must be closed here, not deferred.
	nextPool.Close()

	return bm.swapBase(ctx)
}

func (bm *BaseManager) DropInstanceDB(ctx context.Context, dbName string) error {
	exists, _, err := bm.dbExists(ctx, dbName)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	if err := bm.terminateConnections(ctx, dbName); err != nil {
		return fmt.Errorf("drop %s: terminate connections: %w", dbName, err)
	}

	if _, err := bm.pool.Exec(ctx, fmt.Sprintf("DROP DATABASE %s", dbName)); err != nil {
		return fmt.Errorf("drop %s: %w", dbName, err)
	}

	return nil
}

func (bm *BaseManager) InstanceProvenance(ctx context.Context, dbName string) (*ProvenanceRow, error) {
	exists, err := bm.InstanceDBExists(ctx, dbName)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}

	pool, err := pgxpool.New(ctx, bm.dsnForDB(dbName))
	if err != nil {
		return nil, fmt.Errorf("instance provenance: connect to %s: %w", dbName, err)
	}
	defer pool.Close()

	return ReadProvenance(ctx, pool)
}

func (bm *BaseManager) InstanceDBExists(ctx context.Context, dbName string) (bool, error) {
	exists, _, err := bm.dbExists(ctx, dbName)
	return exists, err
}

func (bm *BaseManager) BaseStatus(ctx context.Context) (BaseInfo, error) {
	info := BaseInfo{}

	exists, locked, err := bm.dbExists(ctx, bm.baseName)
	if err != nil {
		return info, fmt.Errorf("status: query base: %w", err)
	}
	info.Exists = exists
	info.Locked = locked

	if exists {
		prov, err := bm.readProvenance(ctx, bm.baseName)
		if err == nil && prov != nil {
			info.ProvenanceVer = prov.Version
			info.CreatedAt = prov.CreatedAt
		}
	}

	nextExists, _, err := bm.dbExists(ctx, bm.nextName)
	if err != nil {
		return info, fmt.Errorf("status: query base_next: %w", err)
	}
	info.HasBaseNext = nextExists

	return info, nil
}

// baseNextReady reports whether base_next holds a fully staged refresh:
// locked and provenance-stamped. Only then is it safe to resume the swap.
func (bm *BaseManager) baseNextReady(ctx context.Context) (bool, error) {
	_, locked, err := bm.dbExists(ctx, bm.nextName)
	if err != nil {
		return false, err
	}
	if !locked {
		return false, nil
	}
	prov, err := bm.readProvenance(ctx, bm.nextName)
	if err != nil {
		return false, err
	}
	return prov != nil, nil
}

// readProvenance reads the provenance row from a database that may be
// locked, briefly allowing connections if so. The unlock window is
// read-only; the database's lock state is restored before returning. A
// concurrent clone colliding with the window fails with 55006, which the
// design treats as recoverable.
func (bm *BaseManager) readProvenance(ctx context.Context, dbName string) (*ProvenanceRow, error) {
	_, locked, err := bm.dbExists(ctx, dbName)
	if err != nil {
		return nil, err
	}

	if locked {
		if err := bm.setLock(ctx, dbName, true); err != nil {
			return nil, fmt.Errorf("unlock %s: %w", dbName, err)
		}
	}

	pool, err := pgxpool.New(ctx, bm.dsnForDB(dbName))
	if err != nil {
		if locked {
			_ = bm.setLock(ctx, dbName, false)
		}
		return nil, fmt.Errorf("connect to %s: %w", dbName, err)
	}
	prov, readErr := ReadProvenance(ctx, pool)
	pool.Close()

	if locked {
		if err := bm.setLock(ctx, dbName, false); err != nil {
			return nil, fmt.Errorf("re-lock %s: %w", dbName, err)
		}
	}
	if readErr != nil {
		return nil, readErr
	}
	return prov, nil
}

func (bm *BaseManager) dbExists(ctx context.Context, dbName string) (exists, locked bool, err error) {
	var allowConn bool
	err = bm.pool.QueryRow(ctx, "SELECT datallowconn FROM pg_database WHERE datname = $1", dbName).Scan(&allowConn)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("db exists %s: %w", dbName, err)
	}
	return true, !allowConn, nil
}

// setLock allows or refuses new connections to dbName. Existing connections
// are unaffected — callers must close pools they hold before relying on the
// lock for DDL.
func (bm *BaseManager) setLock(ctx context.Context, dbName string, allowConnections bool) error {
	_, err := bm.pool.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s WITH ALLOW_CONNECTIONS %t", dbName, allowConnections))
	return err
}

// relock re-locks the base after SeedBase. Returns nil when the lock
// succeeds so it composes with errors.Join on failure paths.
func (bm *BaseManager) relock(ctx context.Context) error {
	if err := bm.setLock(ctx, bm.baseName, false); err != nil {
		return fmt.Errorf("re-lock base: %w", err)
	}
	return nil
}

func (bm *BaseManager) dsnForDB(dbName string) string {
	u, err := url.Parse(bm.dsn)
	if err != nil {
		return bm.dsn
	}
	u.Path = "/" + dbName
	return u.String()
}

func (bm *BaseManager) runCommand(ctx context.Context, cmdStr string, dbName string) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Dir = filepath.Join(bm.repoRoot, bm.bp.Seed.Workdir)

	env := cmd.Environ()
	key := "DATABASE_URL="
	val := key + bm.dsnForDB(dbName)
	found := false
	for i, e := range env {
		if strings.HasPrefix(e, key) {
			env[i] = val
			found = true
			break
		}
	}
	if !found {
		env = append(env, val)
	}
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return err
		}
		return fmt.Errorf("%s: %w", msg, err)
	}
	return nil
}

func (bm *BaseManager) runMigrate(ctx context.Context, dbName string) error {
	if err := bm.runCommand(ctx, bm.bp.Seed.Migrate, dbName); err != nil {
		return fmt.Errorf("migrate: command failed: %w", err)
	}
	return nil
}

func (bm *BaseManager) runSeed(ctx context.Context, dbName string) error {
	if err := bm.runCommand(ctx, bm.bp.Seed.Command, dbName); err != nil {
		return fmt.Errorf("seed: command failed: %w", err)
	}
	return nil
}

func (bm *BaseManager) dropDB(ctx context.Context, dbName string) error {
	_, err := bm.pool.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s", dbName))
	return err
}

// cleanupDB removes a partially provisioned database after a failure.
// Connections are terminated first because a pool that touched the database
// may still hold idle connections, and Postgres refuses DROP with live
// connections. Returns nil when cleanup succeeds so it composes with
// errors.Join.
func (bm *BaseManager) cleanupDB(ctx context.Context, dbName string) error {
	if err := bm.terminateConnections(ctx, dbName); err != nil {
		return fmt.Errorf("cleanup: terminate connections to %s: %w", dbName, err)
	}
	if err := bm.dropDB(ctx, dbName); err != nil {
		return fmt.Errorf("cleanup: drop %s: %w", dbName, err)
	}
	return nil
}

func (bm *BaseManager) terminateConnections(ctx context.Context, dbName string) error {
	_, err := bm.pool.Exec(ctx,
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()",
		dbName,
	)
	return err
}

func (bm *BaseManager) swapBase(ctx context.Context) error {
	if err := bm.terminateConnections(ctx, bm.baseName); err != nil {
		return fmt.Errorf("swap: terminate connections: %w", err)
	}

	backoffs := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond,
		800 * time.Millisecond, 1600 * time.Millisecond, 3200 * time.Millisecond, 6400 * time.Millisecond}

	for i, d := range backoffs {
		_, err := bm.pool.Exec(ctx, fmt.Sprintf("DROP DATABASE %s", bm.baseName))
		if err == nil {
			break
		}

		if i == len(backoffs)-1 {
			return fmt.Errorf("%w: %s is staged and ready, but the old base is still in use. Run 'plax base refresh' again when instances are idle", ErrDeferredSwap, bm.nextName)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}

		_ = bm.terminateConnections(ctx, bm.baseName)
	}

	_, err := bm.pool.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s RENAME TO %s", bm.nextName, bm.baseName))
	if err != nil {
		return fmt.Errorf("swap: rename: %w", err)
	}

	return nil
}

// ListPlaxDatabases returns every database name on the connected server that
// starts with the well-known "plax_" prefix (excluding "plax_base" and
// "plax_base_next"). Used by doctor to detect orphans.
func (bm *BaseManager) ListPlaxDatabases(ctx context.Context) ([]string, error) {
	rows, err := bm.pool.Query(ctx,
		"SELECT datname FROM pg_database WHERE datname LIKE 'plax_%' AND datname != $1 AND datname != $2",
		bm.baseName, bm.nextName)
	if err != nil {
		return nil, fmt.Errorf("list plax databases: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("list plax databases: scan: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list plax databases: rows: %w", err)
	}
	return names, nil
}

// connStringForTemplate will be re-added when Phase 3 needs it.
// It renders a Postgres connection string from a blueprint hole template.
