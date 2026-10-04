// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

package xoluver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Tests for AsOf and the time checks in CheckHistory. Tests marked "unit" use a
// fake server; the others need XOLU_URL.

// ---- unit ------------------------------------------------------------------

type asofRow struct {
	version int
	kind    string
	at      string // saved_at as stored
	id      int    // 0 means the deterministic id
}

func tm(sec, ns int) time.Time { return time.Date(2026, 10, 3, 10, 0, sec, ns, time.UTC) }
func ts(sec, ns int) string    { return FormatTime(tm(sec, ns)) }

// asofFake serves the history of thing 7 and counts OQL queries and row reads.
func asofFake(t *testing.T, entityVersion int, rows []asofRow) (c *Client, oql, gets *int32) {
	t.Helper()
	oql, gets = new(int32), new(int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/oql/query":
			atomic.AddInt32(oql, 1)
			var data []map[string]any
			for i := len(rows) - 1; i >= 0; i-- { // newest first, as xolu returns it
				row := rows[i]
				id := row.id
				if id == 0 {
					id = historyID(7, row.version)
				}
				data = append(data, map[string]any{"id": id, "version": row.version, "change_kind": row.kind, "saved_at": row.at, "saved_by": "u"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/thing/7":
			fmt.Fprintf(w, `{"_version":%d,"id":7}`, entityVersion)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/thing_version/"):
			atomic.AddInt32(gets, 1)
			for _, row := range rows {
				if row.id == 0 && r.URL.Path == fmt.Sprintf("/api/v1/thing_version/%d", historyID(7, row.version)) {
					_ = json.NewEncoder(w).Encode(map[string]any{"id": historyID(7, row.version), "entity_id": 7, "version": row.version,
						"change_kind": row.kind, "saved_at": row.at, "snapshot": map[string]any{"v": row.version}})
					return
				}
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL), oql, gets
}

func TestAsOfSemantics(t *testing.T) {
	plain := []asofRow{{1, ChangeBaseline, ts(0, 0), 0}, {2, ChangeSave, ts(10, 0), 0}, {3, ChangeSave, ts(20, 0), 0}}
	deleted := []asofRow{{1, ChangeBaseline, ts(0, 0), 0}, {2, ChangeSave, ts(10, 0), 0}, {3, ChangeDelete, ts(20, 0), 0}}
	revived := []asofRow{{1, ChangeBaseline, ts(0, 0), 0}, {2, ChangeDelete, ts(10, 0), 0}, {3, ChangeSave, ts(30, 0), 0}}
	skewed := []asofRow{{1, ChangeBaseline, ts(0, 0), 0}, {2, ChangeSave, ts(20, 0), 0}, {3, ChangeSave, ts(10, 0), 0}}
	gap := []asofRow{{1, ChangeBaseline, ts(0, 0), 0}, {3, ChangeSave, ts(30, 0), 0}}
	// A stray row (not at its deterministic id) with a lower version and an earlier
	// time would win the walk if it were counted.
	stray := append([]asofRow{{0, ChangeSave, FormatTime(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)), 12345}}, plain...)
	badTime := []asofRow{{1, ChangeBaseline, ts(0, 0), 0}, {2, ChangeSave, "yesterday", 0}}

	version := func(v int) func(*testing.T, VersionRecord, error) {
		return func(t *testing.T, rec VersionRecord, err error) {
			t.Helper()
			if err != nil || rec.Version != v || asInt(rec.Snapshot["v"]) != v {
				t.Fatalf("got %+v err=%v, want version %d", rec, err, v)
			}
		}
	}
	before := func(since time.Time, v int) func(*testing.T, VersionRecord, error) {
		return func(t *testing.T, _ VersionRecord, err error) {
			t.Helper()
			var e *ErrBeforeHistory
			if !errors.As(err, &e) || !e.Since.Equal(since) || e.SinceVersion != v {
				t.Fatalf("err=%v, want ErrBeforeHistory since %v (version %d)", err, since, v)
			}
		}
	}
	deletedAt := func(at time.Time, v int) func(*testing.T, VersionRecord, error) {
		return func(t *testing.T, _ VersionRecord, err error) {
			t.Helper()
			var e *ErrDeletedAsOf
			if !errors.As(err, &e) || !e.DeletedAt.Equal(at) || e.Version != v {
				t.Fatalf("err=%v, want ErrDeletedAsOf at %v (version %d)", err, at, v)
			}
		}
	}
	corrupt := func(t *testing.T, _ VersionRecord, err error) {
		t.Helper()
		if !errors.As(err, new(*ErrCorruptHistory)) {
			t.Fatalf("err=%v, want ErrCorruptHistory", err)
		}
	}
	none := func(t *testing.T, _ VersionRecord, err error) {
		t.Helper()
		if !errors.Is(err, ErrNoHistory) {
			t.Fatalf("err=%v, want ErrNoHistory", err)
		}
	}

	cases := []struct {
		name  string
		rows  []asofRow
		at    time.Time
		check func(*testing.T, VersionRecord, error)
	}{
		{"plain: before everything", plain, tm(0, 0).Add(-time.Second), before(tm(0, 0), 1)},
		{"plain: exactly at the baseline", plain, tm(0, 0), version(1)},
		{"plain: one nanosecond before the second save", plain, tm(9, 999_999_999), version(1)},
		{"plain: exactly at the second save", plain, tm(10, 0), version(2)},
		{"plain: between saves", plain, tm(19, 0), version(2)},
		{"plain: exactly at the newest", plain, tm(20, 0), version(3)},
		{"plain: far in the future", plain, tm(20, 0).Add(1000 * time.Hour), version(3)},

		{"deleted: before the deletion", deleted, tm(15, 0), version(2)},
		{"deleted: at the deletion", deleted, tm(20, 0), deletedAt(tm(20, 0), 3)},
		{"deleted: long after", deleted, tm(20, 0).Add(1000 * time.Hour), deletedAt(tm(20, 0), 3)},

		{"revived: while deleted", revived, tm(20, 0), deletedAt(tm(10, 0), 2)},
		{"revived: after the new save", revived, tm(30, 0), version(3)},

		// Version order wins over time order: AsOf stops at the first version saved after t.
		{"skewed: before the late-stamped version", skewed, tm(15, 0), version(1)},
		{"skewed: after both", skewed, tm(25, 0), version(3)},

		{"gap: inside the gap", gap, tm(20, 0), version(1)},
		{"gap: after it", gap, tm(30, 0), version(3)},

		{"a stray row with an earlier time is ignored", stray, time.Date(2026, 10, 3, 9, 30, 0, 0, time.UTC), before(tm(0, 0), 1)},
		{"an unreadable saved_at is corrupt history", badTime, tm(5, 0), corrupt},
		{"no history at all", nil, tm(5, 0), none},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, oql, gets := asofFake(t, 3, tc.rows)
			rec, err := c.AsOf(context.Background(), EntityRef{Type: "thing", ID: 7}, tc.at)
			tc.check(t, rec, err)
			if n := atomic.LoadInt32(oql); n != 1 {
				t.Errorf("OQL queries = %d, want 1", n)
			}
			wantGets := int32(0)
			if err == nil {
				wantGets = 1
			}
			if n := atomic.LoadInt32(gets); n != wantGets {
				t.Errorf("row reads = %d, want %d (a record needs one; every other answer needs none)", n, wantGets)
			}
		})
	}
}

func TestAsOfTreatsEquivalentInstantsAlike(t *testing.T) {
	c, _, _ := asofFake(t, 3, []asofRow{{1, ChangeBaseline, ts(0, 0), 0}, {2, ChangeSave, ts(10, 0), 0}})
	ref := EntityRef{Type: "thing", ID: 7}
	utc := tm(10, 0)
	local := utc.In(time.FixedZone("UYT", -3*3600))
	for _, at := range []time.Time{utc, local} {
		if rec, err := c.AsOf(context.Background(), ref, at); err != nil || rec.Version != 2 {
			t.Fatalf("AsOf(%v) = %+v err=%v, want version 2", at, rec, err)
		}
	}
}

func TestAsOfValidatesWithoutTheNetwork(t *testing.T) {
	c := New("http://127.0.0.1:1") // a network attempt would fail with "dial"
	for _, ref := range []EntityRef{{Type: "Bad Type", ID: 1}, {Type: "asset", ID: 0}, {Type: "asset_version", ID: 1}, {Type: "asset", ID: MaxEntityID + 1}} {
		_, err := c.AsOf(context.Background(), ref, time.Now())
		if err == nil || strings.Contains(err.Error(), "dial") {
			t.Errorf("AsOf(%+v): err=%v, want a validation error", ref, err)
		}
	}
}

// Each answer says which claim it makes. In particular "no record" is never
// worded as "did not exist", and nothing says "never existed".
func TestAsOfErrorsNameTheirClaim(t *testing.T) {
	before := (&ErrBeforeHistory{At: tm(0, 0), Since: tm(10, 0), SinceVersion: 1}).Error()
	deleted := (&ErrDeletedAsOf{At: tm(30, 0), DeletedAt: tm(20, 0), Version: 3}).Error()
	none := ErrNoHistory.Error()

	if !strings.Contains(before, "no record at") || !strings.Contains(before, "cannot say whether the entity existed") || strings.Contains(before, "did not exist") {
		t.Errorf("ErrBeforeHistory must claim only that there is no record: %q", before)
	}
	if !strings.Contains(deleted, "did not exist at") || !strings.Contains(deleted, "had been deleted at") {
		t.Errorf("ErrDeletedAsOf must say the entity did not exist and why: %q", deleted)
	}
	if !strings.Contains(none, "no history is recorded") || strings.Contains(none, "did not exist") {
		t.Errorf("ErrNoHistory must claim only that nothing is recorded: %q", none)
	}
	for _, s := range []string{before, deleted, none} {
		if strings.Contains(strings.ToLower(s), "never") {
			t.Errorf("%q claims something about all time", s)
		}
	}
	// None of them is the "does not exist now" family.
	for _, e := range []error{&ErrBeforeHistory{}, &ErrDeletedAsOf{}, ErrNoHistory} {
		if errors.Is(e, ErrNotFound) {
			t.Errorf("%T must not match ErrNotFound: that is about now", e)
		}
	}
}

func TestCheckHistoryReportsTimeAnomalies(t *testing.T) {
	ref := EntityRef{Type: "thing", ID: 7}
	clean := []asofRow{{1, ChangeBaseline, ts(0, 0), 0}, {2, ChangeSave, ts(0, 0), 0}, {3, ChangeSave, ts(5, 0), 0}}
	skewed := []asofRow{{1, ChangeBaseline, ts(0, 0), 0}, {2, ChangeSave, ts(20, 0), 0}, {3, ChangeSave, ts(10, 0), 0}}
	badTime := []asofRow{{1, ChangeBaseline, ts(0, 0), 0}, {2, ChangeSave, "yesterday", 0}, {3, ChangeSave, ts(5, 0), 0}}

	c, _, _ := asofFake(t, 3, clean)
	if rep, err := c.CheckHistory(context.Background(), ref); err != nil || !rep.OK() || len(rep.OutOfOrder) != 0 || len(rep.BadTime) != 0 {
		t.Fatalf("equal times are fine: %+v err=%v", rep, err)
	}
	// A stray row (not at its deterministic id) is reported as misplaced, but its
	// time is not part of the sequence, so it does not count as out of order.
	strayRow := asofRow{2, ChangeSave, FormatTime(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)), 12345}
	c, _, _ = asofFake(t, 3, append(append([]asofRow(nil), clean...), strayRow))
	if rep, _ := c.CheckHistory(context.Background(), ref); !reflect.DeepEqual(rep.Misplaced, []int{12345}) || len(rep.OutOfOrder) != 0 {
		t.Fatalf("a stray row: %+v, want Misplaced [12345] and no OutOfOrder", rep)
	}
	c, _, _ = asofFake(t, 3, skewed)
	if rep, _ := c.CheckHistory(context.Background(), ref); rep.OK() || !reflect.DeepEqual(rep.OutOfOrder, []int{3}) {
		t.Fatalf("a version saved earlier than the one before it: %+v, want OutOfOrder [3]", rep)
	}
	c, _, _ = asofFake(t, 3, badTime)
	if rep, _ := c.CheckHistory(context.Background(), ref); rep.OK() || !reflect.DeepEqual(rep.BadTime, []int{2}) || len(rep.OutOfOrder) != 0 {
		t.Fatalf("an unreadable time: %+v, want BadTime [2] only", rep)
	}
}

