package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alecthomas/kong"
	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/derive/postgres"
)

type CLI struct {
	Init InitCmd `cmd:"" help:"Scaffold a blueprint by parsing the repo's docker-compose.yml and .env.example"`
	Base BaseCmd `cmd:"" help:"Manage the shared Postgres base database"`
}

type InitCmd struct {
	Root string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
}

type BaseCmd struct {
	Create  BaseCreateCmd  `cmd:"" help:"Create an empty base (migrated, no seed data)"`
	Seed    BaseSeedCmd    `cmd:"" help:"Run the seed command into the base"`
	Reset   BaseResetCmd   `cmd:"" help:"Drop and recreate the base (migrated only)"`
	Refresh BaseRefreshCmd `cmd:"" help:"Staged refresh via base_next swap"`
	Status  BaseStatusCmd  `cmd:"" help:"Print base health and provenance info"`
}

type BaseCreateCmd struct {
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (overrides blueprint env)"`
}

type BaseSeedCmd struct {
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (overrides blueprint env)"`
}

type BaseResetCmd struct {
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (overrides blueprint env)"`
}

type BaseRefreshCmd struct {
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (overrides blueprint env)"`
}

type BaseStatusCmd struct {
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (overrides blueprint env)"`
	JSON  bool   `name:"json" help:"Output as JSON"`
}

func main() {
	var cli CLI
	ctx := kong.Parse(&cli,
		kong.Name("plax"),
		kong.Description("Run many parallel dev environments for coding agents."),
		kong.UsageOnError(),
	)

	switch ctx.Command() {
	case "init":
		ctx.FatalIfErrorf(runInit(cli.Init))
	case "base create":
		ctx.FatalIfErrorf(runBaseCreate(cli.Base.Create))
	case "base seed":
		ctx.FatalIfErrorf(runBaseSeed(cli.Base.Seed))
	case "base reset":
		ctx.FatalIfErrorf(runBaseReset(cli.Base.Reset))
	case "base refresh":
		ctx.FatalIfErrorf(runBaseRefresh(cli.Base.Refresh))
	case "base status":
		ctx.FatalIfErrorf(runBaseStatus(cli.Base.Status))
	}
}

func runInit(cmd InitCmd) error {
	bp, err := blueprint.InitFromRepo(cmd.Root)
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(bp); err != nil {
		return fmt.Errorf("init: encoding output: %w", err)
	}

	return nil
}

func runBaseCreate(cmd BaseCreateCmd) error {
	bp, connStr, err := loadBlueprintAndConnString(cmd.Root, cmd.PgURL)
	if err != nil {
		return err
	}

	ctx := context.Background()
	bm, err := postgres.NewBaseManager(ctx, connStr, cmd.Root, bp)
	if err != nil {
		return err
	}
	defer bm.Close()

	fmt.Fprintln(os.Stderr, "creating plax_base...")
	fmt.Fprintln(os.Stderr, "running migrations...")
	fmt.Fprintln(os.Stderr, "locking...")
	return bm.CreateBase(ctx)
}

func runBaseSeed(cmd BaseSeedCmd) error {
	bp, connStr, err := loadBlueprintAndConnString(cmd.Root, cmd.PgURL)
	if err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "SeedBase is not safe while instances exist; use 'plax base refresh' for ongoing updates")

	ctx := context.Background()
	bm, err := postgres.NewBaseManager(ctx, connStr, cmd.Root, bp)
	if err != nil {
		return err
	}
	defer bm.Close()

	fmt.Fprintln(os.Stderr, "seeding plax_base...")
	return bm.SeedBase(ctx)
}

func runBaseReset(cmd BaseResetCmd) error {
	bp, connStr, err := loadBlueprintAndConnString(cmd.Root, cmd.PgURL)
	if err != nil {
		return err
	}

	ctx := context.Background()
	bm, err := postgres.NewBaseManager(ctx, connStr, cmd.Root, bp)
	if err != nil {
		return err
	}
	defer bm.Close()

	fmt.Fprintln(os.Stderr, "resetting plax_base...")
	fmt.Fprintln(os.Stderr, "running migrations...")
	return bm.ResetBase(ctx)
}

func runBaseRefresh(cmd BaseRefreshCmd) error {
	bp, connStr, err := loadBlueprintAndConnString(cmd.Root, cmd.PgURL)
	if err != nil {
		return err
	}

	ctx := context.Background()
	bm, err := postgres.NewBaseManager(ctx, connStr, cmd.Root, bp)
	if err != nil {
		return err
	}
	defer bm.Close()

	fmt.Fprintln(os.Stderr, "refreshing plax_base...")
	if err := bm.RefreshBase(ctx); err != nil {
		if errors.Is(err, postgres.ErrDeferredSwap) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return err
	}

	return nil
}

func runBaseStatus(cmd BaseStatusCmd) error {
	bp, connStr, err := loadBlueprintAndConnString(cmd.Root, cmd.PgURL)
	if err != nil {
		return err
	}

	ctx := context.Background()
	bm, err := postgres.NewBaseManager(ctx, connStr, cmd.Root, bp)
	if err != nil {
		return err
	}
	defer bm.Close()

	info, err := bm.BaseStatus(ctx)
	if err != nil {
		return err
	}

	if cmd.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}

	locked := "no"
	if info.Locked {
		locked = "yes"
	}
	provenance := "-"
	if info.Exists && info.ProvenanceVer > 0 {
		provenance = fmt.Sprintf("v%d (seeded %s)", info.ProvenanceVer, info.CreatedAt.Format("2006-01-02 15:04:05"))
	}
	exists := "no"
	if info.Exists {
		exists = "yes"
	}
	baseNext := "no"
	if info.HasBaseNext {
		baseNext = "yes"
	}

	fmt.Println("plax_base:")
	fmt.Printf("  Exists:          %s\n", exists)
	fmt.Printf("  Locked:          %s\n", locked)
	fmt.Printf("  Provenance:      %s\n", provenance)
	fmt.Printf("  Base next:       %s\n", baseNext)

	return nil
}

func loadBlueprintAndConnString(root, pgURL string) (*blueprint.Blueprint, string, error) {
	plaxPath := filepath.Join(root, "plax.json")
	data, err := os.ReadFile(plaxPath)
	if err != nil {
		return nil, "", fmt.Errorf("plax.json not found at %s — run 'plax init' first", plaxPath)
	}

	var bp blueprint.Blueprint
	if err := json.Unmarshal(data, &bp); err != nil {
		return nil, "", fmt.Errorf("parsing plax.json: %w", err)
	}

	// Base commands die here rather than midway: an empty migrate command
	// would otherwise "succeed" as a no-op and stamp provenance on a base
	// with no schema.
	if bp.Seed.Migrate == "" || bp.Seed.Command == "" || bp.Seed.Workdir == "" {
		return nil, "", fmt.Errorf("plax.json: seed.migrate, seed.command, and seed.workdir are required")
	}

	if pgURL != "" {
		return &bp, pgURL, nil
	}

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
		return nil, "", fmt.Errorf("no logical postgres service in blueprint")
	}

	connStr := fmt.Sprintf("postgres://%s:%s@localhost:5432/postgres?sslmode=disable", user, password)
	return &bp, connStr, nil
}
