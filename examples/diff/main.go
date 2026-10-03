// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

// Command diff prints what changed between each pair of versions, by comparing
// the snapshots xoluver keeps. xoluver stores whole documents; showing the
// difference is a few lines of your own code.
//
//	XOLU_URL=http://localhost:9090 go run ./examples/diff
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"github.com/ha1tch/xoluver"
	"github.com/ha1tch/xoluver/examples/internal/demo"
)

func main() {
	ctx := context.Background()
	c := xoluver.New(demo.BaseURL())
	id := demo.CreateAsset(map[string]any{"name": "pump-1", "site": "north", "status": "active"})
	ref := xoluver.EntityRef{Type: "asset", ID: id}

	save := func(base int, doc map[string]any, by, reason string) int {
		v, err := c.Save(ctx, ref, base, doc, xoluver.SaveOptions{SavedBy: by, Reason: reason})
		demo.Must(err)
		return v
	}
	v := save(1, map[string]any{"name": "pump-1b", "site": "north", "status": "active"}, "alice", "renamed")
	v = save(v, map[string]any{"name": "pump-1b", "site": "south", "status": "active", "tags": []string{"critical"}}, "bob", "moved")
	save(v, map[string]any{"name": "pump-1b", "site": "south", "tags": []string{"critical"}}, "carol", "retired status")

	list, err := c.ListVersions(ctx, ref, 0) // newest first
	demo.Must(err)
	for i := len(list) - 1; i > 0; i-- { // oldest pair first
		from, err := c.GetVersion(ctx, ref, list[i].Version)
		demo.Must(err)
		to, err := c.GetVersion(ctx, ref, list[i-1].Version)
		demo.Must(err)
		fmt.Printf("v%d -> v%d  (%s: %s)\n", from.Version, to.Version, to.SavedBy, to.Reason)
		for _, line := range changes(from.Snapshot, to.Snapshot) {
			fmt.Println("  " + line)
		}
	}
}

// changes lists the top-level fields that were added (+), removed (-) or changed (~).
func changes(from, to map[string]any) []string {
	keys := map[string]bool{}
	for k := range from {
		keys[k] = true
	}
	for k := range to {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	var out []string
	for _, k := range sorted {
		a, inFrom := from[k]
		b, inTo := to[k]
		switch {
		case !inFrom:
			out = append(out, fmt.Sprintf("+ %s: %s", k, show(b)))
		case !inTo:
			out = append(out, fmt.Sprintf("- %s: %s", k, show(a)))
		case !reflect.DeepEqual(a, b):
			out = append(out, fmt.Sprintf("~ %s: %s -> %s", k, show(a), show(b)))
		}
	}
	return out
}

func show(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
