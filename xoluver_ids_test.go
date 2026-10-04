// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

package xoluver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests for deterministic history row ids and the fixed-width saved_at.
// Tests marked "unit" need no server; the others need XOLU_URL.

// ---- unit ------------------------------------------------------------------

func TestHistoryIDIsInjectiveAndDoesNotOverflow(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	seen := map[int][2]int{}
	for i := 0; i < 20000; i++ {
		e, v := 1+r.Intn(5000), 1+r.Intn(MaxVersion)
		id := historyID(e, v)
		if prev, dup := seen[id]; dup && prev != [2]int{e, v} {
			t.Fatalf("id %d is shared by %v and %v", id, prev, [2]int{e, v})
		}
		seen[id] = [2]int{e, v}
		if id/historyIDStride != e || id%historyIDStride != v {
			t.Fatalf("id %d does not decode back to (%d, %d)", id, e, v)
		}
	}
	// The largest id must be exactly representable as a float64: xolu's OQL
	// returns numbers as float64, and ListVersions and CheckHistory read ids
	// from OQL results.
	if top := historyID(MaxEntityID, MaxVersion); top <= 0 || top >= 1<<53 || int(float64(top)) != top {
		t.Fatalf("historyID(MaxEntityID, MaxVersion) = %d is not exact in a float64", top)
	}
	// Neighbouring entities never share an id, even at the version limit.
	if historyID(1, MaxVersion) >= historyID(2, 1) {
		t.Fatal("versions of entity 1 reach into entity 2's ids")
	}
}

func TestFormatTimeSortsLikeTime(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	base := time.Date(2026, 10, 3, 19, 12, 5, 0, time.UTC)
	var times []time.Time
	for i := 0; i < 2000; i++ {
		// Many instants inside one second, some exactly on it, some whose
		// fraction ends in zeros: the cases a variable-width format sorts wrongly.
		ns := []int{0, 500_000_000, 500_000_001, 100, 999_999_999, r.Intn(1_000_000_000), r.Intn(1000) * 1_000_000}[r.Intn(7)]
		times = append(times, base.Add(time.Duration(r.Intn(3))*time.Second+time.Duration(ns)))
	}
	byTime := append([]time.Time(nil), times...)
	sort.SliceStable(byTime, func(i, j int) bool { return byTime[i].Before(byTime[j]) })
	byText := append([]time.Time(nil), times...)
	sort.SliceStable(byText, func(i, j int) bool { return FormatTime(byText[i]) < FormatTime(byText[j]) })
	for i := range byTime {
		if !byTime[i].Equal(byText[i]) {
			t.Fatalf("position %d: sorted by time %v, sorted by text %v", i, byTime[i], byText[i])
		}
	}

	for _, tm := range times[:300] {
		s := FormatTime(tm)
		if len(s) != len(TimeLayout) || !strings.HasSuffix(s, "Z") {
			t.Fatalf("FormatTime(%v) = %q: not fixed width UTC", tm, s)
		}
		back, err := ParseTime(s)
		if err != nil || !back.Equal(tm) {
			t.Fatalf("round trip of %v: %v err=%v", tm, back, err)
		}
	}

	// A local zone is converted, not printed.
	local := time.Date(2026, 10, 3, 16, 12, 5, 0, time.FixedZone("UYT", -3*3600))
	if got := FormatTime(local); got != "2026-10-03T19:12:05.000000000Z" {
		t.Fatalf("FormatTime of a non-UTC time = %q", got)
	}

	// Why not RFC3339Nano: it drops trailing zeros, so a later instant sorts first.
	later, earlier := base.Add(500*time.Millisecond).Format(time.RFC3339Nano), base.Format(time.RFC3339Nano)
	if !(later < earlier) {
		t.Fatalf("expected the old format to mis-sort (%q vs %q); the reason for FormatTime no longer holds", later, earlier)
	}
}

