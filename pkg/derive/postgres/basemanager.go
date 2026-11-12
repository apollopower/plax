package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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
	timeouts timeouts
}

type BaseInfo struct {
	Exists        bool      `json:"exists"`
	Locked        bool      `json:"locked"`
	ProvenanceVer int       `json:"provenance_version"`
	CreatedAt     time.Time `json:"created_at"`
	HasBaseNext   bool      `json:"has_base_next"`
}

func NewBaseManager(ctx context.Context, connString string, repoRoot string, bp *blueprint.Blueprint) (*BaseManager, error) {
	return newBaseManager(ctx, connString, repoRoot, bp, defaultTimeouts())
}

func newBaseManager(ctx context.Context, connString string, repoRoot string, bp *blueprint.Blueprint, limits timeouts) (bm *BaseManager, err error) {
	if limits.connect <= 0 || limits.operation <= 0 || limits.cleanup <= 0 {
		return nil, fmt.Errorf("postgres: timeouts must be positive")
	}
	ctx, finish := phase(ctx, limits.connect, "postgres: connect")
	defer func() {
		finish(&err)
		if err != nil && bm != nil {
			bm.Close()
			bm = nil
		}
	}()
	// Per-database pools rewrite the URL path. Reject key=value DSNs instead of
	// silently connecting every per-database pool to the maintenance DB.
	if !strings.HasPrefix(connString, "postgres://") && !strings.HasPrefix(connString, "postgresql://") {
		return nil, fmt.Errorf("postgres: connString must be URL form (postgres://user:pass@host:port/dbname)")
	}

	pool, err := newPool(ctx, connString, limits.connect)
	if err != nil {
		return nil, &connectionError{cause: err}
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, &connectionError{cause: errors.Join(err, ctx.Err())}
	}

	bm = &BaseManager{
		pool:     pool,
		bp:       bp,
		repoRoot: repoRoot,
		baseName: "plax_base",
		nextName: "plax_base_next",
		dsn:      connString,
		timeouts: limits,
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

func (bm *BaseManager) CreateBase(ctx context.Context) (err error) {
	created := false
	defer func() {
		if err != nil && created {
			err = errors.Join(err, bm.cleanupDB(ctx, bm.baseName))
		}
	}()
	err = bm.databasePhase(ctx, "create base "+bm.baseName, func(ctx context.Context) error {
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
		if err := bm.createDB(ctx, bm.baseName, ""); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil || !created {
		return err
	}
	if err := bm.runMigrate(ctx, bm.baseName); err != nil {
		return err
	}
	hash, err := ComputeSchemaHash(bm.migrationsDir())
	if err != nil {
		return err
	}
	return bm.databasePhase(ctx, "stamp/lock base "+bm.baseName, func(ctx context.Context) error {
		pool, err := bm.poolForDB(ctx, bm.baseName)
		if err != nil {
			return err
		}
		defer pool.Close()
		if err := CreateProvenance(ctx, pool, ProvenanceRow{Version: 1, Source: "base", SeedCommand: bm.bp.Seed.Command, SchemaHash: hash}); err != nil {
			return err
		}
		return bm.setLock(ctx, bm.baseName, false)
	})
}

func (bm *BaseManager) SeedBase(ctx context.Context) (err error) {
	restore := false
	defer func() {
		if restore {
			err = errors.Join(err, bm.relock(ctx))
		}
	}()
	err = bm.databasePhase(ctx, "seed unlock "+bm.baseName, func(ctx context.Context) error {
		exists, locked, err := bm.dbExists(ctx, bm.baseName)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("base does not exist — run 'plax base create' first")
		}
		if locked {
			// ALTER may succeed even when its response is lost.
			restore = true
			return bm.setLock(ctx, bm.baseName, true)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := bm.runSeed(ctx, bm.baseName); err != nil {
		return err
	}
	return bm.databasePhase(ctx, "seed provenance "+bm.baseName, func(ctx context.Context) error {
		pool, err := bm.poolForDB(ctx, bm.baseName)
		if err != nil {
			return err
		}
		defer pool.Close()
		if err := bm.assertSeedNonEmpty(ctx, pool); err != nil {
			return err
		}
		prov, err := ReadProvenance(ctx, pool)
		if err != nil {
			return err
		}
		row := ProvenanceRow{Version: 1, Source: "base", SeedCommand: bm.bp.Seed.Command}
		if prov != nil {
			row.Version, row.SchemaHash = prov.Version+1, prov.SchemaHash
		}
		return CreateProvenance(ctx, pool, row)
	})
}

func (bm *BaseManager) ResetBase(ctx context.Context) error {
	// Drop base_next as well as the base: it is the documented recovery path
	// for an interrupted refresh, so reset must clear the wedge.
	for _, name := range []string{bm.baseName, bm.nextName} {
		if err := bm.DropInstanceDB(ctx, name); err != nil {
			return fmt.Errorf("reset: drop %s: %w", name, err)
		}
	}
	return bm.CreateBase(ctx)
}

func (bm *BaseManager) CloneBase(ctx context.Context, targetDB string) (err error) {
	parent := ctx
	ctx, finish := phase(ctx, bm.timeouts.operation, "clone "+targetDB)
	defer finish(&err)
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

	if err := bm.createDB(ctx, targetDB, bm.baseName); err != nil {
		return err
	}

	// The target database now exists. Any failure below must drop it, or an
	// untracked database would block the next clone of the same name.
	defer func() {
		if err != nil || ctx.Err() != nil {
			err = errors.Join(err, ctx.Err(), bm.cleanupDB(parent, targetDB))
		}
	}()

	// Clones are born with ALLOW_CONNECTIONS true (datallowconn is not
	// inherited from the template), so they are ready for instance use.
	clonePool, err := bm.poolForDB(ctx, targetDB)
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

	return nil
}

func (bm *BaseManager) RefreshBase(ctx context.Context) (err error) {
	created, exists, staged := false, false, false
	defer func() {
		if err != nil && created && !staged {
			err = errors.Join(err, bm.cleanupDB(ctx, bm.nextName))
		}
	}()
	err = bm.databasePhase(ctx, "refresh stage "+bm.nextName, func(ctx context.Context) error {
		var err error
		exists, _, err = bm.dbExists(ctx, bm.baseName)
		if err != nil {
			return err
		}
		nextExists, _, err := bm.dbExists(ctx, bm.nextName)
		if err != nil {
			return err
		}
		if nextExists {
			if !exists {
				return fmt.Errorf("interrupted swap: %s is absent and %s remains; inspect staged data before recovery; 'plax base reset' discards staged state", bm.baseName, bm.nextName)
			}
			ready, err := bm.baseNextReady(ctx)
			if err != nil {
				return err
			}
			if !ready {
				return fmt.Errorf("'%s' exists but is incomplete — a previous refresh was interrupted; 'plax base reset' discards staged state", bm.nextName)
			}
			staged = true
			return nil
		}
		if !exists {
			return nil
		}
		if err := bm.createDB(ctx, bm.nextName, ""); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return err
	}
	if !exists {
		return bm.CreateBase(ctx)
	}
	if staged {
		return bm.swapBase(ctx)
	}
	if err := bm.runMigrate(ctx, bm.nextName); err != nil {
		return err
	}
	hash, err := ComputeSchemaHash(bm.migrationsDir())
	if err != nil {
		return err
	}
	err = bm.databasePhase(ctx, "refresh provenance "+bm.nextName, func(ctx context.Context) error {
		old, err := bm.readProvenance(ctx, bm.baseName)
		if err != nil {
			return err
		}
		ver := 1
		if old != nil {
			ver = old.Version + 1
		}
		pool, err := bm.poolForDB(ctx, bm.nextName)
		if err != nil {
			return err
		}
		defer pool.Close()
		return CreateProvenance(ctx, pool, ProvenanceRow{Version: ver, Source: "base", SeedCommand: bm.bp.Seed.Command, SchemaHash: hash})
	})
	if err != nil {
		return err
	}
	if err := bm.runSeed(ctx, bm.nextName); err != nil {
		return err
	}
	err = bm.databasePhase(ctx, "refresh lock "+bm.nextName, func(ctx context.Context) error {
		pool, err := bm.poolForDB(ctx, bm.nextName)
		if err != nil {
			return err
		}
		defer pool.Close()
		if err := bm.assertSeedNonEmpty(ctx, pool); err != nil {
			return err
		}
		return bm.setLock(ctx, bm.nextName, false)
	})
	if err != nil {
		return err
	}
	staged = true
	return bm.swapBase(ctx)
}

func (bm *BaseManager) DropInstanceDB(ctx context.Context, dbName string) (err error) {
	ctx, finish := phase(ctx, bm.timeouts.operation, "drop "+dbName)
	defer finish(&err)
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

func (bm *BaseManager) InstanceProvenance(ctx context.Context, dbName string) (_ *ProvenanceRow, err error) {
	ctx, finish := phase(ctx, bm.timeouts.operation, "instance provenance "+dbName)
	defer finish(&err)
	exists, err := bm.InstanceDBExists(ctx, dbName)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}

	pool, err := bm.poolForDB(ctx, dbName)
	if err != nil {
		return nil, fmt.Errorf("instance provenance: connect to %s: %w", dbName, err)
	}
	defer pool.Close()

	return ReadProvenance(ctx, pool)
}

// AppliedMigrations returns the set of migration identifiers recorded as
// applied in the instance's tracking table, or nil if the table does not exist
// yet (migrations not yet run). Callers must check that the blueprint declares
// seed.applied_migrations before calling.
func (bm *BaseManager) AppliedMigrations(ctx context.Context, dbName string) (_ []string, err error) {
	ctx, finish := phase(ctx, bm.timeouts.operation, "applied migrations "+dbName)
	defer finish(&err)
	am := bm.bp.Seed.AppliedMigrations
	if am == nil {
		return nil, fmt.Errorf("applied migrations: no seed.applied_migrations configured")
	}

	exists, err := bm.InstanceDBExists(ctx, dbName)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}

	pool, err := bm.poolForDB(ctx, dbName)
	if err != nil {
		return nil, fmt.Errorf("applied migrations: connect to %s: %w", dbName, err)
	}
	defer pool.Close()

	var tableExists bool
	err = pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = $1
		)
	`, am.Table).Scan(&tableExists)
	if err != nil {
		return nil, fmt.Errorf("applied migrations: check table: %w", err)
	}
	if !tableExists {
		return nil, nil
	}

	var names []string
	q := fmt.Sprintf(`SELECT DISTINCT "%s" FROM "%s"`, am.Column, am.Table)
	rows, err := pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("applied migrations: query %s.%s: %w", am.Table, am.Column, err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("applied migrations: scan: %w", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("applied migrations: rows: %w", err)
	}

	sort.Strings(names)
	return names, nil
}

func (bm *BaseManager) InstanceDBExists(ctx context.Context, dbName string) (_ bool, err error) {
	ctx, finish := phase(ctx, bm.timeouts.operation, "database exists "+dbName)
	defer finish(&err)
	exists, _, err := bm.dbExists(ctx, dbName)
	return exists, err
}

func (bm *BaseManager) BaseStatus(ctx context.Context) (_ BaseInfo, err error) {
	ctx, finish := phase(ctx, bm.timeouts.operation, "base status "+bm.baseName)
	defer finish(&err)
	info := BaseInfo{}

	exists, locked, err := bm.dbExists(ctx, bm.baseName)
	if err != nil {
		return info, fmt.Errorf("status: query base: %w", err)
	}
	info.Exists = exists
	info.Locked = locked

	if exists {
		prov, err := bm.readProvenance(ctx, bm.baseName)
		if err != nil {
			return info, fmt.Errorf("status: provenance %s: %w", bm.baseName, err)
		}
		if prov != nil {
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
func (bm *BaseManager) readProvenance(ctx context.Context, dbName string) (_ *ProvenanceRow, err error) {
	_, locked, err := bm.dbExists(ctx, dbName)
	if err != nil {
		return nil, err
	}

	if locked {
		defer func() { err = errors.Join(err, bm.restoreLock(ctx, dbName)) }()
		if err := bm.setLock(ctx, dbName, true); err != nil {
			return nil, fmt.Errorf("unlock %s: %w", dbName, err)
		}
	}

	pool, err := bm.poolForDB(ctx, dbName)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", dbName, err)
	}
	prov, readErr := ReadProvenance(ctx, pool)
	pool.Close()

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
		return false, false, fmt.Errorf("db exists %s: %w", dbName, safeDBError(err))
	}
	return true, !allowConn, nil
}

// setLock allows or refuses new connections to dbName. Existing connections
// are unaffected — callers must close pools they hold before relying on the
// lock for DDL.
func (bm *BaseManager) setLock(ctx context.Context, dbName string, allowConnections bool) error {
	_, err := bm.pool.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s WITH ALLOW_CONNECTIONS %t", dbName, allowConnections))
	return safeDBError(err)
}

// relock re-locks the base after SeedBase. Returns nil when the lock
// succeeds so it composes with errors.Join on failure paths.
func (bm *BaseManager) relock(ctx context.Context) error {
	return bm.restoreLock(ctx, bm.baseName)
}

func (bm *BaseManager) restoreLock(ctx context.Context, dbName string) error {
	return bm.recovery(ctx, "re-lock "+dbName+" (lock state unconfirmed on failure)", func(ctx context.Context) error {
		return bm.setLock(ctx, dbName, false)
	})
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
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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

	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
		if ctx.Err() != nil {
			return errors.Join(ctx.Err(), err)
		}
	case <-ctx.Done():
		killErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(killErr, syscall.ESRCH) {
			killErr = nil
		}
		return errors.Join(ctx.Err(), killErr, <-done)
	}
	if err != nil {
		msg := strings.TrimSpace(out.String())
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

func (bm *BaseManager) assertSeedNonEmpty(ctx context.Context, pool *pgxpool.Pool) error {
	var sum int
	err := pool.QueryRow(ctx, "SELECT COALESCE(SUM(n_live_tup), 0) FROM pg_stat_user_tables").Scan(&sum)
	if err != nil {
		return fmt.Errorf("seed row count: %w", err)
	}
	if sum > 0 {
		return nil
	}
	for i := 0; i < 3; i++ {
		if err := waitContext(ctx, 500*time.Millisecond); err != nil {
			return fmt.Errorf("seed row count: %w", err)
		}
		err = pool.QueryRow(ctx, "SELECT COALESCE(SUM(n_live_tup), 0) FROM pg_stat_user_tables").Scan(&sum)
		if err != nil {
			return fmt.Errorf("seed row count: %w", err)
		}
		if sum > 0 {
			return nil
		}
	}
	return fmt.Errorf("seeded base has zero rows across all user tables — the seed command may have failed silently")
}

func (bm *BaseManager) runSeed(ctx context.Context, dbName string) error {
	if err := bm.runCommand(ctx, bm.bp.Seed.Command, dbName); err != nil {
		return fmt.Errorf("seed: command failed: %w", err)
	}
	return nil
}

func (bm *BaseManager) dropDB(ctx context.Context, dbName string) error {
	_, err := bm.pool.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s", dbName))
	return safeDBError(err)
}

// cleanupDB removes a partially provisioned database after a failure.
// Connections are terminated first because a pool that touched the database
// may still hold idle connections, and Postgres refuses DROP with live
// connections. Returns nil when cleanup succeeds so it composes with
// errors.Join.
func (bm *BaseManager) cleanupDB(ctx context.Context, dbName string) error {
	return bm.recovery(ctx, "cleanup "+dbName+" (database may remain on failure)", func(ctx context.Context) error {
		if err := bm.terminateConnections(ctx, dbName); err != nil {
			return err
		}
		return bm.dropDB(ctx, dbName)
	})
}

func (bm *BaseManager) createDB(ctx context.Context, name, template string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize()
	if template != "" {
		q += " TEMPLATE " + pgx.Identifier{template}.Sanitize()
	}
	_, err := bm.pool.Exec(ctx, q)
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Errorf("create %s: %w", name, safeDBError(err))
	}
	// A missing response is not proof CREATE failed. Only recover the target
	// that passed the caller's pre-existing-database check.
	return errors.Join(fmt.Errorf("create %s: outcome uncertain: %w", name, safeDBError(err)), bm.cleanupDB(ctx, name))
}

func (bm *BaseManager) terminateConnections(ctx context.Context, dbName string) error {
	_, err := bm.pool.Exec(ctx,
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()",
		dbName,
	)
	return safeDBError(err)
}

type dbExecer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func (bm *BaseManager) swapBase(ctx context.Context) (err error) {
	ctx, finish := phase(ctx, bm.timeouts.operation, "swap "+bm.baseName)
	defer finish(&err)
	return bm.swap(ctx, bm.pool)
}

func (bm *BaseManager) swap(ctx context.Context, db dbExecer) error {
	terminate := func() error {
		_, err := db.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", bm.baseName)
		return safeDBError(err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := terminate(); err != nil {
		return fmt.Errorf("swap: terminate connections: %w", err)
	}

	backoffs := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond,
		800 * time.Millisecond, 1600 * time.Millisecond, 3200 * time.Millisecond, 6400 * time.Millisecond}

	for i, d := range backoffs {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := db.Exec(ctx, fmt.Sprintf("DROP DATABASE %s", bm.baseName))
		if ctx.Err() != nil {
			return fmt.Errorf("swap: drop %s unconfirmed; %s remains staged; inspect both names before recovery; 'plax base reset' discards staged state: %w", bm.baseName, bm.nextName, errors.Join(safeDBError(err), ctx.Err()))
		}
		if err == nil {
			break
		}

		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55006" {
			return fmt.Errorf("swap: drop %s: %w", bm.baseName, safeDBError(err))
		}
		if i == len(backoffs)-1 {
			return fmt.Errorf("%w: %s is staged and ready, but the old base is still in use. Run 'plax base refresh' again when instances are idle: %w", ErrDeferredSwap, bm.nextName, err)
		}

		if err := waitContext(ctx, d); err != nil {
			return err
		}

		if err := terminate(); err != nil {
			return fmt.Errorf("swap: terminate connections: %w", err)
		}
	}

	var err error
	if ctx.Err() == nil {
		_, err = db.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s RENAME TO %s", bm.nextName, bm.baseName))
	}
	err = errors.Join(safeDBError(err), ctx.Err())
	if err != nil {
		return fmt.Errorf("swap: rename %s to %s unconfirmed after dropping old base; inspect both names; 'plax base reset' discards staged state: %w", bm.nextName, bm.baseName, safeDBError(err))
	}

	return nil
}

// ListPlaxDatabases returns every database name on the connected server that
// starts with the well-known "plax_" prefix (excluding "plax_base" and
// "plax_base_next"). Used by doctor to detect orphans.
func (bm *BaseManager) ListPlaxDatabases(ctx context.Context) (_ []string, err error) {
	ctx, finish := phase(ctx, bm.timeouts.operation, "list plax databases")
	defer finish(&err)
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

// ConnString builds a Postgres connection URL from a blueprint's logical
// Postgres service definition, extracting user and password from the service
// environment.
func ConnString(bp *blueprint.Blueprint) (string, error) {
	user := "postgres"
	password := "postgres"
	found := false
	for _, svc := range bp.Services {
		if svc.Isolation == blueprint.IsolationLogical && svc.Type == "postgres" {
			found = true
			if u, ok := svc.Env["POSTGRES_USER"]; ok && u != "" {
				user = u
			}
			if p, ok := svc.Env["POSTGRES_PASSWORD"]; ok && p != "" {
				password = p
			}
			break
		}
	}
	if !found {
		return "", fmt.Errorf("no logical postgres service in blueprint")
	}
	return fmt.Sprintf("postgres://%s:%s@localhost:5432/postgres?sslmode=disable", user, password), nil
}

// connStringForTemplate will be re-added when Phase 3 needs it.
// It renders a Postgres connection string from a blueprint hole template.
