# xoluver: entity versioning for xolu (design)

Status: implemented in xoluver 0.2.0 · Updated: 2026-10-03
Audience: developers using or extending xoluver
Basis: xolu v0.30.38. Items in §11 marked "Confirmed" were checked against a live server; the others say how to check them.

---

## 1. Summary

A versioned entity keeps its current document in its normal xolu entity row. Every save goes through xoluver, which makes one xolu call, `POST /commit`, which in a single SQLite transaction:

1. replaces the entity document with a compare-and-set on `_version`,
2. inserts a full snapshot row into a companion history entity type, and
3. optionally advances the entity's /fsm machine.

xolu supplies atomicity, version numbering, stale-write rejection, storage and queries. The application supplies policy: which entity types are versioned, who may save, restore and label, the UI, diffs, and audit events. No new xolu code is required.

| Need | Delegated to | Mechanism |
|------|--------------|-----------|
| Version number | xolu entity | The entity's own `_version` column |
| Reject stale writes | xolu `/commit` | `update.version` compare-and-set, HTTP 409 `XOLU-CM001` with `current_version` |
| Atomic "document + snapshot" | xolu `/commit` | `update` + `append` in one `BEGIN IMMEDIATE` transaction |
| Snapshot storage | xolu entities | A history entity type per versioned type (`<type>_version`) |
| History queries | xolu OQL | `POST /api/v1/oql/query` |
| Named versions ("published", "approved") | /meta | Key/value annotations on the entity subject |
| Lifecycle atomic with a save | /fsm via `/commit` | `fsm_walk` field |
| Diff, UI, permissions, audit event | The application | Application layer |

---

## 2. Goals and non-goals

Goals:

- Every save of a versioned entity produces an immutable, complete snapshot.
- Concurrent edits cannot silently overwrite each other.
- A prior version can be read, listed and restored. Restoring creates a new version; history is never rewritten.
- Specific versions can carry names such as `published`.
- A lifecycle transition can commit atomically with the save that caused it.

Non-goals for the first release:

- Field-level or delta storage (full snapshots only).
- Automatic merge of conflicting edits (the caller receives the conflict and decides).
- Retention and purge of old versions.
- Consistent point-in-time snapshots across several entities.
- Versioning of blob contents.

---

## 3. xolu primitives used, and why

| Primitive | Used for | Notes |
|-----------|----------|-------|
| Entity `_version` + `/commit` | Numbering, CAS, atomic multi-write | Core of the design |
| Entity (history type) | Snapshots | Plain SQLite rows in the same transaction as the update |
| OQL | History reads | Filtering by `entity_id` and `version` |
| /meta | Labels | Engine-inert annotations; xolu never reads them to make a decision |
| /fsm | Lifecycle | Optional `fsm_walk` inside the same commit |

Considered and not used in this design (see §9): /ts, /gen, /dxp.

---

## 4. Data model

### 4.1 The versioned entity

No change to the entity's schema. The version is xolu's own `_version` field. It is created at 1 and, per `COMMIT_ENDPOINT.md`, each commit increments it by one (the documented example goes 7 to 8; confirm in §11).

The application keeps a registry (configuration, not a xolu feature) of which entity types are versioned. Writes to a versioned type must go through the versioning service; see §6 on bypass.

### 4.2 The history entity type

One history type per versioned type, named `<type>_version` (for example `asset_version`). Register its schema in the same place and with the same mechanism the application uses for the parent type, so it ships in the same module.

| Field | Type | Description |
|-------|------|-------------|
| `id` | integer | xolu's row id, chosen by the writer: `entity_id * 1,000,000 + version` (see below) |
| `entity_id` | integer | ID of the versioned entity |
| `version` | integer | The `_version` the entity had after this save |
| `snapshot` | object | The complete document as saved, with xolu's own fields removed: `_version` and `id`. User fields that start with an underscore are kept |
| `change_kind` | string | `baseline`, `save`, `restore` or `delete` (a tombstone: the entity was deleted at `saved_at`; `snapshot` is its last document) |
| `saved_by` | string | The application's user identifier |
| `saved_at` | string | UTC, nine fractional digits, fixed width (`2026-10-03T19:12:05.120000000Z`), set by the service, so the text sorts like the time |
| `reason` | string, optional | Free text from the user |
| `restored_from` | integer, optional | Source version when `change_kind` is `restore` |
| `lifecycle_input` | string, optional | The /fsm input that was walked in the same commit, if any |

All field names and the `change_kind` values are English constants, so stored data does not depend on a user-interface language. User-entered text (`reason`) is stored as typed and is never translated.

