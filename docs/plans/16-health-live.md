# Plan 16 — Live health: `ls`/`status` probe live; process ports probed

## Objective

Make health a live computation rather than a stale snapshot: `plax ls` and
`plax status` run live `process-liveness` and `tcp-reachability` checks
(including process-bound ports) at call time, replacing the passive
`rec.Health` read.

## Decision recorded from triage 2026-08-14

The triage doc proposed an "async settle" writer that persists health after
`up` via a bounded poll loop. **Rejected.** A goroutine spawned inside `up`
cannot outlive the `up` process — it dies at exit. Making `up` fork a
detached background process to do the polling violates the project's own
UNIX-philosophy value (Plax is a tool, not a daemon). Instead the live
command **is** the bounded poll: a single `ls`/`status` waits up to a short
deadline for workloads to answer, then reports what it observed. Persisted
`rec.Health` remains advisory (used by `up`'s summary and `attach`'s drift
notice) but is never the answer given by `ls`/`status`.

`up`/`resume` still run synchronous static checks (env, template) — those are
immediate and catch misconfiguration. They also keep the cheap
`process-liveness` check (catches immediate-exit) and the DB checks, but **do
not run the TCP probe**: a freshly started app legitimately takes seconds to
bind, and `up` must not block on readiness. The TCP reachability probe (now
including process ports) runs only in the live read paths — `plax ls`,
`plax status`, and the explicit `plax verify` — via a `RuntimeChecks` flag on
`verify.Deps` that gates `CheckServices`.

## Package layout

```
pkg/verify/verify.go        # CheckServices gains processes arg + bounded poll-with-deadline
pkg/verify/verify_test.go   # Tests for process ports and deadline behavior
pkg/status/status.go        # healthDrift -> live CheckServices+CheckProcesses; Deps unchanged
cmd/plax/main.go            # runLs probes live health; status report health via live path
docs/plans/index.md         # Add plan 16 to phase table
```

## Type specifications

`verify.Deps` gains one field:

```go
type Deps struct {
    // ... existing fields unchanged ...
    RuntimeChecks bool // enables the TCP reachability probe
}
```

`RuntimeChecks` is false by default so `up`/`resume` skip the TCP probe; the
explicit `plax verify` sets it true. `status.Deps` already carries `Blueprint`
and the `InstanceRecord` (fetched inside `Build`); `CheckServices` needs only
the blueprint services/processes and `rec.Ports`, `CheckProcesses` only
`rec.PIDs` — both already in hand.

`CheckServices` signature:

```go
func CheckServices(ctx context.Context, services map[string]blueprint.ServiceDef,
    processes []blueprint.ProcessDef, allocated map[string]int) []CheckResult
```

A new `pollDeadline` default (~3s) bounds how long a probe set waits; ports
are re-dialed on a ~500ms interval until the deadline or all reachable.

## Algorithms

### `CheckServices` — probe both service and process ports

```
endpoints = []
for each svc in services where Isolation == dedicated:
    for each portDef in svc.Ports:
        port = allocated[portDef.Var]; if present: endpoints.append(label=svcName, port)
for each proc in processes where PortVar != "":
    port = allocated[proc.PortVar]; if present: endpoints.append(label=proc.Name, port)
if no endpoints: return nil

deadline = now + pollDeadline
loop:
    failed = []
    for ep in endpoints:
        if dial(127.0.0.1:ep.port) succeeds: continue
        failed.append(ep)
    if failed empty: return all-reachable pass result
    if now >= deadline: return a failure result per failed endpoint
    sleep 500ms
```

`⚠` A port var may be allocated but the workload's service or process may not
yet be listening — that is the settle case the poll absorbs. A port var absent
from `allocated` is skipped (matches existing behaviour).

### `status.Build` — live health dimension

```
if rec.State == suspended:
    health = Unknown "suspended"        # no runtime to probe
else:
    results = CheckServices(ctx, bp.Services, bp.Processes, rec.Ports)
             + CheckProcesses(rec.PIDs, process.IsAlive)
    if any result failed:  health = Drift (detail = first failed detail)
    else if any results:   health = OK (detail = "live check passed")
    else:                  health = Unknown (detail = "no runtime checks defined")
```

The live path does **not** persist — a read must not write. Existing
`healthDrift` is removed; `rec.Health`/`rec.VerifiedAt` continue to be written
only by `verify.RunVerify`.

### `runLs` — live health column

For `StateRunning` instances only, probe health in parallel (bounded
goroutines) so N instances do not serialize their dials. Suspended and
never-probed instances show their stored value / `-`. `--json` output leaves
the stored record untouched.

## CLI specification

Unchanged command syntax. `plax ls` HEALTH column and `plax status <name>`
health row now reflect live checks for running instances. No new flags. If
live probing exceeds the internal deadline, the health reflects the partial
observation (failures reported) rather than blocking indefinitely.

## Error handling

| Failure | Behavior |
|---|---|
| `rec.State` suspended | health `Unknown`, detail `"suspended"`, no probes |
| A workload never binds within deadline | health `Drift`, detail names the endpoint(s) |
| Process dead (PGID gone) | health `Drift`, detail `process <name> (PGID=N) is not alive` |
| No services/processes defined | health `Unknown`, detail `no runtime checks defined` |
| Probe set empty / no allocated ports | no `tcp-reachability` result (as today) |

## Tests

- `TestVerify_Services_ProcessPortReachable` — process `PortVar` bound on a
  real listener is reported healthy.
- `TestVerify_Services_ProcessPortUnreachable` — process `PortVar` with no
  listener fails `tcp-reachability`.
- `TestVerify_Services_MixedServiceAndProcess` — both a service port and a
  process port are probed in one call.
- `TestVerify_Services_NoEndpoints` — no services/processes yields no results.
- `TestVerify_Services_UnallocatedPortSkipped` — a port var absent from
  `allocated` is ignored.
- `TestStatus_Health_UnknownWhenSuspended` — `status.Build` yields `Unknown`
  for a suspended instance without probing.
- `TestStatus_Health_LivePass` — running instance with live-reachable endpoint
  reports `OK`.
- `TestStatus_Health_LiveFail` — running instance with dead process reports
  `Drift` without relying on stored `rec.Health`.

## Acceptance criteria

- `plax status <name>` on a running instance reflects live `process-liveness`
  and `tcp-reachability` (including process `PortVar` ports), not `rec.Health`.
- `plax ls` HEALTH column for running instances reflects live checks.
- `plax status` on a suspended instance reports health `Unknown`, never `Drift`.
- `plax status`/`plax ls` do not modify the registry (no write on read).
- `rec.Health` is still written by `verify.RunVerify` for `up` summary and
  `attach` drift notice.

## Dependencies

None new — `net`, `time`, `process`, `registry`, `blueprint` all already
imported by the touched packages.
