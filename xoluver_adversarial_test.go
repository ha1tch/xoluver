// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

package xoluver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Adversarial tests. Most need a live xolu server (XOLU_URL, see
// xoluver_test.go); the ones marked "unit" do not.
//
// Several tests characterize xolu behavior that the client has to work around
// (compare-and-set creating a missing entity, no uniqueness on history rows, an
// undersized request limit). They assert what xolu does today, so a change in
// xolu shows up here.

// ---- helpers ---------------------------------------------------------------

func rawDo(t *testing.T, c *Client, method, path string, body any) error {
	t.Helper()
	_, err := c.doJSON(context.Background(), method, path, body, nil)
	return err
}

func entityGone(t *testing.T, c *Client, ref EntityRef) bool {
	t.Helper()
	_, _, err := c.getEntity(context.Background(), ref)
	if err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatalf("getEntity: %v", err)
	}
	return errors.Is(err, ErrNotFound)
}

// canon normalizes a decoded JSON value for comparison. Integers that fit in
// int64 compare exactly; every other number compares as the float64 it parses
// to, so 0.1 equals 1e-1 and an exact binary expansion of a float equals its
// shortest decimal form.
func canon(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = canon(vv)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = canon(vv)
		}
		return out
	case json.Number:
		return canonNumber(x.String())
	case float64:
		return canonNumber(strconv.FormatFloat(x, 'g', -1, 64))
	case int:
		return canonNumber(strconv.FormatInt(int64(x), 10))
	case int64:
		return canonNumber(strconv.FormatInt(x, 10))
	default:
		return v
	}
}

func canonNumber(s string) string {
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return "int:" + strconv.FormatInt(i, 10)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return "badnum:" + s
	}
	return "float:" + strconv.FormatFloat(f, 'g', -1, 64)
}

// diffPaths lists where two canonicalized values differ.
func diffPaths(a, b any, path string, out *[]string) {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, path+": object vs "+fmt.Sprintf("%T", b))
			return
		}
		for k, v := range x {
			if w, ok := y[k]; ok {
				diffPaths(v, w, path+"."+k, out)
			} else {
				*out = append(*out, path+"."+k+": missing after round trip")
			}
		}
		for k := range y {
			if _, ok := x[k]; !ok {
				*out = append(*out, path+"."+k+": appeared after round trip")
			}
		}
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			*out = append(*out, path+": array changed")
			return
		}
		for i := range x {
			diffPaths(x[i], y[i], fmt.Sprintf("%s[%d]", path, i), out)
		}
	default:
		if !reflect.DeepEqual(a, b) {
			*out = append(*out, fmt.Sprintf("%s: %v -> %v", path, a, b))
		}
	}
}

var fuzzStrings = []string{
	"", " ", "plain", "h\u00e9llo \u4e16\u754c", "emoji \U0001F600 ok",
	"quote \" backslash \\ slash /", "line1\nline2\ttab", "<script>alert(1)</script>&amp;",
	"\u2028\u2029", "null", "0", `{"nested":"json"}`, "nul\u0000byte", strings.Repeat("x", 5000),
}

func genValue(r *rand.Rand, depth int) any {
	k := r.Intn(10)
	if depth <= 0 && k >= 8 {
		k = r.Intn(8)
	}
	switch k {
	case 0:
		return int64(r.Intn(2000) - 1000)
	case 1:
		return []int64{9007199254740993, -9007199254740993, math.MaxInt64, 0}[r.Intn(4)]
	case 2:
		return r.NormFloat64() * 1e6
	case 3:
		return []float64{0.1, 1e21, 1.5e-7, 123456789.123456789}[r.Intn(4)]
	case 4:
		return r.Intn(2) == 0
	case 5, 6:
		return fuzzStrings[r.Intn(len(fuzzStrings))]
	case 7:
		return nil
	case 8:
		m := map[string]any{}
		for i, n := 0, r.Intn(5); i < n; i++ {
			keys := []string{"a", "b", "_u", "id", "\u00fcnicode", "with space", "k" + fmt.Sprint(i)}
			m[keys[r.Intn(len(keys))]] = genValue(r, depth-1)
		}
		return m
	default:
		a := []any{}
		for i, n := 0, r.Intn(5); i < n; i++ {
			a = append(a, genValue(r, depth-1))
		}
		return a
	}
}

func genDoc(r *rand.Rand) map[string]any {
	d := map[string]any{}
	for i, n := 0, 1+r.Intn(6); i < n; i++ {
		keys := []string{"title", "meta", "list", "_flag", "n", "text", "k" + fmt.Sprint(i)}
		d[keys[r.Intn(len(keys))]] = genValue(r, 3)
	}
	return d
}

// ---- field handling --------------------------------------------------------

