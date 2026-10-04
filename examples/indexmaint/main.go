// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

// Command indexmaint is a maintenance job for the version index: for each
// entity it checks the index against the history, rebuilds what is missing, and
// reports what only an administrator can fix. The example first builds seven
// assets in the states a real index drifts into, then runs the job over them.
//
// It needs a xolu with /ts on tenant routes (see examples/timeindex).
//
//	XOLU_URL=http://localhost:9091 XOLU_TENANT=acme go run ./examples/indexmaint
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ha1tch/xoluver"
	"github.com/ha1tch/xoluver/examples/internal/demo"
)

type asset struct {
	label string
	ref   xoluver.EntityRef
}

func main() {
	ctx := context.Background()
	c, timeline := demo.NewIndexedClient(ctx)
	plain := demo.NewClient() // a client with no index: it writes history and no events
	opt := xoluver.SaveOptions{SavedBy: "maint-demo"}

	// build creates an asset and saves it twice with the given client (versions 1 to 3).
	build := func(label string, cl *xoluver.Client) asset {
		id := demo.CreateAsset(map[string]any{"name": label, "n": 0})
		a := asset{label: label, ref: xoluver.EntityRef{Type: "asset", ID: id}}
		v, err := cl.Save(ctx, a.ref, 1, map[string]any{"name": label, "n": 1}, opt)
		demo.Must(err)
		_, err = cl.Save(ctx, a.ref, v, map[string]any{"name": label, "n": 2}, opt)
		demo.Must(err)
		return a
	}
	// writeEvent writes an index event directly, as a stray writer or an
	// interrupted commit would leave one.
	writeEvent := func(a asset, version int, at time.Time) {
		ev := map[string]any{"timeline": timeline, "dims": []uint64{uint64(a.ref.ID), uint64(version)}, "time": at.UTC().Format(time.RFC3339Nano), "nums": []float64{0}}
		demo.Call("POST", "/api/v1/ts/events", ev, nil)
	}

	a1, a2 := build("a1", c), build("a2", c)         // healthy
	b1, b2 := build("b1", plain), build("b2", plain) // history from before the index existed
	c1 := build("c1", c)                             // healthy, then an event for a version that was never saved
	writeEvent(c1, 99, time.Now())
	d1 := build("d1", c) // healthy, then a second event for version 2
	row, err := c.GetVersion(ctx, d1.ref, 2)
	demo.Must(err)
	t2, _ := xoluver.ParseTime(row.SavedAt)
	writeEvent(d1, 2, t2.Add(time.Millisecond))
	e1 := build("e1", plain) // old history, plus one event with the wrong time
	row, err = c.GetVersion(ctx, e1.ref, 2)
	demo.Must(err)
	t2, _ = xoluver.ParseTime(row.SavedAt)
	writeEvent(e1, 2, t2.Add(time.Millisecond))
	assets := []asset{a1, a2, b1, b2, c1, d1, e1}

	// The job.
	fmt.Printf("%-6s %-30s %-16s %s\n", "asset", "before", "action", "after")
	healthy, needsAdmin := 0, 0
	for _, a := range assets {
		rep, err := c.CheckIndex(ctx, a.ref)
		demo.Must(err)
		before, action := summarize(rep), "-"
		if len(rep.Missing) > 0 {
			n, err := c.RebuildIndex(ctx, a.ref)
			demo.Must(err)
			action = fmt.Sprintf("wrote %d events", n)
			rep, err = c.CheckIndex(ctx, a.ref)
			demo.Must(err)
		}
		if rep.OK() {
			healthy++
		} else {
			needsAdmin++
		}
		fmt.Printf("%-6s %-30s %-16s %s\n", a.label, before, action, summarize(rep))
	}
	fmt.Printf("\n%d of %d healthy after maintenance; %d need an administrator.\n", healthy, len(assets), needsAdmin)
	fmt.Println("RebuildIndex only adds events. xolu removes /ts events only by time-range purge, so")
	fmt.Println("orphans, duplicates and wrong times are reported, not repaired.")

	// What AsOf does meanwhile: it answers from a repaired index and refuses a damaged one.
	fmt.Println()
	rec, err := c.AsOf(ctx, b1.ref, time.Now().Add(time.Hour))
	demo.Must(err)
	fmt.Printf("b1 (rebuilt):  AsOf answers from the index: version %d\n", rec.Version)
	_, err = c.AsOf(ctx, c1.ref, time.Now().Add(time.Hour))
	if errors.Is(err, xoluver.ErrTenantOQL) {
		fmt.Println("c1 (orphan):   AsOf will not answer from a damaged index, and on tenant routes it cannot scan")
	} else {
		demo.Must(err)
	}
}

// summarize turns an index report into one line.
func summarize(r xoluver.IndexReport) string {
	if r.OK() {
		return "ok"
	}
	var parts []string
	if len(r.Missing) > 0 {
		parts = append(parts, fmt.Sprintf("missing %v", r.Missing))
	}
	if len(r.Orphans) > 0 {
		parts = append(parts, fmt.Sprintf("orphans %v", r.Orphans))
	}
	if len(r.Duplicates) > 0 {
		parts = append(parts, fmt.Sprintf("duplicates %v", r.Duplicates))
	}
	if len(r.Mismatched) > 0 {
		parts = append(parts, fmt.Sprintf("mismatched %v", r.Mismatched))
	}
	if !r.NoExpiry {
		parts = append(parts, "timeline expires")
	}
	return strings.Join(parts, "; ")
}
