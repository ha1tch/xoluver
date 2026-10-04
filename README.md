# xoluver

Version 0.2.0; tested against xolu v0.30.38.

Entity versioning on xolu. Every save of an entity writes the new document and a snapshot of it in one atomic `/commit`. You can then list, read, restore and label old versions, find the version in effect at a given time (`AsOf`), and delete an entity with a record that it was deleted. It uses xolu's `/commit`, a companion history entity type, OQL and `/meta`, and has two optional features built on other xolu primitives: a lifecycle step (`/fsm`) and a version index (`/ts`). Go standard library only, no third-party dependencies.

## Documentation

| Document | Read it for |
|----------|-------------|
| [GUIDE.md](GUIDE.md) | Using xoluver: save, conflicts, history, restore, labels, delete, `AsOf`, errors, and the runnable examples. Start here |
| This README | The reference: what each call does and returns, what "does not exist" means, the version index, the xolu behavior it depends on, and the test findings |
| [docs/FSM_AND_TS.md](docs/FSM_AND_TS.md) | The two optional features: how much faster finding an old version is with /ts, how to use /fsm and /ts correctly, and what each costs |
| [docs/DESIGN.md](docs/DESIGN.md) | Why it works the way it does: the data model, the algorithms, failure behavior and the alternatives considered |
| [CHANGELOG.md](CHANGELOG.md) | What changed in each version |