A history row is never updated or deleted by xoluver in normal operation.

Row ids are deterministic: `entity_id * 1,000,000 + version`. xolu refuses a second row with the same id, so one row per (entity, version) is enforced by xolu itself, and a row is read by id (`GET /api/v1/<type>_version/<id>`) instead of found by an OQL scan, which reads every row of the type for any filter. Limits: 999,999 versions per entity, and entity ids up to 9,007,199,253, because every id must be exact in a float64 (xolu's OQL returns numbers as float64). A row written directly gets an auto id elsewhere, cannot shadow the real row, and is reported by `CheckHistory`.

### 4.3 Labels (/meta)

A label names one version of one entity.

- Subject: `{type}/{id}` on `/api/v2/meta`, for example `asset/123`.
- Key: `label_<name>`, matching `[a-zA-Z0-9_]{1,64}` (xolu's key rule). Example: `label_published`.
- Value (JSON): `{"version": 8, "by": "<user id>", "at": "<RFC 3339>"}`.
- No `expires_at`.

Moving a label is a `PUT` of the same key. Label names are English constants; the text shown to users goes through the application's own i18n like any other UI string.

A label has no enforcement meaning inside xolu. A rule such as "a published version cannot be edited" must be enforced by the application's service layer or by the entity's /fsm state, never by reading a label inside a guard.

### 4.4 Lifecycle (/fsm)

Versioning is orthogonal to lifecycle. A version is created when the document changes, not when state changes.

- Document change with a transition (for example submit for review): include `fsm_walk` in the same commit, so the snapshot and the state can never disagree. The walk is optional per call: omit it and the commit carries no `fsm_walk`. xolu does not check that the machine belongs to the entity, so the caller must pass the machine bound to this one.
- Document change without a transition: no `fsm_walk`.
- Transition without a document change: use the normal `/walk` endpoint. No version is created. (`/commit` requires an `update`.)

---

## 5. Operations

### 5.1 Save

Signature: `Save(ref, baseVersion, newDoc, options)`. `baseVersion` is the `_version` the caller read before editing. `newDoc` is the complete new document, because `update.data` replaces the stored document.

Steps:

1. Check permission (the application).
2. `GET` the entity.
   - If it does not exist, return not-found. (xolu's compare-and-set would otherwise create it; see §6.)
   - If its `_version` is not `baseVersion`, return the conflict immediately without writing anything.
3. Read the history row for `baseVersion` by its id (a primary-key read; 404 means there is none). If none exists, the baseline is missing, as for a freshly created entity or one that predates versioning. Keep the document from step 2 (system fields removed) for the baseline row.
4. Build one commit request:
   - `update`: `{entity, id, version: baseVersion, data: newDoc}`
   - `append[0]` (only when step 3 applies): a history row with `change_kind: "baseline"`, `version: baseVersion`, `snapshot` = the current document.
   - `append[next]`: a history row with `change_kind: "save"`, `version: baseVersion + 1`, `snapshot` = `newDoc`.
   - `fsm_walk` (optional): `{machine, input, payload}`.
5. `POST` it to the tenant-scoped commit route (`/api/v1/tenant/{tenant_id}/commit`), through the same tenant-aware client the application already uses.
6. Success (200): return `update.version` from the response. It must equal `baseVersion + 1`; if it does not, log an error, because the snapshot row would carry the wrong number.
7. Conflict (409): return a typed conflict error carrying `current_version`. Do not retry automatically. The caller re-reads and decides.
8. Emit the application's audit event for the save.

Why the baseline rule is safe: if two saves race on the same base, both may include a baseline row, but compare-and-set lets exactly one commit succeed and the other rolls back entirely, so a duplicate baseline cannot be written. The same property makes step 2's read safe without a lock.

The snapshot row and the entity document are written from the same `newDoc` object in the same request, so they cannot diverge.

### 5.2 Restore

`Restore(ref, targetVersion, baseVersion, options)`:

1. Read the history row for `targetVersion` (fail with not-found if absent).
2. Call Save with `newDoc = row.snapshot`, `change_kind: "restore"`, `restored_from: targetVersion`.

Restore is an ordinary save. It produces version N+1 whose content equals version `targetVersion`. Nothing is deleted or rewritten.

### 5.3 Reads

| Read | How |
|------|-----|
| Current document | The existing entity read, unchanged |
| Version list | OQL against `<type>_version` where `entity_id` = id, ordered by `version` descending. Apply any limit in the client; OQL rejected `LIMIT` in testing (`XOLU-QL004`). Select the summary columns only (everything except `snapshot`). A history type that has never been written returns 400 `XOLU-QL004` "does not exist"; treat it as no rows |
| One version | `GET` the row by its id. A row that names another entity or version is `ErrCorruptHistory` |
| Labels for an entity | `GET /api/v2/meta/{type}/{id}` and keep the `label_` keys |
| Diff between versions | Computed by the application from two snapshots. No xolu primitive is involved |
| State at a past instant | `AsOf` (§5.8): a record, or one of three distinct non-answers |

OQL is sent as text to `POST /api/v1/oql/query`. Build queries only from validated integers (`entity_id`, `version`). Never interpolate user-supplied strings into the query text.

### 5.4 Label

`SetLabel(ref, name, version, by)`:

1. Read the history row for `version` and fail if it does not exist (/meta does not check this itself).
2. `PUT /api/v2/meta/{type}/{id}/label_{name}` with the value from §4.3.
3. Emit the audit event.

If the commit succeeded earlier and this step fails, the system is consistent and the label call can simply be retried; the `PUT` is idempotent.

### 5.5 Creation and deletion

Creation: use the normal xolu create. No snapshot is written at creation time. The baseline rule in §5.1 adds the first snapshot on the first versioned save.

Deletion: `Delete` (§5.6) writes a tombstone and then removes the entity. History rows are kept after the entity is gone and stay readable by id. xolu removes the entity's /meta rows (labels) in the delete transaction.

### 5.6 Delete and tombstones

`Delete(ref, baseVersion, options)`:

1. `GET` the entity. If it does not exist: `ErrEntityNotFound`.
2. If the history row for the entity's current version is already a tombstone, an earlier `Delete` recorded it and did not finish: skip to step 5.
3. If the entity's version is not `baseVersion`: conflict, nothing written.
4. One `/commit`: `update` with the unchanged document and `version: baseVersion` (raises `_version` by one), `append` of a baseline row if none exists, and a tombstone row at `baseVersion + 1` (`change_kind: "delete"`, `snapshot` = the document as it was). The optional `fsm_walk` rides in this commit, so a refused transition cancels the deletion.
5. Plain xolu `DELETE`. A 404 here counts as success (a concurrent `Delete` finished first). Any other failure returns `ErrDeleteIncomplete{Version}`.

The tombstone comes first because the other order can leave an entity gone with no record of it, which is unrecoverable. `/commit` cannot delete, so the two steps cannot be atomic. The result is five states, each visible to `CheckHistory`:

| State | Entity | History | `CheckHistory` |
|-------|--------|---------|----------------|
| Normal | exists | newest row is a baseline, save or restore | OK |
| Deleted | gone | newest row is a tombstone | OK, `Deleted` |
| Deletion interrupted | exists | the row for its current version is a tombstone | not OK, `PendingDelete`. `Save` and `Restore` return `ErrDeleted`, which also closes the window in which a writer could save on top of a recorded deletion. `Delete` finishes it, with any base |
| Removed behind xoluver's back | gone | newest row is not a tombstone | not OK, `UntrackedDelete` |
| Saved again after deletion | exists | rows after a tombstone | not OK, `Revived` |

Other entities that xolu deletes along with this one (the `DELETE` response lists them) are returned in `DeleteResult.Cascaded` and get no tombstone. Ids are not reused: an entity recreated under a deleted entity's id would collide with the old rows, and `Save` returns `ErrCorruptHistory` and writes nothing. A tombstone's `snapshot` is the last state the entity had. It is not a state the entity has after the deletion, and `AsOf` (§5.8) does not return it as existing.

### 5.7 What "does not exist" means

Existence statements differ in tense and in scope, and each needs different evidence. Documentation, errors and report fields name which one they make; a bare "not found" is not used for existence.

| Statement | Tense and scope | Evidence | In the API | Status |
|-----------|-----------------|----------|------------|--------|
| The entity **does not exist** | Present: now | xolu answers 404 | `ErrEntityNotFound` | Implemented |
| The entity **was deleted** (at D) | A past event | A tombstone row | `CheckHistory.Deleted`; the tombstone's `saved_at`, `saved_by`, `reason` | Implemented |
| There is **no record** of the entity | Present, about xoluver's own data | No entity and no rows | `CheckHistory`: no entity, no `Versions` | Implemented |
| No history row for **version v** | Present, about one version | No row at the id for v | `ErrVersionNotFound` | Implemented |
| The entity **did not exist at T** | A past instant | `AsOf` finds that the newest row at or before T is a tombstone, so it was deleted at D <= T and not revived | `ErrDeletedAsOf{DeletedAt}` | Implemented |
| There is **no record at T** | A past instant | T is before the oldest row. The entity may or may not have existed; xoluver cannot say | `ErrBeforeHistory{Since}` | Implemented |
| There is **no history** for the entity | Present, about xoluver's own data, for every instant | No rows at all. An entity never saved through xoluver looks the same as an id that never existed | `ErrNoHistory` | Implemented |
| The entity **has never existed** | All time | Cannot be established | None: xoluver never claims it | Not provided |

Why the last row is not provided: xolu answers 404 both for an entity that was deleted and for one that never existed. An entity created and deleted outside xoluver leaves no rows. History begins with the first save or delete that goes through xoluver, and the baseline's `saved_at` is the time it was captured, not the time the entity was created. "No record" is therefore weaker than "never existed", and the two must not be conflated, in either direction: "there is no record" must not be reported as "it did not exist", and "it did not exist at T" is only said when a tombstone proves the entity was gone at T.

### 5.8 AsOf

`AsOf(ref, t)` answers "what was the entity at time T".

1. Read the summaries of the entity's history with one OQL query (no snapshots). Keep only rows at their deterministic id; stray rows are not part of the history.
2. Parse each row's `saved_at` (fixed width, nanoseconds). A row whose time cannot be read is `ErrCorruptHistory`.
3. If there are no rows: `ErrNoHistory`.
4. Walk the rows in version order and stop at the first row saved after `t` (the instant is inclusive). The last row visited is the answer.
5. If no row was visited: `ErrBeforeHistory{Since, SinceVersion}`, the time and version of the oldest row.
6. If the answer is a tombstone: `ErrDeletedAsOf{DeletedAt, Version}`.
7. Otherwise read that one row by its id and return it.

Why this and not "the row with the latest time at or before T": time and version can disagree when writers' clocks disagree, and version order is the order in which the entity actually changed. Walking in version order and stopping at the first row saved after T never returns a state later than T claims to be, and errs toward an older state when clocks are inconsistent. `CheckHistory` reports those versions as `OutOfOrder`, so the inconsistency is visible instead of silently shaping answers.

Decisions that follow from the existence vocabulary (§5.7):

| Situation | Answer | Why not the other statement |
|-----------|--------|-----------------------------|
| T is before the oldest row | `ErrBeforeHistory` | The entity may have existed. A baseline's `saved_at` is when it was captured, not when the entity was created, so "did not exist" would be a claim xoluver has no evidence for |
| The newest row at or before T is a tombstone | `ErrDeletedAsOf` | A tombstone is positive evidence the entity was gone. Rows saved after it (a revival) are reached normally when T is later |
| No rows at all | `ErrNoHistory` | An entity never saved through xoluver and an id that never existed look the same, so xoluver says neither "did not exist" nor "never existed" |
| T falls in a gap left by a write that skipped Save | The older recorded state | xoluver can only report recorded states. `CheckHistory` reports the gap, and a caller who needs certainty should treat a history with gaps as unreliable for AsOf |

Limits: times are the writers' clocks (`Client.Now`), not xolu's; xolu stamps no time on entity rows, so a trusted server time would need either a new xolu facility or routing every save through one service with one clock. There is no whole-type form ("all assets as of T"): `GROUP BY entity_id` with `MAX(version)` works in OQL but scans the whole history type and cannot apply the version-order rule or detect strays.

---

### 5.9 Version index (optional)

The index makes `AsOf` independent of the size of the whole history type. It is derived data: every claim it makes is checked against the history rows, and the rows stay the authority.

Shape: one /ts timeline per indexed entity type, two dimensions (entity id, version), no expiry. Each history row written by `Save`, `Restore` or `Delete` comes with one event in the same `/commit`: dims (entity id, version), time = the row's own `saved_at` instant (one `Now()` per save, so the baseline, the save row and the events agree to the nanosecond), nums [1 for a tombstone, else 0]. The Pebble leg of a commit is not truly atomic with SQLite (xolu writes /ts first and undoes it if SQLite fails), so an event can outlive a failed commit in a double failure, and an event can be missing for history that predates the index. Both are therefore checked, not assumed.

`AsOf` with an index:

1. One `ts/events/latest` query for the entity's events (at most 10,000; a full page is not trusted).
2. Build the version-ordered sequence. Two events for one version: leave the index.
3. Completeness: no history row before the first event, none in a gap between events (at most 64 reads), none after the last. That is two row reads when the events are contiguous.
4. Apply the same walk as the scan (a single shared function): stop at the first entry saved after t.
5. Read the row the answer names, and the entry that ended the walk, by id. Each must exist and have the event's time and kind. Otherwise leave the index.

"Leave the index" means use the scan, so the answer is the scan's. The residual: step 5 verifies two events, so a wrong time on an earlier event is trusted. `CheckIndex` verifies all of them (and reports `Missing`, `Orphans`, `Duplicates`, `Mismatched`, `NoExpiry`), and `RebuildIndex` writes the missing ones. xolu offers no removal of a single /ts event, so orphans and duplicates need an administrator.

Tenant routes. /ts exists only on `/api/v1/tenant/<t>/...`, so the index needs `Client.Tenant`, which maps every request (v1 and v2) to its tenant route. On xolu v0.30.38 OQL cannot see tenant entities (§11 item 18), so on tenant routes the OQL reads fail with `ErrTenantOQL` instead of looking empty: the index is the only fast and available `AsOf` there, and `CheckIndex` and `RebuildIndex` read history by id (one read per version, up to 10,000). Fixing the OQL validator in xolu lifts the restriction without any change here.

## 6. Concurrency, failure and bypass

| Situation | Result |
|-----------|--------|
| Two saves with the same base version | One returns 200; the other returns 409 with `current_version`. The loser writes nothing, including no history row |
| Validation fails (schema, cycle check) | 400, no writes. Strict mode (`XOLU_STRICT_COMMIT`, default true) runs these checks before the transaction |
| History row invalid (bad type or schema) | 400, whole commit rejected, entity unchanged |
| SQLite failure mid-commit | 500 `XOLU-CM008`, everything rolled back |
| Process crash around the commit | Atomic: either both entity and snapshot exist or neither does |
| Label write fails after a successful save | Version exists without the label. Retry the label call |
| `fsm_walk` rejected (guard fails, no transition for the input, machine not found) | 409 `XOLU-FSM008` "commit rolled back". Entity, snapshot and machine state are unchanged (confirmed on v0.30.38) |
| Backend is not SQLite | 501 `XOLU-CM009`. Versioning requires the SQLite backend |
| Entity deleted between the existence check and the commit | xolu's compare-and-set creates the entity at `_version` 1 instead of failing, and the history gets a row numbered `base + 1`. The client cannot undo it; it returns `ErrEntityRecreated`, and `CheckHistory` reports the history as ahead of the entity. Confirmed on v0.30.38 |
| Response lost after xolu committed | The caller sees a transport error. The save landed. A retry with the same base returns a conflict, never a second version; `ListVersions` shows whether it landed |
| A second row for one version | Not possible through `Save`: the id is deterministic and xolu refuses a reused id (409 `XOLU-CM007`, commit rolled back). A row written directly lands at another id, cannot shadow the real one, and `CheckHistory` reports it as misplaced |
| A stale row at the id of the next version | `Save` refuses with `ErrCorruptHistory` and writes nothing. Delete the stray row to repair. Confirmed on v0.30.38 |
| Lost race | Returns a conflict whichever xolu check trips first: the version compare-and-set (`XOLU-CM001`) or the history id that now exists (`XOLU-CM007`) |
| Delete interrupted between the tombstone and the removal | The entity exists and its current version is a tombstone. `Delete` returns `ErrDeleteIncomplete`; `Save` and `Restore` return `ErrDeleted`; `CheckHistory` reports `PendingDelete`. Calling `Delete` again finishes it. Confirmed on v0.30.38 |
| Entity removed with a plain xolu `DELETE` | No tombstone is written. `CheckHistory` reports `UntrackedDelete`; the history cannot say when or whether the deletion was intended |
| Entity recreated under a deleted entity's id | `Save` returns `ErrCorruptHistory` and writes nothing; `CheckHistory` flags it. Confirmed on v0.30.38 |
| Writers' clocks disagree | A later version carries an earlier time. `CheckHistory` lists it in `OutOfOrder`; `AsOf` stops before it and answers with an older state, never a newer one. Confirmed with two clients on v0.30.38 |
| Index damaged in any way (events missing, a lost start or end, an event with no row, two events for one version, a wrong time on the answer or the entry after it) | `AsOf` leaves the index for the scan and gives the answer the scan gives. Verified by a differential test of 9,660 calls over seven kinds of damage, and by 16 mutations of the verification code, each caught by a test |
| Index damaged and the client is on tenant routes | The scan is unavailable (OQL), so `AsOf` returns `ErrTenantOQL`: an error, never a guess |
| `/commit` rejected after its /ts write | xolu removes the events. Confirmed on v0.30.38 with a competing save and `CheckIndex` |
| A history time that cannot be read | `CheckHistory` lists the version in `BadTime`; `AsOf` returns `ErrCorruptHistory` |
| `AsOf` over a history with gaps or untracked deletes | It reports only recorded states. The gap or untracked delete is visible in `CheckHistory`, not in `AsOf` |

This design deliberately does not use the commit's `timeseries` field. Mixing a Pebble write into the commit introduces the only non-atomic path (`XOLU-CM016`), and nothing here needs it.

Bypass: xolu does not stop a client from writing a versioned entity through a plain `PUT`, `PATCH` or `POST`. Such a write bumps `_version` without a snapshot, leaving a gap. Mitigations:

- All of the application's write paths for versioned types call xoluver. Add a lint or code-review rule, and a test that fails if a handler for a versioned type calls the plain entity write.
- A reconciliation job (phase 4) checks, for each versioned entity that has any history, that a history row exists for its current `_version`, and reports gaps.

---

## 7. Service interface (illustrative)

```go
type EntityRef struct {
    Type string
    ID   int
}

type SaveOptions struct {
    SavedBy string
    Reason  string
    Walk    *LifecycleWalk // optional /fsm transition committed with the save
}

type LifecycleWalk struct {
    MachineID int
    Input     string
    Payload   map[string]any
}

type SaveResult struct{ Version int }

// ErrVersionConflict maps XOLU-CM001 / HTTP 409.
type ErrVersionConflict struct{ Current int }

type Versioner interface {
    Save(ctx context.Context, ref EntityRef, baseVersion int, doc map[string]any, opt SaveOptions) (SaveResult, error)
    Restore(ctx context.Context, ref EntityRef, target, baseVersion int, opt SaveOptions) (SaveResult, error)
    ListVersions(ctx context.Context, ref EntityRef, page Page) ([]VersionSummary, error)
    GetVersion(ctx context.Context, ref EntityRef, version int) (VersionRecord, error)
    SetLabel(ctx context.Context, ref EntityRef, name string, version int, by string) error
    Labels(ctx context.Context, ref EntityRef) ([]Label, error)
}
```

In the reference package `Save` returns the new version number, and `SaveDetailed` returns the `SaveResult` shown here, including the /fsm outcome.

Error mapping:

| xolu response | Service error |
|---------------|---------------|
| 409 `XOLU-CM001` | `ErrVersionConflict{Current}` |
| 409 `XOLU-FSM008` | `ErrLifecycleRejected` (the whole commit was rolled back) |
| 400 `XOLU-CM003` to `CM006`, schema errors | Validation error (a bug in the service if CM003 to CM006) |
| 413 `XOLU-ST007` | Document too large (see §8) |
| 501 `XOLU-CM009` | Configuration error: unsupported backend, or v2 disabled when `fsm_walk` was sent |
| 500 `XOLU-CM008` | Transient storage error; safe to retry the whole save |

---

## 8. Application integration points

| Topic | Rule |
|-------|------|
| Identifiers | History type names, field names, `change_kind` values and label keys are English constants |
| i18n | Any user-visible text (label display names, "restored from version N") goes through the application's own locale mechanism |
| Permissions | Reading history: same as reading the entity. Saving and restoring: same as updating it. Labeling: the application's decision (§12). Map onto its existing permission model; do not add a parallel one |
| Audit log | Emit through the application's audit log on save, restore and label. History rows are a content record, not a replacement for the audit trail. Confirm the event shape with whoever owns the audit log |
| Modules and seeds | Ship each history schema in the module that owns the parent type |
| Tenancy | History types and /meta rows are tenant-scoped automatically. Use the tenant-scoped client for every call |
| Size | The request limit (`XOLU_MAX_ENTITY_SIZE`, default 1 MiB) applies to the whole `/commit` body, and a save carries the document twice, three times on the first save of an entity with no history. Measured on v0.30.38: about 524,000 characters of document normally, about 349,000 when a baseline is needed. xolu answers an oversize commit with `400 XOLU-VL002 Invalid JSON`. The reference client refuses locally with `ErrDocumentTooLarge`. Large files belong in /blob and are referenced from the document |
| Configuration | `XOLU_API_V2_ENABLED=true` is required for /meta and for `fsm_walk` |

---

## 9. Alternatives considered

| Option | Why not |
|--------|---------|
| /ts as the version log | Events are numeric dims plus an opaque payload up to 64 KiB, subject to retention that can expire old versions, and written to Pebble. In `/commit` the Pebble leg is not truly atomic with SQLite (rollback is a compensating delete, with a documented double-failure case) |
| /gen sequence for version numbers | `_version` already provides a per-entity counter with compare-and-set. A sequence would add one definition per entity or give non-dense numbers, and `/gen/seq/{name}/reset` can move it backwards |
| /fsm machine as the version store | History can be discarded by machine deletion or the machine garbage collector, and payloads are not a content store. /fsm is used for lifecycle only |
| /dxp transaction | Appropriate only when a save must be atomic with a participant that enforces its own admission rules (cal, bal, loc, obj). The collapsed all-SQL path gives no more than `/commit` here. Reconsider if versioning must commit together with such a primitive |
| /meta for snapshots | Values are capped at 64 KB (default) and meta is engine-inert annotation, not a content store. Used for labels only |
| Pre-image history (store the replaced document) | Needs no baseline rule, but the stored document is supplied by the caller rather than being the exact payload of the same request |

---

## 10. Phasing

| Phase | Scope | Exit criterion |
|-------|-------|----------------|
| 1 | History schema for one pilot type; `Save`, `ListVersions`, `GetVersion`, `Restore`; conflict handling in the UI | Concurrent-save test passes on multi-core; restore round-trips a document byte-for-byte |
| 2 | `/meta` labels | Label set, move, list; invalid version rejected |
| 3 | `fsm_walk` in `Save` for entities with a bound machine (implemented in the reference package as `SaveOptions.Walk`) | A guard rejection leaves no new version |
| 4 | Reconciliation job and bypass lint (`CheckHistory` is implemented in the reference package) | Gap injected by a plain write is reported |
| Later | Retention and purge, delta storage, diff UI, /dxp participation | Separate design |

Roll out per entity type via the registry in §4.1; start with one type.

Steps added after the phases above, in the reference package:

| Step | Scope | State |
|------|-------|-------|
| 1 | Deterministic history row ids; fixed-width `saved_at` | Done |
| 2 | `Delete` with a tombstone; precise not-found errors | Done |
| 3 | `AsOf` with `ErrDeletedAsOf`, `ErrBeforeHistory`, `ErrNoHistory`; time checks in `CheckHistory` | Done |
| 4 | Optional /ts version index: indexed lookups for `AsOf`, `Client.Tenant`, `CheckIndex`, `RebuildIndex` | Done (§5.9) |

---

## 11. Verify before building

Each item is a short test or a code read against the target xolu version. Record the results in the implementation notes.

| # | Question | Why it matters |
|---|----------|----------------|
| 1 | After create, `_version` is 1; each successful commit increments it by exactly 1 (confirmed on v0.30.38) | Snapshot `version` is computed as `baseVersion + 1` |
| 2 | Which system fields appear in a returned document (`_version` and others) | The snapshot must exclude them so a restore does not write them back |
| 3 | Whether `append` and `update` accept entity types with no registered schema under strict commit (confirmed on v0.30.38: accepted, and the history type is created on first append) | Decides whether the history schema is mandatory |
| 4 | OQL: named columns, two-field filters and `ORDER BY` work on v0.30.38 and `LIMIT` fails; still to check: response time on a history type with about 10,000 rows | The version list must not return snapshots, and queries must stay fast |
| 5 | `/commit` is exposed by the Go client package, or a thin wrapper is needed | Affects the service implementation |
| 6 | (Passed on a 1-CPU sandbox, including 48 contended increments with no lost update and a run under the race detector; still open for true multi-core) Two concurrent saves with the same base on a multi-core machine: exactly one 200 and one 409, and no history row from the loser | Single-CPU runs do not exercise real races |
| 7 | Docs conflict: `API_REFERENCE.md` shows a flat `update` with `_version`, while `COMMIT_ENDPOINT.md` and the `CommitUpdate` struct use `update.version` and `update.data` | Follow the struct and `COMMIT_ENDPOINT.md` |
| 8 | (Confirmed) JSON numbers are stored as float64, so integers beyond 2^53 change in the entity itself, for example `9007199254740993` becomes `9007199254740992` | Snapshots match the stored form, so versioning adds no drift. Carry large identifiers as strings |
| 9 | (Confirmed) `/commit` with `update.version` on a missing entity creates it instead of returning 409 | Candidate for the xolu register. Worked around as in §6 |
| 10 | (Confirmed) The only fields xolu owns are `_version` and `id`; other underscore-prefixed fields are stored as user data | Stripping every underscore field would delete user data |
| 11 | (Confirmed) OQL reads every row of a type for any filter, including `WHERE id = N` (`rows_scanned` equals the table size) | Rows are read by id and written with deterministic ids; `Save` makes no OQL query |
| 12 | (Confirmed) OQL returns numbers as float64, so ids above 2^53 lose digits. A first version of the id scheme allowed entity ids up to 9.2e12 and failed here | Entity ids are capped at 9,007,199,253 |
| 13 | (Confirmed) A plain `DELETE` of an entity that was deleted and of one that never existed both answer 404 `XOLU-ST001` | xolu cannot tell "was deleted" from "never existed"; xoluver does not claim the latter |
| 14 | (Confirmed) A commit whose `update.data` is the stored document raises `_version` by one and changes nothing else | This is how the tombstone is written |
| 15 | (Confirmed) The `DELETE` response lists the entity itself in `cascaded_deletes` | `Delete` leaves it out of `DeleteResult.Cascaded` |
| 16 | (Confirmed earlier) OQL `saved_at <= 'T'`, `MAX(version)` and `GROUP BY entity_id` worked on the old one-second `saved_at`, which was also fixed width, and scanned the whole history type. Not re-run on the nanosecond format | `AsOf` does not depend on them: it reads the entity's summaries and decides client-side, to apply the version-order rule and ignore stray rows |
| 17 | (Confirmed) Saves 100 ms apart are told apart by `AsOf`; with the old second-resolution `saved_at` they were not | Why step 1 changed the time format first |
| 18 | (Confirmed) `Engine.ExecuteWithStore` validates every OQL query against one shared validator that knows only the default store's entities. On tenant routes OQL answers `400 XOLU-QL004 entity does not exist` for entities that exist, in default and strict tenant mode and for a registered tenant | Candidate for the xolu register. xoluver returns `ErrTenantOQL`; `ListVersions`, `CheckHistory` and the `AsOf` scan are unavailable on tenant routes |
| 19 | (Confirmed) /ts is served only on tenant routes; the tenant must be provisioned; a commit with `timeseries` for an undefined timeline answers `400 XOLU-CM012` | `DefineTimeIndex` provisions and defines; `Save` maps CM012 to `ErrIndexNotReady` |
| 20 | (Confirmed) `POST ts/tl/def` on an existing timeline updates its retention until the first write, and answers 409 `XOLU-TS016` for different dimensions after it | `DefineTimeIndex` defines with `retention_days -1`, reads the definition back and refuses an expiring timeline |
| 21 | (Confirmed) `GET ts/events/latest?dims=<entity>` returns events newest key first (version order for dims entity, version), at most 10,000 | An entity with 10,000 index events is not answered from the index |
| 22 | (Measured) `AsOf` over a history type growing to 50,021 rows: the scan took 1.15 ms at 21 rows and 124.13 ms at 50,021, while the index stayed near 1 ms. At 21 rows the index was only 1.2x faster, and it makes about 3.9 requests per call against the scan's 2 | The method, all the tables and the caveats are in [FSM_AND_TS.md](FSM_AND_TS.md). Not measured: snapshot sizes other than 500 bytes, a networked database, larger machines |

---

## 12. Open decisions for the application

| Decision | Default if unanswered |
|----------|-----------------------|
| Which entity types are versioned first | One pilot type chosen by the team |
| Who may restore and who may label | Restore follows update permission; labeling follows update permission |
| Retention of history after an entity is deleted | Retained indefinitely |
| Whether a published or approved state makes the entity immutable | Not enforced in phase 1; if required, enforce through /fsm state rather than labels |
| Whether users must supply a reason on save | Optional |

---

## 13. Reading list

xolu repository, at the version above:

- `docs/COMMIT_ENDPOINT.md` (request shape, CAS protocol, failure table, error codes)
- `docs/API_V2.md`, sections `/api/v2/meta` and `/api/v2/fsm`
- `docs/API_REFERENCE.md`, OQL query endpoint
- `pkg/storage/storage.go` (`CommitRequest`, `CommitUpdate`, `CommitAppend`, `CommitFsmWalk`)

This document describes the `xoluver` Go package (`github.com/ha1tch/xoluver`). The other documents:

- [README.md](../README.md): the reference, with a map of which document answers which question.
- [GUIDE.md](../GUIDE.md): how to use the package, with runnable examples.
- [FSM_AND_TS.md](FSM_AND_TS.md): choosing and using the optional /fsm and /ts features, and what they cost.
- [CHANGELOG.md](../CHANGELOG.md): what changed in each version.
