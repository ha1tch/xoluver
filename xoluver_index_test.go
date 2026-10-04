// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

package xoluver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Unit tests for tenant routing and the /ts version index, against a fake xolu
// that serves tenant routes, history rows by id, OQL, and /ts. The live tests
// are in xoluver_index_live_test.go.

type fakeEvent struct {
	version int
	at      time.Time
	kind    float64
}

type idxFake struct {
	mu        sync.Mutex
	tenant    string
	timeline  int
	rows      []asofRow // the truth: history rows, at their deterministic ids
	events    []fakeEvent
	entityVer int  // 0: the entity does not exist
	oqlBroken bool // answer OQL like xolu does on tenant routes: "entity does not exist"
	dims      int
	retention int

	oql, rowGets, tsReads, unprefixed int32
	commits                           []map[string]any
	batches                           [][]map[string]any
	log                               []string
	commitStatus                      int
	commitBody                        string
}

func newIdxFake(t *testing.T, rows []asofRow, events []fakeEvent) (*idxFake, *Client) {
	t.Helper()
	f := &idxFake{tenant: "acme", timeline: 9, rows: rows, events: events, entityVer: len(rows), dims: 2, retention: -1}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	c := New(srv.URL)
	c.Tenant = "acme"
	return f, c
}

func (f *idxFake) canonical(path string) string {
	for _, v := range []string{"v1", "v2"} {
		if p := "/api/" + v + "/tenant/" + f.tenant + "/"; f.tenant != "" && strings.HasPrefix(path, p) {
			return "/api/" + v + "/" + strings.TrimPrefix(path, p)
		}
	}
	if f.tenant != "" {
		atomic.AddInt32(&f.unprefixed, 1)
	}
	return path
}

