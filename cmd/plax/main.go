package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"github.com/apollopower/plax/pkg/blueprint"
	"github.com/apollopower/plax/pkg/derive/docker"
	"github.com/apollopower/plax/pkg/derive/env"
	"github.com/apollopower/plax/pkg/derive/postgres"
	"github.com/apollopower/plax/pkg/doctor"
	"github.com/apollopower/plax/pkg/instance"
	"github.com/apollopower/plax/pkg/mailbox"
	"github.com/apollopower/plax/pkg/portpool"
	"github.com/apollopower/plax/pkg/registry"
	"github.com/apollopower/plax/pkg/stamp"
	"github.com/apollopower/plax/pkg/status"
	"github.com/apollopower/plax/pkg/verify"
	"github.com/apollopower/plax/pkg/worktree"
)

type CLI struct {
	Init     InitCmd     `cmd:"" help:"Scaffold a blueprint by parsing the repo's docker-compose.yml and .env.example"`
	Base     BaseCmd     `cmd:"" help:"Manage the shared Postgres base database"`
	Up       UpCmd       `cmd:"" help:"Create and start an instance"`
	Down     DownCmd     `cmd:"" help:"Destroy an instance"`
	Ls       LsCmd       `cmd:"" help:"List instances"`
	Attach   AttachCmd   `cmd:"" help:"Open a shell in an instance's environment"`
	Exec     ExecCmd     `cmd:"" help:"Run a command in an instance's environment"`
	Suspend  SuspendCmd  `cmd:"" help:"Suspend an instance (stop workloads, keep state)"`
	Resume   ResumeCmd   `cmd:"" help:"Resume a suspended instance"`
	Status   StatusCmd   `cmd:"" help:"Print a six-dimension drift report for an instance"`
	Doctor   DoctorCmd   `cmd:"" help:"Validate repo, registry, machine, and base health"`
	Rederive RederiveCmd `cmd:"" help:"Regenerate .env files for all instances"`
	Verify   VerifyCmd   `cmd:"" help:"Run verification checks against an existing instance and update its health state"`
	Send     SendCmd     `cmd:"" help:"Send a message to an instance's mailbox"`
	Recv     RecvCmd     `cmd:"" help:"Read and remove messages from an instance's mailbox"`
}

type InitCmd struct {
	Root string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
}

type BaseCmd struct {
	Create  BaseCreateCmd  `cmd:"" help:"Create an empty base (migrated, no seed data)"`
	Seed    BaseSeedCmd    `cmd:"" help:"Run the seed command into the base"`
	Reset   BaseResetCmd   `cmd:"" help:"Drop and recreate the base (migrated only)"`
	Refresh BaseRefreshCmd `cmd:"" help:"Staged refresh via base_next swap"`
	Status  BaseStatusCmd  `cmd:"" help:"Print base health and provenance info"`
}

type BaseCreateCmd struct {
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (overrides blueprint env)"`
}

type BaseSeedCmd struct {
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (overrides blueprint env)"`
}

type BaseResetCmd struct {
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (overrides blueprint env)"`
}

type BaseRefreshCmd struct {
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (overrides blueprint env)"`
}

type BaseStatusCmd struct {
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (overrides blueprint env)"`
	JSON  bool   `name:"json" help:"Output as JSON"`
}

type UpCmd struct {
	Name  string `arg:"" help:"Instance name (e.g. i1)"`
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (overrides blueprint env)"`
	Ref   string `name:"ref" short:"R" optional:"" help:"Branch, PR number, tag, or commit SHA to branch from (default: current HEAD)"`
}

type DownCmd struct {
	Name  string `arg:"" help:"Instance name"`
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (overrides blueprint env)"`
}

type LsCmd struct {
	Root string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
	JSON bool   `name:"json" help:"Output as JSON"`
}

type AttachCmd struct {
	Name string `arg:"" help:"Instance name"`
	Root string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
}

