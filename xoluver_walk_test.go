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
	"strings"
	"sync"
	"testing"
)

// Tests for the optional /fsm support (SaveOptions.Walk).
//
// The live-server tests need XOLU_URL (see xoluver_test.go) and a server
// with XOLU_API_V2_ENABLED=true. TestWalkIsOptionalOnTheWire and
// TestWalkOptionValidation need no server.

// newLifecycle creates a definition (Draft -> Review on "submit" when
// payload.reviewer is non-empty; Review -> Draft on "reject"; either -> Retired
// on "retire") and one machine bound to ref. It returns the machine id.
func newLifecycle(t *testing.T, c *Client, ref EntityRef) int {
	t.Helper()
	ctx := context.Background()
	def := map[string]any{
		"name":        "Lifecycle_" + ref.Type,
		"determinism": "strict",
		"initial":     "Draft",
		"states": map[string]any{
			"Draft":   map[string]any{"terminal": false},
			"Review":  map[string]any{"terminal": false},
			"Retired": map[string]any{"terminal": true}, // xolu requires a reachable terminal state
		},
		"transitions": []any{
			map[string]any{"from": "Draft", "input": "submit", "to": "Review", "guard": "payload.reviewer != ''"},
			map[string]any{"from": "Review", "input": "reject", "to": "Draft"},
			map[string]any{"from": []any{"Draft", "Review"}, "input": "retire", "to": "Retired"},
		},
	}
	var d struct {
		ID int `json:"id"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, "/api/v2/fsm/def", def, &d); err != nil {
		t.Fatalf("create fsm def: %v", err)
	}
	var m struct {
		ID int `json:"id"`
	}
	body := map[string]any{"definition": d.ID, "ref": fmt.Sprintf("%s:%d", ref.Type, ref.ID)}
	if _, err := c.doJSON(ctx, http.MethodPost, "/api/v2/fsm/machine", body, &m); err != nil {
		t.Fatalf("create fsm machine: %v", err)
	}
	return m.ID
}

func machineState(t *testing.T, c *Client, id int) string {
	t.Helper()
	var m struct {
		State string `json:"state"`
	}
	if _, err := c.doJSON(context.Background(), http.MethodGet, fmt.Sprintf("/api/v2/fsm/machine/%d", id), nil, &m); err != nil {
		t.Fatalf("read machine %d: %v", id, err)
	}
	return m.State
}

func TestWalkCommitsAtomically(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("vw")
	id := newEntity(t, c, typ, map[string]any{"title": "v1"})
	ref := EntityRef{Type: typ, ID: id}
	mid := newLifecycle(t, c, ref)

	// Save + transition in one commit.
	res, err := c.SaveDetailed(ctx, ref, 1, map[string]any{"title": "v2"}, SaveOptions{
		SavedBy: "u1",
		Walk:    &LifecycleWalk{MachineID: mid, Input: "submit", Payload: map[string]any{"reviewer": "r1"}},
	})
	if err != nil {
		t.Fatalf("save with walk: %v", err)
	}
	if res.Version != 2 || res.Walk == nil || res.Walk.Previous != "Draft" || res.Walk.Current != "Review" || res.Walk.Terminal {
		t.Fatalf("result: %+v walk=%+v", res, res.Walk)
	}
	if st := machineState(t, c, mid); st != "Review" {
		t.Fatalf("machine state = %q, want Review", st)
	}
	l := mustList(t, c, ref)
	if len(l) != 2 || l[0].LifecycleInput != "submit" || l[1].ChangeKind != ChangeBaseline || l[1].LifecycleInput != "" {
		t.Fatalf("history: %+v", l)
	}

	// A save without Walk does not touch the machine and records no input.
	if _, err := c.Save(ctx, ref, 2, map[string]any{"title": "v3"}, SaveOptions{SavedBy: "u1"}); err != nil {
		t.Fatalf("save without walk: %v", err)
	}
	if st := machineState(t, c, mid); st != "Review" {
		t.Fatalf("machine moved without a walk: %q", st)
	}
	if l = mustList(t, c, ref); l[0].Version != 3 || l[0].LifecycleInput != "" {
		t.Fatalf("history after plain save: %+v", l)
	}

	// Transition back, no payload.
	res, err = c.SaveDetailed(ctx, ref, 3, map[string]any{"title": "v4"}, SaveOptions{
		SavedBy: "u1",
		Walk:    &LifecycleWalk{MachineID: mid, Input: "reject"},
	})
	if err != nil || res.Walk == nil || res.Walk.Current != "Draft" {
		t.Fatalf("reject walk: res=%+v err=%v", res, err)
	}
}

func TestWalkRejectionRollsBack(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("vr")
	id := newEntity(t, c, typ, map[string]any{"title": "original"})
	ref := EntityRef{Type: typ, ID: id}
	mid := newLifecycle(t, c, ref)

	cases := []struct {
		name string
		walk *LifecycleWalk
	}{
		{"guard fails", &LifecycleWalk{MachineID: mid, Input: "submit", Payload: map[string]any{"reviewer": ""}}},
		{"no transition for input", &LifecycleWalk{MachineID: mid, Input: "reject"}},
		{"machine not found", &LifecycleWalk{MachineID: 999999, Input: "submit", Payload: map[string]any{"reviewer": "r"}}},
	}
	for _, tc := range cases {
		_, err := c.Save(ctx, ref, 1, map[string]any{"title": "changed"}, SaveOptions{SavedBy: "u1", Walk: tc.walk})
		var lr *ErrLifecycleRejected
		if !errors.As(err, &lr) {
			t.Fatalf("%s: err=%v, want ErrLifecycleRejected", tc.name, err)
		}
		// Nothing was written anywhere.
		cur, ver, err := c.getEntity(ctx, ref)
		if err != nil || ver != 1 || cur["title"] != "original" {
			t.Fatalf("%s: entity changed: %v v=%d err=%v", tc.name, cur, ver, err)
		}
		if n := len(mustList(t, c, ref)); n != 0 {
			t.Fatalf("%s: history rows = %d, want 0", tc.name, n)
		}
		if st := machineState(t, c, mid); st != "Draft" {
			t.Fatalf("%s: machine state = %q, want Draft", tc.name, st)
		}
	}

	// After the rejections the same base version still saves normally.
	v, err := c.Save(ctx, ref, 1, map[string]any{"title": "changed"}, SaveOptions{
		SavedBy: "u1",
		Walk:    &LifecycleWalk{MachineID: mid, Input: "submit", Payload: map[string]any{"reviewer": "r1"}},
	})
	if err != nil || v != 2 {
		t.Fatalf("save after rejections: v=%d err=%v", v, err)
	}
}

func TestWalkWithStaleBaseLeavesMachineAlone(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	typ := uniqueType("vs")
	id := newEntity(t, c, typ, map[string]any{"title": "v1"})
	ref := EntityRef{Type: typ, ID: id}
	mid := newLifecycle(t, c, ref)

	if _, err := c.Save(ctx, ref, 1, map[string]any{"title": "v2"}, SaveOptions{SavedBy: "u1"}); err != nil {
		t.Fatalf("setup save: %v", err)
	}
	_, err := c.Save(ctx, ref, 1, map[string]any{"title": "stale"}, SaveOptions{
		SavedBy: "u2",
		Walk:    &LifecycleWalk{MachineID: mid, Input: "submit", Payload: map[string]any{"reviewer": "r1"}},
	})
	var ce *ErrVersionConflict
	if !errors.As(err, &ce) || ce.Current != 2 {
		t.Fatalf("err=%v, want conflict with current 2", err)
	}
	if st := machineState(t, c, mid); st != "Draft" {
		t.Fatalf("stale save advanced the machine: %q", st)
	}
}

// TestWalkIsOptionalOnTheWire checks the request bodies against a fake server:
// no fsm_walk key unless SaveOptions.Walk is set.
func TestWalkIsOptionalOnTheWire(t *testing.T) {
	var mu sync.Mutex
	var commits []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/oql/query":
			fmt.Fprint(w, `{"data":[]}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/thing/"):
			fmt.Fprint(w, `{"_version":1,"id":7,"name":"x"}`)
		case r.URL.Path == "/api/v1/commit":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			commits = append(commits, body)
			mu.Unlock()
			if _, ok := body["fsm_walk"]; ok {
				fmt.Fprint(w, `{"update":{"version":2},"fsm_walk":{"machine":5,"previous":"A","current":"B","terminal":false}}`)
			} else {
				fmt.Fprint(w, `{"update":{"version":2}}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL)
	ref := EntityRef{Type: "thing", ID: 7}
	ctx := context.Background()

	if _, err := c.Save(ctx, ref, 1, map[string]any{"name": "y"}, SaveOptions{SavedBy: "u"}); err != nil {
		t.Fatalf("save without walk: %v", err)
	}
	res, err := c.SaveDetailed(ctx, ref, 1, map[string]any{"name": "y"}, SaveOptions{
		SavedBy: "u",
		Walk:    &LifecycleWalk{MachineID: 5, Input: "go", Payload: map[string]any{"k": "v"}},
	})
	if err != nil || res.Walk == nil || res.Walk.Current != "B" {
		t.Fatalf("save with walk: res=%+v err=%v", res, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commits) != 2 {
		t.Fatalf("commits seen = %d, want 2", len(commits))
	}
	if _, present := commits[0]["fsm_walk"]; present {
		t.Errorf("fsm_walk sent although Walk was nil: %v", commits[0])
	}
	w, _ := commits[1]["fsm_walk"].(map[string]any)
	if w == nil || w["machine"] != float64(5) || w["input"] != "go" {
		t.Errorf("fsm_walk body wrong: %v", commits[1]["fsm_walk"])
	}
	// lifecycle_input lands on the new-version row only, never on the baseline.
	rows, _ := commits[1]["append"].([]any)
	if len(rows) != 2 {
		t.Fatalf("append rows = %d, want baseline + save", len(rows))
	}
	base := rows[0].(map[string]any)["data"].(map[string]any)
	save := rows[1].(map[string]any)["data"].(map[string]any)
	if _, has := base["lifecycle_input"]; has || save["lifecycle_input"] != "go" {
		t.Errorf("lifecycle_input placement wrong: baseline=%v save=%v", base, save)
	}
}

func TestWalkOptionValidation(t *testing.T) {
	c := New("http://127.0.0.1:1") // never contacted: validation fails first
	ctx := context.Background()
	ref := EntityRef{Type: "asset", ID: 1}
	for name, w := range map[string]*LifecycleWalk{
		"machine 0":   {MachineID: 0, Input: "x"},
		"empty input": {MachineID: 1, Input: ""},
	} {
		if _, err := c.Save(ctx, ref, 1, map[string]any{}, SaveOptions{Walk: w}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
