package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/derive/docker"
	"github.com/apollopower/plax/pkg/derive/env"
	"github.com/apollopower/plax/pkg/derive/postgres"
	"github.com/apollopower/plax/pkg/instance"
	"github.com/apollopower/plax/pkg/portpool"
	"github.com/apollopower/plax/pkg/registry"
)

type CLI struct {
	Init   InitCmd   `cmd:"" help:"Scaffold a blueprint by parsing the repo's docker-compose.yml and .env.example"`
	Base   BaseCmd   `cmd:"" help:"Manage the shared Postgres base database"`
	Up     UpCmd     `cmd:"" help:"Create and start an instance"`
	Down   DownCmd   `cmd:"" help:"Destroy an instance"`
	Ls     LsCmd     `cmd:"" help:"List instances"`
	Attach AttachCmd `cmd:"" help:"Open a shell in an instance's environment"`
	Exec   ExecCmd   `cmd:"" help:"Run a command in an instance's environment"`
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

type UpCmd struct {
	Name  string `arg:"" help:"Instance name (e.g. i1)"`
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (overrides blueprint env)"`
}

type DownCmd struct {
	Name  string `arg:"" help:"Instance name"`
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (overrides blueprint env)"`
}

type LsCmd struct {
	Root string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
	JSON bool   `name:"json" help:"Output as JSON"`
}

type AttachCmd struct {
	Name string `arg:"" help:"Instance name"`
	Root string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
}

type ExecCmd struct {
	Name string   `arg:"" help:"Instance name"`
	Cmd  []string `arg:"" help:"Command to run" passthrough:""`
	Root string   `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
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
	case "up <name>":
		ctx.FatalIfErrorf(runUp(cli.Up))
	case "down <name>":
		ctx.FatalIfErrorf(runDown(cli.Down))
	case "ls":
		ctx.FatalIfErrorf(runLs(cli.Ls))
	case "attach <name>":
		ctx.FatalIfErrorf(runAttach(cli.Attach))
	case "exec <name> <cmd>":
		ctx.FatalIfErrorf(runExec(cli.Exec))
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

// --- base commands (Phase 2) ---

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

// --- lifecycle commands (Phase 3) ---

func runUp(cmd UpCmd) error {
	deps, err := buildDeps(cmd.Root, cmd.PgURL)
	if err != nil {
		return err
	}
	defer deps.BM.Close()
	defer func() { _ = deps.Docker.Close() }()

	return instance.Up(context.Background(), deps, cmd.Name)
}

func runDown(cmd DownCmd) error {
	deps, err := buildDeps(cmd.Root, cmd.PgURL)
	if err != nil {
		return err
	}
	defer deps.BM.Close()
	defer func() { _ = deps.Docker.Close() }()

	return instance.Down(context.Background(), deps, cmd.Name)
}

func runLs(cmd LsCmd) error {
	reg, err := openRegistry(cmd.Root)
	if err != nil {
		return err
	}

	if cmd.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(reg.Instances)
	}

	if len(reg.Instances) == 0 {
		fmt.Printf("%-8s %-10s %-20s %-24s %s\n", "NAME", "STATE", "BRANCH", "PORTS", "CREATED")
		return nil
	}

	fmt.Printf("%-8s %-10s %-20s %-24s %s\n", "NAME", "STATE", "BRANCH", "PORTS", "CREATED")

	names := make([]string, 0, len(reg.Instances))
	for name := range reg.Instances {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		rec := reg.Instances[name]
		ports := formatPorts(rec.Ports)
		age := formatAge(rec.CreatedAt)
		fmt.Printf("%-8s %-10s %-20s %-24s %s\n", name, rec.State, rec.Branch, ports, age)
	}

	return nil
}

func runAttach(cmd AttachCmd) error {
	reg, err := openRegistry(cmd.Root)
	if err != nil {
		return err
	}

	rec, found := reg.GetInstance(cmd.Name)
	if !found {
		return fmt.Errorf("instance %q not found", cmd.Name)
	}

	envVars, err := loadInstanceEnv(rec.WorktreePath)
	if err != nil {
		return err
	}

	shell := findShell()
	if shell == "" {
		return fmt.Errorf("attach: no shell found (checked $SHELL, /bin/bash, /bin/sh)")
	}

	c := exec.Command(shell, "--login")
	c.Dir = rec.WorktreePath
	c.Env = envVars
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr

	return c.Run()
}

func runExec(cmd ExecCmd) error {
	args := cmd.Cmd
	// kong's passthrough keeps the "--" token in the args; strip it.
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return fmt.Errorf("exec: no command given — usage: plax exec <name> -- <cmd> [args...]")
	}

	reg, err := openRegistry(cmd.Root)
	if err != nil {
		return err
	}

	rec, found := reg.GetInstance(cmd.Name)
	if !found {
		return fmt.Errorf("instance %q not found", cmd.Name)
	}

	envVars, err := loadInstanceEnv(rec.WorktreePath)
	if err != nil {
		return err
	}

	c := exec.Command(args[0], args[1:]...)
	c.Dir = rec.WorktreePath
	c.Env = envVars
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr

	return c.Run()
}

// --- helpers ---

// buildDeps assembles all dependencies needed by Up and Down.
func buildDeps(root, pgURL string) (*instance.Deps, error) {
	bp, connStr, err := loadBlueprintAndConnString(root, pgURL)
	if err != nil {
		return nil, err
	}

	reg, err := openRegistry(root)
	if err != nil {
		return nil, err
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolving repo root: %w", err)
	}

	ctx := context.Background()
	bm, err := postgres.NewBaseManager(ctx, connStr, absRoot, bp)
	if err != nil {
		return nil, err
	}

	drv, err := docker.NewDriver()
	if err != nil {
		bm.Close()
		return nil, err
	}

	pool := portpool.New(bp.PortPool.Start, bp.PortPool.End, reg)

	return &instance.Deps{
		Blueprint: bp,
		Registry:  reg,
		Pool:      pool,
		BM:        bm,
		Docker:    drv,
		RepoRoot:  absRoot,
	}, nil
}

func openRegistry(root string) (*registry.Registry, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolving repo root: %w", err)
	}
	return registry.Open(filepath.Join(absRoot, ".plax", "registry.json"))
}

// loadInstanceEnv reads the derived .env from the worktree and merges it
// over the host environment.
func loadInstanceEnv(worktreePath string) ([]string, error) {
	envPath := filepath.Join(worktreePath, ".env")
	derived, err := env.ParseFile(envPath)
	if err != nil {
		return nil, fmt.Errorf("env: .env not found at %s — was the instance created with 'plax up'?", envPath)
	}

	envMap := map[string]string{}
	for _, e := range os.Environ() {
		k, v, _ := strings.Cut(e, "=")
		envMap[k] = v
	}
	for k, v := range derived {
		envMap[k] = v
	}

	result := make([]string, 0, len(envMap))
	for k, v := range envMap {
		result = append(result, k+"="+v)
	}
	return result, nil
}

func findShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	for _, s := range []string{"/bin/bash", "/bin/sh"} {
		if _, err := os.Stat(s); err == nil {
			return s
		}
	}
	return ""
}

func formatPorts(ports map[string]int) string {
	if len(ports) == 0 {
		return "-"
	}
	var nums []int
	for _, p := range ports {
		nums = append(nums, p)
	}
	sort.Ints(nums)
	ss := make([]string, len(nums))
	for i, n := range nums {
		ss[i] = fmt.Sprintf("%d", n)
	}
	return strings.Join(ss, " ")
}

func formatAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
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
