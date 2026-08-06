package postgres

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/jackc/pgx/v5/pgxpool"
)

type BaseManager struct {
	pool     *pgxpool.Pool
	bp       *blueprint.Blueprint
	repoRoot string
	baseName string
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
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}

	return &BaseManager{
		pool:     pool,
		bp:       bp,
		repoRoot: repoRoot,
		baseName: "plax_base",
		dsn:      connString,
	}, nil
}

func (bm *BaseManager) Close() {
	bm.pool.Close()
}

func (bm *BaseManager) CreateBase(ctx context.Context) error {
	exists, _, err := bm.dbExists(ctx, bm.baseName)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	if _, err := bm.pool.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", bm.baseName)); err != nil {
		return fmt.Errorf("create base: %w", err)
	}

	baseDSN := bm.dsnForDB(bm.baseName)
	basePool, err := pgxpool.New(ctx, baseDSN)
	if err != nil {
		_ = bm.dropDB(ctx, bm.baseName)
		return fmt.Errorf("create base: connect to %s: %w", bm.baseName, err)
	}
	defer basePool.Close()

	if err := bm.runMigrate(ctx, bm.baseName); err != nil {
		_ = bm.dropDB(ctx, bm.baseName)
		return err
	}

	schemaHash, err := ComputeSchemaHash(filepath.Join(bm.repoRoot, "src", "db", "migrations"))
	if err != nil {
		_ = bm.dropDB(ctx, bm.baseName)
		return err
	}

	if err := CreateProvenance(ctx, basePool, ProvenanceRow{
		Version:     1,
		Source:      "base",
		SeedCommand: bm.bp.Seed.Command,
		SchemaHash:  schemaHash,
	}); err != nil {
		_ = bm.dropDB(ctx, bm.baseName)
		return err
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

	baseDSN := bm.dsnForDB(bm.baseName)
	basePool, err := pgxpool.New(ctx, baseDSN)
	if err != nil {
		return fmt.Errorf("seed: connect to %s: %w", bm.baseName, err)
	}
	defer basePool.Close()

	if err := bm.runSeed(ctx, bm.baseName); err != nil {
		return err
	}

	prov, err := ReadProvenance(ctx, basePool)
	if err != nil {
		return fmt.Errorf("seed: read provenance: %w", err)
	}
	newVer := 1
	if prov != nil {
		newVer = prov.Version + 1
	}

	hash := ""
	if prov != nil {
		hash = prov.SchemaHash
	}

	if err := CreateProvenance(ctx, basePool, ProvenanceRow{
		Version:     newVer,
		Source:      "base",
		SeedCommand: bm.bp.Seed.Command,
		SchemaHash:  hash,
	}); err != nil {
		return fmt.Errorf("seed: update provenance: %w", err)
	}

	return nil
}

func (bm *BaseManager) ResetBase(ctx context.Context) error {
	if err := bm.dropDB(ctx, bm.baseName); err != nil {
		return err
	}
	return bm.CreateBase(ctx)
}

func (bm *BaseManager) CloneBase(ctx context.Context, targetDB string) error {
	exists, _, err := bm.dbExists(ctx, bm.baseName)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("base database does not exist — run 'plax base create'")
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
		if strings.Contains(err.Error(), "55006") || strings.Contains(err.Error(), "being accessed by other users") {
			return fmt.Errorf("clone: base has active connections — is it locked?")
		}
		return fmt.Errorf("clone: create %s: %w", targetDB, err)
	}

	cloneDSN := bm.dsnForDB(targetDB)
	clonePool, err := pgxpool.New(ctx, cloneDSN)
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

func (bm *BaseManager) RefreshBase(ctx context.Context) error {
	exists, _, err := bm.dbExists(ctx, bm.baseName)
	if err != nil {
		return err
	}
	if !exists {
		return bm.CreateBase(ctx)
	}

	baseNext := "plax_base_next"
	nextExists, _, err := bm.dbExists(ctx, baseNext)
	if err != nil {
		return err
	}
	if nextExists {
		return fmt.Errorf("'plax_base_next' exists — a previous refresh may have been interrupted. Run 'plax base reset' to clean up")
	}

	if _, err := bm.pool.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", baseNext)); err != nil {
		return fmt.Errorf("refresh: create base_next: %w", err)
	}

	nextDSN := bm.dsnForDB(baseNext)
	nextPool, err := pgxpool.New(ctx, nextDSN)
	if err != nil {
		_ = bm.dropDB(ctx, baseNext)
		return fmt.Errorf("refresh: connect to %s: %w", baseNext, err)
	}
	defer nextPool.Close()

	if err := bm.runMigrate(ctx, baseNext); err != nil {
		_ = bm.dropDB(ctx, baseNext)
		return err
	}

	schemaHash, err := ComputeSchemaHash(filepath.Join(bm.repoRoot, "src", "db", "migrations"))
	if err != nil {
		_ = bm.dropDB(ctx, baseNext)
		return err
	}

	baseDSN := bm.dsnForDB(bm.baseName)
	basePool, err := pgxpool.New(ctx, baseDSN)
	if err != nil {
		_ = bm.dropDB(ctx, baseNext)
		return fmt.Errorf("refresh: read old provenance: %w", err)
	}
	oldProv, err := ReadProvenance(ctx, basePool)
	basePool.Close()
	if err != nil {
		_ = bm.dropDB(ctx, baseNext)
		return fmt.Errorf("refresh: read old provenance: %w", err)
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
		_ = bm.dropDB(ctx, baseNext)
		return err
	}

	if err := bm.runSeed(ctx, baseNext); err != nil {
		_ = bm.dropDB(ctx, baseNext)
		return err
	}

	if err := bm.swapBase(ctx); err != nil {
		return err
	}

	return nil
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