func TestVersionAndIDLimits(t *testing.T) {
	c := New("http://127.0.0.1:1") // a network attempt would fail with "dial"
	ctx := context.Background()
	ref := EntityRef{Type: "asset", ID: 1}
	doc := map[string]any{"a": 1}

	if _, err := c.Save(ctx, ref, MaxVersion, doc, SaveOptions{}); !errors.Is(err, ErrVersionLimit) {
		t.Errorf("save at MaxVersion: err=%v, want ErrVersionLimit", err)
	}
	// One below the limit is allowed: it gets as far as the network.
	if _, err := c.Save(ctx, ref, MaxVersion-1, doc, SaveOptions{}); err == nil || errors.Is(err, ErrVersionLimit) {
		t.Errorf("save at MaxVersion-1: err=%v, want a network error", err)
	}
	if err := (EntityRef{Type: "asset", ID: MaxEntityID + 1}).validate(); err == nil {
		t.Error("entity id above MaxEntityID accepted")
	}
	if err := (EntityRef{Type: "asset", ID: MaxEntityID}).validate(); err != nil {
		t.Errorf("entity id MaxEntityID refused: %v", err)
	}
	for _, v := range []int{0, -1, MaxVersion + 1, math.MaxInt32} {
		if _, err := c.GetVersion(ctx, ref, v); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetVersion(%d): err=%v, want ErrNotFound without a request", v, err)
		}
	}
}