func (f *idxFake) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := f.canonical(r.URL.Path)
	f.mu.Lock()
	f.log = append(f.log, r.Method+" "+path)
	f.mu.Unlock()
	switch {
	case path == "/api/v1/oql/query":
		atomic.AddInt32(&f.oql, 1)
		if f.oqlBroken {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":"XOLU-QL004","message":"entity 'thing_version' does not exist","status":400}}`)
			return
		}
		var data []map[string]any
		for i := len(f.rows) - 1; i >= 0; i-- {
			row := f.rows[i]
			id := row.id
			if id == 0 {
				id = historyID(7, row.version)
			}
			data = append(data, map[string]any{"id": id, "version": row.version, "change_kind": row.kind, "saved_at": row.at, "saved_by": "u"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	case r.Method == http.MethodGet && path == "/api/v1/thing/7":
		if f.entityVer == 0 {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"_version":%d,"id":7}`, f.entityVer)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/thing_version/"):
		atomic.AddInt32(&f.rowGets, 1)
		for _, row := range f.rows {
			if row.id == 0 && path == fmt.Sprintf("/api/v1/thing_version/%d", historyID(7, row.version)) {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": historyID(7, row.version), "entity_id": 7, "version": row.version,
					"change_kind": row.kind, "saved_at": row.at, "snapshot": map[string]any{"v": row.version}})
				return
			}
		}
		http.NotFound(w, r)
	case r.Method == http.MethodGet && path == "/api/v1/ts/events/latest":
		atomic.AddInt32(&f.tsReads, 1)
		dim, _ := strconv.Atoi(r.URL.Query().Get("dims"))
		var evs []fakeEvent
		f.mu.Lock()
		evs = append(evs, f.events...)
		f.mu.Unlock()
		sort.SliceStable(evs, func(i, j int) bool { return evs[i].version > evs[j].version }) // newest key first
		out := []map[string]any{}
		for _, e := range evs {
			if dim == 7 {
				out = append(out, map[string]any{"timeline": f.timeline, "dims": []int{7, e.version}, "time": e.at.UTC().Format(time.RFC3339Nano), "nums": []float64{e.kind}})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"count": len(out), "events": out})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/ts/tl/"):
		fmt.Fprintf(w, `{"id":%d,"dims":%d,"retention_days":%d}`, f.timeline, f.dims, f.retention)
	case path == "/api/v1/ts/provision" || path == "/api/v1/ts/tl/def":
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{}`)
	case path == "/api/v1/ts/events/batch":
		var body struct {
			Events []map[string]any `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.batches = append(f.batches, body.Events)
		for _, e := range body.Events {
			f.events = append(f.events, decodeFakeEvent(e))
		}
		f.mu.Unlock()
		fmt.Fprint(w, `{}`)
	case path == "/api/v1/commit":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.commits = append(f.commits, body)
		f.mu.Unlock()
		if f.commitStatus != 0 {
			w.WriteHeader(f.commitStatus)
			fmt.Fprint(w, f.commitBody)
			return
		}
		base := int(body["update"].(map[string]any)["version"].(float64))
		if ts, ok := body["timeseries"].([]any); ok {
			f.mu.Lock()
			for _, e := range ts {
				f.events = append(f.events, decodeFakeEvent(e.(map[string]any)))
			}
			f.mu.Unlock()
		}
		fmt.Fprintf(w, `{"update":{"version":%d}}`, base+1)
	default:
		http.NotFound(w, r)
	}
}

func decodeFakeEvent(m map[string]any) fakeEvent {
	dims := m["dims"].([]any)
	at, _ := time.Parse(time.RFC3339Nano, m["time"].(string))
	kind := 0.0
	if nums, ok := m["nums"].([]any); ok && len(nums) > 0 {
		kind = nums[0].(float64)
	}
	return fakeEvent{version: int(dims[1].(float64)), at: at, kind: kind}
}

func (f *idxFake) count(entry string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.log {
		if e == entry {
			n++
		}
	}
	return n
}

// ---- history generator for the differential test ---------------------------

type genEntry struct {
	version int
	kind    string
	at      time.Time
}

func genHistory(r *rand.Rand) []genEntry {
	n := 1 + r.Intn(8)
	at := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	tomb := -1
	if n >= 2 && r.Intn(3) == 0 {
		tomb = 2 + r.Intn(n-1)
	}
	out := make([]genEntry, 0, n)
	for v := 1; v <= n; v++ {
		at = at.Add(time.Duration(r.Intn(4)) * time.Millisecond) // some versions share an instant
		kind := ChangeSave
		switch {
		case v == 1:
			kind = ChangeBaseline
		case v == tomb:
			kind = ChangeDelete
		}
		out = append(out, genEntry{version: v, kind: kind, at: at})
	}
	if n >= 3 && r.Intn(4) == 0 { // clocks disagree: two neighbours swap times
		i := r.Intn(n - 1)
		out[i].at, out[i+1].at = out[i+1].at, out[i].at
	}
	return out
}

func rowsOf(h []genEntry) []asofRow {
	rows := make([]asofRow, 0, len(h))
	for _, e := range h {
		rows = append(rows, asofRow{version: e.version, kind: e.kind, at: FormatTime(e.at)})
	}
	return rows
}

func eventsOf(h []genEntry) []fakeEvent {
	evs := make([]fakeEvent, 0, len(h))
	for _, e := range h {
		kind := 0.0
		if e.kind == ChangeDelete {
			kind = 1
		}
		evs = append(evs, fakeEvent{version: e.version, at: e.at, kind: kind})
	}
	return evs
}

func outcome(rec VersionRecord, err error) string {
	var before *ErrBeforeHistory
	var deleted *ErrDeletedAsOf
	switch {
	case err == nil:
		return fmt.Sprintf("record:%d", rec.Version)
	case errors.As(err, &before):
		return fmt.Sprintf("before:%s:%d", FormatTime(before.Since), before.SinceVersion)
	case errors.As(err, &deleted):
		return fmt.Sprintf("deleted:%s:%d", FormatTime(deleted.DeletedAt), deleted.Version)
	case errors.Is(err, ErrNoHistory):
		return "nohistory"
	}
	return "error:" + err.Error()
}

func instantsFor(h []genEntry, r *rand.Rand) []time.Time {
	first, last := h[0].at, h[0].at
	var out []time.Time
	for _, e := range h {
		out = append(out, e.at, e.at.Add(-time.Nanosecond), e.at.Add(time.Nanosecond))
		if e.at.Before(first) {
			first = e.at
		}
		if e.at.After(last) {
			last = e.at
		}
	}
	out = append(out, first.Add(-time.Millisecond), last.Add(time.Hour))
	for i := 0; i < 4; i++ {
		out = append(out, first.Add(time.Duration(r.Int63n(int64(last.Sub(first))+int64(time.Millisecond)))))
	}
	return out
}

// damage turns a correct index into a broken one, in the ways an index can go
// wrong without anyone noticing: events never written, a head or tail lost, an
// event with no row behind it, and two events for one version.
func damage(mode int, evs []fakeEvent, r *rand.Rand) []fakeEvent {
	out := append([]fakeEvent(nil), evs...)
	switch mode {
	case 1: // each event lost with probability 0.4
		kept := out[:0]
		for _, e := range out {
			if r.Float64() >= 0.4 {
				kept = append(kept, e)
			}
		}
		return kept
	case 2: // the start lost, as by an expiring timeline
		return out[min(1+r.Intn(len(out)), len(out)):]
	case 3: // the end lost
		return out[:len(out)-min(1+r.Intn(len(out)), len(out))]
	case 4: // an event for the next version, which has no row (a commit whose /ts write was not undone)
		last := out[len(out)-1]
		return append(out, fakeEvent{version: last.version + 1, at: last.at.Add(time.Duration(r.Intn(3)) * time.Millisecond)})
	case 5: // a second event for an existing version, at another time
		e := out[r.Intn(len(out))]
		return append(out, fakeEvent{version: e.version, at: e.at.Add(time.Millisecond), kind: e.kind})
	case 6: // no events at all
		return nil
	}
	return out
}

var damageNames = []string{"intact", "lost events", "lost start", "lost end", "orphan event", "duplicate event", "empty"}

// The index is an accelerator, never an authority: whatever state it is in,
// AsOf must give the answer the scan gives.
func TestAsOfWithAnIndexAgreesWithTheScanWhateverTheIndexLooksLike(t *testing.T) {
	r := rand.New(rand.NewSource(2026))
	calls := 0
	for i := 0; i < 70; i++ {
		h := genHistory(r)
		for mode := range damageNames {
			f, indexed := newIdxFake(t, rowsOf(h), damage(mode, eventsOf(h), r))
			scan := New(indexed.BaseURL)
			scan.Tenant = "acme"
			if err := indexed.UseTimeIndex("thing", 9); err != nil {
				t.Fatal(err)
			}
			ref := EntityRef{Type: "thing", ID: 7}
			for _, at := range instantsFor(h, r) {
				want := outcome(scan.AsOf(context.Background(), ref, at))
				got := outcome(indexed.AsOf(context.Background(), ref, at))
				calls++
				if got != want {
					t.Fatalf("history %d, index %s, t=%s:\n  index answers %s\n  scan answers  %s\n  history: %+v", i, damageNames[mode], FormatTime(at), got, want, h)
				}
			}
			hits, falls := indexed.IndexStats()
			if mode == 0 {
				if falls != 0 || hits == 0 {
					t.Fatalf("an intact index must serve every call: hits=%d fallbacks=%d (history %+v)", hits, falls, h)
				}
				// Every query of the indexed client was a /ts read; the scan client made the OQL queries.
				if f.count("POST /api/v1/oql/query") != int(atomic.LoadInt32(&f.oql)) {
					t.Fatal("log and counter disagree")
				}
			}
		}
	}
	t.Logf("%d AsOf calls compared", calls)
}

// ---- tenant routing --------------------------------------------------------

func TestTenantRouting(t *testing.T) {
	c := New("http://x")
	for _, tc := range []struct{ tenant, path, want string }{
		{"", "/api/v1/commit", "/api/v1/commit"},
		{"acme", "/api/v1/commit", "/api/v1/tenant/acme/commit"},
		{"acme", "/api/v1/asset/7", "/api/v1/tenant/acme/asset/7"},
		{"acme", "/api/v2/meta/asset/7/label_x", "/api/v2/tenant/acme/meta/asset/7/label_x"},
		{"acme", "/api/v1/ts/events/latest?timeline=7&dims=1&n=10", "/api/v1/tenant/acme/ts/events/latest?timeline=7&dims=1&n=10"},
		{"Team-1_b", "/api/v1/oql/query", "/api/v1/tenant/Team-1_b/oql/query"},
		{"acme", "/health", "/health"},
	} {
		c.Tenant = tc.tenant
		got, err := c.route(tc.path)
		if err != nil || got != tc.want {
			t.Errorf("route(%q) with tenant %q = %q err=%v, want %q", tc.path, tc.tenant, got, err, tc.want)
		}
	}
	for _, bad := range []string{"../x", "a b", "a/b", "a?b", "-lead", strings.Repeat("a", 65), "a\x00b", "caf\u00e9"} {
		c.Tenant = bad
		if _, err := c.route("/api/v1/commit"); err == nil {
			t.Errorf("tenant %q accepted", bad)
		}
	}
	// A bad tenant never reaches the network.
	c = New("http://127.0.0.1:1")
	c.Tenant = "a/b"
	if _, err := c.Save(context.Background(), EntityRef{Type: "thing", ID: 7}, 1, map[string]any{}, SaveOptions{}); err == nil || strings.Contains(err.Error(), "dial") {
		t.Errorf("Save with a bad tenant: %v", err)
	}
}

func TestEveryRequestOnTenantRoutesCarriesTheTenant(t *testing.T) {
	f, c := newIdxFake(t, rowsOf([]genEntry{{1, ChangeBaseline, tm(0, 0)}, {2, ChangeSave, tm(1, 0)}}), eventsOf([]genEntry{{1, ChangeBaseline, tm(0, 0)}, {2, ChangeSave, tm(1, 0)}}))
	if err := c.UseTimeIndex("thing", 9); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref := EntityRef{Type: "thing", ID: 7}
	_, _ = c.Save(ctx, ref, 2, map[string]any{"a": 1}, SaveOptions{SavedBy: "u"})
	_, _ = c.AsOf(ctx, ref, tm(1, 0))
	_, _ = c.CheckIndex(ctx, ref)
	_, _ = c.RebuildIndex(ctx, ref)
	_ = c.DefineTimeIndex(ctx, 9)
	_, _ = c.GetVersion(ctx, ref, 1)
	_ = c.SetLabel(ctx, ref, "x", 1, "u")
	if n := atomic.LoadInt32(&f.unprefixed); n != 0 {
		t.Fatalf("%d request(s) skipped the tenant prefix: %v", n, f.log)
	}
}

// ---- writing the index -----------------------------------------------------

func TestSaveWritesIndexEventsInTheSameCommit(t *testing.T) {
	f, c := newIdxFake(t, nil, nil)
	f.entityVer = 3
	at := time.Date(2026, 10, 3, 12, 0, 0, 123_000_000, time.UTC)
	c.Now = func() time.Time { return at }
	if err := c.UseTimeIndex("thing", 9); err != nil {
		t.Fatal(err)
	}
	ref := EntityRef{Type: "thing", ID: 7}

	if _, err := c.Save(context.Background(), ref, 3, map[string]any{"a": 1}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	body := f.commits[0]
	events, _ := body["timeseries"].([]any)
	rows, _ := body["append"].([]any)
	if len(events) != 2 || len(rows) != 2 {
		t.Fatalf("events=%d rows=%d, want a baseline and a save for each", len(events), len(rows))
	}
	for i, e := range events {
		ev := e.(map[string]any)
		savedAt := rows[i].(map[string]any)["data"].(map[string]any)["saved_at"].(string)
		parsed, err := time.Parse(time.RFC3339Nano, ev["time"].(string))
		if ev["timeline"] != float64(9) || err != nil || FormatTime(parsed) != savedAt || savedAt != FormatTime(at) {
			t.Fatalf("event %d = %v does not match row time %q", i, ev, savedAt)
		}
		if !reflect.DeepEqual(ev["dims"], []any{float64(7), float64(3 + i)}) || !reflect.DeepEqual(ev["nums"], []any{float64(0)}) {
			t.Fatalf("event %d dims/nums = %v / %v", i, ev["dims"], ev["nums"])
		}
	}

	// A tombstone is flagged in the event.
	f.entityVer = 4
	if _, err := c.Delete(context.Background(), ref, 4, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	evs := f.commits[1]["timeseries"].([]any)
	if last := evs[len(evs)-1].(map[string]any); !reflect.DeepEqual(last["nums"], []any{float64(1)}) || !reflect.DeepEqual(last["dims"], []any{float64(7), float64(5)}) {
		t.Fatalf("tombstone event = %v", last)
	}

	// Without an index there is no timeseries field at all.
	plain := New(c.BaseURL)
	plain.Tenant = "acme"
	f.entityVer = 5
	if _, err := plain.Save(context.Background(), ref, 5, map[string]any{"a": 1}, SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, has := f.commits[2]["timeseries"]; has {
		t.Fatal("a client with no index sent timeseries events")
	}
}

func TestAnIndexedTypeNeedsATenantBeforeAnythingIsWritten(t *testing.T) {
	c := New("http://127.0.0.1:1") // a network attempt would fail with "dial"
	if err := c.UseTimeIndex("thing", 9); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"save": func() error {
			_, err := c.Save(context.Background(), EntityRef{Type: "thing", ID: 7}, 1, map[string]any{}, SaveOptions{})
			return err
		},
		"define": func() error { return c.DefineTimeIndex(context.Background(), 9) },
		"check": func() error {
			_, err := c.CheckIndex(context.Background(), EntityRef{Type: "thing", ID: 7})
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrTenantRequired) {
			t.Errorf("%s: err=%v, want ErrTenantRequired", name, err)
		}
	}
	if err := c.UseTimeIndex("Bad Type", 9); err == nil {
		t.Error("an invalid type was accepted")
	}
	if err := c.UseTimeIndex("thing_version", 9); err == nil {
		t.Error("a history type was accepted")
	}
	for _, id := range []int{0, -1, 65536} {
		if err := c.UseTimeIndex("thing", id); err == nil {
			t.Errorf("timeline id %d was accepted", id)
		}
	}
}

func TestSaveSaysWhenTheIndexTimelineIsNotDefined(t *testing.T) {
	f, c := newIdxFake(t, nil, nil)
	f.entityVer = 1
	f.commitStatus = http.StatusBadRequest
	f.commitBody = `{"error":{"code":"XOLU-CM012","message":"timeseries[0]: timeline 9 not defined for tenant (XOLU-CM012)","status":400}}`
	if err := c.UseTimeIndex("thing", 9); err != nil {
		t.Fatal(err)
	}
	_, err := c.Save(context.Background(), EntityRef{Type: "thing", ID: 7}, 1, map[string]any{"a": 1}, SaveOptions{})
	var nr *ErrIndexNotReady
	if !errors.As(err, &nr) || nr.Type != "thing" || nr.Timeline != 9 || !strings.Contains(err.Error(), "DefineTimeIndex") {
		t.Fatalf("err=%v, want ErrIndexNotReady naming the type, the timeline and the fix", err)
	}
}

func TestDefineTimeIndexAcceptsOnlyATimelineThatNeverExpires(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dims, keep int
		wantErr    string
	}{
		{"fine", 2, -1, ""},
		{"expires after 90 days", 2, 90, "retention_days=90"},
		{"inherits the store default", 2, 0, "retention_days=0"},
		{"wrong dimensions", 3, -1, "dims=3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newIdxFake(t, nil, nil)
			f.dims, f.retention = tc.dims, tc.keep
			err := c.DefineTimeIndex(context.Background(), 9)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err=%v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err=%v, want it to mention %q", err, tc.wantErr)
			}
			for _, want := range []string{"POST /api/v1/ts/provision", "POST /api/v1/ts/tl/def", "GET /api/v1/ts/tl/9"} {
				if f.count(want) != 1 {
					t.Errorf("%q made %d times, want once: %v", want, f.count(want), f.log)
				}
			}
		})
	}
}

// ---- when AsOf leaves the index --------------------------------------------

func TestTheIndexIsUsedWhenIntactAndLeftWhenDamaged(t *testing.T) {
	h := []genEntry{{1, ChangeBaseline, tm(0, 0)}, {2, ChangeSave, tm(10, 0)}, {3, ChangeSave, tm(20, 0)}, {4, ChangeDelete, tm(30, 0)}}
	good := eventsOf(h)
	ev := func(version int, at time.Time, kind float64) fakeEvent { return fakeEvent{version, at, kind} }
	all := []time.Time{tm(10, 0), tm(25, 0), tm(20, 0)}
	cases := []struct {
		name   string
		events []fakeEvent
		at     []time.Time
		falls  int // how many of the calls must leave the index
	}{
		{"intact", good, all, 0},
		{"a middle event missing", []fakeEvent{good[0], good[1], good[3]}, all, 3},
		{"the first event missing", good[1:], all, 3},
		{"the last event missing", good[:3], all, 3},
		{"no events", nil, all, 3},
		// A stray event at the end only matters to an instant at or after it.
		{"an event with no row behind it", append(append([]fakeEvent(nil), good...), ev(5, tm(40, 0), 0)), []time.Time{tm(10, 0), tm(45, 0)}, 1},
		{"two events for one version", append(append([]fakeEvent(nil), good...), ev(2, tm(11, 0), 0)), all, 3},
		// Only the answer and the entry that ended the walk are verified against
		// their rows. Version 2's event is a nanosecond late: that matters to the
		// instant just before it, where version 2 ends the walk, and to no other.
		{"the event that ends the walk has the wrong time", []fakeEvent{good[0], ev(2, tm(10, 1), 0), good[2], good[3]}, all, 1},
		{"a wrong kind on the answer or the entry after it", []fakeEvent{good[0], good[1], ev(3, tm(20, 0), 1), good[3]}, all, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, c := newIdxFake(t, rowsOf(h), tc.events)
			scan := New(c.BaseURL)
			scan.Tenant = "acme"
			if err := c.UseTimeIndex("thing", 9); err != nil {
				t.Fatal(err)
			}
			ref := EntityRef{Type: "thing", ID: 7}
			for _, at := range tc.at {
				want := outcome(scan.AsOf(context.Background(), ref, at))
				if got := outcome(c.AsOf(context.Background(), ref, at)); got != want {
					t.Fatalf("t=%s: index %s, scan %s", FormatTime(at), got, want)
				}
			}
			hits, falls := c.IndexStats()
			if int(falls) != tc.falls || int(hits+falls) != len(tc.at) {
				t.Errorf("hits=%d fallbacks=%d, want %d fallbacks out of %d calls", hits, falls, tc.falls, len(tc.at))
			}
		})
	}
}

// The index is read with one /ts query and a few row reads, however long the
// history: no scan of the history type.
func TestAnIntactIndexNeedsNoScanAndAFewReads(t *testing.T) {
	const n = 800
	var h []genEntry
	at := tm(0, 0)
	for v := 1; v <= n; v++ {
		kind := ChangeSave
		if v == 1 {
			kind = ChangeBaseline
		}
		h = append(h, genEntry{v, kind, at.Add(time.Duration(v) * time.Millisecond)})
	}
	f, c := newIdxFake(t, rowsOf(h), eventsOf(h))
	if err := c.UseTimeIndex("thing", 9); err != nil {
		t.Fatal(err)
	}
	rec, err := c.AsOf(context.Background(), EntityRef{Type: "thing", ID: 7}, at.Add(500*time.Millisecond))
	if err != nil || rec.Version != 500 {
		t.Fatalf("AsOf = %+v err=%v, want version 500", rec, err)
	}
	if f.oql != 0 || f.tsReads != 1 {
		t.Fatalf("oql=%d tsReads=%d, want 0 and 1", f.oql, f.tsReads)
	}
	// The end of the index is checked (one read), the start needs none (it begins
	// at version 1), the answer's row is read once and so is the row after it.
	if f.rowGets != 3 {
		t.Fatalf("row reads = %d, want 3 whatever the length of the history", f.rowGets)
	}
}

// ---- on tenant routes OQL cannot be used -----------------------------------

func TestOnTenantRoutesAScanFailsLoudlyInsteadOfLookingEmpty(t *testing.T) {
	h := []genEntry{{1, ChangeBaseline, tm(0, 0)}, {2, ChangeSave, tm(10, 0)}}
	f, c := newIdxFake(t, rowsOf(h), nil)
	f.oqlBroken = true
	ctx := context.Background()
	ref := EntityRef{Type: "thing", ID: 7}

	if _, err := c.ListVersions(ctx, ref, 0); !errors.Is(err, ErrTenantOQL) {
		t.Errorf("ListVersions: err=%v, want ErrTenantOQL", err)
	}
	if _, err := c.CheckHistory(ctx, ref); !errors.Is(err, ErrTenantOQL) {
		t.Errorf("CheckHistory: err=%v, want ErrTenantOQL", err)
	}
	if _, err := c.AsOf(ctx, ref, tm(5, 0)); !errors.Is(err, ErrTenantOQL) || errors.Is(err, ErrNoHistory) {
		t.Errorf("AsOf without an index: err=%v, want ErrTenantOQL and not ErrNoHistory (which would claim no record)", err)
	}
	// With an intact index AsOf needs no OQL.
	f.events = eventsOf(h)
	if err := c.UseTimeIndex("thing", 9); err != nil {
		t.Fatal(err)
	}
	if rec, err := c.AsOf(ctx, ref, tm(5, 0)); err != nil || rec.Version != 1 {
		t.Errorf("AsOf with an index: %+v err=%v", rec, err)
	}
	// With a damaged one it must not guess.
	f.events = f.events[1:]
	if _, err := c.AsOf(ctx, ref, tm(15, 0)); !errors.Is(err, ErrTenantOQL) {
		t.Errorf("AsOf with a damaged index: err=%v, want ErrTenantOQL", err)
	}

	// Off tenant routes the same OQL answer means "that type has no rows yet".
	g, plain := newIdxFake(t, nil, nil)
	g.tenant, g.oqlBroken = "", true
	plain.Tenant = ""
	if rows, err := plain.ListVersions(ctx, ref, 0); err != nil || len(rows) != 0 {
		t.Errorf("single-tenant ListVersions on a type never written: %v err=%v, want no rows", rows, err)
	}
}

func TestCheckIndexReadsByIDOnTenantRoutes(t *testing.T) {
	h := []genEntry{{1, ChangeBaseline, tm(0, 0)}, {2, ChangeSave, tm(10, 0)}, {3, ChangeSave, tm(20, 0)}, {4, ChangeDelete, tm(30, 0)}}
	good := eventsOf(h)
	ev := func(version int, at time.Time, kind float64) fakeEvent { return fakeEvent{version, at, kind} }
	cases := []struct {
		name   string
		events []fakeEvent
		keep   int
		want   IndexReport
	}{
		{"intact", good, -1, IndexReport{Timeline: 9, Events: 4, NoExpiry: true}},
		{"missing", []fakeEvent{good[0], good[3]}, -1, IndexReport{Timeline: 9, Events: 2, Missing: []int{2, 3}, NoExpiry: true}},
		{"orphan and duplicate", append(append([]fakeEvent(nil), good...), ev(5, tm(40, 0), 0), ev(2, tm(11, 0), 0)),
			-1, IndexReport{Timeline: 9, Events: 6, Orphans: []int{5}, Duplicates: []int{2}, NoExpiry: true}},
		{"wrong time and wrong kind", []fakeEvent{good[0], ev(2, tm(10, 1), 0), ev(3, tm(20, 0), 1), good[3]}, -1,
			IndexReport{Timeline: 9, Events: 4, Mismatched: []int{2, 3}, NoExpiry: true}},
		{"a timeline that expires", good, 90, IndexReport{Timeline: 9, Events: 4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newIdxFake(t, rowsOf(h), tc.events)
			f.oqlBroken, f.entityVer, f.retention = true, 0, tc.keep // the entity was deleted: its tombstone is the last row
			if err := c.UseTimeIndex("thing", 9); err != nil {
				t.Fatal(err)
			}
			rep, err := c.CheckIndex(context.Background(), EntityRef{Type: "thing", ID: 7})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(rep, tc.want) {
				t.Fatalf("report = %+v\n  want     %+v", rep, tc.want)
			}
			if rep.OK() != (tc.name == "intact") {
				t.Errorf("OK() = %v for %s", rep.OK(), tc.name)
			}
			if f.oql != 0 {
				t.Errorf("CheckIndex used OQL %d time(s) on tenant routes", f.oql)
			}
		})
	}
	if _, err := New("http://x").CheckIndex(context.Background(), EntityRef{Type: "thing", ID: 7}); !errors.Is(err, ErrNoIndex) {
		t.Errorf("a type with no index: err=%v, want ErrNoIndex", err)
	}
}

func TestRebuildIndexWritesOnlyWhatIsMissing(t *testing.T) {
	const n = 1200
	var h []genEntry
	for v := 1; v <= n; v++ {
		kind := ChangeSave
		switch v {
		case 1:
			kind = ChangeBaseline
		case n:
			kind = ChangeDelete
		}
		h = append(h, genEntry{v, kind, tm(0, 0).Add(time.Duration(v) * time.Microsecond)})
	}
	f, c := newIdxFake(t, rowsOf(h), eventsOf(h[:2])) // the index starts with versions 1 and 2
	f.oqlBroken, f.entityVer = true, 0
	if err := c.UseTimeIndex("thing", 9); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref := EntityRef{Type: "thing", ID: 7}

	written, err := c.RebuildIndex(ctx, ref)
	if err != nil || written != n-2 {
		t.Fatalf("RebuildIndex wrote %d err=%v, want %d", written, err, n-2)
	}
	var sizes []int
	for _, b := range f.batches {
		sizes = append(sizes, len(b))
	}
	if !reflect.DeepEqual(sizes, []int{500, 500, 198}) {
		t.Fatalf("batch sizes = %v, want 500, 500, 198", sizes)
	}
	last := f.batches[2][197]
	if !reflect.DeepEqual(last["dims"], []any{float64(7), float64(n)}) && fmt.Sprint(last["dims"]) != fmt.Sprint([]uint64{7, n}) {
		t.Fatalf("last event dims = %v", last["dims"])
	}
	if fmt.Sprint(last["nums"]) != fmt.Sprint([]float64{1}) {
		t.Fatalf("the tombstone was not flagged: %v", last["nums"])
	}
	if rep, err := c.CheckIndex(ctx, ref); err != nil || !rep.OK() || rep.Events != n {
		t.Fatalf("after rebuilding: %+v err=%v", rep, err)
	}
	if again, err := c.RebuildIndex(ctx, ref); err != nil || again != 0 {
		t.Fatalf("a second rebuild wrote %d err=%v, want 0", again, err)
	}
}

func TestIndexEventsReadAndWrittenWithNanosecondTimes(t *testing.T) {
	at := time.Date(2026, 10, 3, 12, 0, 0, 100, time.UTC) // 100 nanoseconds
	h := []genEntry{{1, ChangeBaseline, at}, {2, ChangeSave, at.Add(100)}, {3, ChangeSave, at.Add(200)}}
	_, c := newIdxFake(t, rowsOf(h), eventsOf(h))
	if err := c.UseTimeIndex("thing", 9); err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		t    time.Time
		want string
	}{
		{at.Add(99), "record:1"}, {at.Add(100), "record:2"}, {at.Add(199), "record:2"}, {at.Add(200), "record:3"},
	} {
		if got := outcome(c.AsOf(context.Background(), EntityRef{Type: "thing", ID: 7}, tc.t)); got != tc.want {
			t.Errorf("case %d: t=+%dns: %s, want %s", i, tc.t.Sub(at), got, tc.want)
		}
	}
}