func TestAdvUserFieldsAndSystemFields(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("ua")
	id := newEntity(t, c, typ, map[string]any{"a": 1})
	ref := EntityRef{Type: typ, ID: id}

	doc := map[string]any{
		"_note":    "user field with an underscore prefix",
		"_version": 999,
		"id":       777,
		"n":        map[string]any{"_x": 1, "id": 5},
		"z":        1,
	}
	v, err := c.Save(ctx, ref, 1, doc, SaveOptions{SavedBy: "u1"})
	if err != nil || v != 2 {
		t.Fatalf("save: v=%d err=%v", v, err)
	}
	cur, ver, err := c.getEntity(ctx, ref)
	if err != nil || ver != 2 {
		t.Fatalf("entity: v=%d err=%v", ver, err)
	}
	if cur["_note"] != "user field with an underscore prefix" {
		t.Fatalf("user field _note was lost: %v", cur)
	}
	if nested, _ := cur["n"].(map[string]any); nested == nil || nested["_x"] == nil {
		t.Fatalf("nested underscore field lost: %v", cur["n"])
	}
	if !entityGone(t, c, EntityRef{Type: typ, ID: 777}) {
		t.Fatal("a body id created or moved an entity")
	}

	rec, err := c.GetVersion(ctx, ref, 2)
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	if _, has := rec.Snapshot["id"]; has {
		t.Errorf("snapshot carries id: %v", rec.Snapshot)
	}
	if _, has := rec.Snapshot["_version"]; has {
		t.Errorf("snapshot carries _version: %v", rec.Snapshot)
	}
	if !reflect.DeepEqual(canon(cur), canon(rec.Snapshot)) {
		var d []string
		diffPaths(canon(cur), canon(rec.Snapshot), "$", &d)
		t.Fatalf("snapshot differs from stored document: %v", d)
	}

	// A restore must not drop those fields either.
	if _, err := c.Restore(ctx, ref, 2, 2, SaveOptions{SavedBy: "u1"}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if cur, _, _ = c.getEntity(ctx, ref); cur["_note"] == nil {
		t.Fatalf("restore dropped _note: %v", cur)
	}
}

func TestAdvSnapshotEqualsStoredDocument(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("fz")
	id := newEntity(t, c, typ, map[string]any{"a": 1})
	ref := EntityRef{Type: typ, ID: id}
	r := rand.New(rand.NewSource(42))

	base := 1
	changed := 0
	for i := 0; i < 40; i++ {
		in := genDoc(r)
		v, err := c.Save(ctx, ref, base, in, SaveOptions{SavedBy: "fuzz"})
		if err != nil {
			t.Fatalf("doc %d: save: %v\ninput: %v", i, err, in)
		}
		base = v
		stored, ver, err := c.getEntity(ctx, ref)
		if err != nil || ver != v {
			t.Fatalf("doc %d: entity v=%d err=%v", i, ver, err)
		}
		rec, err := c.GetVersion(ctx, ref, v)
		if err != nil {
			t.Fatalf("doc %d: GetVersion: %v", i, err)
		}
		// The invariant that matters: the snapshot is exactly what xolu stored.
		if !reflect.DeepEqual(canon(stored), canon(rec.Snapshot)) {
			var d []string
			diffPaths(canon(stored), canon(rec.Snapshot), "$", &d)
			t.Fatalf("doc %d: snapshot differs from stored document: %v", i, d)
		}
		// Informational: how xolu's stored form differs from what was sent.
		var d []string
		diffPaths(canon(stripSystem(in)), canon(stored), "$", &d)
		if len(d) > 0 {
			changed++
			if changed <= 3 {
				t.Logf("doc %d: xolu stored a different document than sent: %v", i, d)
			}
		}
	}
	if changed > 0 {
		t.Logf("%d of 40 documents were altered by xolu on storage (snapshots matched the stored form in all cases)", changed)
	}
}

// xolu stores JSON numbers as float64, so integers beyond 2^53 lose precision in
// the entity itself. The versioning layer must not make that worse: snapshots
// must match the stored document, integers up to 2^53 stay exact, and large
// values that matter (ids, counters) have to travel as strings.
func TestAdvLargeIntegers(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("li")
	id := newEntity(t, c, typ, map[string]any{"a": 1})
	ref := EntityRef{Type: typ, ID: id}
	in := map[string]any{
		"safe":   int64(9007199254740991), // 2^53 - 1
		"big":    int64(9007199254740993), // 2^53 + 1
		"neg":    int64(-9007199254740993),
		"max":    int64(math.MaxInt64),
		"as_str": "9007199254740993",
	}
	v, err := c.Save(ctx, ref, 1, in, SaveOptions{SavedBy: "u"})
	if err != nil {
		t.Fatal(err)
	}
	stored, _, _ := c.getEntity(ctx, ref)
	rec, _ := c.GetVersion(ctx, ref, v)
	if !reflect.DeepEqual(canon(stored), canon(rec.Snapshot)) {
		t.Fatalf("snapshot differs from stored document: %v vs %v", rec.Snapshot, stored)
	}
	if n, _ := stored["safe"].(json.Number); n.String() != "9007199254740991" {
		t.Errorf("integer within 2^53 changed: %v", stored["safe"])
	}
	if stored["as_str"] != "9007199254740993" {
		t.Errorf("string value changed: %v", stored["as_str"])
	}
	for _, k := range []string{"big", "neg", "max"} {
		if n, _ := stored[k].(json.Number); n.String() != fmt.Sprint(in[k]) {
			t.Logf("xolu limitation: %s sent as %v, stored as %v", k, in[k], stored[k])
		}
	}
	// A restore writes back exactly what the snapshot holds, with no further drift.
	v2, err := c.Restore(ctx, ref, v, v, SaveOptions{SavedBy: "u"})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	after, _, _ := c.getEntity(ctx, ref)
	if !reflect.DeepEqual(canon(after), canon(stored)) {
		t.Fatalf("restore changed the document (version %d): %v vs %v", v2, after, stored)
	}
}

// ---- concurrency -----------------------------------------------------------

func increment(ctx context.Context, c *Client, ref EntityRef, by string) error {
	for attempt := 0; attempt < 1000; attempt++ {
		doc, ver, err := c.getEntity(ctx, ref)
		if err != nil {
			return err
		}
		n := asInt(doc["counter"])
		_, err = c.Save(ctx, ref, ver, map[string]any{"counter": n + 1}, SaveOptions{SavedBy: by})
		var ce *ErrVersionConflict
		if errors.As(err, &ce) {
			continue
		}
		return err
	}
	return errors.New("gave up after 1000 conflicts")
}

func TestAdvNoLostUpdates(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("lu")
	id := newEntity(t, c, typ, map[string]any{"counter": 0})
	ref := EntityRef{Type: typ, ID: id}
	const workers, each = 6, 8

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := increment(ctx, c, ref, fmt.Sprintf("w%d", w)); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("worker: %v", err)
	}

	total := workers * each
	cur, ver, _ := c.getEntity(ctx, ref)
	if asInt(cur["counter"]) != total || ver != total+1 {
		t.Fatalf("counter=%d version=%d, want %d and %d (lost update)", asInt(cur["counter"]), ver, total, total+1)
	}
	rep, err := c.CheckHistory(ctx, ref)
	if err != nil || !rep.OK() || len(rep.Versions) != total+1 {
		t.Fatalf("history: %+v err=%v, want %d clean versions", rep, err, total+1)
	}
	// Every snapshot holds exactly the counter value its version implies.
	list, _ := c.ListVersions(ctx, ref, 0)
	for _, s := range list {
		rec, err := c.GetVersion(ctx, ref, s.Version)
		if err != nil {
			t.Fatalf("GetVersion(%d): %v", s.Version, err)
		}
		if got := asInt(rec.Snapshot["counter"]); got != s.Version-1 {
			t.Fatalf("version %d snapshot counter = %d, want %d", s.Version, got, s.Version-1)
		}
	}
}