// A save must not scan: it reads the entity and one history row by id, then
// commits. And the rows it writes carry deterministic ids and a fixed-width time.
func TestSaveMakesNoScansAndWritesDeterministicRows(t *testing.T) {
	var mu sync.Mutex
	var commits []map[string]any
	var oqlCalls, rowReads int32
	historyHasBase := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/oql/query":
			atomic.AddInt32(&oqlCalls, 1)
			fmt.Fprint(w, `{"data":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/thing/7":
			fmt.Fprint(w, `{"_version":3,"id":7,"name":"x"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/thing_version/"):
			atomic.AddInt32(&rowReads, 1)
			if historyHasBase && r.URL.Path == fmt.Sprintf("/api/v1/thing_version/%d", historyID(7, 3)) {
				fmt.Fprint(w, `{"id":7000003,"entity_id":7,"version":3}`)
				return
			}
			http.NotFound(w, r)
		case r.URL.Path == "/api/v1/commit":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			commits = append(commits, body)
			mu.Unlock()
			fmt.Fprint(w, `{"update":{"version":4}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL)
	c.Now = func() time.Time { return time.Date(2026, 10, 3, 19, 12, 5, 120_000_000, time.UTC) }
	ref := EntityRef{Type: "thing", ID: 7}

	rowsOf := func(i int) []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		var out []map[string]any
		for _, a := range commits[i]["append"].([]any) {
			out = append(out, a.(map[string]any))
		}
		return out
	}

	// No history for version 3: baseline plus save, ids 7000003 and 7000004.
	if _, err := c.Save(context.Background(), ref, 3, map[string]any{"name": "y"}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	rows := rowsOf(0)
	if len(rows) != 2 || rows[0]["id"] != float64(historyID(7, 3)) || rows[1]["id"] != float64(historyID(7, 4)) {
		t.Fatalf("rows = %v, want ids %d and %d", rows, historyID(7, 3), historyID(7, 4))
	}
	for _, row := range rows {
		at, _ := row["data"].(map[string]any)["saved_at"].(string)
		if at != "2026-10-03T19:12:05.120000000Z" {
			t.Fatalf("saved_at = %q, want the fixed-width form", at)
		}
	}

	// History for version 3 exists: only the save row.
	historyHasBase = true
	if _, err := c.Save(context.Background(), ref, 3, map[string]any{"name": "y"}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatalf("second save: %v", err)
	}
	if rows = rowsOf(1); len(rows) != 1 || rows[0]["id"] != float64(historyID(7, 4)) {
		t.Fatalf("rows = %v, want one row with id %d", rows, historyID(7, 4))
	}

	if n := atomic.LoadInt32(&oqlCalls); n != 0 {
		t.Fatalf("Save issued %d OQL queries, want 0 (a scan per save)", n)
	}
	if n := atomic.LoadInt32(&rowReads); n != 2 {
		t.Fatalf("Save read the history row %d times, want 2 (once per save)", n)
	}
}

// ---- live server -----------------------------------------------------------

func TestDeterministicHistoryIDsLive(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("id")
	id := newEntity(t, c, typ, map[string]any{"n": 1})
	ref := EntityRef{Type: typ, ID: id}
	for base := 1; base <= 2; base++ {
		if _, err := c.Save(ctx, ref, base, map[string]any{"n": base + 1}, SaveOptions{SavedBy: "u"}); err != nil {
			t.Fatal(err)
		}
	}
	// Versions 1 (baseline), 2 and 3 sit at their deterministic ids, read directly.
	for v := 1; v <= 3; v++ {
		var row map[string]any
		path := fmt.Sprintf("/api/v1/%s/%d", ref.historyType(), historyID(id, v))
		if _, err := c.doJSON(ctx, http.MethodGet, path, nil, &row); err != nil {
			t.Fatalf("direct read of version %d: %v", v, err)
		}
		if asInt(row["entity_id"]) != id || asInt(row["version"]) != v {
			t.Fatalf("row at %s is %v", path, row)
		}
		if at, _ := row["saved_at"].(string); len(at) != len(TimeLayout) {
			t.Fatalf("saved_at %q is not fixed width", at)
		}
	}
	rep, err := c.CheckHistory(ctx, ref)
	if err != nil || !rep.OK() || len(rep.Misplaced) != 0 || len(rep.Versions) != 3 {
		t.Fatalf("history: %+v err=%v", rep, err)
	}
}

// The same ids hold at the largest entity id the scheme allows.
func TestExtremeEntityIDsLive(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("ex")
	id := MaxEntityID
	if err := rawDo(t, c, http.MethodPost, fmt.Sprintf("/api/v1/%s/save/%d", typ, id), map[string]any{"n": 1}); err != nil {
		t.Skipf("xolu refuses entity id %d (%v): lower MaxEntityID", id, err)
	}
	ref := EntityRef{Type: typ, ID: id}
	if v, err := c.Save(ctx, ref, 1, map[string]any{"n": 2}, SaveOptions{SavedBy: "u"}); err != nil || v != 2 {
		t.Fatalf("save at the largest entity id: v=%d err=%v", v, err)
	}
	if rec, err := c.GetVersion(ctx, ref, 1); err != nil || asInt(rec.Snapshot["n"]) != 1 {
		t.Fatalf("GetVersion(1): %+v err=%v", rec, err)
	}
	if rep, err := c.CheckHistory(ctx, ref); err != nil || !rep.OK() {
		t.Fatalf("history: %+v err=%v", rep, err)
	}
}

// Losing a race returns a conflict whichever xolu check trips first: the
// version compare-and-set (CM001) or the history row id that now exists (CM007).
func TestLostRaceMapsToConflictLive(t *testing.T) {
	c0 := testClient(t)
	typ := uniqueType("lr")
	id := newEntity(t, c0, typ, map[string]any{"n": 1})
	ref := EntityRef{Type: typ, ID: id}

	up, _ := url.Parse(c0.BaseURL)
	proxy := httputil.NewSingleHostReverseProxy(up)
	var once int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == apiPath(c0, "/api/v1/commit") && atomic.CompareAndSwapInt32(&once, 0, 1) {
			// A competing save lands after this client looked, before it commits.
			if _, err := c0.Save(context.Background(), ref, 1, map[string]any{"n": 99}, SaveOptions{SavedBy: "rival"}); err != nil {
				t.Errorf("competing save: %v", err)
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.Tenant = c0.Tenant
	_, err := c.Save(context.Background(), ref, 1, map[string]any{"n": 2}, SaveOptions{SavedBy: "slow"})
	var ce *ErrVersionConflict
	if !errors.As(err, &ce) || ce.Current != 2 {
		t.Fatalf("err=%v, want ErrVersionConflict with current 2", err)
	}
	// Exactly the rival's rows exist: nothing from the loser.
	rep, _ := c0.CheckHistory(context.Background(), ref)
	if !rep.OK() || len(rep.Versions) != 2 {
		t.Fatalf("history after the lost race: %+v", rep)
	}
	if rec, _ := c0.GetVersion(context.Background(), ref, 2); rec.SavedBy != "rival" || asInt(rec.Snapshot["n"]) != 99 {
		t.Fatalf("version 2 is %+v, want the rival's save", rec)
	}
}
