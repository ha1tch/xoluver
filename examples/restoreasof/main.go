// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

// Command restoreasof recovers from a bad edit by time instead of by version
// number: "put it back the way it was at 14:00". AsOf finds the version that
// was in effect then, and Restore saves its content as a new version, so the
// history keeps both the bad edit and the recovery. It also shows the two cases
// that cannot be recovered this way, and why.
//
// It needs a xolu with /ts on tenant routes (see examples/timeindex). The
// history is written with an explicit clock, so the output is the same every run.
//
//	XOLU_URL=http://localhost:9091 XOLU_TENANT=acme go run ./examples/restoreasof
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ha1tch/xoluver"
	"github.com/ha1tch/xoluver/examples/internal/demo"
)

func main() {
	ctx := context.Background()
	c, _ := demo.NewIndexedClient(ctx)
	at := func(m time.Month, d, h, min int) time.Time { return time.Date(2026, m, d, h, min, 0, 0, time.UTC) }
	clock := at(time.June, 1, 9, 0)
	c.Now = func() time.Time { return clock }

	id := demo.CreateAsset(map[string]any{"name": "boiler-3", "owner": "ops", "threshold": 70})
	ref := xoluver.EntityRef{Type: "asset", ID: id}
	ver := 1
	save := func(when time.Time, by string, doc map[string]any) {
		clock = when
		v, err := c.Save(ctx, ref, ver, doc, xoluver.SaveOptions{SavedBy: by})
		demo.Must(err)
		ver = v
	}
	save(at(time.June, 1, 10, 0), "alice", map[string]any{"name": "boiler-3", "owner": "ops", "threshold": 80, "notes": "calibrated"})
	save(at(time.June, 2, 9, 0), "bob", map[string]any{"name": "boiler-3", "owner": "ops", "threshold": 80, "notes": "calibrated, inspected"})
	save(at(time.June, 2, 14, 5), "import-job", map[string]any{"name": "boiler-3"}) // a bad import replaces the whole document

	cur, curVer := demo.Load(id)
	fmt.Printf("now (version %d):        %s\n", curVer, doc(cur))

	// What was it at 14:00, before the import?
	rec, err := c.AsOf(ctx, ref, at(time.June, 2, 14, 0))
	demo.Must(err)
	fmt.Printf("as of Jun 2 14:00 (version %d): %s\n", rec.Version, doc(rec.Snapshot))

	// Put it back. This is a new version; nothing is rewritten.
	clock = at(time.June, 2, 15, 0)
	v, err := c.Restore(ctx, ref, rec.Version, curVer, xoluver.SaveOptions{SavedBy: "ops-bot", Reason: "undo the 14:05 import"})
	demo.Must(err)
	cur, curVer = demo.Load(id)
	fmt.Printf("restored (version %d):  %s\n", curVer, doc(cur))
	row, err := c.GetVersion(ctx, ref, v)
	demo.Must(err)
	fmt.Printf("version %d is a %q of version %d, by %s: %s\n", v, row.ChangeKind, row.RestoredFrom, row.SavedBy, row.Reason)

	// Case 1: before the first record there is nothing to restore.
	fmt.Println()
	_, err = c.AsOf(ctx, ref, at(time.January, 1, 0, 0))
	var before *xoluver.ErrBeforeHistory
	if errors.As(err, &before) {
		fmt.Printf("Jan 1: no record (history starts at version %d); there is no state to restore\n", before.SinceVersion)
	}

	// Case 2: an entity that has been deleted. AsOf still finds what it was, but
	// Restore needs a live entity, and xoluver does not recreate one.
	gone := demo.CreateAsset(map[string]any{"name": "boiler-9"})
	goneRef := xoluver.EntityRef{Type: "asset", ID: gone}
	clock = at(time.June, 1, 12, 0)
	gv, err := c.Save(ctx, goneRef, 1, map[string]any{"name": "boiler-9", "owner": "ops"}, xoluver.SaveOptions{SavedBy: "alice"})
	demo.Must(err)
	clock = at(time.June, 2, 12, 0)
	del, err := c.Delete(ctx, goneRef, gv, xoluver.SaveOptions{SavedBy: "alice"})
	demo.Must(err)
	was, err := c.AsOf(ctx, goneRef, at(time.June, 1, 18, 0))
	demo.Must(err)
	fmt.Printf("boiler-9 as of Jun 1 18:00 (version %d): %s\n", was.Version, doc(was.Snapshot))
	_, err = c.Restore(ctx, goneRef, was.Version, del.Version, xoluver.SaveOptions{SavedBy: "ops-bot"})
	switch {
	case errors.Is(err, xoluver.ErrEntityNotFound):
		fmt.Println("restoring it: the entity does not exist now, so there is nothing to put a version back on")
	default:
		demo.Must(err)
	}
}

// doc prints a document without xolu's own fields, keys sorted.
func doc(m map[string]any) string {
	out := map[string]any{}
	for k, v := range m {
		if k != "_version" && k != "id" {
			out[k] = v
		}
	}
	b, _ := json.Marshal(out)
	return string(b)
}
