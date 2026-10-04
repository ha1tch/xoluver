# xoluver usage guide

xoluver keeps a full history of every change to an entity stored in xolu. Each save writes the new document and a snapshot of it in one step, so you can list versions, read an old one, restore it, or name one ("published").

This is the usage guide. The [README](README.md) is the reference, [docs/FSM_AND_TS.md](docs/FSM_AND_TS.md) covers the two optional features (the /fsm lifecycle step and the /ts version index) and what they cost, and [docs/DESIGN.md](docs/DESIGN.md) explains the design. The README has a map of which document answers which question.

## Before you start

- A xolu server on the SQLite backend, with `XOLU_API_V2_ENABLED=true` (labels use `/meta`).
- Go 1.25 or later.
- An entity that already exists. xoluver versions entities; it does not create them. Create one with xolu's normal API. A new entity is at version 1.

```go
import "github.com/ha1tch/xoluver"

c := xoluver.New("http://localhost:9090")
ref := xoluver.EntityRef{Type: "asset", ID: 123}
```

## Save a change

Pass the version you read and the **complete** new document (not just the changed fields):

```go
newVersion, err := c.Save(ctx, ref, 3, map[string]any{"name": "pump-1b", "site": "north"},
    xoluver.SaveOptions{SavedBy: "alice", Reason: "renamed"})
```

`3` is the version you edited from. The call returns the new version, 4. To find the version of the live entity, read it with xolu's normal `GET /api/v1/asset/123`; the response carries `_version`.

## Handle a conflict

If someone saved after you read, your save is rejected and **nothing is written**:

```go
var conflict *xoluver.ErrVersionConflict
if errors.As(err, &conflict) {
    // conflict.Current is the newest version. Reload it, redo your change
    // on top, and save again with base version conflict.Current.
}
```

A retry loop is: read the entity and its version, apply your change, `Save`; on a conflict, start over.

## Read the history

```go
versions, _ := c.ListVersions(ctx, ref, 10) // newest first; 0 means no limit
for _, v := range versions {
    fmt.Println(v.Version, v.ChangeKind, v.SavedBy, v.Reason)
}

rec, err := c.GetVersion(ctx, ref, 2) // the whole document as it was at version 2
if errors.Is(err, xoluver.ErrVersionNotFound) { /* no history row for that version */ }
fmt.Println(rec.Snapshot["name"])
```

`ListVersions` leaves out the documents, so it stays small. Use `GetVersion` for one.

## Restore an old version

```go
newVersion, err := c.Restore(ctx, ref, 1, 4, xoluver.SaveOptions{SavedBy: "alice", Reason: "undo"})
```

This puts the content of version 1 back as a **new** version (5 here). History is never rewritten. The third argument, `4`, is the version you are restoring over, and a conflict applies as for `Save`.

## Name a version

```go
_ = c.SetLabel(ctx, ref, "published", 2, "alice") // setting it again moves it
labels, _ := c.Labels(ctx, ref)                   // [{Name:"published" Version:2 ...}]
```

Label names are letters, digits and underscores, up to 57 characters. A label is a note: xolu never acts on it, so a rule like "a published version is read-only" is yours to enforce.

## Delete an entity

```go
res, err := c.Delete(ctx, ref, 3, xoluver.SaveOptions{SavedBy: "alice", Reason: "decommissioned"})
// res.Version is the version of the tombstone, 4 here
```

`Delete` first records a tombstone: a history row saying the entity was deleted, holding its last document. Then it removes the entity. The two steps are not atomic. If the second fails you get `*ErrDeleteIncomplete`: the entity still exists, saving it returns `ErrDeleted`, and calling `Delete` again finishes the job. The history stays after deletion. Labels do not: xolu removes them with the entity.

## Does not exist, was deleted, no record

These are three different claims, and xoluver keeps them apart:

| Statement | About | You see |
|-----------|-------|---------|
| The entity **does not exist** | Now | `ErrEntityNotFound` |
| The entity **was deleted** at some time | A past event | `CheckHistory` reports `Deleted`; the newest history row has `ChangeKind == "delete"` and says who and when |
| There is **no record** of the entity | What xoluver holds | `CheckHistory` reports no entity and no `Versions` |
| The entity **did not exist at T** | A past instant | `*ErrDeletedAsOf` from `AsOf`: a tombstone shows it had been deleted by then |
| There is **no record at T** | A past instant | `*ErrBeforeHistory` from `AsOf`: the history starts later, and xoluver cannot say whether the entity existed |

