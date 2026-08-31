package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
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
	"github.com/apollopower/plax/pkg/record"
	"github.com/apollopower/plax/pkg/registry"
	"github.com/apollopower/plax/pkg/stamp"
	"github.com/apollopower/plax/pkg/status"
	"github.com/apollopower/plax/pkg/upgrade"
	"github.com/apollopower/plax/pkg/verify"
	"github.com/apollopower/plax/pkg/worktree"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

//go:embed guide.md
var guideDoc string

// versionFlag mirrors kong's helpFlag: BeforeReset fires before command
// resolution, so `plax --version` works without a subcommand.
type versionFlag bool

func (v versionFlag) IgnoreDefault() {}

func (v versionFlag) BeforeReset(ctx *kong.Context) error {
	fmt.Printf("plax %s (commit: %s, built: %s)\n", version, commit, date)
	ctx.Exit(0)
	return nil
}

type CLI struct {
	Version  versionFlag `name:"version" help:"Print version and exit"`
	Init     InitCmd     `cmd:"" help:"Scaffold a blueprint by parsing the repo's docker-compose.yml and .env.example"`
	Guide    GuideCmd    `cmd:"" help:"Print the full plax guide for coding agents (markdown)"`
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
	Log      LogCmd      `cmd:"" help:"Append a timestamped note to an instance's work record"`
	Record   RecordCmd   `cmd:"" help:"Print an instance's work record"`
	Verdict  VerdictCmd  `cmd:"" help:"Author the terminal verdict on an instance's work record"`
	Upgrade  UpgradeCmd  `cmd:"" help:"Update plax to the latest release (honors the install method)"`
}

type InitCmd struct {
	Root string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
}

type GuideCmd struct{}

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
	Name   string   `arg:"" help:"Instance name (e.g. i1)"`
	Root   string   `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
	PgURL  string   `name:"pg-url" type:"string" optional:"" help:"Postgres connection DSN (overrides blueprint env)"`
	Ref    string   `name:"ref" short:"R" optional:"" help:"Branch, PR number, tag, or commit SHA to branch from (default: current HEAD)"`
	Intent string   `name:"intent" type:"path" optional:"" help:"Intent file: the task statement stored in the instance's work record"`
	Parent string   `name:"parent" optional:"" help:"Existing instance whose exact worktree HEAD becomes the child's branch base"`
	Skip   []string `name:"skip" optional:"" help:"Steps to skip: migrate, verify (comma-separated or repeated)"`
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

type UpgradeCmd struct {
	Check bool `name:"check" help:"Report the latest release and update path without changing anything"`
	Force bool `name:"force" help:"Proceed even when the current version is a dev build"`
}

type LogCmd struct {
	Name string   `arg:"" help:"Instance name"`
	Text []string `arg:"" optional:"" passthrough:"" help:"Note text (use -- to separate from flags)"`
	Root string   `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
}