func TestAdvMixedConcurrentOperations(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("mx")
	id := newEntity(t, c, typ, map[string]any{"counter": 0})
	ref := EntityRef{Type: typ, ID: id}
	const workers, ops = 5, 15

	var committed int64
	var wg sync.WaitGroup
	errs := make(chan error, workers*ops)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(w) + 100))
			by := fmt.Sprintf("w%d", w)
			for i := 0; i < ops; i++ {
				doc, ver, err := c.getEntity(ctx, ref)
				if err != nil {
					errs <- err
					return
				}
				var opErr error
				switch k := r.Intn(10); {
				case k < 6:
					_, opErr = c.Save(ctx, ref, ver, map[string]any{"counter": asInt(doc["counter"]) + 1, "by": by}, SaveOptions{SavedBy: by})
				case k < 8:
					_, opErr = c.Restore(ctx, ref, 1+r.Intn(ver), ver, SaveOptions{SavedBy: by, Reason: "mixed"})
				default:
					opErr = c.SetLabel(ctx, ref, fmt.Sprintf("l%d", r.Intn(3)), 1+r.Intn(ver), by)
					if opErr == nil {
						continue
					}
				}
				var ce *ErrVersionConflict
				switch {
				case opErr == nil:
					atomic.AddInt64(&committed, 1)
				case errors.As(opErr, &ce), errors.Is(opErr, ErrNotFound):
					// lost a race, or labeled a version not written yet
				default:
					errs <- fmt.Errorf("worker %d op %d: %w", w, i, opErr)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	cur, ver, _ := c.getEntity(ctx, ref)
	if int64(ver) != 1+committed {
		t.Fatalf("version %d, but %d saves/restores succeeded (want version %d)", ver, committed, 1+committed)
	}
	rep, err := c.CheckHistory(ctx, ref)
	if err != nil || !rep.OK() {
		t.Fatalf("history: %+v err=%v", rep, err)
	}
	rec, _ := c.GetVersion(ctx, ref, ver)
	if !reflect.DeepEqual(canon(cur), canon(rec.Snapshot)) {
		t.Fatalf("latest snapshot differs from the entity: %v vs %v", rec.Snapshot, cur)
	}
	labels, err := c.Labels(ctx, ref)
	if err != nil {
		t.Fatalf("Labels: %v", err)
	}
	for _, l := range labels {
		if l.Malformed || l.Version < 1 || l.Version > ver {
			t.Fatalf("bad label after race: %+v (current version %d)", l, ver)
		}
	}
}

// ---- ambiguous outcomes ----------------------------------------------------

// A response lost after xolu committed: the client sees a network error, the
// save did land, and a retry with the same base must be a clean conflict, not a
// second version.
func TestAdvResponseLostRetryIsSafe(t *testing.T) {
	c0 := testClient(t)
	up, _ := url.Parse(c0.BaseURL)
	proxy := httputil.NewSingleHostReverseProxy(up)
	var dropped int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/commit" && atomic.CompareAndSwapInt32(&dropped, 0, 1) {
			body, _ := io.ReadAll(r.Body)
			resp, err := http.Post(c0.BaseURL+r.URL.Path, "application/json", bytes.NewReader(body))
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
				}
			}
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer srv.Close()

	c := New(srv.URL)
	ctx := context.Background()
	typ := uniqueType("rl")
	id := newEntity(t, c0, typ, map[string]any{"title": "v1"})
	ref := EntityRef{Type: typ, ID: id}

	_, err := c.Save(ctx, ref, 1, map[string]any{"title": "v2"}, SaveOptions{SavedBy: "u1"})
	var ce *ErrVersionConflict
	if err == nil || errors.As(err, &ce) {
		t.Fatalf("first save: err=%v, want a transport error", err)
	}
	// The outcome is ambiguous to the caller, but the save did land.
	if _, ver, _ := c.getEntity(ctx, ref); ver != 2 {
		t.Fatalf("entity version = %d, want 2 (commit should have landed)", ver)
	}
	// Retrying with the same base is rejected: no duplicate version.
	_, err = c.Save(ctx, ref, 1, map[string]any{"title": "v2"}, SaveOptions{SavedBy: "u1"})
	if !errors.As(err, &ce) || ce.Current != 2 {
		t.Fatalf("retry: err=%v, want conflict with current 2", err)
	}
	rep, _ := c.CheckHistory(ctx, ref)
	if !rep.OK() || len(rep.Versions) != 2 {
		t.Fatalf("history after retry: %+v", rep)
	}
}

// ---- deletion --------------------------------------------------------------