type ExecCmd struct {
	Name string   `arg:"" help:"Instance name"`
	Cmd  []string `arg:"" help:"Command to run" passthrough:""`
	Root string   `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
}

type SuspendCmd struct {
	Name string `arg:"" help:"Instance name"`
	Root string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
}

type ResumeCmd struct {
	Name  string `arg:"" help:"Instance name"`
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (for drift report)"`
}

type StatusCmd struct {
	Name  string `arg:"" help:"Instance name"`
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (for Data/Schema dimensions)"`
	JSON  bool   `name:"json" help:"Output as JSON"`
}

type DoctorCmd struct {
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (for base health checks)"`
	JSON  bool   `name:"json" help:"Output as JSON"`
}

type RederiveCmd struct {
	Root string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
}

type VerifyCmd struct {
	Name  string `arg:"" help:"Instance name"`
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (overrides blueprint env)"`
	JSON  bool   `name:"json" help:"Output results as JSON array"`
}

type SendCmd struct {
	Name    string   `arg:"" help:"Instance name"`
	Root    string   `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
	From    string   `name:"from" help:"Sender (defaults to PLAX_INSTANCE env)"`
	Subject string   `name:"subject" short:"s" help:"Message subject"`
	Body    []string `arg:"" optional:"" passthrough:"" help:"Message body (use -- to separate from flags)"`
	JSON    bool     `name:"json" help:"Output as JSON"`
}

type RecvCmd struct {
	Name  string `arg:"" help:"Instance name"`
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
	All   bool   `name:"all" short:"a" xor:"mode" help:"Read and remove all messages"`
	Count int    `name:"count" short:"n" xor:"mode" help:"Number of messages to read (default 1)"`
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
	case "suspend <name>":
		ctx.FatalIfErrorf(runSuspend(cli.Suspend))
	case "resume <name>":
		ctx.FatalIfErrorf(runResume(cli.Resume))
	case "status <name>":
		ctx.FatalIfErrorf(runStatus(cli.Status))
	case "doctor":
		ctx.FatalIfErrorf(runDoctor(cli.Doctor))
	case "rederive":
		ctx.FatalIfErrorf(runRederive(cli.Rederive))
	case "verify <name>":
		ctx.FatalIfErrorf(runVerify(cli.Verify))
	case "send <name>", "send <name> <body>":
		ctx.FatalIfErrorf(runSend(cli.Send))
	case "recv <name>":
		ctx.FatalIfErrorf(runRecv(cli.Recv))
	default:
		ctx.FatalIfErrorf(fmt.Errorf("unknown command: %s", ctx.Command()))
	}
}

func runInit(cmd InitCmd) error {
	absRoot, err := filepath.Abs(cmd.Root)
	if err != nil {
		return fmt.Errorf("init: resolving root: %w", err)
	}
	bp, warnings, err := blueprint.InitFromRepo(absRoot)
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}

	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, w)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(bp); err != nil {
		return fmt.Errorf("init: encoding output: %w", err)
	}

	return nil
}

// --- base commands (Phase 2) ---

// requireSeedConfig fails fast for commands that run migrate/seed: an empty
// migrate command would otherwise "succeed" as a no-op and stamp provenance
// on a base with no schema. Lifecycle commands (up/down) never run seed
// config, so they do not require it.
func requireSeedConfig(bp *blueprint.Blueprint) error {
	if bp.Seed.Migrate == "" || bp.Seed.Command == "" || bp.Seed.Workdir == "" {
		return fmt.Errorf("plax.json: seed.migrate, seed.command, and seed.workdir are required")
	}
	return nil
}

func runBaseCreate(cmd BaseCreateCmd) error {
	root, _ := discoverRoot(cmd.Root)
	bp, connStr, err := loadBlueprintAndConnString(root, cmd.PgURL)
	if err != nil {
		return err
	}
	if err := requireSeedConfig(bp); err != nil {
		return err
	}

	ctx := context.Background()
	bm, err := postgres.NewBaseManager(ctx, connStr, root, bp)
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
	root, _ := discoverRoot(cmd.Root)
	bp, connStr, err := loadBlueprintAndConnString(root, cmd.PgURL)
	if err != nil {
		return err
	}
	if err := requireSeedConfig(bp); err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "SeedBase is not safe while instances exist; use 'plax base refresh' for ongoing updates")

	ctx := context.Background()
	bm, err := postgres.NewBaseManager(ctx, connStr, root, bp)
	if err != nil {
		return err
	}
	defer bm.Close()

	fmt.Fprintln(os.Stderr, "seeding plax_base...")
	return bm.SeedBase(ctx)
}

func runBaseReset(cmd BaseResetCmd) error {
	root, _ := discoverRoot(cmd.Root)
	bp, connStr, err := loadBlueprintAndConnString(root, cmd.PgURL)
	if err != nil {
		return err
	}
	if err := requireSeedConfig(bp); err != nil {
		return err
	}

	ctx := context.Background()
	bm, err := postgres.NewBaseManager(ctx, connStr, root, bp)
	if err != nil {
		return err
	}
	defer bm.Close()

	fmt.Fprintln(os.Stderr, "resetting plax_base...")
	fmt.Fprintln(os.Stderr, "running migrations...")
	return bm.ResetBase(ctx)
}

func runBaseRefresh(cmd BaseRefreshCmd) error {
	root, _ := discoverRoot(cmd.Root)
	bp, connStr, err := loadBlueprintAndConnString(root, cmd.PgURL)
	if err != nil {
		return err
	}
	if err := requireSeedConfig(bp); err != nil {
		return err
	}

	ctx := context.Background()
	bm, err := postgres.NewBaseManager(ctx, connStr, root, bp)
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
	root, _ := discoverRoot(cmd.Root)
	bp, connStr, err := loadBlueprintAndConnString(root, cmd.PgURL)
	if err != nil {
		return err
	}

	ctx := context.Background()
	bm, err := postgres.NewBaseManager(ctx, connStr, root, bp)
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root, _ := discoverRoot(cmd.Root)

	resolvedRef, err := worktree.ResolveRef(root, cmd.Ref)
	if err != nil {
		return err
	}

	deps, err := buildDeps(ctx, root, cmd.PgURL)
	if err != nil {
		return err
	}
	defer deps.Close()

	printStampNotice(root, deps.Blueprint, deps.Registry)

	deps.Registry.BlueprintStamp = stamp.Compute(root, deps.Blueprint)

	deps.SourceRef = cmd.Ref
	deps.ResolvedRef = resolvedRef

	return instance.Up(ctx, deps.Deps, cmd.Name)
}

func runDown(cmd DownCmd) error {
	// Down is best-effort: the registry is required, but each backend is
	// optional. A stopped Postgres or broken Docker client must not prevent
	// teardown of everything else.
	//
	// Deliberately not signal-cancellable: an interrupted down is safely
	// re-runnable because every step tolerates missing resources.
	root, _ := discoverRoot(cmd.Root)
	reg, err := openRegistry(root)
	if err != nil {
		return err
	}
	defer reg.Close()

	deps := &instance.Deps{Registry: reg, RepoRoot: root}

	bp, connStr, err := loadBlueprintAndConnString(root, cmd.PgURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v — skipping database teardown\n", err)
	} else if bm, err := postgres.NewBaseManager(context.Background(), connStr, root, bp); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v — skipping database teardown\n", err)
	} else {
		defer bm.Close()
		deps.BM = bm
	}

	if drv, err := docker.NewDriver(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v — skipping container teardown\n", err)
	} else {
		defer func() { _ = drv.Close() }()
		deps.Docker = drv
	}

	return instance.Down(context.Background(), deps, cmd.Name)
}

func runLs(cmd LsCmd) error {
	root, found := discoverRoot(cmd.Root)
	if !found {
		return fmt.Errorf("ls: %w", ErrNoRoot)
	}

	reg, err := openRegistry(root)
	if err != nil {
		return err
	}
	defer reg.Close()

	bp, _ := loadBlueprint(root)
	printStampNotice(root, bp, reg)

	if cmd.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(reg.Instances)
	}

	if len(reg.Instances) == 0 {
		fmt.Printf("%-8s %-10s %-20s %-5s %-24s %-10s %s\n", "NAME", "STATE", "BRANCH", "MAIL", "PORTS", "HEALTH", "CREATED")
		return nil
	}

	fmt.Printf("%-8s %-10s %-20s %-5s %-24s %-10s %s\n", "NAME", "STATE", "BRANCH", "MAIL", "PORTS", "HEALTH", "CREATED")

	names := make([]string, 0, len(reg.Instances))
	for name := range reg.Instances {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		rec := reg.Instances[name]
		ports := formatPorts(rec.Ports)
		age := formatAge(rec.CreatedAt)
		mailCount, mailErr := mailbox.Count(root, name)
		mailStr := fmt.Sprintf("%d", mailCount)
		if mailErr != nil {
			mailStr = "?"
		}
		healthStr := formatHealth(rec.Health)
		fmt.Printf("%-8s %-10s %-20s %-5s %-24s %-10s %s\n", name, rec.State, rec.Branch, mailStr, ports, healthStr, age)
	}

	return nil
}

func runAttach(cmd AttachCmd) error {
	root, found := discoverRoot(cmd.Root)
	if !found {
		return fmt.Errorf("attach: %w", ErrNoRoot)
	}

	reg, err := openRegistry(root)
	if err != nil {
		return err
	}
	defer reg.Close()

	rec, found := reg.GetInstance(cmd.Name)
	if !found {
		return fmt.Errorf("instance %q not found", cmd.Name)
	}

	bp, _ := loadBlueprint(root)
	printStampNotice(root, bp, reg)

	if rec.State == registry.StateSuspended {
		fmt.Fprintf(os.Stderr, "note: instance %s is suspended — services and processes are stopped\n", cmd.Name)
	}

	if n, err := mailbox.Count(root, cmd.Name); err == nil && n > 0 {
		fmt.Fprintf(os.Stderr, "note: %d unread message(s) — run 'plax recv %s' to read\n", n, cmd.Name)
	}

	if bp != nil {
		currentStamp := stamp.Compute(root, bp)
		sdeps := &status.Deps{
			Blueprint:    bp,
			Registry:     reg,
			RepoRoot:     root,
			CurrentStamp: currentStamp,
		}
		if report, err := status.Build(context.Background(), sdeps, cmd.Name); err == nil {
			var drifted []string
			for _, d := range []struct {
				n   string
				dim status.Dimension
			}{
				{"code", report.Code}, {"host", report.Host}, {"config", report.Config},
			} {
				if d.dim.Level == status.Drift {
					drifted = append(drifted, d.n)
				}
			}
			if len(drifted) > 0 {
				fmt.Fprintf(os.Stderr, "note: drift detected (%s) — run 'plax status %s'\n",
					strings.Join(drifted, ", "), cmd.Name)
			}
		}
	}

	envVars, err := env.LoadInstanceEnv(rec.WorktreePath, rec.Ports)
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
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return fmt.Errorf("exec: no command given — usage: plax exec <name> -- <cmd> [args...]")
	}

	root, found := discoverRoot(cmd.Root)
	if !found {
		return fmt.Errorf("exec: %w", ErrNoRoot)
	}

	reg, err := openRegistry(root)
	if err != nil {
		return err
	}
	defer reg.Close()

	rec, found := reg.GetInstance(cmd.Name)
	if !found {
		return fmt.Errorf("instance %q not found", cmd.Name)
	}

	bp, _ := loadBlueprint(root)
	printStampNotice(root, bp, reg)

	if rec.State == registry.StateSuspended {
		fmt.Fprintf(os.Stderr, "note: instance %s is suspended — services and processes are stopped\n", cmd.Name)
	}

	envVars, err := env.LoadInstanceEnv(rec.WorktreePath, rec.Ports)
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

// cliDeps wraps instance.Deps with the concrete backends the CLI opened,
// so they can be closed after the command finishes.
type cliDeps struct {
	*instance.Deps
	bm     *postgres.BaseManager
	docker *docker.Driver
}

func (d *cliDeps) Close() {
	if d.Registry != nil {
		d.Registry.Close()
	}
	if d.Pool != nil {
		d.Pool.Close()
	}
	if d.bm != nil {
		d.bm.Close()
	}
	if d.docker != nil {
		_ = d.docker.Close()
	}
}

// buildDeps assembles all dependencies needed by Up. Down does not use it:
// teardown builds its own tolerant, partial dependencies.
func buildDeps(ctx context.Context, root, pgURL string) (*cliDeps, error) {
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
		reg.Close()
		return nil, fmt.Errorf("resolving repo root: %w", err)
	}

	bm, err := postgres.NewBaseManager(ctx, connStr, absRoot, bp)
	if err != nil {
		reg.Close()
		return nil, err
	}

	drv, err := docker.NewDriver()
	if err != nil {
		bm.Close()
		reg.Close()
		return nil, err
	}

	pool, err := portpool.New(bp.PortPool.Start, bp.PortPool.End, reg)
	if err != nil {
		bm.Close()
		_ = drv.Close()
		reg.Close()
		return nil, fmt.Errorf("portpool: %w", err)
	}

	deps, err := instance.NewUpDeps(bp, reg, pool, bm, drv, absRoot)
	if err != nil {
		pool.Close()
		bm.Close()
		_ = drv.Close()
		reg.Close()
		return nil, err
	}

	return &cliDeps{
		Deps:   deps,
		bm:     bm,
		docker: drv,
	}, nil
}

var ErrNoRoot = errors.New("no plax repo root found: run from a directory containing plax.json, or pass --root")

// discoverRoot walks up from start looking for plax.json.
// If found, returns the directory containing it and true.
// If not found, returns start and false.
func discoverRoot(start string) (string, bool) {
	start, err := filepath.Abs(start)
	if err != nil {
		return start, false
	}
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "plax.json")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return start, false
		}
		dir = parent
	}
}

func openRegistry(root string) (*registry.Registry, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolving repo root: %w", err)
	}
	return registry.Open(filepath.Join(absRoot, ".plax", "registry.json"))
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

func formatHealth(h registry.Health) string {
	switch h {
	case registry.HealthHealthy:
		return "healthy"
	case registry.HealthUnhealthy:
		return "unhealthy"
	default:
		return "—"
	}
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

	if pgURL != "" {
		return &bp, pgURL, nil
	}

	connStr, err := postgres.ConnString(&bp)
	if err != nil {
		return nil, "", err
	}
	return &bp, connStr, nil
}

func loadBlueprint(root string) (*blueprint.Blueprint, error) {
	plaxPath := filepath.Join(root, "plax.json")
	data, err := os.ReadFile(plaxPath)
	if err != nil {
		return nil, err
	}
	var bp blueprint.Blueprint
	if err := json.Unmarshal(data, &bp); err != nil {
		return nil, err
	}
	return &bp, nil
}

func printStampNotice(root string, bp *blueprint.Blueprint, reg *registry.Registry) {
	if bp == nil {
		return
	}
	current := stamp.Compute(root, bp)
	msg, changed := stamp.Check(current, reg.BlueprintStamp)
	if changed {
		fmt.Fprintln(os.Stderr, msg)
	}
}

func runSuspend(cmd SuspendCmd) error {
	root, _ := discoverRoot(cmd.Root)

	reg, err := openRegistry(root)
	if err != nil {
		return err
	}
	defer reg.Close()

	bp, _ := loadBlueprint(root)
	printStampNotice(root, bp, reg)

	deps := &instance.Deps{Registry: reg, RepoRoot: root}

	if drv, err := docker.NewDriver(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	} else {
		defer func() { _ = drv.Close() }()
		deps.Docker = drv
	}

	return instance.Suspend(context.Background(), deps, cmd.Name)
}

func runResume(cmd ResumeCmd) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root, _ := discoverRoot(cmd.Root)

	bp, connStr, err := loadBlueprintAndConnString(root, cmd.PgURL)
	if err != nil {
		return err
	}

	reg, err := openRegistry(root)
	if err != nil {
		return err
	}
	defer reg.Close()

	printStampNotice(root, bp, reg)

	deps := &instance.Deps{Blueprint: bp, Registry: reg, RepoRoot: root}

	// Open BM before Resume — tolerantly; nil skips DB checks.
	if connStr != "" {
		if bm, err := postgres.NewBaseManager(ctx, connStr, root, bp); err == nil {
			defer bm.Close()
			deps.BM = bm
		} else {
			fmt.Fprintf(os.Stderr, "note: postgres unreachable — DB checks skipped: %v\n", err)
		}
	}

	if drv, err := docker.NewDriver(); err != nil {
		if rec, found := reg.GetInstance(cmd.Name); found && len(rec.ContainerIDs) > 0 {
			return fmt.Errorf("docker unavailable — cannot start %d container(s); fix Docker and retry", len(rec.ContainerIDs))
		}
	} else {
		defer func() { _ = drv.Close() }()
		deps.Docker = drv
	}

	if err := instance.Resume(ctx, deps, cmd.Name); err != nil {
		return err
	}

	currentStamp := stamp.Compute(root, bp)
	sdeps := &status.Deps{
		Blueprint:    bp,
		Registry:     reg,
		BM:           deps.BM,
		RepoRoot:     root,
		CurrentStamp: currentStamp,
	}
	report, err := status.Build(ctx, sdeps, cmd.Name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "note: drift report unavailable: %v\n", err)
	} else {
		printReportStderr(report)
	}

	fmt.Fprintf(os.Stderr, "instance %s resumed\n", cmd.Name)
	return nil
}

func runStatus(cmd StatusCmd) error {
	root, found := discoverRoot(cmd.Root)
	if !found {
		return fmt.Errorf("status: %w", ErrNoRoot)
	}

	reg, err := openRegistry(root)
	if err != nil {
		return err
	}
	defer reg.Close()

	if _, found := reg.GetInstance(cmd.Name); !found {
		return fmt.Errorf("instance %q not found", cmd.Name)
	}

	bp, connStr, err := loadBlueprintAndConnString(root, cmd.PgURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v — showing degraded status\n", err)
		bp = &blueprint.Blueprint{}
		connStr = ""
	}

	currentStamp := stamp.Compute(root, bp)
	sdeps := &status.Deps{
		Blueprint:    bp,
		Registry:     reg,
		RepoRoot:     root,
		CurrentStamp: currentStamp,
	}

	if connStr != "" {
		bm, err := postgres.NewBaseManager(context.Background(), connStr, root, bp)
		if err == nil {
			defer bm.Close()
			sdeps.BM = bm
		}
	}

	report, err := status.Build(context.Background(), sdeps, cmd.Name)
	if err != nil {
		return err
	}

	if cmd.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	printReportTable(os.Stdout, report)
	return nil
}

func runDoctor(cmd DoctorCmd) error {
	root, found := discoverRoot(cmd.Root)
	if !found {
		return fmt.Errorf("doctor: %w", ErrNoRoot)
	}

	bp, connStr, err := loadBlueprintAndConnString(root, cmd.PgURL)
	if err != nil {
		return err
	}

	reg, err := openRegistry(root)
	if err != nil {
		return err
	}
	defer reg.Close()

	ddeps := &doctor.Deps{
		Blueprint: bp,
		Registry:  reg,
		RepoRoot:  root,
	}

	pgURL := connStr
	if pgURL != "" {
		bm, err := postgres.NewBaseManager(context.Background(), pgURL, root, bp)
		if err == nil {
			defer bm.Close()
			ddeps.BM = bm
		}
	}

	if drv, err := docker.NewDriver(); err == nil {
		defer func() { _ = drv.Close() }()
		ddeps.Docker = drv
	}

	report := doctor.Run(context.Background(), ddeps)

	if cmd.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
		if report.Failed() {
			os.Exit(1)
		}
		return nil
	}

	area := ""
	for _, c := range report.Checks {
		if c.Area != area {
			if area != "" {
				fmt.Println()
			}
			fmt.Printf("%s:\n", c.Area)
			area = c.Area
		}
		fmt.Printf("  [%s] %s\n", c.Level, c.Message)
	}

	if report.Failed() {
		os.Exit(1)
	}
	return nil
}

func runRederive(cmd RederiveCmd) error {
	root, _ := discoverRoot(cmd.Root)

	bp, err := loadBlueprint(root)
	if err != nil {
		return fmt.Errorf("plax.json not found at %s — run 'plax init' first", filepath.Join(root, "plax.json"))
	}

	reg, err := openRegistry(root)
	if err != nil {
		return err
	}
	defer reg.Close()

	deps := &instance.Deps{Blueprint: bp, Registry: reg, RepoRoot: root}
	return instance.Rederive(deps)
}

func runVerify(cmd VerifyCmd) error {
	root, _ := discoverRoot(cmd.Root)

	bp, connStr, err := loadBlueprintAndConnString(root, cmd.PgURL)
	if err != nil {
		return err
	}

	reg, err := openRegistry(root)
	if err != nil {
		return err
	}
	defer reg.Close()

	rec, found := reg.GetInstance(cmd.Name)
	if !found {
		return fmt.Errorf("instance %q not found", cmd.Name)
	}

	ctx := context.Background()

	vDeps := &verify.Deps{
		Blueprint: bp,
		Registry:  reg,
		RepoRoot:  root,
	}

	if connStr != "" {
		bm, err := postgres.NewBaseManager(ctx, connStr, root, bp)
		if err != nil {
			fmt.Fprintf(os.Stderr, "note: postgres unreachable — DB checks skipped: %v\n", err)
		} else {
			defer bm.Close()
			vDeps.BM = bm
		}
	}

	if rec.State == registry.StateSuspended {
		fmt.Fprintf(os.Stderr, "note: %s is suspended — runtime checks (tcp-reachability, process-liveness) skipped\n", cmd.Name)
	}

	results, err := verify.RunVerify(ctx, vDeps, cmd.Name)

	if cmd.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(results); encErr != nil {
			return encErr
		}
	} else {
		fmt.Printf("%s:\n", cmd.Name)
		passCount := 0
		failCount := 0
		for _, r := range results {
			status := "pass"
			if !r.Passed {
				status = "fail"
				failCount++
			} else {
				passCount++
			}
			detail := fmt.Sprintf("  [%s] %s", status, r.Check)
			if r.Detail != "" {
				detail += ": " + r.Detail
			}
			fmt.Println(detail)
		}
		if failCount > 0 {
			fmt.Fprintf(os.Stderr, "%d check(s) failed\n", failCount)
		} else if passCount > 0 {
			fmt.Fprintf(os.Stderr, "  all %d check(s) passed\n", passCount)
		}
	}

	if err != nil {
		var vErr *verify.VerificationError
		if errors.As(err, &vErr) {
			return vErr
		}
		return err
	}
	return nil
}

func runSend(cmd SendCmd) error {
	root, found := discoverRoot(cmd.Root)
	if !found {
		return fmt.Errorf("send: %w", ErrNoRoot)
	}

	reg, err := openRegistry(root)
	if err != nil {
		return err
	}
	defer reg.Close()

	if _, found := reg.GetInstance(cmd.Name); !found {
		return fmt.Errorf("instance %q not found", cmd.Name)
	}

	parts := cmd.Body
	for len(parts) > 0 && parts[0] == "--" {
		parts = parts[1:]
	}
	body := strings.Join(parts, " ")
	if body == "" {
		return fmt.Errorf("send: body is required")
	}

	from := cmd.From
	if from == "" {
		from = os.Getenv("PLAX_INSTANCE")
	}
	if from == "" {
		fmt.Fprintf(os.Stderr, "send: no sender set — pass --from or set PLAX_INSTANCE\n")
	}

	msg := mailbox.Message{
		From:    from,
		Subject: cmd.Subject,
		Body:    body,
	}

	filename, err := mailbox.Send(root, cmd.Name, msg)
	if err != nil {
		return err
	}

	if cmd.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]string{"status": "sent", "instance": cmd.Name, "file": filename})
	}

	fmt.Println(filename)
	return nil
}

func runRecv(cmd RecvCmd) error {
	root, found := discoverRoot(cmd.Root)
	if !found {
		return fmt.Errorf("recv: %w", ErrNoRoot)
	}

	reg, err := openRegistry(root)
	if err != nil {
		return err
	}
	defer reg.Close()

	if _, found := reg.GetInstance(cmd.Name); !found {
		return fmt.Errorf("instance %q not found", cmd.Name)
	}

	n := cmd.Count
	if n < 0 {
		return fmt.Errorf("recv: --count must be positive")
	}
	if !cmd.All && n == 0 {
		n = 1
	}

	var result *mailbox.RecvResult
	if cmd.All {
		result, err = mailbox.RecvAll(root, cmd.Name)
	} else {
		result, err = mailbox.Recv(root, cmd.Name, n)
	}
	if err != nil {
		return err
	}
	msgs := result.Messages

	for _, name := range result.Skipped {
		fmt.Fprintf(os.Stderr, "warning: skipped unreadable message: %s\n", name)
	}

	if cmd.JSON {
		if msgs == nil {
			msgs = []mailbox.Message{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(msgs)
	}

	if len(msgs) == 0 {
		fmt.Fprintln(os.Stderr, "no messages")
		return nil
	}

	for i, msg := range msgs {
		if i > 0 {
			fmt.Println()
		}
		if msg.From != "" {
			fmt.Printf("From: %s\n", msg.From)
		}
		if msg.Subject != "" {
			fmt.Printf("Subject: %s\n", msg.Subject)
		}
		fmt.Printf("---\n%s\n---\n", msg.Body)
	}

	remaining, _ := mailbox.Count(root, cmd.Name)
	if remaining > 0 {
		fmt.Fprintf(os.Stderr, "%d message(s) remaining\n", remaining)
	}

	return nil
}

func printReportStderr(r *status.Report) {
	for _, d := range []struct {
		n   string
		dim status.Dimension
	}{
		{"code", r.Code}, {"schema", r.Schema}, {"data", r.Data},
		{"host", r.Host}, {"config", r.Config}, {"health", r.Health},
	} {
		if d.dim.Level != status.OK && d.dim.Level != status.Unknown {
			fmt.Fprintf(os.Stderr, "  %s: [%s] %s\n", d.n, d.dim.Level, d.dim.Detail)
		}
	}
}

func printReportTable(w *os.File, r *status.Report) {
	_, _ = fmt.Fprintf(w, "%-8s %-12s %-10s %s\n", "NAME", "DIMENSION", "STATUS", "DETAIL")
	for _, d := range []struct {
		n   string
		dim status.Dimension
	}{
		{"code", r.Code}, {"schema", r.Schema}, {"data", r.Data},
		{"host", r.Host}, {"config", r.Config}, {"health", r.Health},
	} {
		_, _ = fmt.Fprintf(w, "%-8s %-12s %-10s %s\n", r.Instance, d.n, d.dim.Level, d.dim.Detail)
	}
}

// deriveConnString removed — call postgres.ConnString directly