xoluver never says an entity **has never existed**. xolu answers the same 404 for a deleted entity and for one that never was, and history starts with the first save or delete that goes through xoluver. `ErrVersionNotFound` is likewise about records: xoluver holds no row for that version number, which may be a gap, not proof the version never existed.

## Look at the past

```go
rec, err := c.AsOf(ctx, ref, time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC))

var noRecord *xoluver.ErrBeforeHistory
var deleted *xoluver.ErrDeletedAsOf
switch {
case err == nil:
    fmt.Println(rec.Snapshot["name"], rec.Version) // the entity as it was then
case errors.As(err, &deleted):
    fmt.Println("did not exist; deleted at", deleted.DeletedAt)
case errors.As(err, &noRecord):
    fmt.Println("no record before", noRecord.Since) // it may or may not have existed
case errors.Is(err, xoluver.ErrNoHistory):
    fmt.Println("nothing is recorded for this entity")
}
```

`AsOf` returns the newest version saved at or before the instant (inclusive). The three failures are different statements: "did not exist" needs a tombstone to prove it, "no record" means the history starts later, and "no history" means there is nothing at all. `examples/timemachine` shows all of them, with output:

```
before any save:         no history is recorded for the entity

before the first save:   no record: history starts at version 1, and it cannot say whether the entity existed
after the first save:    name = "pump-1b" (version 2)
after the second save:   name = "pump-1c" (version 3)
after the delete:        did not exist: it had been deleted (version 4)
an hour from now:        did not exist: it had been deleted (version 4)

saving now:              does not exist now: true
```

Times are the writers' clocks, not xolu's. If two writers' clocks disagree, `CheckHistory` reports `OutOfOrder` and `AsOf` errs toward the older state. The first version of an entity that existed before its first save is stamped with the time it was captured, so earlier instants answer "no record".

## Check the history

```go
rep, _ := c.CheckHistory(ctx, ref)
if !rep.OK() {
    fmt.Println(rep.Duplicates, rep.Gaps, rep.Misplaced, rep.OutOfOrder, rep.CurrentMissing, rep.Ahead)
}
```

This catches changes made without `Save` (see below). Run it from a periodic job if you want to be sure.

## Optional: move a state machine in the same save

If the entity has an /fsm machine, walk a transition in the same commit. If the transition is refused, nothing is saved:

```go
res, err := c.SaveDetailed(ctx, ref, 3, doc, xoluver.SaveOptions{
    SavedBy: "alice",
    Walk:    &xoluver.LifecycleWalk{MachineID: 12, Input: "submit", Payload: map[string]any{"reviewer": "bob"}},
})
// res.Version, res.Walk.Previous, res.Walk.Current
```

Leave `Walk` nil and no state machine is touched. xolu does not check that the machine belongs to this entity, so pass the right machine ID.

## Rules of thumb

