// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

// Package xoluver implements entity versioning on top of xolu primitives.
//
// Design: every save is one POST /api/v1/commit that, in a single SQLite
// transaction, (1) replaces the entity document with a compare-and-set on
// _version, and (2) inserts a full snapshot row into a companion history
// entity type named "<type>_version". Named versions ("labels") are /meta
// entries on the entity.
package xoluver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	typeRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,50}$`)
	labelRe = regexp.MustCompile(`^[a-zA-Z0-9_]{1,57}$`) // "label_" prefix keeps the key within xolu's 64-char limit
)

const (
	ChangeBaseline = "baseline"
	ChangeSave     = "save"
	ChangeRestore  = "restore"
	ChangeDelete   = "delete"

	labelPrefix = "label_"
)

const (
	// MaxVersion is the highest version xoluver records for one entity. History
	// rows get the deterministic id entity*historyIDStride+version, so a version
	// must fit below the stride.
	MaxVersion = historyIDStride - 1

	// MaxEntityID is the highest entity id the scheme supports. Every history
	// id must be exactly representable as a float64, because xolu's OQL returns
	// numbers as float64, so ids stay below 2^53.
	MaxEntityID = (1<<53 - 1 - MaxVersion) / historyIDStride

	historyIDStride = 1_000_000

	// TimeLayout is the layout of saved_at: UTC, nanoseconds, always the same
	// width, so the strings sort in time order.
	TimeLayout = "2006-01-02T15:04:05.000000000Z"
)

// historyID is the id of the history row for (entity, version). Rows are
// written with this id, so xolu itself refuses a second row for a version and
// a row is read by id instead of found by scanning the history type.
func historyID(entityID, version int) int { return entityID*historyIDStride + version }

// FormatTime formats t as a fixed-width UTC string that sorts like the instant
// it names. saved_at uses it.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeLayout) }

// ParseTime parses a string made by FormatTime.
func ParseTime(s string) (time.Time, error) { return time.Parse(TimeLayout, s) }

// Client talks to one xolu server.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Now     func() time.Time
	// MaxCommitBytes is the largest /commit request body the server accepts
	// (xolu's XOLU_MAX_ENTITY_SIZE, default 1 MiB). A save carries the document
	// twice, three times when it also writes a baseline row, so the largest
	// versionable document is roughly half of this (a third on the first save).
	// Saves that would exceed it fail locally with ErrDocumentTooLarge instead
	// of reaching xolu, which answers 400 "Invalid JSON" for an oversize body.
	MaxCommitBytes int
}

// New returns a Client for the given base URL, e.g. "http://localhost:9090".
func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 15 * time.Second},
		Now:     time.Now,

		MaxCommitBytes: 1 << 20,
	}
}

// EntityRef identifies one versioned entity.
type EntityRef struct {
	Type string
	ID   int
}

func (r EntityRef) validate() error {
	if !typeRe.MatchString(r.Type) {
		return fmt.Errorf("xoluver: invalid entity type %q", r.Type)
	}
	if r.ID <= 0 {
		return fmt.Errorf("xoluver: invalid entity id %d", r.ID)
	}
	if r.ID > MaxEntityID {
		return fmt.Errorf("xoluver: entity id %d is above the supported maximum %d", r.ID, MaxEntityID)
	}
	if strings.HasSuffix(r.Type, "_version") {
		// "<type>_version" is the history type of <type>; versioning a history
		// type would make its own history type ambiguous.
		return fmt.Errorf("xoluver: entity type %q is reserved for history rows", r.Type)
	}
	return nil
}

func (r EntityRef) historyType() string { return r.Type + "_version" }

// SaveOptions carries the audit fields stored on the history row and, if Walk
// is set, an /fsm transition to commit atomically with the save.
type SaveOptions struct {
	SavedBy string
	Reason  string
	// Walk is optional. When nil (the default) the commit contains no
	// fsm_walk and versioning behaves exactly as without /fsm. When set, the
	// transition runs in the same transaction as the document write and the
	// snapshot row; if the transition is rejected, nothing is written.
	// The machine is not checked to belong to the entity; the caller owns that.
	Walk *LifecycleWalk
}

// LifecycleWalk is one /fsm transition to walk inside the save's commit.
// Requires XOLU_API_V2_ENABLED=true on the server.
type LifecycleWalk struct {
	MachineID int
	Input     string
	Payload   map[string]any
}

func (w *LifecycleWalk) validate() error {
	if w.MachineID <= 0 {
		return fmt.Errorf("xoluver: invalid machine id %d", w.MachineID)
	}
	if w.Input == "" {
		return errors.New("xoluver: lifecycle input must not be empty")
	}
	return nil
}

// WalkOutcome is the machine's result for a walk committed with a save.
type WalkOutcome struct {
	MachineID int
	Previous  string
	Current   string
	Terminal  bool
}

// SaveResult is the outcome of SaveDetailed and RestoreDetailed.
type SaveResult struct {
	Version int
	Walk    *WalkOutcome // nil unless SaveOptions.Walk was set
}

// ErrLifecycleRejected is returned when the /fsm transition in SaveOptions.Walk
// was refused (guard failed, no transition for that input, machine not found).
// The whole commit was rolled back: no document change, no snapshot row.
type ErrLifecycleRejected struct {
	Code    string
	Message string
}

func (e *ErrLifecycleRejected) Error() string {
	return fmt.Sprintf("xoluver: lifecycle transition rejected (%s): %s", e.Code, e.Message)
}

// VersionSummary is one history row without its snapshot.
type VersionSummary struct {
	Version      int
	ChangeKind   string
	SavedBy      string
	SavedAt      string
	Reason       string
	RestoredFrom int
	// LifecycleInput is the /fsm input walked in the same commit, if any.
	LifecycleInput string
}

// VersionRecord is a history row including the full snapshot.
type VersionRecord struct {
	VersionSummary
	Snapshot map[string]any
}

// Label names one version of one entity.
type Label struct {
	Name    string
	Version int
	By      string
	At      string
	// Malformed is true when the stored value is not a JSON object (written by
	// something other than SetLabel). Version is 0 in that case.
	Malformed bool
}

// ErrVersionConflict is returned when the caller's base version is stale.
type ErrVersionConflict struct{ Current int }

func (e *ErrVersionConflict) Error() string {
	return fmt.Sprintf("xoluver: version conflict, current version is %d", e.Current)
}

// The "not found" errors say what is missing and when. errors.Is(err,
// ErrNotFound) matches all of them; ErrEntityNotFound and ErrVersionNotFound
// tell them apart.
//
// None of them says an entity has never existed. xolu answers 404 both for an
// entity that was deleted and for one that never was, and xoluver can only
// speak for the history it holds. See the README, "What does not exist mean".

// ErrNotFound is the parent of ErrEntityNotFound and ErrVersionNotFound.
var ErrNotFound = errors.New("xoluver: not found")

// ErrEntityNotFound: the entity does not exist now. xolu has no entity with that
// type and id at this moment. It may have been deleted or never created, and
// xolu does not say which; CheckHistory reports what the history says.
var ErrEntityNotFound = fmt.Errorf("%w: the entity does not exist now", ErrNotFound)

// ErrVersionNotFound: xoluver holds no history row for that version. The
// version may be beyond the entity's current version, may sit in a gap left by a
// write that skipped Save, or may be outside 1..MaxVersion.
var ErrVersionNotFound = fmt.Errorf("%w: no history row for that version", ErrNotFound)

// ErrDeleted is returned by Save and Restore when the entity's current version
// is a deletion tombstone: the entity is deleted, or its deletion was recorded
// and not finished. Calling Delete finishes it.
var ErrDeleted = errors.New("xoluver: the entity is deleted or its deletion is not finished")

// ErrVersionLimit is returned when an entity has reached MaxVersion.
var ErrVersionLimit = errors.New("xoluver: entity has reached the maximum version")

// ErrDocumentTooLarge is returned before anything is sent when the commit
// request would exceed Client.MaxCommitBytes.
type ErrDocumentTooLarge struct{ Bytes, Limit int }

func (e *ErrDocumentTooLarge) Error() string {
	return fmt.Sprintf("xoluver: commit request is %d bytes, limit %d (the document is sent two or three times)", e.Bytes, e.Limit)
}

// ErrEntityRecreated is returned when the entity did not exist at commit time.
// xolu's compare-and-set creates a missing entity instead of failing, so if the
// entity is deleted between Save's existence check and its commit, the commit
// succeeds as a create: the entity now exists at _version Version and the
// history holds a row numbered Expected. The commit cannot be undone from here;
// the caller must repair (delete the entity, or reconcile the history row).
type ErrEntityRecreated struct{ Version, Expected int }

func (e *ErrEntityRecreated) Error() string {
	return fmt.Sprintf("xoluver: entity was recreated by the commit at version %d (history row says %d); repair required", e.Version, e.Expected)
}

// ErrCorruptHistory is returned when history violates its invariants, for
// example two rows for one (entity, version). xolu does not enforce uniqueness
// on the history type, so a direct write can create this.
type ErrCorruptHistory struct{ Version, Rows int }

func (e *ErrCorruptHistory) Error() string {
	return fmt.Sprintf("xoluver: history is inconsistent at version %d (%d conflicting row(s))", e.Version, e.Rows)
}

// APIError is any other non-2xx response from xolu.
type APIError struct {
	Status         int
	Code           string
	Message        string
	CurrentVersion int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("xolu %d %s: %s", e.Status, e.Code, e.Message)
}

// ---- Save and Restore ------------------------------------------------------

// Save writes doc as the new version of ref. baseVersion is the _version the
// caller read before editing; doc is the complete new document.
// It returns the new version number, or *ErrVersionConflict if baseVersion
// is stale. On any error nothing is written.
func (c *Client) Save(ctx context.Context, ref EntityRef, baseVersion int, doc map[string]any, opt SaveOptions) (int, error) {
	res, err := c.SaveDetailed(ctx, ref, baseVersion, doc, opt)
	return res.Version, err
}

// SaveDetailed is Save, but also returns the /fsm outcome when opt.Walk is set.
func (c *Client) SaveDetailed(ctx context.Context, ref EntityRef, baseVersion int, doc map[string]any, opt SaveOptions) (SaveResult, error) {
	return c.save(ctx, ref, baseVersion, doc, opt, ChangeSave, 0)
}

// Restore saves the content of targetVersion as a new version. History is
// never rewritten.
func (c *Client) Restore(ctx context.Context, ref EntityRef, targetVersion, baseVersion int, opt SaveOptions) (int, error) {
	res, err := c.RestoreDetailed(ctx, ref, targetVersion, baseVersion, opt)
	return res.Version, err
}

// RestoreDetailed is Restore, but also returns the /fsm outcome when opt.Walk is set.
func (c *Client) RestoreDetailed(ctx context.Context, ref EntityRef, targetVersion, baseVersion int, opt SaveOptions) (SaveResult, error) {
	rec, err := c.GetVersion(ctx, ref, targetVersion)
	if err != nil {
		return SaveResult{}, err
	}
	return c.save(ctx, ref, baseVersion, rec.Snapshot, opt, ChangeRestore, targetVersion)
}

// DeleteResult describes a completed deletion.
type DeleteResult struct {
	// Version is the version number of the tombstone row: the entity's final
	// _version.
	Version int
	// Walk is the /fsm outcome when SaveOptions.Walk was set.
	Walk *WalkOutcome
	// Cascaded lists other entities ("type:id") that xolu deleted along with this
	// one. xoluver records no tombstone for them.
	Cascaded []string
}

// ErrDeleteIncomplete is returned by Delete when the tombstone was recorded but
// removing the entity failed. The entity still exists at Version; call Delete
// again to finish.
type ErrDeleteIncomplete struct {
	Version int
	Err     error
}

func (e *ErrDeleteIncomplete) Error() string {
	return fmt.Sprintf("xoluver: deletion recorded at version %d but the entity was not removed: %v", e.Version, e.Err)
}

func (e *ErrDeleteIncomplete) Unwrap() error { return e.Err }

// Delete records that the entity was deleted, then removes it.
//
// It first writes a tombstone: one /commit that raises the entity's version
// without changing its document and appends a history row of kind "delete" that
// holds the document as it was just before the deletion (plus a baseline row if
// the entity has no history yet, and the /fsm transition in opt.Walk if set).
// Only then does it remove the entity with a plain xolu DELETE.
//
// The two steps are not atomic. If the second fails, Delete returns
// *ErrDeleteIncomplete: the entity still exists, Save refuses it with
// ErrDeleted, and calling Delete again finishes the job.
//
// baseVersion is the version you read, as for Save; a stale one returns
// *ErrVersionConflict and writes nothing. A tombstone is the history's
// statement "this entity was deleted at this time". The document in it is the
// last state the entity had, not a state it has after the deletion.
//
// xolu removes the entity's /meta rows (labels) with it, and xoluver does not
// record tombstones for other entities xolu deletes along with this one (see
// DeleteResult.Cascaded). Ids are not reused: saving an entity that returns
// under the id of a deleted one is refused with ErrCorruptHistory.
func (c *Client) Delete(ctx context.Context, ref EntityRef, baseVersion int, opt SaveOptions) (DeleteResult, error) {
	if err := ref.validate(); err != nil {
		return DeleteResult{}, err
	}
	if baseVersion < 1 {
		return DeleteResult{}, fmt.Errorf("xoluver: invalid base version %d", baseVersion)
	}
	cur, ver, err := c.getEntity(ctx, ref)
	if err != nil {
		return DeleteResult{}, err
	}
	row, err := c.getHistoryRow(ctx, ref, ver)
	switch {
	case err == nil && asString(row["change_kind"]) == ChangeDelete:
		// An earlier Delete recorded the tombstone and did not finish. Finish it.
		return c.finishDelete(ctx, ref, ver, nil)
	case err != nil && !errors.Is(err, ErrVersionNotFound):
		return DeleteResult{}, err
	}
	if ver != baseVersion {
		return DeleteResult{}, &ErrVersionConflict{Current: ver}
	}
	res, err := c.save(ctx, ref, baseVersion, cur, opt, ChangeDelete, 0)
	if err != nil {
		return DeleteResult{}, err
	}
	return c.finishDelete(ctx, ref, res.Version, res.Walk)
}

// finishDelete removes the entity after its tombstone is recorded.
func (c *Client) finishDelete(ctx context.Context, ref EntityRef, version int, walk *WalkOutcome) (DeleteResult, error) {
	var resp struct {
		Cascaded []string `json:"cascaded_deletes"`
	}
	path := fmt.Sprintf("/api/v1/%s/%d", ref.Type, ref.ID)
	if _, err := c.doJSON(ctx, http.MethodDelete, path, nil, &resp); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			// Already removed, for example by a concurrent Delete finishing
			// the same job: the deletion is complete.
			return DeleteResult{Version: version, Walk: walk}, nil
		}
		return DeleteResult{}, &ErrDeleteIncomplete{Version: version, Err: err}
	}
	res := DeleteResult{Version: version, Walk: walk}
	self := fmt.Sprintf("%s:%d", ref.Type, ref.ID)
	for _, e := range resp.Cascaded {
		if e != self { // xolu lists the entity itself too
			res.Cascaded = append(res.Cascaded, e)
		}
	}
	return res, nil
}

type commitReq struct {
	Update  commitUpdate   `json:"update"`
	Append  []commitAppend `json:"append"`
	FsmWalk *commitWalk    `json:"fsm_walk,omitempty"` // nil pointer: field omitted entirely
}

type commitWalk struct {
	Machine int            `json:"machine"`
	Input   string         `json:"input"`
	Payload map[string]any `json:"payload,omitempty"`
}

type commitUpdate struct {
	Entity  string         `json:"entity"`
	ID      int            `json:"id"`
	Version int            `json:"version"`
	Data    map[string]any `json:"data"`
}

type commitAppend struct {
	Entity string         `json:"entity"`
	ID     int            `json:"id,omitempty"`
	Data   map[string]any `json:"data"`
}

type commitResp struct {
	Update struct {
		Version int  `json:"version"`
		Created bool `json:"created"`
	} `json:"update"`
	FsmWalk *struct {
		Machine  int    `json:"machine"`
		Previous string `json:"previous"`
		Current  string `json:"current"`
		Terminal bool   `json:"terminal"`
	} `json:"fsm_walk"`
}

func (c *Client) save(ctx context.Context, ref EntityRef, base int, doc map[string]any, opt SaveOptions, kind string, restoredFrom int) (SaveResult, error) {
	if err := ref.validate(); err != nil {
		return SaveResult{}, err
	}
	if base < 1 {
		return SaveResult{}, fmt.Errorf("xoluver: invalid base version %d", base)
	}
	if base+1 > MaxVersion {
		return SaveResult{}, ErrVersionLimit
	}
	if doc == nil {
		return SaveResult{}, errors.New("xoluver: nil document")
	}
	var walk *commitWalk
	lifecycleInput := ""
	if opt.Walk != nil {
		if err := opt.Walk.validate(); err != nil {
			return SaveResult{}, err
		}
		walk = &commitWalk{Machine: opt.Walk.MachineID, Input: opt.Walk.Input, Payload: opt.Walk.Payload}
		lifecycleInput = opt.Walk.Input
	}
	newDoc := stripSystem(doc)

	// Look at the entity first. A missing entity must not be saved: xolu's
	// compare-and-set creates it instead of failing (see ErrEntityRecreated).
	// A stale base is also rejected here without writing anything.
	cur, curVer, err := c.getEntity(ctx, ref)
	if err != nil {
		return SaveResult{}, err
	}
	if curVer != base {
		return SaveResult{}, &ErrVersionConflict{Current: curVer}
	}

	// Baseline rule: if there is no history row for the version being replaced
	// (new entity, or one that predates versioning), add it in the same commit.
	// If two saves race, compare-and-set lets only one commit succeed, so a
	// duplicate baseline cannot be written.
	var appends []commitAppend
	row, err := c.getHistoryRow(ctx, ref, base)
	has := err == nil
	if err != nil && !errors.Is(err, ErrVersionNotFound) {
		return SaveResult{}, err
	}
	if has && asString(row["change_kind"]) == ChangeDelete {
		return SaveResult{}, ErrDeleted
	}
	if !has {
		appends = append(appends, c.historyRow(ref, base, ChangeBaseline, cur, opt, 0, ""))
	}
	appends = append(appends, c.historyRow(ref, base+1, kind, newDoc, opt, restoredFrom, lifecycleInput))

	req := commitReq{
		Update:  commitUpdate{Entity: ref.Type, ID: ref.ID, Version: base, Data: newDoc},
		Append:  appends,
		FsmWalk: walk,
	}
	if err := c.checkSize(req); err != nil {
		return SaveResult{}, err
	}

	var resp commitResp
	_, err = c.doJSON(ctx, http.MethodPost, "/api/v1/commit", req, &resp)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) {
			switch {
			case ae.Status == http.StatusConflict && ae.Code == "XOLU-CM001":
				return SaveResult{}, &ErrVersionConflict{Current: ae.CurrentVersion}
			case ae.Code == "XOLU-CM007":
				// A history row with that id already exists. If the entity has
				// moved on, someone saved this version first: a conflict. If it
				// has not, the row is stale (left by a recreated entity, or
				// written directly) and the history needs repair.
				if _, cur, gerr := c.getEntity(ctx, ref); gerr == nil && cur != base {
					return SaveResult{}, &ErrVersionConflict{Current: cur}
				}
				return SaveResult{}, &ErrCorruptHistory{Version: base + 1, Rows: 1}
			case ae.Code == "XOLU-FSM008":
				return SaveResult{}, &ErrLifecycleRejected{Code: ae.Code, Message: ae.Message}
			}
		}
		return SaveResult{}, err
	}
	res := SaveResult{Version: resp.Update.Version}
	if resp.Update.Created {
		return res, &ErrEntityRecreated{Version: resp.Update.Version, Expected: base + 1}
	}
	if resp.Update.Version != base+1 {
		// The snapshot row was written with base+1; any other result means the
		// history now carries a wrong number. This should be impossible.
		return res, fmt.Errorf("xoluver: xolu returned version %d, expected %d", resp.Update.Version, base+1)
	}
	if opt.Walk != nil {
		if resp.FsmWalk == nil {
			return res, errors.New("xoluver: commit succeeded but response carries no fsm_walk result")
		}
		res.Walk = &WalkOutcome{
			MachineID: resp.FsmWalk.Machine,
			Previous:  resp.FsmWalk.Previous,
			Current:   resp.FsmWalk.Current,
			Terminal:  resp.FsmWalk.Terminal,
		}
	}
	return res, nil
}

func (c *Client) checkSize(req commitReq) error {
	limit := c.MaxCommitBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if len(b) >= limit {
		return &ErrDocumentTooLarge{Bytes: len(b), Limit: limit}
	}
	return nil
}

func (c *Client) historyRow(ref EntityRef, version int, kind string, snapshot map[string]any, opt SaveOptions, restoredFrom int, lifecycleInput string) commitAppend {
	data := map[string]any{
		"entity_id":   ref.ID,
		"version":     version,
		"snapshot":    snapshot,
		"change_kind": kind,
		"saved_by":    opt.SavedBy,
		"saved_at":    FormatTime(c.Now()),
	}
	if opt.Reason != "" {
		data["reason"] = opt.Reason
	}
	if restoredFrom > 0 {
		data["restored_from"] = restoredFrom
	}
	if lifecycleInput != "" {
		data["lifecycle_input"] = lifecycleInput
	}
	return commitAppend{Entity: ref.historyType(), ID: historyID(ref.ID, version), Data: data}
}

// ---- Reads -----------------------------------------------------------------

// ListVersions returns history rows for ref, newest first, without snapshots.
// limit <= 0 means no limit. (Applied client-side: xolu's OQL rejected LIMIT
// in testing, see README.)
func (c *Client) ListVersions(ctx context.Context, ref EntityRef, limit int) ([]VersionSummary, error) {
	if err := ref.validate(); err != nil {
		return nil, err
	}
	rows, err := c.historyRows(ctx, ref)
	if err != nil {
		return nil, err
	}
	out := make([]VersionSummary, 0, len(rows))
	for _, r := range rows {
		// Only the row at the deterministic id is the history; any other row
		// for this entity is reported by CheckHistory.
		if asInt(r["id"]) != historyID(ref.ID, asInt(r["version"])) {
			continue
		}
		out = append(out, summaryFrom(r))
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

// GetVersion returns one history row including its snapshot.
func (c *Client) GetVersion(ctx context.Context, ref EntityRef, version int) (VersionRecord, error) {
	if err := ref.validate(); err != nil {
		return VersionRecord{}, err
	}
	if version < 1 || version > MaxVersion {
		return VersionRecord{}, ErrVersionNotFound
	}
	row, err := c.getHistoryRow(ctx, ref, version)
	if err != nil {
		return VersionRecord{}, err
	}
	// The row at the deterministic id must be the row it claims to be.
	if asInt(row["entity_id"]) != ref.ID || asInt(row["version"]) != version {
		return VersionRecord{}, &ErrCorruptHistory{Version: version, Rows: 1}
	}
	snap, _ := row["snapshot"].(map[string]any)
	return VersionRecord{VersionSummary: summaryFrom(row), Snapshot: snap}, nil
}

// getHistoryRow reads the row for (ref, version) by its deterministic id: a
// primary-key read, not a scan. A missing row is ErrVersionNotFound.
func (c *Client) getHistoryRow(ctx context.Context, ref EntityRef, version int) (map[string]any, error) {
	var row map[string]any
	path := fmt.Sprintf("/api/v1/%s/%d", ref.historyType(), historyID(ref.ID, version))
	if _, err := c.doJSON(ctx, http.MethodGet, path, nil, &row); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil, ErrVersionNotFound
		}
		return nil, err
	}
	return row, nil
}

// historyRows returns every row of the history type that names this entity,
// newest first, including rows that are not at their deterministic id.
func (c *Client) historyRows(ctx context.Context, ref EntityRef) ([]map[string]any, error) {
	q := fmt.Sprintf("SELECT id, version, change_kind, saved_by, saved_at, reason, restored_from, lifecycle_input FROM %s WHERE entity_id = %d ORDER BY version DESC",
		ref.historyType(), ref.ID)
	return c.oql(ctx, q)
}

func (c *Client) getEntity(ctx context.Context, ref EntityRef) (doc map[string]any, version int, err error) {
	var raw map[string]any
	_, err = c.doJSON(ctx, http.MethodGet, fmt.Sprintf("/api/v1/%s/%d", ref.Type, ref.ID), nil, &raw)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil, 0, ErrEntityNotFound
		}
		return nil, 0, err
	}
	return stripSystem(raw), asInt(raw["_version"]), nil
}

// ErrNoHistory is returned by AsOf when xoluver holds no history for the entity
// at all, so it can say nothing about any time. This is not the same as "the
// entity never existed": an entity that was created and deleted outside
// xoluver leaves no rows either.
var ErrNoHistory = errors.New("xoluver: no history is recorded for the entity, so nothing can be said about any time")

// ErrBeforeHistory is returned by AsOf when the instant asked about is earlier
// than the oldest history row. There is no record at At. The history starts at
// Since, and xoluver cannot say whether the entity existed before that. This is
// not the same as "the entity did not exist at At".
type ErrBeforeHistory struct {
	At           time.Time // the instant asked about
	Since        time.Time // when the oldest history row was saved
	SinceVersion int       // the version of that row
}

func (e *ErrBeforeHistory) Error() string {
	return fmt.Sprintf("xoluver: no record at %s: the history starts at %s (version %d), and xoluver cannot say whether the entity existed earlier",
		FormatTime(e.At), FormatTime(e.Since), e.SinceVersion)
}

// ErrDeletedAsOf is returned by AsOf when the entity did not exist at the
// instant asked about, because a tombstone shows it had been deleted by then
// and not saved again since.
type ErrDeletedAsOf struct {
	At        time.Time // the instant asked about
	DeletedAt time.Time // when the tombstone was saved
	Version   int       // the tombstone's version
}

func (e *ErrDeletedAsOf) Error() string {
	return fmt.Sprintf("xoluver: the entity did not exist at %s: it had been deleted at %s (version %d)",
		FormatTime(e.At), FormatTime(e.DeletedAt), e.Version)
}

// AsOf returns the entity as it was at t: the newest history row saved at or
// before t, with its snapshot.
//
// Instead of a record it can return three different statements:
//
//   - *ErrDeletedAsOf: the entity did not exist at t. A tombstone shows it had
//     been deleted by then.
//   - *ErrBeforeHistory: there is no record at t. The history starts later, and
//     xoluver cannot say whether the entity existed at t. This is not "did not
//     exist".
//   - ErrNoHistory: xoluver holds no history for the entity at all, so it can
//     say nothing about any time. This is not "never existed".
//
// How the answer is chosen: the history is read in version order and AsOf stops
// at the first version saved after t. t is inclusive, and when several versions
// share one time the newest wins. Times come from the writers' clocks (the
// Client.Now of whoever called Save), not from xolu. If clocks disagree, a later
// version can carry an earlier time; AsOf then stops before it, and
// CheckHistory reports it in OutOfOrder. The time of a baseline row is when it
// was captured, not when the entity was created, so for instants before it an
// entity that existed earlier is reported as ErrBeforeHistory.
//
// AsOf shows only states that went through Save, Restore and Delete. A write
// that skipped them leaves no row, and CheckHistory reports such gaps.
//
// It reads the summaries of the entity's history (OQL scans the whole history
// type), then one row by id.
func (c *Client) AsOf(ctx context.Context, ref EntityRef, t time.Time) (VersionRecord, error) {
	if err := ref.validate(); err != nil {
		return VersionRecord{}, err
	}
	rows, err := c.historyRows(ctx, ref)
	if err != nil {
		return VersionRecord{}, err
	}
	type entry struct {
		version int
		kind    string
		at      time.Time
	}
	var seq []entry
	for _, r := range rows {
		v := asInt(r["version"])
		if asInt(r["id"]) != historyID(ref.ID, v) {
			continue // a stray row is not part of the history
		}
		at, err := ParseTime(asString(r["saved_at"]))
		if err != nil {
			return VersionRecord{}, &ErrCorruptHistory{Version: v, Rows: 1}
		}
		seq = append(seq, entry{version: v, kind: asString(r["change_kind"]), at: at})
	}
	if len(seq) == 0 {
		return VersionRecord{}, ErrNoHistory
	}
	sort.Slice(seq, func(i, j int) bool { return seq[i].version < seq[j].version })

	last := -1
	for i, e := range seq {
		if e.at.After(t) {
			break
		}
		last = i
	}
	switch {
	case last < 0:
		return VersionRecord{}, &ErrBeforeHistory{At: t.UTC(), Since: seq[0].at, SinceVersion: seq[0].version}
	case seq[last].kind == ChangeDelete:
		return VersionRecord{}, &ErrDeletedAsOf{At: t.UTC(), DeletedAt: seq[last].at, Version: seq[last].version}
	}
	return c.GetVersion(ctx, ref, seq[last].version)
}

// ---- Labels (/meta) --------------------------------------------------------

// SetLabel points a named label at a version. The version must exist in
// history (xolu's /meta does not check this itself). Setting the same name
// again moves the label.
func (c *Client) SetLabel(ctx context.Context, ref EntityRef, name string, version int, by string) error {
	if err := ref.validate(); err != nil {
		return err
	}
	if !labelRe.MatchString(name) {
		return fmt.Errorf("xoluver: invalid label name %q", name)
	}
	if _, err := c.GetVersion(ctx, ref, version); err != nil {
		return err
	}
	body := map[string]any{"value": map[string]any{
		"version": version,
		"by":      by,
		"at":      FormatTime(c.Now()),
	}}
	_, err := c.doJSON(ctx, http.MethodPut, fmt.Sprintf("/api/v2/meta/%s/%d/%s%s", ref.Type, ref.ID, labelPrefix, name), body, nil)
	return err
}

// Labels lists the labels set on ref. Entries under other keys are ignored.
// A label_ entry whose value is not a JSON object is returned with Malformed
// set instead of failing the whole listing.
func (c *Client) Labels(ctx context.Context, ref EntityRef) ([]Label, error) {
	if err := ref.validate(); err != nil {
		return nil, err
	}
	var resp struct {
		Entries []struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		} `json:"entries"`
	}
	if _, err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("/api/v2/meta/%s/%d", ref.Type, ref.ID), nil, &resp); err != nil {
		return nil, err
	}
	var out []Label
	for _, e := range resp.Entries {
		if !strings.HasPrefix(e.Key, labelPrefix) {
			continue
		}
		l := Label{Name: strings.TrimPrefix(e.Key, labelPrefix)}
		var v map[string]any
		dec := json.NewDecoder(bytes.NewReader(e.Value))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil || v == nil {
			l.Malformed = true
		} else {
			l.Version = asInt(v["version"])
			l.By = asString(v["by"])
			l.At = asString(v["at"])
		}
		out = append(out, l)
	}
	return out, nil
}

// HistoryReport describes the integrity of one entity's history.
type HistoryReport struct {
	EntityExists   bool
	CurrentVersion int   // 0 when the entity does not exist
	Versions       []int // distinct versions present, ascending
	Duplicates     []int // versions with more than one row
	Gaps           []int // versions missing between the lowest and highest present
	Misplaced      []int // ids of rows not at their deterministic id (rogue or legacy rows)
	OutOfOrder     []int // versions saved at an earlier time than the version before them (writers' clocks disagree); AsOf stops before them
	BadTime        []int // versions whose saved_at cannot be read as a time
	// CurrentMissing is true when history exists but has no row for the
	// entity's current _version (typically a write that bypassed Save).
	CurrentMissing bool
	// Ahead is true when history holds a version higher than the entity's
	// current _version (for example after ErrEntityRecreated, or a rogue row).
	Ahead bool
	// Deleted is true when the newest history row is a deletion tombstone: the
	// history says the entity was deleted. The entity itself may still exist;
	// see PendingDelete.
	Deleted bool
	// PendingDelete is true when the entity exists and its current version is a
	// tombstone: Delete recorded the deletion and did not remove the entity.
	// Calling Delete again finishes it.
	PendingDelete bool
	// UntrackedDelete is true when history exists, the entity does not, and the
	// newest row is not a tombstone: the entity was removed without Delete (a
	// plain xolu DELETE, or a cascade). The history cannot say when.
	UntrackedDelete bool
	// Revived is true when history has rows after a tombstone: something saved
	// the entity again after its deletion.
	Revived bool
}

// OK reports whether the history shows no duplicates, gaps, misplaced rows,
// missing current row, rows ahead of the entity, unfinished or untracked
// deletions, rows after a tombstone, or times that run backwards or cannot be read. A deleted entity whose newest row is a
// tombstone is consistent: Deleted is true and OK is true.
func (r HistoryReport) OK() bool {
	return len(r.Duplicates) == 0 && len(r.Gaps) == 0 && len(r.Misplaced) == 0 && !r.CurrentMissing && !r.Ahead &&
		!r.PendingDelete && !r.UntrackedDelete && !r.Revived &&
		len(r.OutOfOrder) == 0 && len(r.BadTime) == 0
}

// CheckHistory reads an entity's history and reports violations of its
// invariants. History of a deleted entity is reported with EntityExists false.
func (c *Client) CheckHistory(ctx context.Context, ref EntityRef) (HistoryReport, error) {
	var rep HistoryReport
	if err := ref.validate(); err != nil {
		return rep, err
	}
	_, ver, err := c.getEntity(ctx, ref)
	switch {
	case err == nil:
		rep.EntityExists, rep.CurrentVersion = true, ver
	case errors.Is(err, ErrNotFound):
	default:
		return rep, err
	}
	rows, err := c.historyRows(ctx, ref)
	if err != nil {
		return rep, err
	}
	count := map[int]int{}
	tomb := map[int]bool{}
	for _, r := range rows {
		v := asInt(r["version"])
		count[v]++
		id := asInt(r["id"])
		if id != historyID(ref.ID, v) {
			rep.Misplaced = append(rep.Misplaced, id)
		} else if asString(r["change_kind"]) == ChangeDelete {
			tomb[v] = true
		}
	}
	sort.Ints(rep.Misplaced)
	rep.OutOfOrder, rep.BadTime = timeAnomalies(ref, rows)
	for v, n := range count {
		rep.Versions = append(rep.Versions, v)
		if n > 1 {
			rep.Duplicates = append(rep.Duplicates, v)
		}
	}
	sort.Ints(rep.Versions)
	sort.Ints(rep.Duplicates)
	if len(rep.Versions) > 0 {
		for v := rep.Versions[0]; v <= rep.Versions[len(rep.Versions)-1]; v++ {
			if count[v] == 0 {
				rep.Gaps = append(rep.Gaps, v)
			}
		}
		rep.CurrentMissing = rep.EntityExists && count[rep.CurrentVersion] == 0
		top := rep.Versions[len(rep.Versions)-1]
		rep.Ahead = rep.EntityExists && top > rep.CurrentVersion
		rep.Deleted = tomb[top]
		rep.PendingDelete = rep.EntityExists && rep.Deleted && top == rep.CurrentVersion
		rep.UntrackedDelete = !rep.EntityExists && !rep.Deleted
		for v := range tomb {
			if v < top {
				rep.Revived = true
			}
		}
	}
	return rep, nil
}

// timeAnomalies finds history rows whose saved_at runs backwards or cannot be
// read as a time. Only rows at their deterministic id are considered.
func timeAnomalies(ref EntityRef, rows []map[string]any) (outOfOrder, bad []int) {
	type point struct {
		version int
		at      time.Time
		ok      bool
	}
	var pts []point
	for _, r := range rows {
		v := asInt(r["version"])
		if asInt(r["id"]) != historyID(ref.ID, v) {
			continue
		}
		at, err := ParseTime(asString(r["saved_at"]))
		pts = append(pts, point{version: v, at: at, ok: err == nil})
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].version < pts[j].version })
	var prev time.Time
	havePrev := false
	for _, p := range pts {
		if !p.ok {
			bad = append(bad, p.version)
			continue
		}
		if havePrev && p.at.Before(prev) {
			outOfOrder = append(outOfOrder, p.version)
		}
		prev, havePrev = p.at, true
	}
	return outOfOrder, bad
}

// ---- HTTP plumbing ---------------------------------------------------------

type errBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Status  int    `json:"status"`
	} `json:"error"`
	CurrentVersion json.Number `json:"current_version"`
}

func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) (int, error) {
	var rd io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		var eb errBody
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		_ = dec.Decode(&eb)
		cv, _ := eb.CurrentVersion.Int64()
		return resp.StatusCode, &APIError{
			Status:         resp.StatusCode,
			Code:           eb.Error.Code,
			Message:        eb.Error.Message,
			CurrentVersion: int(cv),
		}
	}
	if out != nil && len(body) > 0 {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("xoluver: decoding response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

func (c *Client) oql(ctx context.Context, query string) ([]map[string]any, error) {
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, "/api/v1/oql/query", map[string]string{"query": query}, &resp); err != nil {
		if isMissingEntity(err) {
			// A history type that has never been written to does not exist
			// yet (xolu answers 400 XOLU-QL004); for reads that means no rows.
			return nil, nil
		}
		return nil, err
	}
	return resp.Data, nil
}

// isMissingEntity reports xolu's "entity does not exist" OQL error.
func isMissingEntity(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == "XOLU-QL004" && strings.Contains(ae.Message, "does not exist")
}

// ---- helpers ---------------------------------------------------------------

// stripSystem returns a copy of doc without the fields xolu owns: _version
// (the compare-and-set counter, ignored on write and returned on read) and id
// (taken from the URL or commit; a different id in the body is overwritten).
// Other underscore-prefixed fields are user data and are kept.
func stripSystem(doc map[string]any) map[string]any {
	out := make(map[string]any, len(doc))
	for k, v := range doc {
		if k == "_version" || k == "id" {
			continue
		}
		out[k] = v
	}
	return out
}

func asInt(v any) int {
	switch n := v.(type) {
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func summaryFrom(r map[string]any) VersionSummary {
	return VersionSummary{
		Version:      asInt(r["version"]),
		ChangeKind:   asString(r["change_kind"]),
		SavedBy:      asString(r["saved_by"]),
		SavedAt:      asString(r["saved_at"]),
		Reason:       asString(r["reason"]),
		RestoredFrom: asInt(r["restored_from"]),

		LifecycleInput: asString(r["lifecycle_input"]),
	}
}
