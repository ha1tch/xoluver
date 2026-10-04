// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

// Command windowdiff answers "what changed between two instants?" by asking
// AsOf for both and comparing the answers. The interesting part is what to say
// when one of the two instants has no record, or is after a deletion: each pair
// of answers means something different, and a careless report would claim
// more than xoluver knows.
//
// It needs a xolu with /ts on tenant routes (see examples/timeindex). The
// history is written with an explicit clock, so the output is the same every run.
//
//	XOLU_URL=http://localhost:9091 XOLU_TENANT=acme go run ./examples/windowdiff
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/ha1tch/xoluver"
	"github.com/ha1tch/xoluver/examples/internal/demo"
)

func main() {
	ctx := context.Background()
	c, _ := demo.NewIndexedClient(ctx)
	opt := xoluver.SaveOptions{SavedBy: "alice"}

	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 12, 0, 0, 0, time.UTC) }
	clock := day(time.March, 1)
	c.Now = func() time.Time { return clock }

	id := demo.CreateAsset(map[string]any{"name": "valve-7"})
	ref := xoluver.EntityRef{Type: "asset", ID: id}
	ver := 1
	save := func(when time.Time, doc map[string]any) {
		clock = when
		v, err := c.Save(ctx, ref, ver, doc, opt)
		demo.Must(err)
		ver = v
	}
	save(day(time.March, 1), map[string]any{"name": "valve-7", "status": "active", "location": "north"})
	save(day(time.March, 10), map[string]any{"name": "valve-7", "status": "fault", "location": "north"})
	save(day(time.March, 20), map[string]any{"name": "valve-7", "status": "fault", "location": "south", "tags": []string{"critical"}})
	save(day(time.April, 2), map[string]any{"name": "valve-7", "status": "active", "location": "south"})
	clock = day(time.April, 15)
	_, err := c.Delete(ctx, ref, ver, opt)
	demo.Must(err)

	windows := []struct{ from, to time.Time }{
		{day(time.March, 5), day(time.March, 15)},
		{day(time.March, 15), day(time.April, 5)},
		{day(time.March, 25), day(time.April, 1)},
		{day(time.February, 1), day(time.March, 5)},
		{day(time.April, 5), day(time.April, 20)},
		{day(time.March, 15), day(time.April, 20)},
		{day(time.April, 20), day(time.May, 1)},
		{day(time.February, 1), day(time.February, 15)},
	}
	for _, w := range windows {
		fmt.Printf("%s to %s\n", w.from.Format("Jan 2"), w.to.Format("Jan 2"))
		for _, line := range describe(ctx, c, ref, w.from, w.to) {
			fmt.Println("  " + line)
		}
	}
}

// describe says what can be said about the change between two instants. Each
// combination of answers is a different statement.
func describe(ctx context.Context, c *xoluver.Client, ref xoluver.EntityRef, from, to time.Time) []string {
	a, errA := c.AsOf(ctx, ref, from)
	b, errB := c.AsOf(ctx, ref, to)
	for _, err := range []error{errA, errB} {
		if !known(err) {
			demo.Must(err)
		}
	}
	var delA, delB *xoluver.ErrDeletedAsOf
	var befA, befB *xoluver.ErrBeforeHistory
	deletedA, deletedB := errors.As(errA, &delA), errors.As(errB, &delB)
	beforeA, beforeB := errors.As(errA, &befA), errors.As(errB, &befB)

	switch {
	case errA == nil && errB == nil && a.Version == b.Version:
		return []string{fmt.Sprintf("no change (still version %d)", a.Version)}
	case errA == nil && errB == nil:
		out := []string{fmt.Sprintf("changed: version %d -> %d", a.Version, b.Version)}
		return append(out, changes(a.Snapshot, b.Snapshot)...)
	case beforeA && errB == nil:
		return []string{
			fmt.Sprintf("no record at the start; first recorded at version %d", befA.SinceVersion),
			"(xoluver cannot say whether it existed before: this is not a creation)",
		}
	case errA == nil && deletedB:
		return []string{fmt.Sprintf("deleted at %s (version %d)", delB.DeletedAt.Format("Jan 2"), delB.Version)}
	case beforeA && deletedB:
		return []string{fmt.Sprintf("recorded and then deleted inside the window (deleted %s)", delB.DeletedAt.Format("Jan 2"))}
	case deletedA && deletedB:
		return []string{fmt.Sprintf("already deleted before the window (since %s); nothing to compare", delA.DeletedAt.Format("Jan 2"))}
	case beforeA && beforeB:
		return []string{"no record at either instant; nothing can be said"}
	}
	return []string{"unexpected combination of answers"}
}

// known reports whether err is one of AsOf's statements about time rather than a failure.
func known(err error) bool {
	var deleted *xoluver.ErrDeletedAsOf
	var before *xoluver.ErrBeforeHistory
	return err == nil || errors.As(err, &deleted) || errors.As(err, &before)
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
	show := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	var out []string
	for _, k := range sorted {
		x, inFrom := from[k]
		y, inTo := to[k]
		switch {
		case !inFrom:
			out = append(out, fmt.Sprintf("+ %s: %s", k, show(y)))
		case !inTo:
			out = append(out, fmt.Sprintf("- %s: %s", k, show(x)))
		case !reflect.DeepEqual(x, y):
			out = append(out, fmt.Sprintf("~ %s: %s -> %s", k, show(x), show(y)))
		}
	}
	return out
}