| Do | Because |
|----|---------|
| Change versioned entities only through `Save` | A plain xolu `PUT` or `PATCH` raises the version with no snapshot. One such write is repaired by the next save; two in a row leave a gap that `CheckHistory` reports |
| Send the whole document | `Save` replaces the document; it does not merge |
| Keep documents under about 500 KB (about 340 KB for an entity's first save) | A save sends the document two or three times in one 1 MiB request. Larger saves fail with `ErrDocumentTooLarge` before anything is sent. Put big files in xolu's blob store |
| Send numbers above 9007199254740991 as strings | xolu stores JSON numbers as floats, so larger integers lose digits |
| Don't name an entity type ending in `_version` | That suffix is the history type |
| Delete versioned entities with `Delete`, not a plain xolu `DELETE` | A plain delete leaves no tombstone. The history stays, but cannot say the entity was deleted or when: `CheckHistory` reports `UntrackedDelete` |
| Stay under 999,999 versions per entity, and entity ids up to 9,007,199,253 | History rows get the id `entity id * 1,000,000 + version`, which must stay exact in a float64. `Save` returns `ErrVersionLimit` at the version limit |

## Errors

| Error | Meaning | What to do |
|-------|---------|------------|
| `*ErrVersionConflict` | Someone saved after you read | Reload and retry |
| `ErrEntityNotFound` | The entity does not exist now (deleted, or never created: xolu does not say which) | Check the ID; `CheckHistory` shows what the history says |
| `ErrVersionNotFound` | xoluver holds no history row for that version number | Check the number; it may be a gap |
| `ErrNotFound` | Matches both of the above (`errors.Is`) | |
| `ErrDeleted` | The entity is deleted, or its deletion was recorded and not finished | Call `Delete` to finish, or stop |
| `*ErrDeleteIncomplete` | The tombstone is recorded but removing the entity failed | Call `Delete` again |
| `*ErrDeletedAsOf` | `AsOf`: the entity did not exist at that instant; it had been deleted | Use `DeletedAt` |
| `*ErrBeforeHistory` | `AsOf`: no record at that instant; the history starts later | Use `Since`; do not read it as "did not exist" |
| `ErrNoHistory` | `AsOf`: nothing is recorded for the entity | Nothing can be said about any time |
| `ErrTenantOQL` | The read needs OQL and `Client.Tenant` is set; xolu cannot run OQL over tenant entities | Use `AsOf` with a version index, or `CheckIndex` and `RebuildIndex` |
| `ErrTenantRequired` | A version index is configured and `Client.Tenant` is not | Set `Tenant`; /ts is served only on tenant routes |
| `*ErrIndexNotReady` | The index timeline is not defined; nothing was written | `DefineTimeIndex` |
| `ErrNoIndex` | `CheckIndex` or `RebuildIndex` for a type with no index | `UseTimeIndex` |
| `*ErrDocumentTooLarge` | The save would exceed the request limit | Shrink the document |
| `*ErrLifecycleRejected` | The state-machine transition was refused; nothing saved | Fix the input or payload |
| `*ErrEntityRecreated` | The entity was deleted during the save and xolu recreated it | Rare; run `CheckHistory` and repair |
| `*ErrCorruptHistory` | A history row is out of place: it names another entity or version, or a stale row sits at the next version's id | Run `CheckHistory`, delete the stray row, retry |
| `ErrVersionLimit` | The entity has used all 999,999 versions | Start a new entity |
| `*APIError` | Any other xolu error | Read `Status`, `Code` and `Message` |

## Try it

`examples/quickstart` runs the whole flow against a server:

```
XOLU_URL=http://localhost:9090 go run ./examples/quickstart
```

```
created asset 2 at version 1
saved: now at version 2
saved: now at version 3
carol's save was rejected: the current version is 3
history:
  v3  save     by bob   moved
  v2  save     by alice renamed
  v1  baseline by alice renamed
restored version 1 as version 4
label "published" points at version 2
history ok: true (versions [1 2 3 4])
```

The `v1 baseline` row is the state before the first save. It carries the name and reason of the save that triggered it, not those of whoever created the entity.

Short code samples for each call are also in `example_test.go` and appear in `go doc`.

## More examples

Each runs like the quickstart: `XOLU_URL=http://localhost:9090 go run ./examples/<name>`.

| Example | Shows |
|---------|-------|
| `retry` | Several writers updating one entity at once with the read, change, save, retry loop. No update is lost |
| `diff` | What changed between versions, worked out from the stored snapshots |
| `lifecycle` | A save that moves a state machine, and a refused transition that leaves nothing behind (needs `XOLU_API_V2_ENABLED=true`) |
| `bypass` | What happens when something writes around xoluver, and how `CheckHistory` reports it |
| `timemachine` | What an entity looked like at earlier instants with `AsOf`, and the three kinds of "no" |
| `timeindex` | The optional version index: `AsOf` answered from /ts, and a rebuild for history that predates it (needs a tenant-and-timeseries xolu: see the README) |

`diff` prints:

```
v1 -> v2  (alice: renamed)
  ~ name: "pump-1" -> "pump-1b"
v2 -> v3  (bob: moved)
  ~ site: "north" -> "south"
  + tags: ["critical"]
v3 -> v4  (carol: retired status)
  - status: "active"
```

`bypass` prints (the plain writes are ones that skip `Save`):

```
after a normal save:                         ok=true  entity at v2, history has [1 2]
after one plain write:                       ok=false entity at v3, history has [1 2]  [no history row for the current version]
after the next save (repaired):              ok=true  entity at v4, history has [1 2 3 4]
after two plain writes and a save:           ok=false entity at v7, history has [1 2 3 4 6 7]  [missing versions [5]]
```

The examples share `examples/internal/demo`, a few plain-HTTP helpers that create and read the live entity, so each example stays short.

## Make AsOf faster with the version index (optional)

Without an index, `AsOf` makes xolu read every row of the history type. With one, it reads only that entity's /ts events. Set it up once, per entity type:

```go
c.Tenant = "acme"                    // /ts is served only on tenant routes
err := c.DefineTimeIndex(ctx, 7)     // provision /ts and define timeline 7 (no expiry)
err = c.UseTimeIndex("asset", 7)     // index entities of type "asset" in timeline 7
```

After that, `Save`, `Restore` and `Delete` write one index event per history row, in the same commit, and `AsOf` uses them. You do not change how you call `AsOf`: it checks the index against the history and falls back to the scan whenever it cannot trust it, so the answers are the same. `c.IndexStats()` tells you how often each happened.

History that existed before the index is not indexed. `c.CheckIndex(ctx, ref)` shows what is missing, and `c.RebuildIndex(ctx, ref)` writes it. `examples/timeindex` shows the whole sequence.

On xolu v0.30.38, tenant routes cannot run OQL, so on them `AsOf` without a usable index, `ListVersions` and `CheckHistory` return `ErrTenantOQL`. The README explains this and lists what works.

## Should I use /fsm or /ts?

They are independent options and answer different questions: /fsm decides whether a change may happen and keeps the entity's state (writes only), and /ts makes `AsOf` fast on a large history (writes and reads). On the machine I measured, a walk added about 25 to 30% to a save and the index about 44%, while `AsOf` stayed near 1 ms as the history type grew to 50,000 rows and the scan reached 124 ms. [docs/FSM_AND_TS.md](docs/FSM_AND_TS.md) has the setup, the rules for using each correctly, the full measurements and a table for choosing, and `examples/benchmark` reruns the measurements on your own system.

## Worked examples with the version index

These five need a xolu that serves /ts on tenant routes. Start one, then run any of them:

```bash
XOLU_PORT=9091 XOLU_BASE_DIR=/tmp/xolu-ts XOLU_API_V2_ENABLED=true XOLU_AUTH_TYPE=none \
  XOLU_TIMESERIES_ENABLED=true XOLU_TENANT_AUTO_REGISTER=true ./xolu
XOLU_URL=http://localhost:9091 XOLU_TENANT=acme go run ./examples/pointintime
```

| Example | Scenario | What it shows |
|---------|----------|---------------|
| `pointintime` | A month-end report: five assets, three month-ends | One `AsOf` per cell, every one answered from the index. The three kinds of "no" stay separate in the report, plus "not indexed" for an entity the index knows nothing about |
| `windowdiff` | "What changed between these two instants?" | Eight windows, each a different pair of answers. A first record is reported as a first record, never as a creation |
| `restoreasof` | "Put it back the way it was at 14:00" | `AsOf` finds the version in effect then, `Restore` saves it as a new version. Before the first record and after a deletion there is nothing to restore, and it says why |
| `contention` | Four writers incrementing one counter | Colliding commits (27 to 29 per run in my runs) were rejected and retried; the index still has exactly one event per version, and `AsOf` at each version's own instant returns that version |
| `indexmaint` | A maintenance job over seven assets | `CheckIndex` on each, `RebuildIndex` where events are missing, and a report of what only an administrator can fix |

`pointintime` prints, the same every run:

```
asset   Jan 31        Feb 28        Mar 31
pump-1  active        maintenance   active
pump-2  active        deleted       deleted
pump-3  no record     no record     active
pump-4  fault         active        active
pump-5  not indexed   not indexed   not indexed

12 of 15 lookups were answered from the index; 3 left it
```

`pump-3` has no record before March because xoluver first saw it then: that is not "did not exist". `pump-2` is "deleted" because a tombstone proves it. `pump-5` was never saved through xoluver, so the index holds nothing, and on tenant routes xoluver will not scan.

`indexmaint` prints, the same every run (it shows labels, not ids):

```
asset  before                         action           after
a1     ok                             -                ok
a2     ok                             -                ok
b1     missing [1 2 3]                wrote 3 events   ok
b2     missing [1 2 3]                wrote 3 events   ok
c1     orphans [99]                   -                orphans [99]
d1     duplicates [2]                 -                duplicates [2]
e1     missing [1 3]; mismatched [2]  wrote 2 events   mismatched [2]

4 of 7 healthy after maintenance; 3 need an administrator.
```

`RebuildIndex` only adds events. xolu removes /ts events only by time-range purge, so orphans, duplicates and wrong times are reported, not repaired. `contention` varies in its retry count only (27 to 29 in my runs).

## The library's own version

`xoluver.Version` holds the library version, and the `VERSION` file carries the same number. To change it, run `repoman syncver set X.Y.Z` (or `bump-patch`, `bump-minor`, `bump-major`) so the file, the constant and the README line move together.