// ---- live server -----------------------------------------------------------

// All times here are explicit, so the test does not depend on the wall clock.
// Every instant falls inside one second: the old second-resolution saved_at could
// not tell these saves apart.
func TestAsOfEndToEndLive(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("ao")
	id := newEntity(t, c, typ, map[string]any{"name": "a"})
	ref := EntityRef{Type: typ, ID: id}

	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	ms := func(n int) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }
	clock := base
	c.Now = func() time.Time { return clock }

	clock = ms(100)
	if _, err := c.Save(ctx, ref, 1, map[string]any{"name": "b"}, SaveOptions{SavedBy: "u"}); err != nil { // baseline v1 and save v2, both at +100ms
		t.Fatal(err)
	}
	clock = ms(200)
	if _, err := c.Save(ctx, ref, 2, map[string]any{"name": "c"}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	clock = ms(300)
	if _, err := c.Save(ctx, ref, 3, map[string]any{"name": "d"}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	clock = ms(400)
	if _, err := c.Delete(ctx, ref, 4, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}

	asName := func(at time.Time, wantName string, wantVersion int) {
		t.Helper()
		rec, err := c.AsOf(ctx, ref, at)
		if err != nil || rec.Snapshot["name"] != wantName || rec.Version != wantVersion {
			t.Fatalf("AsOf(+%v) = %+v err=%v, want %q at version %d", at.Sub(base), rec, err, wantName, wantVersion)
		}
	}
	asName(ms(100), "b", 2) // the baseline and the first save share this instant: the newest wins
	asName(ms(150), "b", 2)
	asName(ms(250), "c", 3)
	asName(ms(300), "d", 4)
	asName(base.Add(399*time.Millisecond+999_999*time.Nanosecond), "d", 4)
	asName(ms(250).In(time.FixedZone("UYT", -3*3600)), "c", 3)

	// Before the first record: no record, which is not the same as "did not exist".
	_, err := c.AsOf(ctx, ref, ms(50))
	var nb *ErrBeforeHistory
	if !errors.As(err, &nb) || !nb.Since.Equal(ms(100)) || nb.SinceVersion != 1 {
		t.Fatalf("AsOf before the history: err=%v, want ErrBeforeHistory since +100ms at version 1", err)
	}
	// After the tombstone: did not exist, because it had been deleted.
	for _, at := range []time.Time{ms(400), ms(5000), base.Add(24 * time.Hour)} {
		_, err = c.AsOf(ctx, ref, at)
		var nd *ErrDeletedAsOf
		if !errors.As(err, &nd) || !nd.DeletedAt.Equal(ms(400)) || nd.Version != 5 {
			t.Fatalf("AsOf(+%v): err=%v, want ErrDeletedAsOf at +400ms (version 5)", at.Sub(base), err)
		}
	}
	if rep, _ := c.CheckHistory(ctx, ref); !rep.OK() || !rep.Deleted || len(rep.OutOfOrder) != 0 {
		t.Fatalf("report = %+v", rep)
	}
}

// The state before the first versioned save is not recorded, even though the
// entity existed: AsOf says "no record", not "did not exist".
func TestAsOfBeforeTheFirstSaveOfAnExistingEntityLive(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("ab")
	id := newEntity(t, c, typ, map[string]any{"name": "created long ago"})
	ref := EntityRef{Type: typ, ID: id}

	// An entity that was never saved through xoluver has no history.
	if _, err := c.AsOf(ctx, ref, time.Now()); !errors.Is(err, ErrNoHistory) {
		t.Fatalf("no saves yet: err=%v, want ErrNoHistory", err)
	}
	// An id that does not exist at all gives the same answer: xoluver cannot tell them apart.
	if _, err := c.AsOf(ctx, EntityRef{Type: typ, ID: id + 1000}, time.Now()); !errors.Is(err, ErrNoHistory) {
		t.Fatalf("unknown id: err=%v, want ErrNoHistory", err)
	}

	first := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { return first }
	if _, err := c.Save(ctx, ref, 1, map[string]any{"name": "now versioned"}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	_, err := c.AsOf(ctx, ref, first.Add(-time.Nanosecond))
	if !errors.As(err, new(*ErrBeforeHistory)) {
		t.Fatalf("just before the first save: err=%v, want ErrBeforeHistory", err)
	}
	rec, err := c.AsOf(ctx, ref, first)
	if err != nil || rec.Version != 2 || rec.Snapshot["name"] != "now versioned" {
		t.Fatalf("at the first save: %+v err=%v, want version 2", rec, err)
	}
	// The baseline row holds the earlier state, stamped with when it was captured.
	if b, _ := c.GetVersion(ctx, ref, 1); b.ChangeKind != ChangeBaseline || b.Snapshot["name"] != "created long ago" || b.SavedAt != FormatTime(first) {
		t.Fatalf("baseline = %+v", b)
	}
}

func TestClockSkewIsReportedLive(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("cs")
	id := newEntity(t, c, typ, map[string]any{"n": 1})
	ref := EntityRef{Type: typ, ID: id}

	base := time.Date(2026, 10, 3, 12, 0, 5, 0, time.UTC)
	c.Now = func() time.Time { return base }
	if _, err := c.Save(ctx, ref, 1, map[string]any{"n": 2}, SaveOptions{SavedBy: "fast clock"}); err != nil {
		t.Fatal(err)
	}
	slow := New(c.BaseURL)
	slow.Tenant = c.Tenant
	slow.Now = func() time.Time { return base.Add(-4 * time.Second) } // a writer whose clock is behind
	if _, err := slow.Save(ctx, ref, 2, map[string]any{"n": 3}, SaveOptions{SavedBy: "slow clock"}); err != nil {
		t.Fatal(err)
	}

	rep, _ := c.CheckHistory(ctx, ref)
	if rep.OK() || !reflect.DeepEqual(rep.OutOfOrder, []int{3}) {
		t.Fatalf("report = %+v, want OutOfOrder [3]", rep)
	}
	// AsOf stops at the first version saved after the instant, so the later,
	// earlier-stamped version cannot be reached by time: it answers conservatively.
	_, err := c.AsOf(ctx, ref, base.Add(-3*time.Second))
	if !errors.As(err, new(*ErrBeforeHistory)) {
		t.Fatalf("AsOf between the two clocks: err=%v, want ErrBeforeHistory", err)
	}
	if rec, err := c.AsOf(ctx, ref, base); err != nil || rec.Version != 3 {
		t.Fatalf("AsOf at the later clock: %+v err=%v, want version 3", rec, err)
	}
}
