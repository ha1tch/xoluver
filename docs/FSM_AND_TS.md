# /fsm and /ts with xoluver: what each does, how to use it, what it costs

xoluver has two optional features, each built on another xolu primitive. Both are off by default. They are independent: use either, both or neither. The [README](../README.md) is the reference for every call mentioned here, [GUIDE.md](../GUIDE.md) is the usage guide, and [DESIGN.md](DESIGN.md) gives the design behind each option (section 4.4 for /fsm, section 5.9 for the index).

| | /fsm (a lifecycle step in `Save`) | /ts (the version index) |
|---|---|---|
| What it is | `SaveOptions.Walk`: moves a xolu state machine in the same commit as the save | An index with one event per history row, written in the same commit |
| The question it answers | "May this change happen, and what state is the entity in?" | "Which version was in effect at time T, quickly?" |
| It changes | Writes only | Writes (one extra event per history row) and reads (`AsOf`) |
| It needs | `XOLU_API_V2_ENABLED=true`, a state machine definition, and one machine per entity | Tenant routes (`Client.Tenant`), `XOLU_TIMESERIES_ENABLED=true`, and a timeline |
| Write cost (measured) | About +25 to 30% latency, 0.75x throughput | About +44% latency, 0.82x throughput |
| Read benefit (measured) | None: it does not touch reads | `AsOf` stays near 1 ms while the scan grows to 124 ms (see [How much faster is finding an old version with /ts?](#how-much-faster-is-finding-an-old-version-with-ts)) |
| What you give up | Nothing structural. A refused transition cancels the save | OQL reads on tenant routes (`ListVersions`, `CheckHistory`) |

The numbers are from `examples/benchmark` on a machine with one CPU, with the client and xolu v0.30.38 on the same host over loopback, on xolu's SQLite backend. Read the ratios, not the milliseconds, and run the benchmark on your own system (last section).

## How much faster is finding an old version with /ts?

It depends on how large the history is. On a small history the index barely helps (1.2x). With 10,000 rows in the history type it was 25x faster, and with 50,000 rows 120x.

Only `AsOf` (find the version in effect at a time) gets faster. The index changes nothing else on the read side:

| Operation | With /ts |
|-----------|----------|
| `AsOf` (find by time) | Faster. This is what the index is for |
| `GetVersion` (by number) | Unchanged. It already reads the row by its id, one request with no scan. This comes from the code; it was not benchmarked separately |
| `ListVersions`, `CheckHistory` | Unchanged. They use OQL, which the index does not touch, and OQL is unavailable on tenant routes, which the index requires |
| `Save`, `Restore`, `Delete` | Slower: one extra event per history row (see "What it costs" under "Using /ts") |

### As the whole history type grows

`AsOf` for an entity of 20 versions, with the rest of the history type filled with other entities' rows (100 calls per cell, median with p95 in parentheses):

| Rows in the history type | Without /ts (scan) | With /ts | Speed-up |
|--------------------------|--------------------|----------|----------|
| 21 | 1.15 ms (1.71) | 0.96 ms (2.27) | 1.2x |
| 1,021 | 3.04 ms (4.01) | 0.90 ms (2.57) | 3.4x |
| 10,021 | 26.48 ms (42.28) | 1.05 ms (1.35) | 25x |
| 50,021 | 124.13 ms (159.45) | 1.04 ms (1.77) | 120x |

### As one entity's own history grows

`AsOf` with no other rows in the history type (100 calls per cell):

| Versions of the entity | Without /ts (scan) | With /ts | Speed-up |
|------------------------|--------------------|----------|----------|
| 10 | 1.18 ms (1.99) | 0.98 ms (1.79) | 1.2x |
| 100 | 2.37 ms (3.92) | 1.39 ms (2.04) | 1.7x |
| 1,000 | 20.51 ms (39.91) | 4.54 ms (9.26) | 4.5x |

### Remarks

- **Why the gap grows.** Without the index, `AsOf` asks xolu for the entity's history with an OQL query, and OQL reads every row of the history type, so the time grows with the whole history. With the index it reads that one entity's events, so it stays near 1 ms however large the type is.
- **One entity's own history still matters to both paths.** At 1,000 versions the index took 4.5 ms: it returns every event of the entity, as the scan returns every row of the entity.
- **Small histories.** The index makes about 3.9 requests per `AsOf` (a /ts query, a check at the end of the events, the row the answer names and the row after it) against 2 for the scan. On loopback that is cheap. Over a network each request pays a round trip, so for a small history the index could be slower than the scan. That follows from the request counts and was not measured.
- **What you pay for it.** Saves take about 44% longer (1.31 ms to 1.89 ms) and throughput with four writers falls to 0.82x. The index also requires tenant routes, where on xolu v0.30.38 `ListVersions` and `CheckHistory` do not work.
- **Conditions.** One CPU, the client and xolu v0.30.38 on the same machine over loopback, xolu's SQLite backend, filler snapshots of 500 bytes, medians of 100 calls. Not measured: other snapshot sizes, a networked database, larger machines, and `GetVersion` on its own. Rerun `examples/benchmark` on your own system before relying on the ratios (last section).

## What each is for

**/fsm is about permission and state.** A state machine says which transitions are allowed from which state, and a guard can refuse one. With `Walk` set, the transition and the save are one commit: if the machine refuses, the document is not saved either. The input that went with each version is recorded on the history row (`lifecycle_input`). Reads are not involved.

**/ts is about time-lookup speed.** `AsOf` normally asks xolu for the whole history of an entity with an OQL query, and OQL reads every row of the history type. The version index lets `AsOf` ask for one entity's events instead. The history rows stay the authority: the index is derived from them, checked against them, and can be rebuilt from them. Answers are the same with or without it. It is only faster, and only when the history type is large.

They are not alternatives. A workflow with approvals needs /fsm whether or not anyone ever asks "what was it at T". A large history that people query by time needs /ts whether or not it has a workflow.

## Using /fsm

### Set up once per kind of entity

xoluver does not define state machines. Use xolu's own API.

1. Start xolu with `XOLU_API_V2_ENABLED=true`.
2. Define the machine (`POST /api/v2/fsm/def`). xolu requires a `determinism` setting and a terminal state that can be reached. A guard is a condition on the transition's payload:

```json
{
  "name": "Review",
  "determinism": "strict",
  "initial": "Draft",
  "states": {"Draft": {"terminal": false}, "Review": {"terminal": false}, "Retired": {"terminal": true}},
  "transitions": [
    {"from": "Draft", "input": "submit", "to": "Review", "guard": "payload.reviewer != ''"},
    {"from": "Review", "input": "reject", "to": "Draft"},
    {"from": ["Draft", "Review"], "input": "retire", "to": "Retired"}
  ]
}
```

3. Start one machine per entity (`POST /api/v2/fsm/machine`) with `"ref": "asset:123"`, which ties the machine to the entity `asset` 123.

### Use it

```go
res, err := c.SaveDetailed(ctx, ref, base, doc, xoluver.SaveOptions{
	SavedBy: "alice",
	Walk: &xoluver.LifecycleWalk{
		MachineID: machineID,
		Input:     "submit",
		Payload:   map[string]any{"reviewer": "bob"},
	},
})

var refused *xoluver.ErrLifecycleRejected
var conflict *xoluver.ErrVersionConflict
switch {
case errors.As(err, &refused):
	// The guard failed, there is no such transition, or the machine does not exist.
	// Nothing was saved and the machine did not move.
case errors.As(err, &conflict):
	// Someone saved first. Nothing was saved.
case err == nil:
	fmt.Println(res.Version, res.Walk.Previous, "->", res.Walk.Current)
}
```

`Restore` and `Delete` take the same option. A `Delete` with a "retire" transition retires the machine in the same commit that writes the tombstone, so a refused transition cancels the deletion.

### Rules

| Do | Why |
|----|-----|
| Pass the machine that belongs to this entity | xolu does not check. Saving entity A while walking entity B's machine succeeds and moves B's machine (tested) |
| Expect a refusal to undo everything | The walk runs inside the commit. A refused walk leaves no document change, no history row, no machine move, and, with the index on, no index event (all tested) |
| Do not count on the machine to block edits | A terminal machine does not stop a `Save` that has no `Walk` (tested). If "retired" must mean read-only, check the state in your service before saving, or always pass a `Walk` |
| Use xolu's own `/walk` for a transition with no document change | `/commit` needs an update. A transition alone creates no version |
| Leave `Walk` nil when you do not need it | No `fsm_walk` is sent at all, and no machine is touched |

### What it costs

| Configuration (one writer, 5 rounds pooled) | Median ms | p95 ms | Relative |
|----|----|----|----|
| `Save` | 1.26 | 2.30 | baseline |
| `Save` + walk, no guard | 1.55 | 2.62 | 1.24x |
| `Save` + walk, simple guard | 1.63 | 2.96 | 1.30x |

| Configuration (4 writers, distinct entities, median of 5 rounds) | Saves/second | Relative |
|----|----|----|
| `Save` | 767 | baseline |
| `Save` + guarded walk | 574 | 0.75x |

A walk adds about 0.3 ms to a save of about 1.3 ms. A simple guard added about 0.1 ms more, which is inside the run-to-run noise, so treat simple guards as almost free; complex guards were not measured. The throughput drop is about what the latency rise predicts (saves 1.27x slower is about 0.79x as many per second). On this one-CPU machine I did not separate time spent waiting on the database from CPU time. Each walk also adds an entry to the machine's own history in xolu, so that history grows with every walk; check xolu's machine collection settings for how long it is kept.

## Using /ts

### Set up once per kind of entity

1. Start xolu with `XOLU_TIMESERIES_ENABLED=true`, and with a tenant that exists: `XOLU_TENANT_AUTO_REGISTER=true`, or create the tenant first. /ts is served only on tenant routes.
2. Define the timeline and tell the client which type to index, before the client is shared between goroutines:

```go
c := xoluver.New("http://localhost:9091")
c.Tenant = "acme"
if err := c.DefineTimeIndex(ctx, 7); err != nil { // provisions /ts, defines timeline 7: two dimensions, no expiry
	log.Fatal(err)
}
if err := c.UseTimeIndex("asset", 7); err != nil { // index entities of type "asset" in timeline 7
	log.Fatal(err)
}
```

3. If history already exists, index it once per entity:

```go
rep, err := c.CheckIndex(ctx, ref)
if err == nil && len(rep.Missing) > 0 {
	n, err := c.RebuildIndex(ctx, ref) // writes the missing events
	fmt.Println("wrote", n, "events", err)
}
```

### Use it

You call `AsOf` as before. It uses the index when it can verify it, and otherwise falls back to the scan.

```go
rec, err := c.AsOf(ctx, ref, t)
if err != nil {
	log.Fatal(err) // or handle ErrDeletedAsOf, ErrBeforeHistory and ErrTenantOQL: see the README
}
fmt.Println(rec.Version)
hits, fallbacks := c.IndexStats() // how often each happened
fmt.Println(hits, fallbacks)
```

### Rules

| Do | Why |
|----|-----|
| Create the timeline with `DefineTimeIndex` | It makes two dimensions and no expiry, and refuses an existing timeline that would expire. An expiring index silently loses the start of every history |
| Give each indexed type its own timeline | The entity id is a dimension, and ids repeat across types |
| Configure every writer of the type the same way | A writer with no index writes history rows and no events. The entity's index gets gaps (`Missing`), and `AsOf` leaves the index for it |
| Enable the index before the history, or `RebuildIndex` each entity afterwards | History that predates the index has no events |
| Plan for tenant routes | On xolu v0.30.38, OQL cannot see tenant entities. `ListVersions`, `CheckHistory` and the `AsOf` scan fail with `ErrTenantOQL` there |
| Treat `ErrTenantOQL` from `AsOf` as "the index cannot answer" | An entity with no events, or a damaged index, makes `AsOf` leave the index, and on tenant routes the scan is unavailable. It is an error, never a guess |
| Run `CheckIndex` on a schedule | `AsOf` verifies only the events its answer depends on. `CheckIndex` verifies every event (one history read per version on tenant routes) |
| Expect to need an administrator for orphans and duplicates | xolu removes /ts events only by time-range purge, so `RebuildIndex` only adds |
| Keep an entity under 10,000 versions if you want the index to answer for it | One query returns at most 10,000 events, and a full page is not trusted |

A save of an indexed type without `Client.Tenant` fails before anything is written (`ErrTenantRequired`), and a save when the timeline is not defined fails the same way (`*ErrIndexNotReady`).

### What it costs

| Configuration (one writer, 5 rounds pooled) | Median ms | p95 ms | Relative |
|----|----|----|----|
| tenant `Save` | 1.31 | 2.65 | baseline |
| tenant `Save` + index | 1.89 | 3.03 | 1.44x |
| tenant `Save` + index + guarded walk | 2.33 | 3.68 | 1.78x of the baseline |

| Configuration (4 writers, distinct entities, median of 5 rounds) | Saves/second | Relative |
|----|----|----|
| tenant `Save` | 680 | baseline |
| tenant `Save` + index | 556 | 0.82x |
| tenant `Save` + index + guarded walk | 472 | 0.69x |

The index adds about 0.6 ms to a save. The first save of an entity writes two events (the baseline and the save); every later one writes one.

What it buys is on the read side: see [How much faster is finding an old version with /ts?](#how-much-faster-is-finding-an-old-version-with-ts) at the top of this guide.

## Trade-offs side by side

| | /fsm walk | /ts index |
|---|---|---|
| Write latency | +0.3 ms on 1.3 ms | +0.6 ms on 1.3 ms |
| Write throughput | 0.75x | 0.82x |
| Read latency | No effect | Flat near 1 ms instead of growing with the history type |
| Atomic with the save | Yes: one SQLite transaction | The /ts write is not truly atomic with SQLite. xolu writes it first and undoes it if SQLite fails. A rejected commit and a refused walk both left no events (tested); a failure of the undo itself would leave an orphan, which `CheckIndex` reports |
| Where it changes behavior | A refused transition cancels the save | Tenant routes only; OQL reads fail there |
| Extra storage | One entry in the machine's history per walk | One small event per history row |
| What you operate | State machine definitions and one machine per entity | One timeline per type; `CheckIndex` on a schedule; `RebuildIndex` after enabling it on old history |
| What can go wrong | The wrong machine is walked (xolu does not check); plain saves bypass the machine | Writers configured differently; an expiring timeline; orphans and duplicates that need an administrator |
| Cost when unused | None: no `fsm_walk` is sent | None: no events are written |

## Which to use

| Situation | Use |
|-----------|-----|
| Edits must follow an approval or review flow | /fsm |
| You only need a record of who changed what, and when | Neither |
| `AsOf` is occasional and the history type holds a few thousand rows or fewer (the scan was about 3 ms at 1,000 rows) | Neither |
| `AsOf` runs inside requests, or the history type holds tens of thousands of rows (the scan was 26 ms at 10,000) | /ts |
| You already run on tenant routes | /ts: it is the only `AsOf` that is both fast and available there |
| You use `ListVersions` or `CheckHistory` and cannot move to tenant routes | Neither: /ts requires them |
| An approval flow and time-travel queries on a large history | Both |

## Using both

The options do not interact except in one commit, which is the point: the save, the history rows, the index events and the machine's move all succeed or all fail.

```go
c := xoluver.New("http://localhost:9091")
c.Tenant = "acme"
if err := c.DefineTimeIndex(ctx, 7); err != nil {
	log.Fatal(err)
}
if err := c.UseTimeIndex("asset", 7); err != nil {
	log.Fatal(err)
}

_, err := c.Save(ctx, ref, base, doc, xoluver.SaveOptions{
	SavedBy: "alice",
	Walk:    &xoluver.LifecycleWalk{MachineID: machineID, Input: "submit", Payload: map[string]any{"reviewer": "bob"}},
})
if err != nil {
	log.Fatal(err)
}
```

Measured together, a save took 1.78x the tenant baseline (2.33 ms against 1.31 ms) and throughput was 0.69x.

## Measuring on your own system

`examples/benchmark` produced every number above. It needs a xolu that serves the ordinary routes and tenant routes with /ts and /v2 on (the default tenant mode does both):

```bash
XOLU_PORT=9093 XOLU_BASE_DIR=/tmp/xolu-bench XOLU_API_V2_ENABLED=true XOLU_AUTH_TYPE=none \
  XOLU_TIMESERIES_ENABLED=true XOLU_TENANT_AUTO_REGISTER=true ./xolu
XOLU_URL=http://localhost:9093 go run ./examples/benchmark
```

The scan needs OQL, which works only on the ordinary routes, and the index needs tenant routes, so the two sides of the read comparison run on different routes of one server. The sections are selectable (`-sections 1,2,3,4`) and sizes are flags (`-rows 0,1000,10000,50000`, `-versions 10,100,1000`, `-rounds 5`, `-snapshot 500`). Write latency and throughput run each configuration once per round in rotating order, after a warm-up, so no configuration pays for being first.

The numbers here came from `-sections 1,4 -saves 150 -rounds 5 -per 60` and `-sections 2,3 -calls 100 -rows 0,1000,10000,50000`. Run it on production-like storage before deciding: on slower disks or a networked database the write costs change, and so does the share of an `AsOf` that is network.
