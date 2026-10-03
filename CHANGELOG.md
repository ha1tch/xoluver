# Changelog

All notable changes to xoluver are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

- **History rows have deterministic ids** (`entity id * 1,000,000 + version`). xolu refuses a second row for a version, rows are read by id, and `Save` makes no OQL query (it used to scan the whole history type on every save). Limits: `MaxVersion` 999,999 per entity and `MaxEntityID` 9,007,199,253, so every id stays exact in a float64. `ErrVersionLimit` added.
- **`saved_at` is fixed width**: UTC with nine fractional digits (`FormatTime`, `ParseTime`), so it sorts like the time it names. It had one-second resolution.
- `ListVersions` returns only rows at their deterministic id; `CheckHistory` reports the rest under `Misplaced`. `GetVersion` refuses a row that names another entity or version. A stale row at the id of the next version makes `Save` return `ErrCorruptHistory` instead of overwriting anything. A save that loses a race returns `ErrVersionConflict` whichever xolu check trips first.
- **Breaking:** the stored format changed (ids and `saved_at`). History written before this change is not read correctly.
- **`Delete` with a tombstone.** One `/commit` records a history row of kind `delete` (the last document) and raises the version, then the entity is removed. Not atomic by design: an interrupted deletion is visible (`CheckHistory.PendingDelete`), `Save` and `Restore` refuse it with `ErrDeleted`, and calling `Delete` again finishes it. `DeleteResult` returns the tombstone version, the optional /fsm outcome and any other entities xolu deleted along with this one.
- **`CheckHistory` reports deletions**: `Deleted`, `PendingDelete`, `UntrackedDelete` (removed without `Delete`) and `Revived` (rows after a tombstone). A plain xolu `DELETE` is no longer reported as healthy.
- **Precise not-found errors.** `ErrNotFound` is now the parent of `ErrEntityNotFound` (does not exist now) and `ErrVersionNotFound` (no history row for that version). None claims an entity never existed, which xolu cannot tell apart from a deleted one. The README and guide define what "does not exist", "was deleted" and "no record" each mean.
- **`AsOf(ctx, ref, t)`**: the entity as it was at `t` (the newest row saved at or before `t`, inclusive, nanosecond resolution). Three distinct non-answers: `*ErrDeletedAsOf` (did not exist: a tombstone shows it had been deleted), `*ErrBeforeHistory` (no record: the history starts later, and existence is unknown) and `ErrNoHistory` (nothing recorded). None matches `ErrNotFound`, which is about now. Reads the history summaries in version order and stops at the first version saved after `t`; times are the writers' clocks.
- **`CheckHistory` reports time problems**: `OutOfOrder` (a version saved earlier than the one before it, when writers' clocks disagree) and `BadTime` (an unreadable `saved_at`).
- `examples/timemachine`, `ExampleClient_AsOf`.

## [0.1.0] - 2026-10-03

Initial release.

- **Entity versioning on xolu primitives.** Every save is one `/commit` that replaces the entity document with a compare-and-set on `_version` and appends a full snapshot row to a `<type>_version` history type, atomically. `Save`, `Restore`, `ListVersions`, `GetVersion`. A baseline row is written with the first save of an entity that has no history.
- **Labels** (named versions) through `/meta`: `SetLabel`, `Labels`. Malformed entries are reported with `Malformed` set instead of failing the listing.
- **Optional /fsm lifecycle.** `SaveOptions.Walk` walks a state-machine transition in the same commit; when nil, no `fsm_walk` is sent. `SaveDetailed` and `RestoreDetailed` return the outcome. A rejected transition returns `ErrLifecycleRejected` and writes nothing.
- **Integrity.** `CheckHistory` reports duplicate rows, gaps, a missing row for the current version, and rows ahead of the entity. Typed errors: `ErrVersionConflict`, `ErrLifecycleRejected`, `ErrDocumentTooLarge`, `ErrEntityRecreated`, `ErrCorruptHistory`, `ErrNotFound`.
- **Workarounds for xolu behavior found by the adversarial suite:** a commit with `update.version` on a missing entity creates it (existence is checked first, and a delete in the gap is reported); the commit body limit counts the document two or three times (checked locally); JSON numbers beyond 2^53 are stored as float64 (snapshots match the stored form); the history type has no uniqueness (duplicates are detected).
- `Version` constant and `VERSION` file, kept in step by `repoman syncver`.
- Tested against xolu v0.30.38: 29 tests (8 unit, 21 against a live server), also under `go test -race`.
- Usage guide (`GUIDE.md`), godoc examples (`example_test.go`) and a runnable `examples/quickstart`.
- More runnable examples: `retry`, `diff`, `lifecycle` and `bypass`.
