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
	"net/http/httputil"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Tests for Delete, tombstones, and the "not found" vocabulary. Tests marked
// "unit" use a fake server; the others need XOLU_URL.

// ---- unit ------------------------------------------------------------------

type fakeXolu struct {
	mu      sync.Mutex
	log     []string
	commits []map[string]any
	deletes int
	rows    map[string]string // request path -> history row JSON; absent means 404
	commit  func(w http.ResponseWriter)
}

func newFakeXolu(t *testing.T, version int, rows map[string]string, commit func(w http.ResponseWriter)) (*fakeXolu, *Client) {
	t.Helper()
	f := &fakeXolu{rows: rows, commit: commit}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.log = append(f.log, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/oql/query":
			t.Errorf("unexpected OQL query")
			fmt.Fprint(w, `{"data":null}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/thing/7":
			fmt.Fprintf(w, `{"_version":%d,"id":7,"name":"x"}`, version)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/thing_version/"):
			if body, ok := f.rows[r.URL.Path]; ok {
				fmt.Fprint(w, body)
				return
			}
			http.NotFound(w, r)
		case r.URL.Path == "/api/v1/commit":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.commits = append(f.commits, body)
			f.mu.Unlock()
			f.commit(w)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/thing/7":
			f.mu.Lock()
			f.deletes++
			f.mu.Unlock()
			fmt.Fprint(w, `{"cascaded_deletes":["thing:7","other:3"],"message":"ok"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return f, New(srv.URL)
}

func rowPath(version int) string {
	return fmt.Sprintf("/api/v1/thing_version/%d", historyID(7, version))
}

func (f *fakeXolu) indexOf(entry string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, e := range f.log {
		if e == entry {
			return i
		}
	}
	return -1
}

func TestDeleteWritesTheTombstoneBeforeRemovingTheEntity(t *testing.T) {
	f, c := newFakeXolu(t, 3, nil, func(w http.ResponseWriter) { fmt.Fprint(w, `{"update":{"version":4}}`) })
	ref := EntityRef{Type: "thing", ID: 7}

	res, err := c.Delete(context.Background(), ref, 3, SaveOptions{SavedBy: "alice", Reason: "gone"})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if res.Version != 4 || !reflect.DeepEqual(res.Cascaded, []string{"other:3"}) {
		t.Fatalf("result = %+v, want version 4 and the other cascaded entity only", res)
	}
	if len(f.commits) != 1 || f.deletes != 1 {
		t.Fatalf("commits=%d deletes=%d, want 1 and 1", len(f.commits), f.deletes)
	}
	if f.indexOf("POST /api/v1/commit") > f.indexOf("DELETE /api/v1/thing/7") {
		t.Fatalf("the entity was removed before the tombstone was committed: %v", f.log)
	}

	body := f.commits[0]
	up := body["update"].(map[string]any)
	if up["version"] != float64(3) || !reflect.DeepEqual(up["data"], map[string]any{"name": "x"}) {
		t.Fatalf("update = %v, want the unchanged document at version 3", up)
	}
	rows := body["append"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows = %v, want a baseline and a tombstone", rows)
	}
	base, tomb := rows[0].(map[string]any), rows[1].(map[string]any)
	if base["id"] != float64(historyID(7, 3)) || tomb["id"] != float64(historyID(7, 4)) {
		t.Fatalf("ids = %v and %v", base["id"], tomb["id"])
	}
	td := tomb["data"].(map[string]any)
	if td["change_kind"] != ChangeDelete || td["version"] != float64(4) || td["entity_id"] != float64(7) ||
		td["saved_by"] != "alice" || td["reason"] != "gone" || !reflect.DeepEqual(td["snapshot"], map[string]any{"name": "x"}) {
		t.Fatalf("tombstone data = %v", td)
	}
}

func TestDeleteDoesNotRemoveWhenTheTombstoneFails(t *testing.T) {
	f, c := newFakeXolu(t, 3, nil, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":{"code":"XOLU-CM001","message":"x","status":409},"current_version":4}`)
	})
	_, err := c.Delete(context.Background(), EntityRef{Type: "thing", ID: 7}, 3, SaveOptions{})
	var ce *ErrVersionConflict
	if !errors.As(err, &ce) || ce.Current != 4 {
		t.Fatalf("err=%v, want a conflict with current 4", err)
	}
	if f.deletes != 0 {
		t.Fatalf("the entity was removed although the tombstone was not recorded")
	}
	// A stale base is refused before anything is written.
	if _, err := c.Delete(context.Background(), EntityRef{Type: "thing", ID: 7}, 2, SaveOptions{}); !errors.As(err, &ce) || ce.Current != 3 {
		t.Fatalf("stale base: err=%v, want a conflict with current 3", err)
	}
	if len(f.commits) != 1 {
		t.Fatalf("commits = %d, want only the first attempt", len(f.commits))
	}
}

func TestDeleteFinishesAPendingDeletion(t *testing.T) {
	rows := map[string]string{rowPath(4): `{"id":7000004,"entity_id":7,"version":4,"change_kind":"delete"}`}
	f, c := newFakeXolu(t, 4, rows, func(w http.ResponseWriter) { t.Errorf("a commit was sent"); fmt.Fprint(w, `{}`) })
	res, err := c.Delete(context.Background(), EntityRef{Type: "thing", ID: 7}, 3, SaveOptions{})
	if err != nil || res.Version != 4 {
		t.Fatalf("Delete: %+v err=%v, want it to finish the recorded deletion at version 4", res, err)
	}
	if len(f.commits) != 0 || f.deletes != 1 {
		t.Fatalf("commits=%d deletes=%d, want 0 and 1", len(f.commits), f.deletes)
	}
}

func TestSaveAndRestoreRefuseATombstone(t *testing.T) {
	rows := map[string]string{
		rowPath(4): `{"id":7000004,"entity_id":7,"version":4,"change_kind":"delete"}`,
		rowPath(1): `{"id":7000001,"entity_id":7,"version":1,"change_kind":"baseline","snapshot":{"name":"old"}}`,
	}
	f, c := newFakeXolu(t, 4, rows, func(w http.ResponseWriter) { t.Errorf("a commit was sent"); fmt.Fprint(w, `{}`) })
	ref := EntityRef{Type: "thing", ID: 7}
	if _, err := c.Save(context.Background(), ref, 4, map[string]any{"name": "y"}, SaveOptions{}); !errors.Is(err, ErrDeleted) {
		t.Fatalf("Save: err=%v, want ErrDeleted", err)
	}
	if _, err := c.Restore(context.Background(), ref, 1, 4, SaveOptions{}); !errors.Is(err, ErrDeleted) {
		t.Fatalf("Restore: err=%v, want ErrDeleted", err)
	}
	if len(f.commits) != 0 || f.deletes != 0 {
		t.Fatalf("commits=%d deletes=%d, want nothing written", len(f.commits), f.deletes)
	}
}

func TestNotFoundErrorsAreDistinctAndShareAParent(t *testing.T) {
	if !errors.Is(ErrEntityNotFound, ErrNotFound) || !errors.Is(ErrVersionNotFound, ErrNotFound) {
		t.Fatal("the specific errors must match ErrNotFound")
	}
	if errors.Is(ErrEntityNotFound, ErrVersionNotFound) || errors.Is(ErrVersionNotFound, ErrEntityNotFound) {
		t.Fatal("the entity and version errors must be distinguishable")
	}
	if !strings.Contains(ErrEntityNotFound.Error(), "does not exist now") {
		t.Errorf("entity error does not say when: %q", ErrEntityNotFound)
	}
	if !strings.Contains(ErrVersionNotFound.Error(), "no history row") {
		t.Errorf("version error does not say what is missing: %q", ErrVersionNotFound)
	}
	for _, e := range []error{ErrEntityNotFound, ErrVersionNotFound, ErrDeleted} {
		if strings.Contains(strings.ToLower(e.Error()), "never") {
			t.Errorf("%q claims something about all time; xoluver cannot know that", e)
		}
	}
}

// ---- live server -----------------------------------------------------------

func failFirstDelete(t *testing.T, c0 *Client, typ string, id int) *httptest.Server {
	t.Helper()
	up, _ := url.Parse(c0.BaseURL)
	proxy := httputil.NewSingleHostReverseProxy(up)
	var failed int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == apiPath(c0, fmt.Sprintf("/api/v1/%s/%d", typ, id)) && atomic.CompareAndSwapInt32(&failed, 0, 1) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"code":"XOLU-ST999","message":"injected failure","status":500}}`)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDeleteRecordsATombstone(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("dt")
	id := newEntity(t, c, typ, map[string]any{"title": "v1"})
	ref := EntityRef{Type: typ, ID: id}
	v, err := c.Save(ctx, ref, 1, map[string]any{"title": "v2"}, SaveOptions{SavedBy: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetLabel(ctx, ref, "published", 2, "u"); err != nil {
		t.Fatal(err)
	}

	res, err := c.Delete(ctx, ref, v, SaveOptions{SavedBy: "alice", Reason: "decommissioned"})
	if err != nil || res.Version != 3 || len(res.Cascaded) != 0 {
		t.Fatalf("Delete: %+v err=%v", res, err)
	}
	if !entityGone(t, c, ref) {
		t.Fatal("the entity still exists after Delete")
	}

	list := mustList(t, c, ref)
	var kinds []string
	for _, s := range list {
		kinds = append(kinds, fmt.Sprintf("v%d:%s", s.Version, s.ChangeKind))
	}
	if want := []string{"v3:delete", "v2:save", "v1:baseline"}; !reflect.DeepEqual(kinds, want) {
		t.Fatalf("history = %v, want %v", kinds, want)
	}
	rec, err := c.GetVersion(ctx, ref, 3)
	if err != nil || rec.SavedBy != "alice" || rec.Reason != "decommissioned" || rec.Snapshot["title"] != "v2" {
		t.Fatalf("tombstone = %+v err=%v, want alice's, with the last state as its snapshot", rec, err)
	}
	rep, _ := c.CheckHistory(ctx, ref)
	if !rep.OK() || !rep.Deleted || rep.EntityExists || rep.PendingDelete || rep.UntrackedDelete || len(rep.Versions) != 3 {
		t.Fatalf("report = %+v, want a clean deleted entity with 3 versions", rep)
	}
	if ls, err := c.Labels(ctx, ref); err != nil || len(ls) != 0 {
		t.Fatalf("labels after delete = %+v err=%v; xolu removes them with the entity", ls, err)
	}

	// Existing now, deleted before: each says which one it is.
	if _, err := c.Save(ctx, ref, 3, map[string]any{"title": "x"}, SaveOptions{}); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("Save after delete: err=%v, want ErrEntityNotFound", err)
	}
	if _, err := c.Delete(ctx, ref, 3, SaveOptions{}); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("second Delete: err=%v, want ErrEntityNotFound", err)
	}
	if _, err := c.GetVersion(ctx, ref, 99); !errors.Is(err, ErrVersionNotFound) || errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("GetVersion(99): err=%v, want ErrVersionNotFound only", err)
	}
}

func TestDeleteOfAnEntityWithNoHistory(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("dn")
	id := newEntity(t, c, typ, map[string]any{"t": 1})
	ref := EntityRef{Type: typ, ID: id}

	res, err := c.Delete(ctx, ref, 1, SaveOptions{SavedBy: "u"})
	if err != nil || res.Version != 2 {
		t.Fatalf("Delete: %+v err=%v", res, err)
	}
	list := mustList(t, c, ref)
	if len(list) != 2 || list[0].ChangeKind != ChangeDelete || list[1].ChangeKind != ChangeBaseline {
		t.Fatalf("history = %+v, want a baseline and a tombstone", list)
	}
	if rec, _ := c.GetVersion(ctx, ref, 1); asInt(rec.Snapshot["t"]) != 1 {
		t.Fatalf("baseline = %+v", rec)
	}
	if rep, _ := c.CheckHistory(ctx, ref); !rep.OK() || !rep.Deleted {
		t.Fatalf("report = %+v", rep)
	}
}

func TestDeleteWithAStaleBaseWritesNothing(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("ds")
	id := newEntity(t, c, typ, map[string]any{"t": 1})
	ref := EntityRef{Type: typ, ID: id}
	if _, err := c.Save(ctx, ref, 1, map[string]any{"t": 2}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	_, err := c.Delete(ctx, ref, 1, SaveOptions{SavedBy: "u"})
	var ce *ErrVersionConflict
	if !errors.As(err, &ce) || ce.Current != 2 {
		t.Fatalf("err=%v, want a conflict with current 2", err)
	}
	if entityGone(t, c, ref) {
		t.Fatal("a refused Delete removed the entity")
	}
	if rep, _ := c.CheckHistory(ctx, ref); !rep.OK() || rep.Deleted || len(rep.Versions) != 2 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestInterruptedDeleteIsVisibleAndRecoverable(t *testing.T) {
	c0 := testClient(t)
	ctx := context.Background()
	typ := uniqueType("di")
	id := newEntity(t, c0, typ, map[string]any{"t": 1})
	ref := EntityRef{Type: typ, ID: id}
	if _, err := c0.Save(ctx, ref, 1, map[string]any{"t": 2}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}

	c := New(failFirstDelete(t, c0, typ, id).URL)
	c.Tenant = c0.Tenant
	_, err := c.Delete(ctx, ref, 2, SaveOptions{SavedBy: "alice"})
	var inc *ErrDeleteIncomplete
	if !errors.As(err, &inc) || inc.Version != 3 {
		t.Fatalf("err=%v, want ErrDeleteIncomplete at version 3", err)
	}

	// The entity exists, its history says it is deleted, and nothing may build on it.
	if _, ver, _ := c0.getEntity(ctx, ref); ver != 3 {
		t.Fatalf("entity version = %d, want 3", ver)
	}
	if _, err := c0.Save(ctx, ref, 3, map[string]any{"t": 3}, SaveOptions{}); !errors.Is(err, ErrDeleted) {
		t.Fatalf("Save during a pending deletion: err=%v, want ErrDeleted", err)
	}
	if _, err := c0.Restore(ctx, ref, 1, 3, SaveOptions{}); !errors.Is(err, ErrDeleted) {
		t.Fatalf("Restore during a pending deletion: err=%v, want ErrDeleted", err)
	}
	rep, _ := c0.CheckHistory(ctx, ref)
	if rep.OK() || !rep.PendingDelete || !rep.Deleted || !rep.EntityExists {
		t.Fatalf("report = %+v, want a pending deletion", rep)
	}

	// Delete again, even with an old base: it finishes the recorded deletion.
	res, err := c0.Delete(ctx, ref, 2, SaveOptions{})
	if err != nil || res.Version != 3 {
		t.Fatalf("finishing Delete: %+v err=%v", res, err)
	}
	if !entityGone(t, c0, ref) {
		t.Fatal("the entity still exists")
	}
	if rep, _ = c0.CheckHistory(ctx, ref); !rep.OK() || !rep.Deleted || rep.PendingDelete || len(rep.Versions) != 3 {
		t.Fatalf("report after finishing = %+v", rep)
	}
}

func TestDeleteWithALifecycleStep(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("dw")
	id := newEntity(t, c, typ, map[string]any{"t": 1})
	ref := EntityRef{Type: typ, ID: id}
	mid := newLifecycle(t, c, ref)

	// A refused transition: nothing is recorded and the entity stays.
	_, err := c.Delete(ctx, ref, 1, SaveOptions{SavedBy: "u", Walk: &LifecycleWalk{MachineID: mid, Input: "submit", Payload: map[string]any{"reviewer": ""}}})
	var lr *ErrLifecycleRejected
	if !errors.As(err, &lr) {
		t.Fatalf("err=%v, want ErrLifecycleRejected", err)
	}
	if entityGone(t, c, ref) || len(mustList(t, c, ref)) != 0 || machineState(t, c, mid) != "Draft" {
		t.Fatal("a refused transition left a trace")
	}

	// An accepted one retires the machine in the same commit as the tombstone.
	res, err := c.Delete(ctx, ref, 1, SaveOptions{SavedBy: "u", Walk: &LifecycleWalk{MachineID: mid, Input: "retire"}})
	if err != nil || res.Walk == nil || res.Walk.Current != "Retired" || !res.Walk.Terminal {
		t.Fatalf("Delete: %+v err=%v", res, err)
	}
	if rec, _ := c.GetVersion(ctx, ref, res.Version); rec.LifecycleInput != "retire" || rec.ChangeKind != ChangeDelete {
		t.Fatalf("tombstone = %+v", rec)
	}
	if machineState(t, c, mid) != "Retired" || !entityGone(t, c, ref) {
		t.Fatal("machine or entity in the wrong state")
	}
}

func TestUntrackedDeleteIsReported(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("du")
	id := newEntity(t, c, typ, map[string]any{"t": 1})
	ref := EntityRef{Type: typ, ID: id}
	if _, err := c.Save(ctx, ref, 1, map[string]any{"t": 2}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := rawDo(t, c, http.MethodDelete, fmt.Sprintf("/api/v1/%s/%d", typ, id), nil); err != nil {
		t.Fatal(err)
	}
	rep, _ := c.CheckHistory(ctx, ref)
	if rep.OK() || !rep.UntrackedDelete || rep.Deleted || rep.EntityExists {
		t.Fatalf("report = %+v, want an untracked deletion: the entity is gone and the history never says it was deleted", rep)
	}
}

// Ids are not reused. If something recreates an entity under the id of a deleted
// one, its history would collide with the old life, so Save refuses.
func TestARecreatedIDIsRefusedLoudly(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("dr")
	id := newEntity(t, c, typ, map[string]any{"t": 1})
	ref := EntityRef{Type: typ, ID: id}
	if _, err := c.Save(ctx, ref, 1, map[string]any{"t": 2}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Delete(ctx, ref, 2, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := rawDo(t, c, http.MethodPost, fmt.Sprintf("/api/v1/%s/save/%d", typ, id), map[string]any{"t": "back"}); err != nil {
		t.Fatalf("recreating the id: %v", err)
	}
	_, err := c.Save(ctx, ref, 1, map[string]any{"t": "back again"}, SaveOptions{SavedBy: "u"})
	if !errors.As(err, new(*ErrCorruptHistory)) {
		t.Fatalf("Save on a recreated id: err=%v, want ErrCorruptHistory", err)
	}
	if cur, ver, _ := c.getEntity(ctx, ref); ver != 1 || cur["t"] != "back" {
		t.Fatalf("a refused save changed the entity: %v v=%d", cur, ver)
	}
	if rep, _ := c.CheckHistory(ctx, ref); rep.OK() {
		t.Fatalf("CheckHistory did not flag a recreated id: %+v", rep)
	}
}
