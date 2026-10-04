// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

package xoluver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Live tests for the version index. They need a xolu server that serves /ts
// and tenant routes, for example:
//
//	XOLU_TIMESERIES_ENABLED=true XOLU_TENANT_AUTO_REGISTER=true XOLU_API_V2_ENABLED=true XOLU_AUTH_TYPE=none ./xolu
//	XOLU_TS_URL=http://localhost:9091 go test ./...
//
// XOLU_TS_TENANT names the tenant (default "acme"). On tenant routes xolu v0.30.38
// cannot run OQL over tenant entities, so these tests use only reads by id, the
// /ts index, and CheckIndex and RebuildIndex, which read history by id.

var timelineSeq int32

// newTimeline returns a timeline id unused by any other test or earlier run on
// the same server.
func newTimeline() int {
	return 1000 + int(time.Now().UnixNano()/int64(time.Millisecond)%50000) + int(atomic.AddInt32(&timelineSeq, 1))
}

func tsClient(t *testing.T) *Client {
	t.Helper()
	u := os.Getenv("XOLU_TS_URL")
	if u == "" {
		t.Skip("XOLU_TS_URL not set; skipping version-index integration test")
	}
	c := New(u)
	c.Tenant = os.Getenv("XOLU_TS_TENANT")
	if c.Tenant == "" {
		c.Tenant = "acme"
	}
	return c
}

// indexedClient returns a client for the tenant with a freshly defined index for typ.
func indexedClient(t *testing.T, typ string) (*Client, int) {
	t.Helper()
	c := tsClient(t)
	tl := newTimeline()
	if err := c.DefineTimeIndex(context.Background(), tl); err != nil {
		t.Fatalf("DefineTimeIndex: %v", err)
	}
	if err := c.UseTimeIndex(typ, tl); err != nil {
		t.Fatal(err)
	}
	return c, tl
}

func TestIndexEndToEndLive(t *testing.T) {
	typ := uniqueType("ix")
	c, tl := indexedClient(t, typ)
	ctx := context.Background()
	if err := c.DefineTimeIndex(ctx, tl); err != nil { // defining it again is fine
		t.Fatalf("second DefineTimeIndex: %v", err)
	}
	id := newEntity(t, c, typ, map[string]any{"name": "a"})
	ref := EntityRef{Type: typ, ID: id}

	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	ms := func(n int) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }
	clock := base
	c.Now = func() time.Time { return clock }
	clock = ms(100)
	if _, err := c.Save(ctx, ref, 1, map[string]any{"name": "b"}, SaveOptions{SavedBy: "u"}); err != nil {
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

	// The events xolu stored: five of them, in nanoseconds, the same instants as the rows.
	events, err := c.readIndex(ctx, ref, tl)
	if err != nil || len(events) != 5 {
		t.Fatalf("index events = %d err=%v, want 5 (baseline, three saves, tombstone)", len(events), err)
	}
	calls := 0
	asName := func(at time.Time, wantName string, wantVersion int) {
		t.Helper()
		calls++
		rec, err := c.AsOf(ctx, ref, at)
		if err != nil || rec.Snapshot["name"] != wantName || rec.Version != wantVersion {
			t.Fatalf("AsOf(+%v) = %+v err=%v, want %q at version %d", at.Sub(base), rec, err, wantName, wantVersion)
		}
	}
	asName(ms(100), "b", 2)
	asName(ms(150), "b", 2)
	asName(ms(250), "c", 3)
	asName(ms(300), "d", 4)
	asName(base.Add(399*time.Millisecond+999_999*time.Nanosecond), "d", 4)
	asName(ms(250).In(time.FixedZone("UYT", -3*3600)), "c", 3)

	calls += 4
	_, err = c.AsOf(ctx, ref, ms(50))
	var nb *ErrBeforeHistory
	if !errors.As(err, &nb) || !nb.Since.Equal(ms(100)) || nb.SinceVersion != 1 {
		t.Fatalf("before the history: err=%v", err)
	}
	for _, at := range []time.Time{ms(400), ms(5000), base.Add(24 * time.Hour)} {
		_, err = c.AsOf(ctx, ref, at)
		var nd *ErrDeletedAsOf
		if !errors.As(err, &nd) || !nd.DeletedAt.Equal(ms(400)) || nd.Version != 5 {
			t.Fatalf("AsOf(+%v): err=%v, want deleted at +400ms (version 5)", at.Sub(base), err)
		}
	}
	// Every call was answered by the index: on tenant routes a fallback would have failed.
	if hits, falls := c.IndexStats(); falls != 0 || int(hits) != calls {
		t.Fatalf("hits=%d fallbacks=%d, want %d and 0", hits, falls, calls)
	}
	rep, err := c.CheckIndex(ctx, ref)
	if err != nil || !rep.OK() || rep.Events != 5 || rep.Timeline != tl {
		t.Fatalf("CheckIndex = %+v err=%v", rep, err)
	}
}