type RecordCmd struct {
	Name string `arg:"" help:"Instance name"`
	Root string `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
	JSON bool   `name:"json" help:"Output the parsed record as JSON"`
}

type VerdictCmd struct {
	Name     string   `arg:"" help:"Instance name"`
	Root     string   `name:"root" short:"r" type:"path" default:"." help:"Repo root directory (auto-discovered from cwd)"`
	Status   string   `name:"status" help:"Task outcome: pass or fail (required)"`
	Contract string   `name:"contract" optional:"" help:"Contract outcome: pass or fail (required when the record declares a contract)"`
	Summary  []string `arg:"" optional:"" passthrough:"" help:"Summary prose (use -- to separate from flags)"`
}

func main() {
	var cli CLI
	ctx := kong.Parse(&cli,
		kong.Name("plax"),
		kong.Description("Run many parallel dev environments for coding agents."),
		kong.UsageOnError(),
		kong.Vars{
			"version": fmt.Sprintf("%s (commit: %s, built: %s)", version, commit, date),
		},
	)

	switch ctx.Command() {
	case "init":
		ctx.FatalIfErrorf(runInit(cli.Init))
	case "guide":
		ctx.FatalIfErrorf(runGuide(cli.Guide))
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
	case "log <name>", "log <name> <text>":
		ctx.FatalIfErrorf(runLog(cli.Log))
	case "record <name>":
		ctx.FatalIfErrorf(runRecord(cli.Record))
	case "verdict <name>", "verdict <name> <summary>":
		ctx.FatalIfErrorf(runVerdict(cli.Verdict))
	case "upgrade":
		ctx.FatalIfErrorf(runUpgrade(cli.Upgrade))
	default:
		ctx.FatalIfErrorf(fmt.Errorf("unknown command: %s", ctx.Command()))
	}
}

// runGuide prints the embedded agent-facing reference. Deliberately
// stateless: no repo root, no registry, no blueprint — it must work in an
// empty directory so an agent can read it before a repo exists.
func runGuide(cmd GuideCmd) error {
	_, err := fmt.Print(guideDoc)
	return err
}

// runUpgrade drives the install-method-aware self-update. Deliberately
// stateless like runGuide: it must work in an empty directory, because an
// outdated binary is exactly when nothing else works.
func runUpgrade(cmd UpgradeCmd) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("upgrade: resolving executable: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("upgrade: resolving executable: %w", err)
	}

	method := upgrade.Detect(exe)
	client := &http.Client{Timeout: 15 * time.Second}

	devBuild := version == "dev"
	if !cmd.Check && devBuild && !cmd.Force {
		return errors.New("upgrade: cannot determine current version: dev build — rebuild from source, or pass --force to replace the binary with the latest release")
	}

	rel, err := upgrade.LatestRelease(client, upgrade.UpgradeRepo)
	if err != nil {
		if cmd.Check {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return fmt.Errorf("upgrade: %w", err)
	}

	if cmd.Check {
		return checkUpgrade(method, rel)
	}

	if rel.Tag == "" {
		fmt.Println("no releases found")
		return nil
	}

	outdated, err := upgrade.Outdated(version, rel.Tag)
	if err != nil {
		return fmt.Errorf("upgrade: %w", err)
	}
	proceed := outdated || (devBuild && cmd.Force)
	if !proceed {
		fmt.Printf("already at latest (%s)\n", rel.Tag)
		return nil
	}

	// Deliberately exit with the child's code instead of returning an
	// error: kong's FatalIfErrorf always exits 1, which would mask a
	// brew/go failure. Same precedent as runBaseRefresh's os.Exit(2).
	switch method {
	case upgrade.MethodBrew:
		os.Exit(upgrade.RunChild([]string{"brew", "upgrade", "plax"}))
	case upgrade.MethodGoInstall:
		os.Exit(upgrade.RunChild([]string{"go", "install", "github.com/apollopower/plax/cmd/plax@latest"}))
	case upgrade.MethodDirect:
		return upgradeDirect(exe, rel, client)
	}
	return fmt.Errorf("upgrade: cannot determine install method")
}

// checkUpgrade reports current/latest/method and the command an upgrade
// would run, without writing anything. Exit codes: 0 current, 1 outdated
// (or a dev build with a release available), 2 lookup failure (handled by
// the caller).
func checkUpgrade(method upgrade.Method, rel upgrade.Release) error {
	fmt.Printf("current: %s\n", version)
	if rel.Tag == "" {
		fmt.Println("latest:  none")
		return nil
	}
	fmt.Printf("latest:  %s\n", rel.Tag)
	fmt.Printf("method:  %s\n", method)
	fmt.Printf("run:     %s\n", methodCommand(method))

	if version == "dev" {
		os.Exit(1)
	}
	outdated, err := upgrade.Outdated(version, rel.Tag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if outdated {
		os.Exit(1)
	}
	return nil
}

// methodCommand is the concrete command `plax upgrade` would execute for
// the install method, shown in check mode.
func methodCommand(method upgrade.Method) string {
	switch method {
	case upgrade.MethodBrew:
		return "brew upgrade plax"
	case upgrade.MethodGoInstall:
		return "go install github.com/apollopower/plax/cmd/plax@latest"
	default:
		return "plax upgrade (direct binary replacement)"
	}
}

// upgradeDirect downloads the matching release archive, verifies its
// checksum, and atomically replaces the running binary.
func upgradeDirect(exe string, rel upgrade.Release, client *http.Client) error {
	asset, ok := upgrade.AssetFor(rel.Assets, runtime.GOOS, runtime.GOARCH)
	if !ok {
		return fmt.Errorf("upgrade: latest release has no archive for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	dir := filepath.Dir(exe)
	archive, err := upgrade.Download(client, asset.URL, dir)
	if err != nil {
		return fmt.Errorf("upgrade: %w", err)
	}
	defer func() { _ = os.Remove(archive) }()

	sumURL := upgrade.ChecksumsURL(rel.Assets)
	if sumURL == "" {
		return errors.New("upgrade: latest release has no checksums.txt — refusing an unverified upgrade")
	}
	if err := upgrade.VerifyChecksum(client, archive, sumURL, asset.Name); err != nil {
		return fmt.Errorf("upgrade: %w", err)
	}

	extractDir, err := os.MkdirTemp(dir, ".plax-upgrade-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(extractDir) }()

	bin, err := upgrade.ExtractArchive(archive, extractDir)
	if err != nil {
		return fmt.Errorf("upgrade: %w", err)
	}
	if err := upgrade.AtomicReplace(bin, exe); err != nil {
		return fmt.Errorf("upgrade: replacing %s: %w", exe, err)
	}

	fmt.Printf("plax updated: %s → %s\n", version, rel.Tag)
	return nil
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

	if changed, err := blueprint.EnsureIgnore(absRoot); err != nil {
		fmt.Fprintf(os.Stderr, "init: warning: could not update .gitignore: %v\n", err)
	} else if changed {
		fmt.Fprintln(os.Stderr, "init: added .plax/ to .gitignore so instances are not traversed by root-globbing tooling")
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
	root, _, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
	}
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
	root, _, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
	}
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
	root, _, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
	}
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
	root, _, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
	}
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
	root, _, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
	}
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

	// Skip names validate before any side effect, including opening the
	// registry or connecting to Postgres and Docker.
	skip, err := instance.ParseSkip(cmd.Skip)
	if err != nil {
		return err
	}

	root, _, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
	}

	// Tracked-record setup happens before any side effect: --intent must
	// name a readable, non-empty file, and --parent must name a registered
	// instance whose worktree is a usable exact base.
	rec, err := buildRecordInput(root, cmd)
	if err != nil {
		return err
	}

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
	if rec != nil && rec.BaseCommit != "" {
		// A stacked child records the parent's exact HEAD as its base.
		deps.ResolvedRef = rec.BaseCommit
	}

	return instance.Up(ctx, deps.Deps, cmd.Name, instance.UpOptions{Skip: skip, Record: rec})
}

// buildRecordInput validates the --intent/--parent combination before any
// up side effect and returns the record to create, or nil for an untracked
// instance.
func buildRecordInput(root string, cmd UpCmd) (*record.CreateInput, error) {
	if cmd.Parent != "" && cmd.Ref != "" {
		return nil, fmt.Errorf("up: --parent and --ref are mutually exclusive — the parent selects the Git base, --ref selects an independent external base")
	}
	if cmd.Intent == "" && cmd.Parent == "" {
		fmt.Fprintln(os.Stderr, "warning: no work record will be created — pass --intent <file> to track this instance")
		return nil, nil
	}
	if cmd.Parent != "" && cmd.Intent == "" {
		return nil, fmt.Errorf("up: --parent requires --intent <file> — the child record needs its own intent")
	}

	intent, err := readIntentFile(cmd.Intent)
	if err != nil {
		return nil, err
	}
	rec := &record.CreateInput{Instance: cmd.Name, Intent: intent}
	if cmd.Parent == "" {
		return rec, nil
	}

	parentCommit, err := resolveParent(root, cmd.Parent)
	if err != nil {
		return nil, err
	}
	rec.Parent = cmd.Parent
	rec.BaseCommit = parentCommit
	return rec, nil
}

// readIntentFile reads an --intent file, rejecting a missing, unreadable,
// or empty intent before any up side effect. Prose that would collide with
// the record grammar is also rejected here, so a bad intent never starts
// the expensive up phases.
func readIntentFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("up: reading intent file: %w", err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("up: intent file %s is empty", path)
	}
	if err := record.ValidateProse("intent file "+path, string(data)); err != nil {
		return "", fmt.Errorf("up: %w", err)
	}
	return string(data), nil
}

// resolveParent validates that the named instance is a usable parent for a
// stacked child and returns its exact worktree HEAD commit. The registry
// lock is released on return because runUp re-opens the registry moments
// later via buildDeps, and two exclusive opens would block each other.
func resolveParent(root, parent string) (string, error) {
	reg, err := openRegistry(root)
	if err != nil {
		return "", err
	}
	defer reg.Close()

	rec, found := reg.GetInstance(parent)
	if !found {
		return "", fmt.Errorf("up: parent instance %q not found in the registry — --parent must name a registered instance", parent)
	}

	// The child's record stores parent lineage, so the parent must itself
	// be tracked; its lineage cannot be recorded honestly otherwise.
	if _, err := record.Read(root, parent); err != nil {
		return "", fmt.Errorf("up: parent %q has no work record — run 'plax up --intent <file> %s' to track it first", parent, parent)
	}

	if rec.WorktreePath == "" {
		return "", fmt.Errorf("up: parent %q has no worktree path — resume or recreate it before stacking", parent)
	}
	if info, err := os.Stat(rec.WorktreePath); err != nil || !info.IsDir() {
		return "", fmt.Errorf("up: parent %q has no accessible worktree (%s) — resume or recreate it before stacking", parent, rec.WorktreePath)
	}

	dirty, err := worktree.IsDirty(rec.WorktreePath)
	if err != nil {
		return "", fmt.Errorf("up: checking parent %q worktree: %w", parent, err)
	}
	if dirty {
		return "", fmt.Errorf("up: parent %q has uncommitted changes — stacked ancestry needs an exact commit snapshot; commit or stash them first", parent)
	}

	_, commit, err := worktree.WorktreeHead(rec.WorktreePath)
	if err != nil {
		return "", fmt.Errorf("up: reading parent %q HEAD: %w — a stacked child must branch from the parent's exact commit", parent, err)
	}
	return commit, nil
}

func runDown(cmd DownCmd) error {
	// Down is best-effort: the registry is required, but each backend is
	// optional. A stopped Postgres or broken Docker client must not prevent
	// teardown of everything else.
	//
	// Deliberately not signal-cancellable: an interrupted down is safely
	// re-runnable because every step tolerates missing resources.
	root, found, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("down: %w", ErrNoRoot)
	}
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
	root, found, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
	}
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

	ctx := context.Background()
	health := liveHealthForInstances(ctx, bp, reg.Instances)

	for _, name := range names {
		rec := reg.Instances[name]
		ports := formatPorts(rec.Ports)
		age := formatAge(rec.CreatedAt)
		mailCount, mailErr := mailbox.Count(root, name)
		mailStr := fmt.Sprintf("%d", mailCount)
		if mailErr != nil {
			mailStr = "?"
		}
		healthStr := formatHealth(health[name])
		fmt.Printf("%-8s %-10s %-20s %-5s %-24s %-10s %s\n", name, rec.State, rec.Branch, mailStr, ports, healthStr, age)
	}

	return nil
}

// liveHealthForInstances probes each running instance's workloads in parallel
// and returns a per-instance Health. Suspended instances fall back to their
// stored value. Probing is bounded by verify.CheckDeadline so a slow workload
// does not stall ls for the whole instance set.
func liveHealthForInstances(ctx context.Context, bp *blueprint.Blueprint, instances map[string]registry.InstanceRecord) map[string]registry.Health {
	out := make(map[string]registry.Health, len(instances))
	if bp == nil {
		for name, rec := range instances {
			out[name] = rec.Health
		}
		return out
	}

	type result struct {
		name string
		h    registry.Health
	}
	ch := make(chan result, len(instances))
	for name, rec := range instances {
		name, rec := name, rec
		go func() {
			if rec.State != registry.StateRunning {
				ch <- result{name, rec.Health}
				return
			}
			var h registry.Health
			switch status.LiveHealth(ctx, bp, &rec).Level {
			case status.OK:
				h = registry.HealthHealthy
			case status.Drift:
				h = registry.HealthUnhealthy
			}
			ch <- result{name, h}
		}()
	}
	for range instances {
		r := <-ch
		out[r.name] = r.h
	}
	return out
}

func runAttach(cmd AttachCmd) error {
	root, found, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
	}
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

	root, found, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
	}
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
// If found, returns the (absolute) directory containing it, true, nil.
// If plax.json is not found up to the filesystem root, returns (start, false, nil).
// If Abs fails, returns (start, false, err).
//
// When start is inside a git worktree, the git common dir resolves to the real
// repo root. This matters because a committed plax.json is present in every
// worktree checkout, so walking up would otherwise stop at the worktree's copy
// and miss the root that owns .plax/registry.json.
func discoverRoot(start string) (string, bool, error) {
	start, err := filepath.Abs(start)
	if err != nil {
		return start, false, err
	}

	// Inside a linked worktree, --git-common-dir points at the main repo's
	// .git, whose parent is the real root. Bypass worktree-local plax.json.
	if root := gitCommonRoot(start); root != "" {
		if _, err := os.Stat(filepath.Join(root, "plax.json")); err == nil {
			return root, true, nil
		}
	}

	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "plax.json")); err == nil {
			return dir, true, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return start, false, nil
		}
		dir = parent
	}
}

// gitCommonRoot returns the top-level repository root containing start, or ""
// if start is not inside a git worktree/repository. For linked worktrees this
// is the main repo root, not the worktree.
func gitCommonRoot(start string) string {
	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
	cmd.Dir = start
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	common := strings.TrimSpace(string(out))
	if common == "" {
		return ""
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(start, common)
	}
	common, err = filepath.Abs(common)
	if err != nil {
		return ""
	}
	// Only trust the common dir's parent as the repo root when it is a .git
	// subdirectory (the standard layout and the linked-worktree case). Bare
	// repos and --separate-git-dir return a git dir that is not a .git child
	// of the root, so filepath.Dir would point at the wrong path; fall back to
	// the filesystem walk-up instead.
	if filepath.Base(common) != ".git" {
		return ""
	}
	return filepath.Dir(common)
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
	root, found, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("suspend: %w", ErrNoRoot)
	}

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

	root, _, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
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
	root, found, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
	}
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
	root, found, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
	}
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
	root, _, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
	}

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
	root, _, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
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

	rec, found := reg.GetInstance(cmd.Name)
	if !found {
		return fmt.Errorf("instance %q not found", cmd.Name)
	}

	ctx := context.Background()

	vDeps := &verify.Deps{
		Blueprint:     bp,
		Registry:      reg,
		RepoRoot:      root,
		RuntimeChecks: true,
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
	root, found, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
	}
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
	root, found, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
	}
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

// runLog appends a timestamped prose note to an instance's work record.
// Records resolve from .plax/records/<name>.md, not the registry, so
// logging a preserved record after `down` remains supported.
func runLog(cmd LogCmd) error {
	root, found, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("log: %w", ErrNoRoot)
	}

	parts := cmd.Text
	for len(parts) > 0 && parts[0] == "--" {
		parts = parts[1:]
	}
	text := strings.Join(parts, " ")
	if text == "" {
		return fmt.Errorf("log: note text is required — usage: plax log <name> -- <text>")
	}

	return record.Append(root, cmd.Name, text, time.Now())
}

// runRecord prints an instance's work record: the complete original text to
// stdout by default, or the parsed projection with --json.
func runRecord(cmd RecordCmd) error {
	root, found, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("record: %w", ErrNoRoot)
	}

	if cmd.JSON {
		rec, err := record.Read(root, cmd.Name)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rec)
	}

	// Default output is the original text, not a reconstruction. ReadText
	// validates the parse in the same locked read, so a malformed record
	// fails with a path and parse error instead of silently passing through.
	text, err := record.ReadText(root, cmd.Name)
	if err != nil {
		return err
	}
	_, err = fmt.Print(text)
	return err
}

// runVerdict authors the single terminal verdict on an instance's work
// record. The verdict records the operator's declaration; it never claims
// plax's verify battery independently validated the task.
func runVerdict(cmd VerdictCmd) error {
	root, found, err := discoverRoot(cmd.Root)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("verdict: %w", ErrNoRoot)
	}

	if cmd.Status == "" {
		return fmt.Errorf("verdict: --status is required (pass or fail)")
	}
	if cmd.Status != "pass" && cmd.Status != "fail" {
		return fmt.Errorf("verdict: --status must be %q or %q, got %q", "pass", "fail", cmd.Status)
	}
	if cmd.Contract != "" && cmd.Contract != "pass" && cmd.Contract != "fail" {
		return fmt.Errorf("verdict: --contract must be %q or %q, got %q", "pass", "fail", cmd.Contract)
	}

	parts := cmd.Summary
	for len(parts) > 0 && parts[0] == "--" {
		parts = parts[1:]
	}
	v := record.Verdict{Status: cmd.Status, Contract: cmd.Contract, Summary: strings.Join(parts, " ")}
	return record.WriteVerdict(root, cmd.Name, v, time.Now())
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