func TestAdvDeletedEntity(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("dl")
	id := newEntity(t, c, typ, map[string]any{"title": "v1"})
	ref := EntityRef{Type: typ, ID: id}
	if _, err := c.Save(ctx, ref, 1, map[string]any{"title": "v2"}, SaveOptions{SavedBy: "u1"}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetLabel(ctx, ref, "published", 2, "u1"); err != nil {
		t.Fatal(err)
	}
	if err := rawDo(t, c, http.MethodDelete, fmt.Sprintf("/api/v1/%s/%d", typ, id), nil); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// History survives; labels do not (xolu sweeps meta in the delete transaction).
	if n := len(mustList(t, c, ref)); n != 2 {
		t.Fatalf("history rows after delete = %d, want 2", n)
	}
	if rec, err := c.GetVersion(ctx, ref, 2); err != nil || rec.Snapshot["title"] != "v2" {
		t.Fatalf("GetVersion after delete: %+v err=%v", rec, err)
	}
	if ls, err := c.Labels(ctx, ref); err != nil || len(ls) != 0 {
		t.Fatalf("labels after delete: %+v err=%v, want none", ls, err)
	}
	if err := c.SetLabel(ctx, ref, "late", 2, "u1"); err == nil {
		t.Fatal("SetLabel succeeded on a deleted entity")
	}

	// A save against the deleted entity must not resurrect it.
	_, err := c.Save(ctx, ref, 2, map[string]any{"title": "ghost"}, SaveOptions{SavedBy: "u2"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("save after delete: err=%v, want ErrNotFound", err)
	}
	if !entityGone(t, c, ref) {
		t.Fatal("save after delete recreated the entity")
	}
	if n := len(mustList(t, c, ref)); n != 2 {
		t.Fatalf("history rows after refused save = %d, want 2", n)
	}
	// A plain DELETE leaves no tombstone: the report says so.
	rep, _ := c.CheckHistory(ctx, ref)
	if rep.EntityExists || rep.OK() || !rep.UntrackedDelete || rep.Deleted {
		t.Fatalf("report for a plain DELETE: %+v", rep)
	}
}

// xolu's compare-and-set creates a missing entity instead of failing. If the
// entity is deleted between the client's existence check and its commit, the
// client cannot prevent it, but must report it and CheckHistory must flag it.
func TestAdvDeleteBetweenCheckAndCommit(t *testing.T) {
	c0 := testClient(t)
	typ := uniqueType("dr")
	id := newEntity(t, c0, typ, map[string]any{"title": "v1"})
	ref := EntityRef{Type: typ, ID: id}

	up, _ := url.Parse(c0.BaseURL)
	proxy := httputil.NewSingleHostReverseProxy(up)
	var once int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/commit" && atomic.CompareAndSwapInt32(&once, 0, 1) {
			if err := rawDo(t, c0, http.MethodDelete, fmt.Sprintf("/api/v1/%s/%d", typ, id), nil); err != nil {
				t.Errorf("injected delete: %v", err)
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.Save(context.Background(), ref, 1, map[string]any{"title": "v2"}, SaveOptions{SavedBy: "u1"})
	var re *ErrEntityRecreated
	if !errors.As(err, &re) {
		t.Fatalf("err=%v, want ErrEntityRecreated (xolu behavior changed? then update the client and design)", err)
	}
	if re.Version != 1 || re.Expected != 2 {
		t.Fatalf("ErrEntityRecreated = %+v, want Version 1, Expected 2", re)
	}
	rep, err := c.CheckHistory(context.Background(), ref)
	if err != nil || rep.OK() || !rep.Ahead {
		t.Fatalf("CheckHistory did not flag the inconsistency: %+v err=%v", rep, err)
	}
}

// ---- bypass and tampering --------------------------------------------------

func TestAdvBypassWrites(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("bp")
	id := newEntity(t, c, typ, map[string]any{"title": "v1"})
	ref := EntityRef{Type: typ, ID: id}
	put := func(title string) {
		t.Helper()
		if err := rawDo(t, c, http.MethodPut, fmt.Sprintf("/api/v1/%s/%d", typ, id), map[string]any{"title": title}); err != nil {
			t.Fatalf("plain PUT: %v", err)
		}
	}

	if _, err := c.Save(ctx, ref, 1, map[string]any{"title": "v2"}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	put("bypass-1") // version 3, no history row
	rep, _ := c.CheckHistory(ctx, ref)
	if rep.OK() || !rep.CurrentMissing || rep.CurrentVersion != 3 {
		t.Fatalf("one bypass write not detected: %+v", rep)
	}
	// The next save captures the bypassed state as its baseline: history is whole again.
	if _, err := c.Save(ctx, ref, 3, map[string]any{"title": "v4"}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	rep, _ = c.CheckHistory(ctx, ref)
	if !rep.OK() || len(rep.Versions) != 4 {
		t.Fatalf("history after healing save: %+v", rep)
	}
	if rec, _ := c.GetVersion(ctx, ref, 3); rec.Snapshot["title"] != "bypass-1" || rec.ChangeKind != ChangeBaseline {
		t.Fatalf("bypassed state not captured as baseline: %+v", rec)
	}

	// Two bypass writes in a row leave a permanent gap, which is reported.
	put("bypass-2") // 5
	put("bypass-3") // 6
	if _, err := c.Save(ctx, ref, 6, map[string]any{"title": "v7"}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	rep, _ = c.CheckHistory(ctx, ref)
	if rep.OK() || !reflect.DeepEqual(rep.Gaps, []int{5}) {
		t.Fatalf("gap not reported: %+v", rep)
	}
}

func TestAdvRogueHistoryRows(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("rg")
	id := newEntity(t, c, typ, map[string]any{"title": "v1"})
	ref := EntityRef{Type: typ, ID: id}
	if _, err := c.Save(ctx, ref, 1, map[string]any{"title": "v2"}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	hist := "/api/v1/" + ref.historyType()
	forged := func(entityID, version int) map[string]any {
		return map[string]any{"entity_id": entityID, "version": version, "snapshot": map[string]any{"title": "forged"},
			"change_kind": ChangeSave, "saved_by": "mallory", "saved_at": FormatTime(time.Now())}
	}
	upsert := func(rowID int, row map[string]any) {
		t.Helper()
		if err := rawDo(t, c, http.MethodPost, fmt.Sprintf("%s/save/%d", hist, rowID), row); err != nil {
			t.Fatalf("direct write at id %d: %v", rowID, err)
		}
	}

	// 1. A duplicate written directly lands at an auto id, not at the
	//    deterministic one, so it cannot shadow the real row. It is reported.
	var created struct {
		ID int `json:"id"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, hist, forged(id, 2), &created); err != nil {
		t.Fatalf("direct history write: %v", err)
	}
	if created.ID == historyID(id, 2) {
		t.Fatal("an auto-assigned id collided with the deterministic id of version 2")
	}
	rec, err := c.GetVersion(ctx, ref, 2)
	if err != nil || rec.Snapshot["title"] != "v2" {
		t.Fatalf("GetVersion(2) = %+v err=%v, want the real version 2", rec, err)
	}
	if n := len(mustList(t, c, ref)); n != 2 {
		t.Fatalf("ListVersions shows %d rows, want only the 2 real ones", n)
	}
	rep, _ := c.CheckHistory(ctx, ref)
	misplaced := false
	for _, m := range rep.Misplaced {
		misplaced = misplaced || m == created.ID
	}
	if rep.OK() || !reflect.DeepEqual(rep.Duplicates, []int{2}) || !misplaced {
		t.Fatalf("rogue duplicate not reported: %+v (rogue id %d)", rep, created.ID)
	}

	// 2. A row at a deterministic id that claims to belong elsewhere: reads refuse it.
	upsert(historyID(id, 50), forged(id+1, 7))
	if _, err := c.GetVersion(ctx, ref, 50); !errors.As(err, new(*ErrCorruptHistory)) {
		t.Fatalf("GetVersion on a row that names another entity: err=%v, want ErrCorruptHistory", err)
	}

	// 3. A row far ahead of the entity is reported.
	if _, err := c.doJSON(ctx, http.MethodPost, hist, forged(id, 99), nil); err != nil {
		t.Fatal(err)
	}
	if rep, _ = c.CheckHistory(ctx, ref); !rep.Ahead {
		t.Fatalf("row ahead of the entity not reported: %+v", rep)
	}

	// 4. A stale row sitting at the id of the NEXT version makes Save refuse and
	//    write nothing: loud, not a silent overwrite.
	next := historyID(id, 3)
	upsert(next, forged(id, 3))
	if _, err := c.Save(ctx, ref, 2, map[string]any{"title": "v3"}, SaveOptions{SavedBy: "u"}); !errors.As(err, new(*ErrCorruptHistory)) {
		t.Fatalf("Save over a stale row: err=%v, want ErrCorruptHistory", err)
	}
	if _, ver, _ := c.getEntity(ctx, ref); ver != 2 {
		t.Fatalf("refused save changed the entity: version %d", ver)
	}

	// 5. Removing the stale row repairs it.
	if err := rawDo(t, c, http.MethodDelete, fmt.Sprintf("%s/%d", hist, next), nil); err != nil {
		t.Fatal(err)
	}
	if v, err := c.Save(ctx, ref, 2, map[string]any{"title": "v3"}, SaveOptions{SavedBy: "u"}); err != nil || v != 3 {
		t.Fatalf("Save after repair: v=%d err=%v", v, err)
	}
}

// ---- ordering and volume ---------------------------------------------------

func TestAdvVersionOrderingIsNumeric(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("or")
	id := newEntity(t, c, typ, map[string]any{"n": 0})
	ref := EntityRef{Type: typ, ID: id}
	for base := 1; base <= 24; base++ {
		if _, err := c.Save(ctx, ref, base, map[string]any{"n": base}, SaveOptions{SavedBy: "u"}); err != nil {
			t.Fatalf("save %d: %v", base, err)
		}
	}
	list := mustList(t, c, ref)
	if len(list) != 25 {
		t.Fatalf("rows = %d, want 25", len(list))
	}
	got := make([]int, len(list))
	for i, s := range list {
		got[i] = s.Version
	}
	want := make([]int, 25)
	for i := range want {
		want[i] = 25 - i
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want strictly descending numeric", got)
	}
	top, err := c.ListVersions(ctx, ref, 3)
	if err != nil || len(top) != 3 || top[0].Version != 25 || top[2].Version != 23 {
		t.Fatalf("limit 3: %+v err=%v", top, err)
	}
	if rec, err := c.GetVersion(ctx, ref, 10); err != nil || asInt(rec.Snapshot["n"]) != 9 {
		t.Fatalf("GetVersion(10): %+v err=%v", rec, err)
	}
}

// ---- size ------------------------------------------------------------------

func TestAdvDocumentSizeLimits(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("sz")
	big := func(n int) map[string]any { return map[string]any{"p": strings.Repeat("a", n)} }
	id := newEntity(t, c, typ, big(400_000)) // fits as an entity
	ref := EntityRef{Type: typ, ID: id}

	// First save of a large entity carries the document three times (update,
	// snapshot, baseline): refused locally, nothing written.
	_, err := c.Save(ctx, ref, 1, big(400_000), SaveOptions{SavedBy: "u"})
	var tl *ErrDocumentTooLarge
	if !errors.As(err, &tl) {
		t.Fatalf("err=%v, want ErrDocumentTooLarge", err)
	}
	if _, ver, _ := c.getEntity(ctx, ref); ver != 1 {
		t.Fatalf("refused save changed the entity: version %d", ver)
	}
	if n := len(mustList(t, c, ref)); n != 0 {
		t.Fatalf("refused save wrote %d history rows", n)
	}

	// Shrinking first works (the large baseline plus two small copies fits).
	if _, err := c.Save(ctx, ref, 1, big(10), SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatalf("small save: %v", err)
	}
	// With history in place the same 400 KB document needs only two copies.
	if _, err := c.Save(ctx, ref, 2, big(400_000), SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatalf("400 KB save with history: %v", err)
	}
	// 600 KB twice exceeds the 1 MiB request limit.
	if _, err := c.Save(ctx, ref, 3, big(600_000), SaveOptions{SavedBy: "u"}); !errors.As(err, &tl) {
		t.Fatalf("600 KB save: err=%v, want ErrDocumentTooLarge", err)
	}

	// What xolu itself does with an oversize commit body, for the record: it
	// rejects it, writes nothing, and the entity is untouched.
	raw := commitReq{
		Update: commitUpdate{Entity: typ, ID: id, Version: 3, Data: big(600_000)},
		Append: []commitAppend{{Entity: ref.historyType(), Data: map[string]any{"entity_id": id, "version": 4, "snapshot": big(600_000)}}},
	}
	err = rawDo(t, c, http.MethodPost, "/api/v1/commit", raw)
	var ae *APIError
	if !errors.As(err, &ae) || (ae.Status != 400 && ae.Status != 413) {
		t.Fatalf("oversize commit sent directly: err=%v, want a 400/413 rejection", err)
	}
	t.Logf("xolu answers an oversize /commit with %d %s: %s", ae.Status, ae.Code, ae.Message)
	if _, ver, _ := c.getEntity(ctx, ref); ver != 3 {
		t.Fatalf("oversize commit changed the entity: version %d", ver)
	}
}

// ---- labels ----------------------------------------------------------------

func TestAdvLabels(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("lb")
	id := newEntity(t, c, typ, map[string]any{"title": "v1"})
	ref := EntityRef{Type: typ, ID: id}
	if _, err := c.Save(ctx, ref, 1, map[string]any{"title": "v2"}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}

	long := strings.Repeat("a", 57)
	if err := c.SetLabel(ctx, ref, long, 2, "u"); err != nil {
		t.Fatalf("57-character label: %v", err)
	}
	for _, bad := range []string{strings.Repeat("a", 58), "", "a/b", "a.b", "x y", "caf\u00e9", "a-b", "../x", "a\x00b"} {
		if err := c.SetLabel(ctx, ref, bad, 2, "u"); err == nil {
			t.Errorf("label name %q accepted", bad)
		}
	}
	for _, v := range []int{0, -1, 3, 1 << 30} {
		if err := c.SetLabel(ctx, ref, "nope", v, "u"); !errors.Is(err, ErrNotFound) {
			t.Errorf("label to version %d: err=%v, want ErrNotFound", v, err)
		}
	}

	// Foreign and malformed meta entries must not break the listing.
	meta := func(key string, value any) {
		t.Helper()
		if err := rawDo(t, c, http.MethodPut, fmt.Sprintf("/api/v2/meta/%s/%d/%s", typ, id, key), map[string]any{"value": value}); err != nil {
			t.Fatalf("meta %s: %v", key, err)
		}
	}
	meta("ui_state", map[string]any{"tab": 2})
	meta("label_str", "not an object")
	meta("label_arr", []int{1, 2})
	meta("label_null", nil)
	ls, err := c.Labels(ctx, ref)
	if err != nil {
		t.Fatalf("Labels with malformed entries: %v", err)
	}
	byName := map[string]Label{}
	for _, l := range ls {
		byName[l.Name] = l
	}
	if l := byName[long]; l.Malformed || l.Version != 2 {
		t.Errorf("valid label damaged: %+v", l)
	}
	for _, n := range []string{"str", "arr", "null"} {
		if l, ok := byName[n]; !ok || !l.Malformed || l.Version != 0 {
			t.Errorf("label %q: %+v, want present and Malformed", n, l)
		}
	}
	if _, ok := byName["ui_state"]; ok {
		t.Error("a non-label meta key was listed as a label")
	}

	// A label survives later saves and points at the same version.
	if _, err := c.Save(ctx, ref, 2, map[string]any{"title": "v3"}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	ls, _ = c.Labels(ctx, ref)
	for _, l := range ls {
		if l.Name == long && l.Version != 2 {
			t.Errorf("label moved by a save: %+v", l)
		}
	}
}

// ---- restore ---------------------------------------------------------------

func TestAdvRestoreEdgeCases(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("rs")
	id := newEntity(t, c, typ, map[string]any{"title": "v1"})
	ref := EntityRef{Type: typ, ID: id}
	if _, err := c.Save(ctx, ref, 1, map[string]any{"title": "v2"}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Restore(ctx, ref, 99, 2, SaveOptions{SavedBy: "u"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restore of a missing version: err=%v", err)
	}
	var ce *ErrVersionConflict
	if _, err := c.Restore(ctx, ref, 1, 1, SaveOptions{SavedBy: "u"}); !errors.As(err, &ce) || ce.Current != 2 {
		t.Fatalf("restore with a stale base: err=%v", err)
	}
	if n := len(mustList(t, c, ref)); n != 2 {
		t.Fatalf("refused restores wrote history: %d rows", n)
	}

	// Restoring the current version still creates a new, identical version.
	v, err := c.Restore(ctx, ref, 2, 2, SaveOptions{SavedBy: "u"})
	if err != nil || v != 3 {
		t.Fatalf("restore of the current version: v=%d err=%v", v, err)
	}
	// Restoring a restore: provenance points at the row restored from.
	v, err = c.Restore(ctx, ref, 3, 3, SaveOptions{SavedBy: "u"})
	if err != nil || v != 4 {
		t.Fatalf("restore of a restore: v=%d err=%v", v, err)
	}
	rec, _ := c.GetVersion(ctx, ref, 4)
	if rec.ChangeKind != ChangeRestore || rec.RestoredFrom != 3 || rec.Snapshot["title"] != "v2" {
		t.Fatalf("restore row: %+v", rec)
	}
	// Back to the baseline.
	if v, err = c.Restore(ctx, ref, 1, 4, SaveOptions{SavedBy: "u"}); err != nil || v != 5 {
		t.Fatalf("restore of the baseline: v=%d err=%v", v, err)
	}
	if cur, _, _ := c.getEntity(ctx, ref); cur["title"] != "v1" {
		t.Fatalf("entity after restoring the baseline: %v", cur)
	}
	if rep, _ := c.CheckHistory(ctx, ref); !rep.OK() || len(rep.Versions) != 5 {
		t.Fatalf("history: %+v", rep)
	}
}

// ---- lifecycle -------------------------------------------------------------

// Characterization: xolu does not check that the machine belongs to the entity.
func TestAdvWalkWrongMachineIsNotRejected(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typA, typB := uniqueType("wa"), uniqueType("wb")
	refA := EntityRef{Type: typA, ID: newEntity(t, c, typA, map[string]any{"t": 1})}
	refB := EntityRef{Type: typB, ID: newEntity(t, c, typB, map[string]any{"t": 1})}
	mA, mB := newLifecycle(t, c, refA), newLifecycle(t, c, refB)

	// Save entity A while walking entity B's machine.
	_, err := c.Save(ctx, refA, 1, map[string]any{"t": 2}, SaveOptions{
		SavedBy: "u",
		Walk:    &LifecycleWalk{MachineID: mB, Input: "submit", Payload: map[string]any{"reviewer": "r"}},
	})
	if err != nil {
		t.Skipf("xolu now rejects a walk on a machine bound to another entity (%v): update the design notes", err)
	}
	if machineState(t, c, mB) != "Review" || machineState(t, c, mA) != "Draft" {
		t.Fatalf("states: A=%s B=%s", machineState(t, c, mA), machineState(t, c, mB))
	}
	t.Log("confirmed: a save can advance a machine bound to a different entity; the caller must pass the right machine")
}

func TestAdvTerminalMachineAndInvalidPayload(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("tm")
	id := newEntity(t, c, typ, map[string]any{"t": 1})
	ref := EntityRef{Type: typ, ID: id}
	mid := newLifecycle(t, c, ref)

	// Retire the machine (terminal) as part of a save.
	if _, err := c.Save(ctx, ref, 1, map[string]any{"t": 2}, SaveOptions{SavedBy: "u", Walk: &LifecycleWalk{MachineID: mid, Input: "retire"}}); err != nil {
		t.Fatalf("retire: %v", err)
	}
	// Any further walk is rejected and rolls the whole commit back.
	_, err := c.Save(ctx, ref, 2, map[string]any{"t": 3}, SaveOptions{SavedBy: "u", Walk: &LifecycleWalk{MachineID: mid, Input: "submit", Payload: map[string]any{"reviewer": "r"}}})
	var lr *ErrLifecycleRejected
	if !errors.As(err, &lr) {
		t.Fatalf("walk on a terminal machine: err=%v, want ErrLifecycleRejected", err)
	}
	if cur, ver, _ := c.getEntity(ctx, ref); ver != 2 || asInt(cur["t"]) != 2 {
		t.Fatalf("rejected walk changed the entity: %v v=%d", cur, ver)
	}
	// Characterization: a terminal machine does not stop plain saves. Gating
	// edits on lifecycle state is the caller's policy.
	if _, err := c.Save(ctx, ref, 2, map[string]any{"t": 4}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatalf("plain save on a retired entity: %v", err)
	}

	// Awkward payloads and machine ids: the outcome must be self-consistent,
	// whatever xolu decides (error with nothing written, or success with all of it).
	typ2 := uniqueType("tp")
	id2 := newEntity(t, c, typ2, map[string]any{"t": 1})
	ref2 := EntityRef{Type: typ2, ID: id2}
	mid2 := newLifecycle(t, c, ref2)
	base := 1
	for name, w := range map[string]*LifecycleWalk{
		"bad payload key":  {MachineID: mid2, Input: "submit", Payload: map[string]any{"bad-key": 1, "reviewer": "r"}},
		"huge machine id":  {MachineID: 1 << 40, Input: "submit"},
		"unknown input":    {MachineID: mid2, Input: "no_such_input"},
		"nested payload":   {MachineID: mid2, Input: "submit", Payload: map[string]any{"reviewer": map[string]any{"x": []int{1}}}},
		"quote in input":   {MachineID: mid2, Input: "submit'; DROP TABLE x;--"},
		"unicode in input": {MachineID: mid2, Input: "enviar\u00e9"},
	} {
		before := machineState(t, c, mid2)
		_, err := c.Save(ctx, ref2, base, map[string]any{"t": base + 1}, SaveOptions{SavedBy: "u", Walk: w})
		_, ver, _ := c.getEntity(ctx, ref2)
		n := len(mustList(t, c, ref2))
		after := machineState(t, c, mid2)
		if err != nil {
			if ver != base || after != before {
				t.Errorf("%s: error %v but state changed (version %d->%d, machine %s->%s)", name, err, base, ver, before, after)
			}
			continue
		}
		if ver != base+1 || n < 2 {
			t.Errorf("%s: success but version=%d rows=%d", name, ver, n)
		}
		base = ver
		// Return the machine to Draft so the next case starts from the same state.
		if after == "Review" {
			if _, err := c.Save(ctx, ref2, base, map[string]any{"t": base + 1}, SaveOptions{SavedBy: "u", Walk: &LifecycleWalk{MachineID: mid2, Input: "reject"}}); err != nil {
				t.Fatalf("%s: reset: %v", name, err)
			}
			base++
		}
	}
	if rep, _ := c.CheckHistory(ctx, ref2); !rep.OK() {
		t.Fatalf("history after awkward walks: %+v", rep)
	}
}

// ---- unit tests: no server needed -------------------------------------------

func TestAdvInputsNeverReachTheNetwork(t *testing.T) {
	c := New("http://127.0.0.1:1") // a network attempt would fail with "dial"
	ctx := context.Background()
	notNetwork := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: accepted", what)
		} else if strings.Contains(err.Error(), "dial") || strings.Contains(err.Error(), "connect") {
			t.Errorf("%s: reached the network: %v", what, err)
		}
	}
	for _, typ := range []string{"", "A", "a b", "a;drop", "a-b", "a/b", "../x", "1abc", "a_version", strings.Repeat("a", 60), "caf\u00e9", "a\x00b"} {
		ref := EntityRef{Type: typ, ID: 1}
		_, err := c.Save(ctx, ref, 1, map[string]any{}, SaveOptions{})
		notNetwork(fmt.Sprintf("save type %q", typ), err)
		_, err = c.ListVersions(ctx, ref, 0)
		notNetwork(fmt.Sprintf("list type %q", typ), err)
		_, err = c.GetVersion(ctx, ref, 1)
		notNetwork(fmt.Sprintf("get type %q", typ), err)
		_, err = c.CheckHistory(ctx, ref)
		notNetwork(fmt.Sprintf("check type %q", typ), err)
	}
	for _, id := range []int{0, -1, math.MinInt32} {
		_, err := c.Save(ctx, EntityRef{Type: "asset", ID: id}, 1, map[string]any{}, SaveOptions{})
		notNetwork(fmt.Sprintf("save id %d", id), err)
	}
	if _, err := c.Save(ctx, EntityRef{Type: "asset", ID: 1}, 1, nil, SaveOptions{}); err == nil || strings.Contains(err.Error(), "dial") {
		t.Errorf("nil document: %v", err)
	}
}

func TestAdvMalformedServerResponses(t *testing.T) {
	type script struct {
		name    string
		handler http.HandlerFunc
		check   func(t *testing.T, err error)
	}
	okEntity := func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/thing/"):
			fmt.Fprint(w, `{"_version":1,"id":7,"name":"x"}`)
			return true
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/thing_version/"):
			http.NotFound(w, r) // no history row yet: the first save writes a baseline
			return true
		case r.URL.Path == "/api/v1/oql/query":
			fmt.Fprint(w, `{"data":null}`)
			return true
		}
		return false
	}
	cases := []script{
		{"commit returns the wrong version", func(w http.ResponseWriter, r *http.Request) {
			if !okEntity(w, r) {
				fmt.Fprint(w, `{"update":{"version":9}}`)
			}
		}, func(t *testing.T, err error) {
			if err == nil || !strings.Contains(err.Error(), "expected 2") {
				t.Errorf("err=%v, want a version mismatch error", err)
			}
		}},
		{"commit says created", func(w http.ResponseWriter, r *http.Request) {
			if !okEntity(w, r) {
				fmt.Fprint(w, `{"update":{"version":1,"created":true}}`)
			}
		}, func(t *testing.T, err error) {
			if !errors.As(err, new(*ErrEntityRecreated)) {
				t.Errorf("err=%v, want ErrEntityRecreated", err)
			}
		}},
		{"commit 200 with a non-JSON body", func(w http.ResponseWriter, r *http.Request) {
			if !okEntity(w, r) {
				fmt.Fprint(w, `<html>proxy says hello</html>`)
			}
		}, func(t *testing.T, err error) {
			if err == nil {
				t.Error("non-JSON success body accepted")
			}
		}},
		{"commit 200 with an empty body", func(w http.ResponseWriter, r *http.Request) {
			okEntity(w, r)
		}, func(t *testing.T, err error) {
			// An empty body decodes to version 0, which is not base+1.
			if err == nil {
				t.Error("empty success body accepted")
			}
		}},
		{"commit 500 with an HTML body", func(w http.ResponseWriter, r *http.Request) {
			if !okEntity(w, r) {
				w.WriteHeader(500)
				fmt.Fprint(w, `<html>boom</html>`)
			}
		}, func(t *testing.T, err error) {
			var ae *APIError
			if !errors.As(err, &ae) || ae.Status != 500 {
				t.Errorf("err=%v, want APIError 500", err)
			}
		}},
		{"conflict with a garbage current_version", func(w http.ResponseWriter, r *http.Request) {
			if !okEntity(w, r) {
				w.WriteHeader(409)
				fmt.Fprint(w, `{"error":{"code":"XOLU-CM001","message":"x","status":409},"current_version":"abc"}`)
			}
		}, func(t *testing.T, err error) {
			if err == nil {
				t.Error("garbage conflict accepted as success")
			}
		}},
		{"entity read returns a string _version", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/thing/") {
				fmt.Fprint(w, `{"_version":"1","id":7}`)
				return
			}
			okEntity(w, r)
		}, func(t *testing.T, err error) {
			if err == nil {
				t.Error("save proceeded on an unreadable _version")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			c := New(srv.URL)
			_, err := c.Save(context.Background(), EntityRef{Type: "thing", ID: 7}, 1, map[string]any{"name": "y"}, SaveOptions{SavedBy: "u"})
			tc.check(t, err)
		})
	}
}

func TestAdvTimeoutsAndCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	ref := EntityRef{Type: "thing", ID: 7}
	c := New(srv.URL)
	c.HTTP.Timeout = 150 * time.Millisecond
	start := time.Now()
	if _, err := c.Save(context.Background(), ref, 1, map[string]any{"a": 1}, SaveOptions{}); err == nil {
		t.Error("save against a hung server succeeded")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("client timeout not honoured: %v", d)
	}

	c = New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start = time.Now()
	if _, err := c.Save(ctx, ref, 1, map[string]any{"a": 1}, SaveOptions{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err=%v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("context deadline not honoured: %v", d)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := c.ListVersions(ctx2, ref, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("err=%v, want context.Canceled", err)
	}
}

func TestAdvSizeCheckIsExactAtTheBoundary(t *testing.T) {
	// Fake server that accepts everything; only the local size check can refuse.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			fmt.Fprint(w, `{"_version":1,"id":7}`)
		case r.URL.Path == "/api/v1/oql/query":
			fmt.Fprint(w, `{"data":[{"version":1}]}`)
		default:
			fmt.Fprint(w, `{"update":{"version":2}}`)
		}
	}))
	defer srv.Close()
	c := New(srv.URL)
	c.MaxCommitBytes = 5000
	ref := EntityRef{Type: "thing", ID: 7}

	// Find the largest payload that passes, then confirm the next byte fails.
	lo, hi := 0, 5000
	for lo < hi {
		mid := (lo + hi + 1) / 2
		_, err := c.Save(context.Background(), ref, 1, map[string]any{"p": strings.Repeat("a", mid)}, SaveOptions{SavedBy: "u"})
		if err == nil {
			lo = mid
		} else if errors.As(err, new(*ErrDocumentTooLarge)) {
			hi = mid - 1
		} else {
			t.Fatalf("unexpected error at %d: %v", mid, err)
		}
	}
	if lo == 0 || lo >= 2500 {
		t.Fatalf("largest accepted payload = %d, want between 1 and 2499 (two copies must fit in 5000 bytes)", lo)
	}
	if _, err := c.Save(context.Background(), ref, 1, map[string]any{"p": strings.Repeat("a", lo+1)}, SaveOptions{SavedBy: "u"}); !errors.As(err, new(*ErrDocumentTooLarge)) {
		t.Fatalf("payload %d accepted", lo+1)
	}
}
