// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

// Command lifecycle shows the optional state-machine step: a save can move an
// /fsm machine in the same commit. If the machine refuses the transition,
// the document is not saved either.
//
//	XOLU_URL=http://localhost:9090 go run ./examples/lifecycle
//
// The server needs XOLU_API_V2_ENABLED=true.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ha1tch/xoluver"
	"github.com/ha1tch/xoluver/examples/internal/demo"
)

func main() {
	ctx := context.Background()
	c := xoluver.New(demo.BaseURL())
	id := demo.CreateAsset(map[string]any{"name": "pump-1"})
	ref := xoluver.EntityRef{Type: "asset", ID: id}
	machine := newMachine(id)
	fmt.Printf("asset %d is at version 1; machine %d is in state %s\n", id, machine, state(machine))

	// 1. "submit" needs a reviewer. Without one the machine refuses, so the
	//    document change is refused too.
	fmt.Println("\n1. save, with the transition \"submit\" and no reviewer")
	_, err := c.Save(ctx, ref, 1, map[string]any{"name": "pump-1b"}, xoluver.SaveOptions{
		SavedBy: "alice",
		Walk:    &xoluver.LifecycleWalk{MachineID: machine, Input: "submit", Payload: map[string]any{"reviewer": ""}},
	})
	var refused *xoluver.ErrLifecycleRejected
	if !errors.As(err, &refused) {
		demo.Must(fmt.Errorf("expected the transition to be refused, got %v", err))
	}
	_, version := demo.Load(id)
	fmt.Printf("   refused: %s\n", refused.Message)
	fmt.Printf("   nothing changed: asset at version %d, machine in state %s\n", version, state(machine))

	// 2. With a reviewer the save and the transition happen together.
	fmt.Println("\n2. save, with the transition \"submit\" and reviewer bob")
	res, err := c.SaveDetailed(ctx, ref, 1, map[string]any{"name": "pump-1b"}, xoluver.SaveOptions{
		SavedBy: "alice",
		Reason:  "ready for review",
		Walk:    &xoluver.LifecycleWalk{MachineID: machine, Input: "submit", Payload: map[string]any{"reviewer": "bob"}},
	})
	demo.Must(err)
	fmt.Printf("   saved as version %d; machine %s -> %s\n", res.Version, res.Walk.Previous, res.Walk.Current)

	// 3. The history records which transition went with each version.
	fmt.Println("\n3. history")
	list, err := c.ListVersions(ctx, ref, 0)
	demo.Must(err)
	for _, v := range list {
		input := v.LifecycleInput
		if input == "" {
			input = "-"
		}
		fmt.Printf("   v%d  %-8s transition: %s\n", v.Version, v.ChangeKind, input)
	}
}

// newMachine defines a small review lifecycle and starts a machine for the asset.
// This is ordinary xolu /fsm API use, not xoluver.
func newMachine(assetID int) int {
	def := map[string]any{
		"name":        fmt.Sprintf("Review_%d", time.Now().UnixNano()),
		"determinism": "strict",
		"initial":     "Draft",
		"states": map[string]any{
			"Draft":   map[string]any{"terminal": false},
			"Review":  map[string]any{"terminal": false},
			"Retired": map[string]any{"terminal": true}, // xolu requires a terminal state
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
	demo.Call(http.MethodPost, "/api/v2/fsm/def", def, &d)
	var m struct {
		ID int `json:"id"`
	}
	demo.Call(http.MethodPost, "/api/v2/fsm/machine", map[string]any{"definition": d.ID, "ref": fmt.Sprintf("asset:%d", assetID)}, &m)
	return m.ID
}

func state(machine int) string {
	var m struct {
		State string `json:"state"`
	}
	demo.Call(http.MethodGet, fmt.Sprintf("/api/v2/fsm/machine/%d", machine), nil, &m)
	return m.State
}
