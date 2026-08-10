package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
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
	"github.com/apollopower/plax/pkg/status"
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
	Status   StatusCmd   `cmd:"" help:"Print a five-dimension drift report for an instance"`
	Doctor   DoctorCmd   `cmd:"" help:"Validate repo, registry, machine, and base health"`
	Rederive RederiveCmd `cmd:"" help:"Regenerate .env files for all instances"`
	Send     SendCmd     `cmd:"" help:"Send a message to an instance's mailbox"`
	Recv     RecvCmd     `cmd:"" help:"Read and remove messages from an instance's mailbox"`
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

type SuspendCmd struct {
	Name string `arg:"" help:"Instance name"`
	Root string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
}

type ResumeCmd struct {
	Name  string `arg:"" help:"Instance name"`
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (for drift report)"`
}

type StatusCmd struct {
	Name  string `arg:"" help:"Instance name"`
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (for Data/Schema dimensions)"`
	JSON  bool   `name:"json" help:"Output as JSON"`
}

type DoctorCmd struct {
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
	PgURL string `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (for base health checks)"`
	JSON  bool   `name:"json" help:"Output as JSON"`
}

type RederiveCmd struct {
	Root string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
}

type SendCmd struct {
	Name    string   `arg:"" help:"Instance name"`
	Root    string   `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
	From    string   `name:"from" help:"Sender (defaults to PLAX_INSTANCE env)"`
	Subject string   `name:"subject" short:"s" help:"Message subject"`
	Body    []string `arg:"" optional:"" passthrough:"" help:"Message body (use -- to separate from flags)"`
	JSON    bool     `name:"json" help:"Output as JSON"`
}

type RecvCmd struct {
	Name  string `arg:"" help:"Instance name"`
	Root  string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory"`
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
	case "send <name>", "send <name> <body>":
		ctx.FatalIfErrorf(runSend(cli.Send))
	case "recv <name>":
		ctx.FatalIfErrorf(runRecv(cli.Recv))
	}
}