// History that existed before the index was enabled is not indexed. AsOf must
// not guess from a partial index; RebuildIndex fills it in.
func TestIndexBackfillLive(t *testing.T) {
	typ := uniqueType("ib")
	plain := tsClient(t) // no index: it writes history only
	ctx := context.Background()
	id := newEntity(t, plain, typ, map[string]any{"name": "a"})
	ref := EntityRef{Type: typ, ID: id}
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	ms := func(n int) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }
	clock := ms(100)
	plain.Now = func() time.Time { return clock }
	if _, err := plain.Save(ctx, ref, 1, map[string]any{"name": "b"}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	clock = ms(200)
	if _, err := plain.Save(ctx, ref, 2, map[string]any{"name": "c"}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}

	c, tl := indexedClient(t, typ)
	c.Now = func() time.Time { return clock }
	// An empty index says nothing, and a scan is unavailable on tenant routes: the answer is an error, not a guess.
	if _, err := c.AsOf(ctx, ref, ms(150)); !errors.Is(err, ErrTenantOQL) {
		t.Fatalf("AsOf with an empty index: err=%v, want ErrTenantOQL", err)
	}
	clock = ms(300)
	if _, err := c.Save(ctx, ref, 3, map[string]any{"name": "d"}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	// Now the index holds only version 4; versions 1 to 3 exist in the history.
	if _, err := c.AsOf(ctx, ref, ms(250)); !errors.Is(err, ErrTenantOQL) {
		t.Fatalf("AsOf with an index that starts late: err=%v, want ErrTenantOQL", err)
	}
	rep, err := c.CheckIndex(ctx, ref)
	if err != nil || rep.OK() || fmt.Sprint(rep.Missing) != "[1 2 3]" {
		t.Fatalf("CheckIndex = %+v err=%v, want Missing [1 2 3]", rep, err)
	}

	written, err := c.RebuildIndex(ctx, ref)
	if err != nil || written != 3 {
		t.Fatalf("RebuildIndex wrote %d err=%v, want 3", written, err)
	}
	if rep, err = c.CheckIndex(ctx, ref); err != nil || !rep.OK() || rep.Events != 4 {
		t.Fatalf("CheckIndex after rebuilding = %+v err=%v", rep, err)
	}
	rec, err := c.AsOf(ctx, ref, ms(150))
	if err != nil || rec.Version != 2 || rec.Snapshot["name"] != "b" {
		t.Fatalf("AsOf after rebuilding = %+v err=%v, want version 2", rec, err)
	}
	if _, falls := c.IndexStats(); falls != 2 {
		t.Fatalf("fallbacks = %d, want only the two before the rebuild", falls)
	}
	if again, err := c.RebuildIndex(ctx, ref); err != nil || again != 0 {
		t.Fatalf("a second rebuild wrote %d err=%v", again, err)
	}
	_ = tl
}

// Events with no row behind them, and two events for one version, are what an
// interrupted commit or a manual write leaves. AsOf must never answer from them.
func TestIndexOrphanAndDuplicateEventsLive(t *testing.T) {
	typ := uniqueType("io")
	c, tl := indexedClient(t, typ)
	ctx := context.Background()
	id := newEntity(t, c, typ, map[string]any{"name": "a"})
	ref := EntityRef{Type: typ, ID: id}
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	ms := func(n int) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }
	clock := ms(100)
	c.Now = func() time.Time { return clock }
	if _, err := c.Save(ctx, ref, 1, map[string]any{"name": "b"}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	clock = ms(200)
	if _, err := c.Save(ctx, ref, 2, map[string]any{"name": "c"}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	write := func(version int, at time.Time) {
		t.Helper()
		ev := map[string]any{"timeline": tl, "dims": []uint64{uint64(id), uint64(version)}, "time": at.UTC().Format(time.RFC3339Nano), "nums": []float64{0}}
		if _, err := c.doJSON(ctx, http.MethodPost, "/api/v1/ts/events", ev, nil); err != nil {
			t.Fatalf("direct event write: %v", err)
		}
	}

	write(4, ms(300)) // an event for a version that was never saved
	rep, _ := c.CheckIndex(ctx, ref)
	if rep.OK() || fmt.Sprint(rep.Orphans) != "[4]" {
		t.Fatalf("CheckIndex = %+v, want Orphans [4]", rep)
	}
	// Well before the orphan the answer does not depend on it.
	if rec, err := c.AsOf(ctx, ref, ms(150)); err != nil || rec.Version != 2 {
		t.Fatalf("AsOf well before the orphan: %+v err=%v, want version 2", rec, err)
	}
	// Just before it, the orphan is the entry that ends the walk, and it has no
	// row to verify, so the index is not trusted.
	if _, err := c.AsOf(ctx, ref, ms(250)); !errors.Is(err, ErrTenantOQL) {
		t.Fatalf("AsOf just before the orphan: err=%v, want ErrTenantOQL", err)
	}
	// At or after it the index would name a version that does not exist: it refuses.
	if rec, err := c.AsOf(ctx, ref, ms(350)); err == nil {
		t.Fatalf("AsOf after the orphan answered %+v; it must not name a version with no row", rec)
	} else if !errors.Is(err, ErrTenantOQL) {
		t.Fatalf("AsOf after the orphan: err=%v, want ErrTenantOQL (the scan is unavailable here)", err)
	}

	write(2, ms(201)) // a second event for version 2
	rep, _ = c.CheckIndex(ctx, ref)
	if fmt.Sprint(rep.Duplicates) != "[2]" {
		t.Fatalf("CheckIndex = %+v, want Duplicates [2]", rep)
	}
	if _, err := c.AsOf(ctx, ref, ms(250)); !errors.Is(err, ErrTenantOQL) {
		t.Fatalf("AsOf with a duplicate event: err=%v, want ErrTenantOQL", err)
	}
}

func TestSaveFailsLoudlyWhenTheIndexTimelineIsNotDefinedLive(t *testing.T) {
	c := tsClient(t)
	ctx := context.Background()
	typ := uniqueType("in")
	if err := c.DefineTimeIndex(ctx, newTimeline()); err != nil { // provisions the tenant's /ts
		t.Fatal(err)
	}
	if err := c.UseTimeIndex(typ, newTimeline()); err != nil { // a timeline nobody defined
		t.Fatal(err)
	}
	id := newEntity(t, c, typ, map[string]any{"name": "a"})
	ref := EntityRef{Type: typ, ID: id}

	_, err := c.Save(ctx, ref, 1, map[string]any{"name": "b"}, SaveOptions{SavedBy: "u"})
	var nr *ErrIndexNotReady
	if !errors.As(err, &nr) {
		t.Fatalf("err=%v, want ErrIndexNotReady", err)
	}
	if cur, ver, _ := c.getEntity(ctx, ref); ver != 1 || cur["name"] != "a" {
		t.Fatalf("a refused save changed the entity: %v v=%d", cur, ver)
	}
	if _, err := c.GetVersion(ctx, ref, 2); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("a refused save wrote a history row: err=%v", err)
	}
}

// DefineTimeIndex must not leave a timeline that expires: either it makes the
// timeline keep its events forever, or it fails.
func TestDefineTimeIndexNeverLeavesAnExpiringTimelineLive(t *testing.T) {
	c := tsClient(t)
	ctx := context.Background()
	if err := c.DefineTimeIndex(ctx, newTimeline()); err != nil {
		t.Fatal(err)
	}
	tl := newTimeline()
	if _, err := c.doJSON(ctx, http.MethodPost, "/api/v1/ts/tl/def", map[string]any{"id": tl, "name": "mine", "dims": 2, "retention_days": 30}, nil); err != nil {
		t.Fatal(err)
	}
	err := c.DefineTimeIndex(ctx, tl)
	_, retention, gerr := c.timelineDef(ctx, tl)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if err == nil && retention >= 0 {
		t.Fatalf("DefineTimeIndex succeeded but the timeline expires after %d days", retention)
	}
	if err != nil && !strings.Contains(err.Error(), "retention_days") {
		t.Logf("DefineTimeIndex refused: %v", err)
	}
	t.Logf("redefining a timeline with retention 30: err=%v, retention now %d", err, retention)
}

// A commit that xolu rejects must leave no index events behind: xolu writes the
// /ts events first and is documented to undo them when the SQLite side fails.
func TestARejectedCommitLeavesNoIndexEventsLive(t *testing.T) {
	typ := uniqueType("ir")
	c0, tl := indexedClient(t, typ)
	ctx := context.Background()
	id := newEntity(t, c0, typ, map[string]any{"n": 1})
	ref := EntityRef{Type: typ, ID: id}
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	c0.Now = func() time.Time { return base.Add(100 * time.Millisecond) }

	up, _ := url.Parse(c0.BaseURL)
	proxy := httputil.NewSingleHostReverseProxy(up)
	var once int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == apiPath(c0, "/api/v1/commit") && atomic.CompareAndSwapInt32(&once, 0, 1) {
			// A rival saves between this client's look at the entity and its commit.
			if _, err := c0.Save(ctx, ref, 1, map[string]any{"n": 99}, SaveOptions{SavedBy: "rival"}); err != nil {
				t.Errorf("rival save: %v", err)
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	defer srv.Close()

	loser := New(srv.URL)
	loser.Tenant = c0.Tenant
	loser.Now = func() time.Time { return base.Add(200 * time.Millisecond) }
	if err := loser.UseTimeIndex(typ, tl); err != nil {
		t.Fatal(err)
	}
	_, err := loser.Save(ctx, ref, 1, map[string]any{"n": 2}, SaveOptions{SavedBy: "loser"})
	var ce *ErrVersionConflict
	if !errors.As(err, &ce) || ce.Current != 2 {
		t.Fatalf("err=%v, want a conflict with current 2", err)
	}
	rep, err := c0.CheckIndex(ctx, ref)
	if err != nil || !rep.OK() || rep.Events != 2 {
		t.Fatalf("after a rejected commit CheckIndex = %+v err=%v: events from the rejected commit were left in the index (want exactly the rival's two)", rep, err)
	}
}

// A refused /fsm walk cancels the whole save, and that includes its index
// events: with both options on, a refusal must leave no trace in either.
func TestARefusedWalkLeavesNoIndexEventsLive(t *testing.T) {
	typ := uniqueType("iw")
	c, _ := indexedClient(t, typ)
	ctx := context.Background()
	id := newEntity(t, c, typ, map[string]any{"n": 1})
	ref := EntityRef{Type: typ, ID: id}
	mid := newLifecycle(t, c, ref)
	c.Now = func() time.Time { return time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC) }

	// The guard on "submit" needs a reviewer: without one the walk is refused.
	_, err := c.Save(ctx, ref, 1, map[string]any{"n": 2}, SaveOptions{
		SavedBy: "u",
		Walk:    &LifecycleWalk{MachineID: mid, Input: "submit", Payload: map[string]any{"reviewer": ""}},
	})
	var lr *ErrLifecycleRejected
	if !errors.As(err, &lr) {
		t.Fatalf("err=%v, want ErrLifecycleRejected", err)
	}
	rep, err := c.CheckIndex(ctx, ref)
	if err != nil || rep.Events != 0 || !rep.OK() {
		t.Fatalf("a refused walk left index events behind: %+v err=%v", rep, err)
	}
	if machineState(t, c, mid) != "Draft" {
		t.Fatal("a refused walk moved the machine")
	}

	// An accepted one writes the baseline and the save to the index, and moves the machine.
	res, err := c.SaveDetailed(ctx, ref, 1, map[string]any{"n": 2}, SaveOptions{
		SavedBy: "u",
		Walk:    &LifecycleWalk{MachineID: mid, Input: "submit", Payload: map[string]any{"reviewer": "r1"}},
	})
	if err != nil || res.Walk == nil || res.Walk.Current != "Review" {
		t.Fatalf("accepted walk: %+v err=%v", res, err)
	}
	if rep, err = c.CheckIndex(ctx, ref); err != nil || rep.Events != 2 || !rep.OK() {
		t.Fatalf("after an accepted walk: %+v err=%v, want 2 events", rep, err)
	}
}

// The rest of the client works on tenant routes, v2 routes included.
func TestTenantRoutesCarryEverythingExceptOQLLive(t *testing.T) {
	c := tsClient(t)
	ctx := context.Background()
	typ := uniqueType("tr")
	id := newEntity(t, c, typ, map[string]any{"title": "v1"})
	ref := EntityRef{Type: typ, ID: id}
	mid := newLifecycle(t, c, ref)

	res, err := c.SaveDetailed(ctx, ref, 1, map[string]any{"title": "v2"}, SaveOptions{
		SavedBy: "u", Reason: "tenant",
		Walk: &LifecycleWalk{MachineID: mid, Input: "submit", Payload: map[string]any{"reviewer": "r"}},
	})
	if err != nil || res.Version != 2 || res.Walk == nil || res.Walk.Current != "Review" {
		t.Fatalf("Save with a walk: %+v err=%v", res, err)
	}
	if err := c.SetLabel(ctx, ref, "published", 2, "u"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	if ls, err := c.Labels(ctx, ref); err != nil || len(ls) != 1 || ls[0].Version != 2 {
		t.Fatalf("Labels = %+v err=%v", ls, err)
	}
	if rec, err := c.GetVersion(ctx, ref, 2); err != nil || rec.Reason != "tenant" || rec.LifecycleInput != "submit" {
		t.Fatalf("GetVersion = %+v err=%v", rec, err)
	}
	if v, err := c.Restore(ctx, ref, 1, 2, SaveOptions{SavedBy: "u"}); err != nil || v != 3 {
		t.Fatalf("Restore: v=%d err=%v", v, err)
	}
	if _, err := c.Delete(ctx, ref, 3, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !entityGone(t, c, ref) {
		t.Fatal("the entity still exists")
	}
	// And the OQL reads fail loudly rather than reporting an empty history.
	if _, err := c.ListVersions(ctx, ref, 0); !errors.Is(err, ErrTenantOQL) {
		t.Fatalf("ListVersions on tenant routes: err=%v, want ErrTenantOQL", err)
	}
	if _, err := c.CheckHistory(ctx, ref); !errors.Is(err, ErrTenantOQL) {
		t.Fatalf("CheckHistory on tenant routes: err=%v, want ErrTenantOQL", err)
	}
}
