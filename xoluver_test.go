// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

package xoluver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

// These are integration tests: they need a running xolu server with
// XOLU_API_V2_ENABLED=true and auth disabled. Point XOLU_URL at it, e.g.
//
//	XOLU_URL=http://localhost:9090 go test ./...
//
// Without XOLU_URL they are skipped. Each test uses a fresh entity type, so
// runs do not interfere with each other.

func testClient(t *testing.T) *Client {
	t.Helper()
	u := os.Getenv("XOLU_URL")
	if u == "" {
		t.Skip("XOLU_URL not set; skipping integration test")
	}
	return New(u)
}

// apiPath is the path a request for p takes with c's tenant setting, for tests
// that proxy live traffic and recognise requests by path.
func apiPath(c *Client, p string) string {
	r, err := c.route(p)
	if err != nil {
		panic(err)
	}
	return r
}

func uniqueType(prefix string) string {
	return prefix + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func newEntity(t *testing.T, c *Client, typ string, doc map[string]any) int {
	t.Helper()
	var resp struct {
		ID int `json:"id"`
	}
	if _, err := c.doJSON(context.Background(), "POST", "/api/v1/"+typ, doc, &resp); err != nil {
		t.Fatalf("create %s: %v", typ, err)
	}
	if resp.ID <= 0 {
		t.Fatalf("create %s: no id", typ)
	}
	return resp.ID
}

func mustList(t *testing.T, c *Client, ref EntityRef) []VersionSummary {
	t.Helper()
	l, err := c.ListVersions(context.Background(), ref, 0)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	return l
}

func TestSaveRestoreLabelsEndToEnd(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("vt")
	id := newEntity(t, c, typ, map[string]any{"name": "pump-1", "site": "A"})
	ref := EntityRef{Type: typ, ID: id}

	// First save: base 1, no history exists yet, so a baseline row is added.
	v, err := c.Save(ctx, ref, 1, map[string]any{"name": "pump-1b", "site": "A"}, SaveOptions{SavedBy: "u1", Reason: "rename"})
	if err != nil || v != 2 {
		t.Fatalf("first save: v=%d err=%v", v, err)
	}
	l := mustList(t, c, ref)
	if len(l) != 2 || l[0].Version != 2 || l[0].ChangeKind != ChangeSave || l[1].Version != 1 || l[1].ChangeKind != ChangeBaseline {
		t.Fatalf("history after first save: %+v", l)
	}
	if l[0].Reason != "rename" || l[0].SavedBy != "u1" {
		t.Fatalf("audit fields lost: %+v", l[0])
	}

	// Second save: history for base 2 exists, so no baseline. System fields in
	// the caller's document must be ignored.
	v, err = c.Save(ctx, ref, 2, map[string]any{"name": "pump-1c", "site": "B", "_version": 999}, SaveOptions{SavedBy: "u2"})
	if err != nil || v != 3 {
		t.Fatalf("second save: v=%d err=%v", v, err)
	}
	if n := len(mustList(t, c, ref)); n != 3 {
		t.Fatalf("history rows = %d, want 3", n)
	}

	// Stale save: rejected, nothing written.
	_, err = c.Save(ctx, ref, 1, map[string]any{"name": "stale"}, SaveOptions{SavedBy: "u3"})
	var ce *ErrVersionConflict
	if !errors.As(err, &ce) || ce.Current != 3 {
		t.Fatalf("stale save: err=%v, want conflict with current 3", err)
	}
	if n := len(mustList(t, c, ref)); n != 3 {
		t.Fatalf("stale save wrote history: rows = %d, want 3", n)
	}

	// Restore version 1 as a new version 4.
	v, err = c.Restore(ctx, ref, 1, 3, SaveOptions{SavedBy: "u1", Reason: "undo"})
	if err != nil || v != 4 {
		t.Fatalf("restore: v=%d err=%v", v, err)
	}
	l = mustList(t, c, ref)
	if len(l) != 4 || l[0].ChangeKind != ChangeRestore || l[0].RestoredFrom != 1 {
		t.Fatalf("history after restore: %+v", l)
	}
	cur, curVer, err := c.getEntity(ctx, ref)
	if err != nil || curVer != 4 || cur["name"] != "pump-1" || cur["site"] != "A" {
		t.Fatalf("current after restore: %v v=%d err=%v", cur, curVer, err)
	}

	// Older versions stay readable and unchanged.
	rec, err := c.GetVersion(ctx, ref, 2)
	if err != nil || rec.Snapshot["name"] != "pump-1b" {
		t.Fatalf("GetVersion(2): %+v err=%v", rec, err)
	}
	if _, err := c.GetVersion(ctx, ref, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetVersion(99): err=%v, want ErrNotFound", err)
	}

	// Labels: set, list, reject unknown version, move.
	if err := c.SetLabel(ctx, ref, "published", 2, "u1"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	ls, err := c.Labels(ctx, ref)
	if err != nil || len(ls) != 1 || ls[0].Name != "published" || ls[0].Version != 2 {
		t.Fatalf("Labels: %+v err=%v", ls, err)
	}
	if err := c.SetLabel(ctx, ref, "published", 99, "u1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetLabel(99): err=%v, want ErrNotFound", err)
	}
	if err := c.SetLabel(ctx, ref, "published", 4, "u1"); err != nil {
		t.Fatalf("move label: %v", err)
	}
	if ls, _ = c.Labels(ctx, ref); len(ls) != 1 || ls[0].Version != 4 {
		t.Fatalf("label after move: %+v", ls)
	}
}

// raceSaves runs n concurrent saves against the same base version.
func raceSaves(t *testing.T, c *Client, ref EntityRef, base, n int) (ok, conflicts int) {
	t.Helper()
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := c.Save(context.Background(), ref, base,
				map[string]any{"name": fmt.Sprintf("writer-%d", i)}, SaveOptions{SavedBy: fmt.Sprintf("u%d", i)})
			mu.Lock()
			defer mu.Unlock()
			var ce *ErrVersionConflict
			switch {
			case err == nil:
				ok++
			case errors.As(err, &ce):
				conflicts++
			default:
				t.Errorf("writer %d: unexpected error: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	return ok, conflicts
}

func TestConcurrentSaves(t *testing.T) {
	c := testClient(t)
	typ := uniqueType("vc")
	id := newEntity(t, c, typ, map[string]any{"name": "start"})
	ref := EntityRef{Type: typ, ID: id}
	const n = 8

	// No history yet: every racer wants to add a baseline. Exactly one commit
	// may win, and it must leave exactly baseline + save (no duplicate rows).
	ok, conflicts := raceSaves(t, c, ref, 1, n)
	if ok != 1 || conflicts != n-1 {
		t.Fatalf("race without history: ok=%d conflicts=%d, want 1 and %d", ok, conflicts, n-1)
	}
	if l := mustList(t, c, ref); len(l) != 2 {
		t.Fatalf("history rows after baseline race = %d, want 2: %+v", len(l), l)
	}

	// History exists: exactly one winner, one new row.
	ok, conflicts = raceSaves(t, c, ref, 2, n)
	if ok != 1 || conflicts != n-1 {
		t.Fatalf("race with history: ok=%d conflicts=%d, want 1 and %d", ok, conflicts, n-1)
	}
	l := mustList(t, c, ref)
	if len(l) != 3 || l[0].Version != 3 {
		t.Fatalf("history after second race: %+v", l)
	}
}

func TestInputValidation(t *testing.T) {
	c := New("http://127.0.0.1:1") // never contacted: validation fails first
	ctx := context.Background()
	if _, err := c.Save(ctx, EntityRef{Type: "Bad Type; DROP", ID: 1}, 1, map[string]any{}, SaveOptions{}); err == nil {
		t.Error("invalid type accepted")
	}
	if _, err := c.Save(ctx, EntityRef{Type: "asset", ID: 0}, 1, map[string]any{}, SaveOptions{}); err == nil {
		t.Error("invalid id accepted")
	}
	if _, err := c.Save(ctx, EntityRef{Type: "asset", ID: 1}, 0, map[string]any{}, SaveOptions{}); err == nil {
		t.Error("invalid base version accepted")
	}
	if err := c.SetLabel(ctx, EntityRef{Type: "asset", ID: 1}, "bad name!", 1, "u"); err == nil {
		t.Error("invalid label name accepted")
	}
}