func runInit(cmd InitCmd) error {
	absRoot, err := filepath.Abs(cmd.Root)
	if err != nil {
		return fmt.Errorf("init: resolving root: %w", err)
	}
	bp, err := blueprint.InitFromRepo(absRoot)
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
	bp, connStr, err := loadBlueprintAndConnString(cmd.Root, cmd.PgURL)
	if err != nil {
		return err
	}
	if err := requireSeedConfig(bp); err != nil {
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
	if err := requireSeedConfig(bp); err != nil {
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
	if err := requireSeedConfig(bp); err != nil {
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
	if err := requireSeedConfig(bp); err != nil {
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	deps, err := buildDeps(ctx, cmd.Root, cmd.PgURL)
	if err != nil {
		return err
	}
	defer deps.Close()

	printStampNotice(cmd.Root, deps.Blueprint, deps.Registry)

	deps.Registry.BlueprintStamp = computeStamp(cmd.Root, deps.Blueprint)

	return instance.Up(ctx, deps.Deps, cmd.Name)
}

func runDown(cmd DownCmd) error {
	// Down is best-effort: the registry is required, but each backend is
	// optional. A stopped Postgres or broken Docker client must not prevent
	// teardown of everything else.
	//
	// Deliberately not signal-cancellable: an interrupted down is safely
	// re-runnable because every step tolerates missing resources.
	reg, err := openRegistry(cmd.Root)
	if err != nil {
		return err
	}
	absRoot, err := filepath.Abs(cmd.Root)
	if err != nil {
		return fmt.Errorf("resolving repo root: %w", err)
	}

	deps := &instance.Deps{Registry: reg, RepoRoot: absRoot}

	bp, connStr, err := loadBlueprintAndConnString(cmd.Root, cmd.PgURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v — skipping database teardown\n", err)
	} else if bm, err := postgres.NewBaseManager(context.Background(), connStr, absRoot, bp); err != nil {
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
	reg, err := openRegistry(cmd.Root)
	if err != nil {
		return err
	}

	bp, _ := loadBlueprint(cmd.Root)
	printStampNotice(cmd.Root, bp, reg)

	if cmd.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(reg.Instances)
	}

	if len(reg.Instances) == 0 {
		fmt.Printf("%-8s %-10s %-20s %-5s %-24s %s\n", "NAME", "STATE", "BRANCH", "MAIL", "PORTS", "CREATED")
		return nil
	}

	fmt.Printf("%-8s %-10s %-20s %-5s %-24s %s\n", "NAME", "STATE", "BRANCH", "MAIL", "PORTS", "CREATED")

	names := make([]string, 0, len(reg.Instances))
	for name := range reg.Instances {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		rec := reg.Instances[name]
		ports := formatPorts(rec.Ports)
		age := formatAge(rec.CreatedAt)
		mailCount, mailErr := mailbox.Count(cmd.Root, name)
		mailStr := fmt.Sprintf("%d", mailCount)
		if mailErr != nil {
			mailStr = "?"
		}
		fmt.Printf("%-8s %-10s %-20s %-5s %-24s %s\n", name, rec.State, rec.Branch, mailStr, ports, age)
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

	bp, _ := loadBlueprint(cmd.Root)
	printStampNotice(cmd.Root, bp, reg)

	if rec.State == registry.StateSuspended {
		fmt.Fprintf(os.Stderr, "note: instance %s is suspended — services and processes are stopped\n", cmd.Name)
	}

	if n, err := mailbox.Count(cmd.Root, cmd.Name); err == nil && n > 0 {
		fmt.Fprintf(os.Stderr, "note: %d unread message(s) — run 'plax recv %s' to read\n", n, cmd.Name)
	}

	absRoot, _ := filepath.Abs(cmd.Root)
	if bp != nil {
		currentStamp := computeStamp(cmd.Root, bp)
		sdeps := &status.Deps{
			Blueprint:    bp,
			Registry:     reg,
			RepoRoot:     absRoot,
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

	envVars, err := loadInstanceEnv(rec.WorktreePath, rec.Ports)
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

	reg, err := openRegistry(cmd.Root)
	if err != nil {
		return err
	}

	rec, found := reg.GetInstance(cmd.Name)
	if !found {
		return fmt.Errorf("instance %q not found", cmd.Name)
	}

	bp, _ := loadBlueprint(cmd.Root)
	printStampNotice(cmd.Root, bp, reg)

	if rec.State == registry.StateSuspended {
		fmt.Fprintf(os.Stderr, "note: instance %s is suspended — services and processes are stopped\n", cmd.Name)
	}

	envVars, err := loadInstanceEnv(rec.WorktreePath, rec.Ports)
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
		return nil, fmt.Errorf("resolving repo root: %w", err)
	}

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

	return &cliDeps{
		Deps: &instance.Deps{
			Blueprint: bp,
			Registry:  reg,
			Pool:      pool,
			BM:        bm,
			Docker:    drv,
			RepoRoot:  absRoot,
		},
		bm:     bm,
		docker: drv,
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
// over the host environment, then layers allocated ports on top so that
// exec and attach see the same port variables the instance's managed
// processes receive.
func loadInstanceEnv(worktreePath string, ports map[string]int) ([]string, error) {
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
	for k, v := range ports {
		envMap[k] = strconv.Itoa(v)
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

	if pgURL != "" {
		return &bp, pgURL, nil
	}

	connStr, err := pgConnString(&bp)
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

func computeStamp(repoRoot string, bp *blueprint.Blueprint) registry.BlueprintStamp {
	hashFile := func(path string) string {
		data, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		h := sha256.Sum256(data)
		return fmt.Sprintf("%x", h[:])
	}
	return registry.BlueprintStamp{
		ComposeHash:    hashFile(filepath.Join(repoRoot, "docker-compose.yml")),
		EnvExampleHash: hashFile(filepath.Join(repoRoot, bp.Env.Template)),
		ToolchainHash:  hashFile(filepath.Join(repoRoot, bp.Toolchain)),
	}
}

func stampNotice(stamp registry.BlueprintStamp, reg *registry.Registry) {
	stored := reg.BlueprintStamp
	if stored.ComposeHash == "" && stored.EnvExampleHash == "" && stored.ToolchainHash == "" {
		return
	}
	if stored.ComposeHash != stamp.ComposeHash ||
		stored.EnvExampleHash != stamp.EnvExampleHash ||
		stored.ToolchainHash != stamp.ToolchainHash {
		fmt.Fprintln(os.Stderr, "note: blueprint inputs changed since last 'plax up' — run 'plax doctor' for details")
	}
}

func printStampNotice(root string, bp *blueprint.Blueprint, reg *registry.Registry) {
	if bp == nil {
		return
	}
	current := computeStamp(root, bp)
	stampNotice(current, reg)
}

func runSuspend(cmd SuspendCmd) error {
	absRoot, err := filepath.Abs(cmd.Root)
	if err != nil {
		return fmt.Errorf("resolving repo root: %w", err)
	}

	reg, err := openRegistry(cmd.Root)
	if err != nil {
		return err
	}

	bp, _ := loadBlueprint(cmd.Root)
	printStampNotice(cmd.Root, bp, reg)

	deps := &instance.Deps{Registry: reg, RepoRoot: absRoot}

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

	absRoot, err := filepath.Abs(cmd.Root)
	if err != nil {
		return fmt.Errorf("resolving repo root: %w", err)
	}

	bp, connStr, err := loadBlueprintAndConnString(cmd.Root, cmd.PgURL)
	if err != nil {
		return err
	}

	reg, err := openRegistry(cmd.Root)
	if err != nil {
		return err
	}

	printStampNotice(cmd.Root, bp, reg)

	deps := &instance.Deps{Blueprint: bp, Registry: reg, RepoRoot: absRoot}

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

	bm, err := postgres.NewBaseManager(ctx, connStr, absRoot, bp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "note: postgres unavailable — drift report skipped\n")
		fmt.Fprintf(os.Stderr, "instance %s resumed\n", cmd.Name)
		return nil
	}
	defer bm.Close()

	currentStamp := computeStamp(cmd.Root, bp)
	sdeps := &status.Deps{
		Blueprint:    bp,
		Registry:     reg,
		BM:           bm,
		RepoRoot:     absRoot,
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
	absRoot, err := filepath.Abs(cmd.Root)
	if err != nil {
		return fmt.Errorf("resolving repo root: %w", err)
	}

	reg, err := openRegistry(cmd.Root)
	if err != nil {
		return err
	}

	if _, found := reg.GetInstance(cmd.Name); !found {
		return fmt.Errorf("instance %q not found", cmd.Name)
	}

	bp, connStr, err := loadBlueprintAndConnString(cmd.Root, cmd.PgURL)
	if err != nil {
		bp = nil
		connStr = ""
	}

	if bp == nil {
		bp = &blueprint.Blueprint{}
	}

	currentStamp := computeStamp(cmd.Root, bp)
	sdeps := &status.Deps{
		Blueprint:    bp,
		Registry:     reg,
		RepoRoot:     absRoot,
		CurrentStamp: currentStamp,
	}

	if connStr != "" {
		bm, err := postgres.NewBaseManager(context.Background(), connStr, absRoot, bp)
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
	absRoot, err := filepath.Abs(cmd.Root)
	if err != nil {
		return fmt.Errorf("resolving repo root: %w", err)
	}

	bp, err := loadBlueprint(cmd.Root)
	if err != nil {
		return fmt.Errorf("parsing plax.json: %w", err)
	}

	reg, err := openRegistry(cmd.Root)
	if err != nil {
		return err
	}

	ddeps := &doctor.Deps{
		Blueprint: bp,
		Registry:  reg,
		RepoRoot:  absRoot,
	}

	pgURL := cmd.PgURL
	if pgURL == "" {
		pgURL, _ = deriveConnString(bp)
	}
	if pgURL != "" {
		bm, err := postgres.NewBaseManager(context.Background(), pgURL, absRoot, bp)
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
	absRoot, err := filepath.Abs(cmd.Root)
	if err != nil {
		return fmt.Errorf("resolving repo root: %w", err)
	}

	bp, err := loadBlueprint(cmd.Root)
	if err != nil {
		return fmt.Errorf("plax.json not found at %s — run 'plax init' first", filepath.Join(cmd.Root, "plax.json"))
	}

	reg, err := openRegistry(cmd.Root)
	if err != nil {
		return err
	}

	deps := &instance.Deps{Blueprint: bp, Registry: reg, RepoRoot: absRoot}
	return instance.Rederive(context.Background(), deps)
}

func runSend(cmd SendCmd) error {
	reg, err := openRegistry(cmd.Root)
	if err != nil {
		return err
	}

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

	filename, err := mailbox.Send(cmd.Root, cmd.Name, msg)
	if err != nil {
		return err
	}

	if cmd.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]string{"status": "sent", "instance": cmd.Name, "file": filename})
	}

	fmt.Fprintf(os.Stderr, "message written: %s\n", filename)
	return nil
}

func runRecv(cmd RecvCmd) error {
	reg, err := openRegistry(cmd.Root)
	if err != nil {
		return err
	}

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

	var msgs []mailbox.Message
	if cmd.All {
		msgs, err = mailbox.RecvAll(cmd.Root, cmd.Name)
	} else {
		msgs, err = mailbox.Recv(cmd.Root, cmd.Name, n)
	}
	if err != nil {
		return err
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

	remaining, _ := mailbox.Count(cmd.Root, cmd.Name)
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
		{"host", r.Host}, {"config", r.Config},
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
		{"host", r.Host}, {"config", r.Config},
	} {
		_, _ = fmt.Fprintf(w, "%-8s %-12s %-10s %s\n", r.Instance, d.n, d.dim.Level, d.dim.Detail)
	}
}

func pgConnString(bp *blueprint.Blueprint) (string, error) {
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

func deriveConnString(bp *blueprint.Blueprint) (string, error) {
	return pgConnString(bp)
}