| I want to | Go to |
|-----------|-------|
| Save, restore, label or delete | [GUIDE.md](GUIDE.md) |
| Know what "does not exist", "was deleted" and "no record" each claim | [What "does not exist" means](#what-does-not-exist-means) below, the guide's "Does not exist, was deleted, no record", and design section 5.7 |
| Find what an entity looked like at a past time | The guide's "Look at the past", [AsOf](#asof-what-was-it-at-time-t) below, and design section 5.8 |
| Decide whether to use /fsm or /ts, and what they cost | [docs/FSM_AND_TS.md](docs/FSM_AND_TS.md) |
| Make `AsOf` faster on a large history | [How much faster is finding an old version with /ts?](docs/FSM_AND_TS.md#how-much-faster-is-finding-an-old-version-with-ts), [Version index](#version-index-optional) below, and design section 5.9 |
| Run the tests, or a xolu that the /ts examples can use | [Run the tests](#run-the-tests) below and the guide's "Worked examples with the version index" |
| Reproduce the measurements | The last section of [docs/FSM_AND_TS.md](docs/FSM_AND_TS.md) and `examples/benchmark` |
| Understand an error or a failure mode | [Integrity check and errors](#integrity-check-and-errors) below and design section 6 |

## Files

| File | Purpose |
|------|---------|
| `xoluver.go` | The client: `Save`, `Restore`, `ListVersions`, `GetVersion`, `SetLabel`, `Labels`, plus `SaveDetailed` and `RestoreDetailed` for the optional /fsm outcome |
| `xoluver_test.go` | Integration tests against a live xolu server; skipped when `XOLU_URL` is unset |
| `xoluver_walk_test.go` | Tests for the optional /fsm support: three live-server tests, plus two unit tests that need no server |
| `xoluver_adversarial_test.go` | 20 adversarial tests: 16 against a live server, 4 unit tests with fake servers that need no server |
| `xoluver_ids_test.go` | 7 tests for deterministic history ids and the fixed-width time: 4 unit tests, 3 against a live server |
| `xoluver_delete_test.go` | 12 tests for `Delete`, tombstones and the not-found errors: 5 unit tests, 7 against a live server |
| `xoluver_asof_test.go` | 8 tests for `AsOf` and the time checks: 5 unit tests with a fake server (one is a 19-case table of semantics), 3 against a live server with explicit clocks |
| `xoluver_index_test.go` | 13 tests for tenant routing and the version index, against a fake xolu: a differential test of 9,660 `AsOf` calls comparing index and scan across seven kinds of index damage, event writing, definition checks, `CheckIndex`, `RebuildIndex`, and tenant-route OQL failures |
| `xoluver_index_live_test.go` | 8 tests against a live xolu that serves /ts on tenant routes (`XOLU_TS_URL`), including a rejected commit that must leave no index events |
| `go.mod` | Module `github.com/ha1tch/xoluver`, `go 1.25` |
| `GUIDE.md` | Short usage guide: save, conflicts, history, restore, labels, errors |
| `docs/DESIGN.md` | The design: what each piece does, why, and the xolu behaviors it rests on. Not tied to any application |
| `docs/FSM_AND_TS.md` | Guide to the two optional features: the /fsm lifecycle step and the /ts version index. What each does, how to use it correctly, and what it costs, with measured numbers |
| `examples/quickstart/main.go` | Runnable walkthrough against a live server: `XOLU_URL=http://localhost:9090 go run ./examples/quickstart` |
| `examples/retry`, `diff`, `lifecycle`, `bypass` | Four more runnable examples, one `main.go` each: concurrent writers, what changed between versions, a state-machine step, writes that skip `Save` |
| `examples/timemachine` | What an entity looked like at earlier instants, and the three kinds of "no" |
| `examples/timeindex` | The optional version index: `AsOf` answered from /ts, and a rebuild for history that predates the index (needs a tenant-and-timeseries xolu) |
| `examples/pointintime` | A month-end report across five assets with one AsOf call per cell: records, "deleted", "no record" and "not indexed" kept apart |
| `examples/windowdiff` | What changed between two instants, and what each pair of answers means (a first record is not a creation) |
| `examples/restoreasof` | Recovering from a bad edit by time: AsOf finds the version, Restore saves it as a new one; the two cases that cannot be restored |
| `examples/contention` | Four writers colliding: the index holds exactly one event per version, and AsOf at each version's own instant returns it |
| `examples/indexmaint` | A maintenance job: check the index of seven assets in seven states, rebuild what is missing, report what needs an administrator |
| `examples/benchmark` | Measures write latency, `AsOf` latency against history size, and write throughput, with and without /fsm and /ts: the program behind the numbers in `docs/FSM_AND_TS.md` |
| `examples/internal/demo/demo.go` | Plain-HTTP helpers the examples share (create and read the live entity) |
| `example_test.go` | One short godoc example per call; compiled with the tests, not run |
| `version.go`, `VERSION`, `.repoman.json` | The library version, kept in step by `repoman syncver` |
| `version_test.go` | Unit test: `Version`, `VERSION` and the README line must agree |
| `CHANGELOG.md`, `LICENSE` | Change history; Apache 2.0 |

## Run the tests

Start xolu with v2 enabled and auth off, then point the tests at it:

```bash
XOLU_PORT=9090 XOLU_BASE_DIR=/tmp/xolu-data XOLU_API_V2_ENABLED=true XOLU_AUTH_TYPE=none ./xolu
XOLU_URL=http://localhost:9090 go test -count=1 -v ./...
```

```bash
XOLU_PORT=9091 XOLU_BASE_DIR=/tmp/xolu-ts XOLU_API_V2_ENABLED=true XOLU_AUTH_TYPE=none \
  XOLU_TIMESERIES_ENABLED=true XOLU_TENANT_AUTO_REGISTER=true ./xolu
XOLU_TS_URL=http://localhost:9091 go test -count=1 -v ./...
```

Tests that need `XOLU_TS_URL` (the version index) skip without it, and tests that need `XOLU_URL` skip without that. Both can be set in one run. Tenant routes are used only by the index tests: on xolu v0.30.38 OQL cannot see tenant entities (see "Version index").

## Usage

New here? Start with the [usage guide](GUIDE.md). A runnable walkthrough is in [examples/quickstart](examples/quickstart/main.go).

```go
c := xoluver.New("http://localhost:9090")
ref := xoluver.EntityRef{Type: "asset", ID: 123}

// base is the _version the caller read before editing; doc is the complete new document.
v, err := c.Save(ctx, ref, base, doc, xoluver.SaveOptions{SavedBy: "u1", Reason: "rename"})

var conflict *xoluver.ErrVersionConflict
if errors.As(err, &conflict) {
    // someone else saved first; conflict.Current is the current version
}

list, _ := c.ListVersions(ctx, ref, 20)     // newest first, no snapshots
rec, _  := c.GetVersion(ctx, ref, 2)        // includes rec.Snapshot
v, err   = c.Restore(ctx, ref, 2, v, opt)   // saves version 2's content as a new version
_ = c.SetLabel(ctx, ref, "published", 2, "u1")
```

## Optional /fsm lifecycle

Set `SaveOptions.Walk` to walk an /fsm transition in the same commit as the save. Leave it nil and the request carries no `fsm_walk` field, so behavior is identical to the version without /fsm.

```go
res, err := c.SaveDetailed(ctx, ref, base, doc, xoluver.SaveOptions{
    SavedBy: "u1",
    Walk: &xoluver.LifecycleWalk{
        MachineID: 12,
        Input:     "submit",
        Payload:   map[string]any{"reviewer": "r1"},
    },
})
var rejected *xoluver.ErrLifecycleRejected
if errors.As(err, &rejected) {
    // guard failed, no transition for that input, or machine not found;
    // nothing was written: no document change, no snapshot, no state change
}
// res.Version is the new version; res.Walk.Previous and res.Walk.Current are the states
```

- The input is recorded on the new snapshot row as `lifecycle_input` (never on a baseline row) and returned in `VersionSummary.LifecycleInput`.
- Requires `XOLU_API_V2_ENABLED=true`.
- xolu does not check that the machine belongs to the entity. The caller passes the right machine ID.
- A transition with no document change is not a save: use xolu's normal `/walk` endpoint, which creates no version.

## What "does not exist" means

Three statements sound alike and are different claims. xoluver keeps them apart, and each error and report field names the one it makes.

| Statement | About | How xoluver knows | In the API |
|-----------|-------|-------------------|------------|
| The entity **does not exist** | Now | xolu has no entity with that type and id at this moment (a 404) | `ErrEntityNotFound`; `CheckHistory` reports `EntityExists == false` |
| The entity **was deleted** (at time D) | A past event | A tombstone row (`change_kind` `delete`) written by `Delete` | `CheckHistory` reports `Deleted`; the tombstone's `SavedAt`, `SavedBy` and `Reason` |
| There is **no record** of the entity | What xoluver holds | The entity does not exist now and the history type has no rows for it | `CheckHistory`: `EntityExists == false` and no `Versions` |
| The entity **did not exist at T** | A past instant | A tombstone shows it had been deleted by then and not saved again | `*ErrDeletedAsOf` from `AsOf` |
| There is **no record at T** | A past instant | T is earlier than the oldest history row. The entity may or may not have existed | `*ErrBeforeHistory` from `AsOf` |

xoluver cannot say that an entity **has never existed**. xolu answers 404 both for an entity that was deleted and for one that never was. An entity created and deleted outside xoluver leaves no rows, and history begins with the first save or delete that goes through xoluver. So "no record" is weaker than "never existed". An entity removed with a plain xolu `DELETE` is reported as `UntrackedDelete`: it is gone, and the history cannot say when or whether that was intended.

For versions, `ErrVersionNotFound` means xoluver holds no row for that version number. It does not mean the version never existed: the number may lie in a gap left by a write that skipped `Save`. `errors.Is(err, ErrNotFound)` matches both errors.

## AsOf: what was it at time T

`AsOf(ctx, ref, t)` returns the entity as it was at `t`: the newest history row saved at or before `t`, with its snapshot. It answers in one of four ways, and the three that are not a record are different statements:

| Answer | Statement | Evidence |
|--------|-----------|----------|
| a `VersionRecord` | The entity was in this state at `t` | The newest row saved at or before `t` |
| `*ErrDeletedAsOf` | The entity **did not exist** at `t` | A tombstone shows it had been deleted by then, and nothing was saved after it |
| `*ErrBeforeHistory` | There is **no record** at `t` | `t` is earlier than the oldest row. The entity may or may not have existed; xoluver cannot say |
| `ErrNoHistory` | There is **no history** for the entity, so nothing can be said about any time | No rows at all. An entity that was never saved through xoluver looks the same as an id that never existed |

`ErrEntityNotFound` is a different question: whether the entity exists *now*. None of the `AsOf` errors matches `ErrNotFound`.

How the answer is chosen:

| Rule | Detail |
|------|--------|
| Version order | History is read in version order, and `AsOf` stops at the first version saved after `t` |
| Inclusive | An instant equal to a row's `saved_at` includes that row. When several rows share one instant (a baseline and the save that triggered it do), the newest wins |
| Resolution | `saved_at` is nanosecond-precise, so saves within one second are told apart |
| Whose clock | Times come from the writers' clocks (`Client.Now` of whoever called `Save`), not from xolu. If clocks disagree, a later version can carry an earlier time. `AsOf` then stops before it (it answers with an older state, never a newer one), and `CheckHistory` lists the version in `OutOfOrder` |
| Baselines | A baseline row's time is when it was captured, not when the entity was created. For earlier instants an entity that existed is reported as `ErrBeforeHistory` |
| Only recorded states | `AsOf` shows states that went through `Save`, `Restore` and `Delete`. A write that skipped them leaves no row; `CheckHistory` reports the gap. Stray rows (not at their deterministic id) are ignored |
| Cost | One OQL query for the entity's history summaries (OQL scans the whole history type), then one row read by id |

## Delete

`Delete(ctx, ref, base, opt)` writes a tombstone, then removes the entity. The tombstone is one `/commit` that raises the entity's version without changing its document and appends a history row of kind `delete` holding the document as it was just before the deletion (plus a baseline row if the entity has no history, and the optional /fsm transition in `opt.Walk`). The entity is then removed with a plain xolu `DELETE`. The two steps are not atomic, so there are five states:

| State | Entity | History | `CheckHistory` |
|-------|--------|---------|----------------|
| Normal | exists | newest row is a baseline, save or restore | OK |
| Deleted | gone | newest row is a tombstone | OK, `Deleted` |
| Deletion interrupted | exists | the row for its current version is a tombstone | not OK, `PendingDelete`. `Save` and `Restore` return `ErrDeleted`. Call `Delete` again to finish |
| Removed behind xoluver's back | gone | newest row is not a tombstone | not OK, `UntrackedDelete` |
| Saved again after deletion | exists | rows after a tombstone | not OK, `Revived` |

The tombstone is written first because the other order could leave an entity gone with no record of it. History is kept after deletion. xolu removes the entity's labels with it. Other entities that xolu deletes along with this one (the response lists them) are returned in `DeleteResult.Cascaded` and get no tombstone. Ids are not reused: if something recreates an entity under a deleted entity's id, `Save` returns `ErrCorruptHistory` and writes nothing, because the new entity's versions would collide with the old one's.

## Integrity check and errors

`CheckHistory(ctx, ref)` reports duplicates, gaps, a missing row for the current `_version`, and rows ahead of the entity. It is the building block for the reconciliation job in the design (phase 4). Typed errors beyond `ErrVersionConflict` and `ErrLifecycleRejected`:

| Error | When |
|-------|------|
| `ErrNotFound` | Matches both errors below (`errors.Is`) |
| `ErrEntityNotFound` | The entity does not exist now. It may have been deleted or never created; xolu does not say which. `Save` returns it for a missing entity instead of letting xolu recreate it |
| `ErrVersionNotFound` | xoluver holds no history row for that version number |
| `ErrDeleted` | `Save` or `Restore` on an entity whose current version is a tombstone: it is deleted, or its deletion was recorded and not finished |
| `ErrDeleteIncomplete` | `Delete` recorded the tombstone but could not remove the entity. Call `Delete` again |
| `ErrDocumentTooLarge` | The commit request would exceed `Client.MaxCommitBytes` (default 1 MiB). Returned before anything is sent |
| `ErrEntityRecreated` | The entity was deleted between `Save`'s check and its commit, and xolu's compare-and-set created it. The commit cannot be undone from the client; `CheckHistory` flags the result |
| `ErrCorruptHistory` | A history row is out of place: it names another entity or version, or a stale row sits at the id of the next version (`Save` refuses and writes nothing). `CheckHistory` lists stray rows |
| `ErrVersionLimit` | The entity has used all `MaxVersion` (999,999) versions |

## How history rows are stored

Each history row is written with the id `entity id * 1,000,000 + version`. xolu refuses a second row with the same id, a row is read by id instead of found by scanning the history type, and `Save` makes no OQL query. `saved_at` looks like `2026-10-03T19:12:05.120000000Z`: UTC, nine fractional digits, always the same width, so the text sorts like the time it names (`FormatTime`, `ParseTime`).

| Limit | Value | Why |
|-------|-------|-----|
| Versions per entity | 999,999 (`MaxVersion`) | The id has six digits for the version. `Save` returns `ErrVersionLimit` |
| Entity id | 9,007,199,253 (`MaxEntityID`) | Every id must be exact in a float64, because xolu's OQL returns numbers as floats and loses digits above 2^53 |

## Behavior confirmed against xolu v0.30.38

| Item | Result |
|------|--------|
| `_version` after create | 1 |
| `_version` after a successful commit | Previous value plus 1 |
| A commit whose `update.data` is the stored document | Raises `_version` by one and changes nothing else. This is how the tombstone is written |
| A plain `DELETE` of an entity that was deleted, and of one that never existed | Both answer 404 `XOLU-ST001`: xolu cannot tell them apart |
| The `DELETE` response lists the entity itself in `cascaded_deletes`, plus anything deleted with it | `Delete` leaves the entity itself out of `DeleteResult.Cascaded` |
| Stale `update.version` | 409 `XOLU-CM001` with `current_version`; no history row written |
| Entity types with no registered schema | Accepted by `/commit` `append`; the history type is created on first write |
| 8 concurrent saves, same base, with and without existing history | Exactly one succeeds, seven conflict, no duplicate baseline or snapshot rows |
| OQL named columns, `WHERE` on two fields, `ORDER BY ... DESC` | Work |
| `fsm_walk` rejected (guard fails, no transition for the input, unknown machine) | 409 `XOLU-FSM008` "commit rolled back"; entity, history and machine state all unchanged |
| `fsm_walk` accepted | Entity, snapshot row and machine state advance together; the response carries previous and current state |
| Save with a stale base version and a walk | 409 `XOLU-CM001`; the machine does not move |
| Save without `Walk` on an entity that has a machine | The machine does not move; no `fsm_walk` key is sent |

## Findings that affect the design

| Finding | Handling |
|---------|----------|
| OQL rejects `LIMIT` (`400 XOLU-QL004 only single statements are supported`), although the xolu README shows it | `ListVersions` applies the limit client-side |
| Querying a history type that has never been written returns `400 XOLU-QL004 ... does not exist`, not 404 | Read paths treat it as "no rows" |
| The concurrency test ran on a 1-CPU machine | It shows correct behavior under interleaved requests, not under true parallelism. Run it on a multi-core runner before relying on it |

## Adversarial test findings

The suite found real problems, in the client and in xolu. Client bugs were fixed; xolu behaviors are worked around or reported by the client, and are candidates for the xolu register.

| Finding | Where | Handling |
|---------|-------|----------|
| The client dropped every top-level field starting with an underscore, which silently deleted user data (xolu stores such fields) | Client bug, fixed | Only `_version` and `id` are removed; `id` in a body is overwritten by xolu anyway |
| `/commit` with `update.version` on a missing entity does not fail: it creates the entity at `_version` 1 | xolu | `Save` checks existence first and returns `ErrNotFound`. A delete landing between the check and the commit still recreates the entity; the client returns `ErrEntityRecreated` and `CheckHistory` reports the history as ahead of the entity |
| The commit body limit applies to the whole request, and a save sends the document twice (three times when it also writes a baseline). Measured limit: about 524,000 characters of document, or about 349,000 when a baseline is needed. xolu answers an oversize commit with `400 XOLU-VL002 Invalid JSON`, not 413 | xolu | The client refuses locally with `ErrDocumentTooLarge`. The first save of a large entity can fail where later saves succeed |
| xolu stores JSON numbers as float64: integers beyond 2^53 change (`9007199254740993` is stored as `9007199254740992`), in the entity itself | xolu | Snapshots match the stored form exactly, so versioning adds no drift. Carry large identifiers as strings |
| xolu does not enforce uniqueness on the history type: a second row for one version is accepted | xolu | History rows are written with deterministic ids, so xolu itself refuses a second row for a version (409 `XOLU-CM007`, commit rolled back). A row written directly lands at another id, cannot shadow the real one, and `CheckHistory` lists it under `Misplaced` |
| OQL reads every row of a type for any filter, including `WHERE id = N` (`rows_scanned` equals the table size), so each `Save` scanned the whole history type | xolu | Rows are read by id (`GET`), and `Save` makes no OQL query |
| OQL returns numbers as float64, so integers above 2^53 lose digits, ids included | xolu | Entity ids are capped at `MaxEntityID` so every history id stays exact |
| `Engine.ExecuteWithStore` validates every OQL query against one shared validator that knows only the default store's entities, so on tenant routes OQL answers `400 XOLU-QL004 entity does not exist` for entities that do (strict or not, tenant registered or not) | xolu | `ListVersions`, `CheckHistory` and the `AsOf` scan return `ErrTenantOQL` on tenant routes instead of reading the error as "no rows". `AsOf` can use the version index; `CheckIndex` and `RebuildIndex` read history by id. A fix in xolu would remove the restriction |
| /ts is served only on tenant routes and needs the tenant provisioned; `POST ts/tl/def` on an existing timeline updates its retention until the first write | xolu | `DefineTimeIndex` provisions, defines with `retention_days -1` (no expiry), reads the definition back and refuses anything that would expire |
| `GET ts/events/latest` with a dimension prefix returns events newest key first, which for dims (entity, version) is version order, up to 10,000 | xolu | `AsOf` sorts anyway, and an entity with 10,000 events is not answered from the index |
| A commit that xolu rejects (version compare-and-set) leaves no /ts events behind | xolu | Confirmed by a live test; `CheckIndex` would show any that did |
| A /meta entry under a `label_` key with a non-object value made the whole label listing fail to decode | Client bug, fixed | Such entries are returned with `Malformed` set |
| A plain write bumps `_version` with no history row | By design (bypass) | One bypass write is healed by the next save's baseline; two in a row leave a gap, which `CheckHistory` reports |
| xolu does not check that a walked machine belongs to the entity, and a terminal machine does not stop plain saves | xolu | Policy belongs to the caller; characterized in tests |
| Deleting an entity removes its labels and keeps its history | xolu | As designed; confirmed |

The tests also cover: no lost updates under contention (6 workers, 48 increments, every snapshot holds the counter its version implies), a mixed save/restore/label race, a response lost after xolu committed (a retry is a clean conflict, not a second version), numeric ordering of 25 versions, restore edge cases, label name limits, malformed server responses, timeouts and cancellation, and inputs that must never reach the network.

All tests pass against xolu v0.30.38, also under `go test -race`. The sandbox has one CPU, so the contention tests show correct behavior under interleaving, not true parallelism.

## Version index (optional)

The Go names say "time index" (`UseTimeIndex`, `DefineTimeIndex`, `Client.TimeIndex`); this document and the others call the feature the version index. For when to use it, how to use it correctly and what it costs next to the /fsm option, see [docs/FSM_AND_TS.md](docs/FSM_AND_TS.md).

`AsOf` normally reads the entity's history with one OQL query, and OQL reads every row of the history type. With a version index it reads the entity's own /ts events instead (one query by entity), then a few history rows by id, so the cost no longer grows with the size of the whole history type. This is counted in requests, not timed.

An index is a /ts timeline with two dimensions (entity id, version) and no expiry. Every history row written by `Save`, `Restore` and `Delete` is accompanied, in the same `/commit`, by one event: the same instant as the row's `saved_at`, to the nanosecond, and one number (1 for a tombstone, 0 otherwise). The history rows stay the authority. The index is derived from them, checked against them, and rebuilt from them.

```go
c := xoluver.New("http://localhost:9091")
c.Tenant = "acme"                                   // /ts is served only on tenant routes
err := c.DefineTimeIndex(ctx, 7)                    // once: provision /ts, define timeline 7
err = c.UseTimeIndex("asset", 7)                    // index entities of type "asset" in timeline 7
rec, err := c.AsOf(ctx, ref, t)                     // same answers as without the index
n, err := c.RebuildIndex(ctx, ref)                  // index history that predates the index
rep, err := c.CheckIndex(ctx, ref)                  // compare the index with the history
```

Give each indexed type its own timeline: the entity id is a dimension, and ids repeat across types.

### How AsOf trusts the index

`AsOf` answers from the index only when the answer is exactly what the scan would give, and otherwise leaves it:

| Check | What it catches | If it fails |
|-------|-----------------|-------------|
| One event per version | Two events for a version (an interrupted commit followed by a retry) | Leave the index |
| Nothing before the first event, in a gap between events, or after the last | Events never written (history older than the index), a lost start (an expiring timeline), a lost end | Leave the index. More than 64 reads inside gaps also leaves it |
| The row the answer names exists, and has the event's time and kind | An event with no row (a commit whose /ts write was not undone) or the wrong time | Leave the index |
| The entry that ended the walk has a row with the event's time and kind | A wrong time on the first entry after the instant | Leave the index |
| Fewer than 10,000 events for the entity | A page too full to be known complete | Leave the index |

What it does not check: the times of the events before the answer are trusted. A wrong time there (from a writer that did not use xoluver) can change an answer. `CheckIndex` compares every event with its row and reports it as `Mismatched`. Leaving the index means the OQL scan, which is unavailable on tenant routes (below), so there the answer is `ErrTenantOQL`: an error, never a guess.

`IndexStats` counts calls answered from the index and calls that left it.

### Tenant routes and OQL

On xolu v0.30.38 OQL cannot see tenant entities: the query validator checks entity names against the default store, so every tenant query for a type that lives only under the tenant is answered "entity does not exist". xoluver reports this as `ErrTenantOQL` instead of reading it as an empty history.

| On tenant routes | Works |
|------------------|-------|
| `Save`, `Restore`, `Delete`, `GetVersion`, labels, the /fsm step | Yes |
| `AsOf` with an intact index | Yes: no OQL |
| `AsOf` that leaves the index, `ListVersions`, `CheckHistory` | No: `ErrTenantOQL` |
| `CheckIndex`, `RebuildIndex` | Yes, by reading history rows by id (one read per version, up to 10,000) |

### Operating the index

| Situation | What happens | What to do |
|-----------|--------------|------------|
| Index enabled on existing history | The entity's events are missing; `AsOf` leaves the index | `RebuildIndex` writes the missing events |
| A save fails with `*ErrIndexNotReady` | The timeline is not defined; nothing was written | `DefineTimeIndex` |
| A commit fails after its /ts write and the undo fails too | An event with no row (`Orphans` in `CheckIndex`) | `AsOf` refuses to answer from it. xolu removes /ts events only by time range, so an administrator purges it |
| The timeline expires events | The start of every history is lost | `DefineTimeIndex` makes the timeline permanent, and refuses one that expires. `CheckIndex` reports `NoExpiry` |
| Index and history disagree on an event's time | `Mismatched` | An administrator corrects the event |

A save of an indexed type without `Client.Tenant` fails with `ErrTenantRequired` before anything is written.

## Not included

- API-key auth (add a header in `doJSON`). Tenant routes are supported through `Client.Tenant`.
- The bypass lint and a scheduled reconciliation job (design phase 4); `CheckHistory` is the building block.
- Retention and purge.
- Application-level permission checks and audit events; `Save` and `SetLabel` are the places to call them.
- Version-list pagination beyond the client-side limit. `ListVersions` on a very large history was not benchmarked; `AsOf` and the write path were (see [docs/FSM_AND_TS.md](docs/FSM_AND_TS.md)).

## Requirements

- Go 1.25 or later
- xolu on the SQLite backend (`/commit` is not available on others), with `XOLU_API_V2_ENABLED=true` for labels (/meta) and the optional /fsm support

## License

Copyright (c) 2026 haitch

Apache 2.0 — see [LICENSE](LICENSE).