func (bm *BaseManager) BaseStatus(ctx context.Context) (BaseInfo, error) {
	info := BaseInfo{}

	err := bm.pool.QueryRow(ctx, "SELECT 1 FROM pg_database WHERE datname = $1", bm.baseName).Scan(new(int))
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return info, nil
		}
		return info, fmt.Errorf("status: query base: %w", err)
	}
	info.Exists = true

	baseDSN := bm.dsnForDB(bm.baseName)
	basePool, err := pgxpool.New(ctx, baseDSN)
	if err == nil {
		prov, err := ReadProvenance(ctx, basePool)
		basePool.Close()
		if err == nil && prov != nil {
			info.ProvenanceVer = prov.Version
			if t, err := time.Parse(time.RFC3339Nano, prov.CreatedAt); err == nil {
				info.CreatedAt = t
			}
		}
	}

	err = bm.pool.QueryRow(ctx, "SELECT 1 FROM pg_database WHERE datname = 'plax_base_next'").Scan(new(int))
	info.HasBaseNext = err == nil

	return info, nil
}

func (bm *BaseManager) dbExists(ctx context.Context, dbName string) (exists, locked bool, err error) {
	var one int
	err = bm.pool.QueryRow(ctx, "SELECT 1 FROM pg_database WHERE datname = $1", dbName).Scan(&one)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return false, false, nil
		}
		return false, false, fmt.Errorf("db exists %s: %w", dbName, err)
	}
	return true, false, nil
}

func (bm *BaseManager) dsnForDB(dbName string) string {
	s := bm.dsn
	idx := strings.LastIndex(s, "/")
	if idx < 0 {
		return s
	}

	prefix := s[:idx+1]
	rest := s[idx+1:]

	if qi := strings.IndexByte(rest, '?'); qi >= 0 {
		return prefix + dbName + rest[qi:]
	}
	return prefix + dbName
}

func (bm *BaseManager) runCommand(ctx context.Context, cmdStr string, dbName string) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Dir = filepath.Join(bm.repoRoot, bm.bp.Seed.Workdir)

	env := cmd.Environ()
	env = append(env, fmt.Sprintf("DATABASE_URL=%s", bm.dsnForDB(dbName)))
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
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

func (bm *BaseManager) terminateConnections(ctx context.Context, dbName string) error {
	_, err := bm.pool.Exec(ctx, fmt.Sprintf(
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s' AND pid <> pg_backend_pid()",
		dbName,
	))
	return err
}

func (bm *BaseManager) swapBase(ctx context.Context) error {
	if err := bm.terminateConnections(ctx, bm.baseName); err != nil {
		return fmt.Errorf("swap: terminate connections: %w", err)
	}

	baseNext := "plax_base_next"

	backoffs := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond,
		800 * time.Millisecond, 1600 * time.Millisecond, 3200 * time.Millisecond, 6400 * time.Millisecond}

	for i, d := range backoffs {
		_, err := bm.pool.Exec(ctx, fmt.Sprintf("DROP DATABASE %s", bm.baseName))
		if err == nil {
			break
		}

		if i == len(backoffs)-1 {
			return fmt.Errorf("refresh: could not swap; plax_base_next is ready but the old base is still in use. Run 'plax base refresh' again or 'plax base status' for details")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}

		_ = bm.terminateConnections(ctx, bm.baseName)
	}

	_, err := bm.pool.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s RENAME TO %s", baseNext, bm.baseName))
	if err != nil {
		return fmt.Errorf("swap: rename: %w", err)
	}

	return nil
}

// connStringForTemplate will be re-added when Phase 3 needs it.
// It renders a Postgres connection string from a blueprint hole template.
