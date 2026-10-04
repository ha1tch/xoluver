// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

// Command pointintime builds a month-end report: what each asset looked like at
// the end of January, February and March. Each cell is one AsOf call answered
// from the version index, and the report shows every kind of answer, including
// the ones that are not a record.
//
// It needs a xolu with /ts on tenant routes (see examples/timeindex). The
// history is written with an explicit clock, so the output is the same every run.
//
//	XOLU_URL=http://localhost:9091 XOLU_TENANT=acme go run ./examples/pointintime
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ha1tch/xoluver"
	"github.com/ha1tch/xoluver/examples/internal/demo"
)

type asset struct {
	label string
	ref   xoluver.EntityRef
	ver   int
}

func main() {
	ctx := context.Background()
	c, _ := demo.NewIndexedClient(ctx)
	opt := xoluver.SaveOptions{SavedBy: "report-demo"}

	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 12, 0, 0, 0, time.UTC) }
	clock := day(time.January, 1)
	c.Now = func() time.Time { return clock } // the history is stamped with this clock

	create := func(label string) *asset {
		id := demo.CreateAsset(map[string]any{"name": label, "status": "new"})
		return &asset{label: label, ref: xoluver.EntityRef{Type: "asset", ID: id}, ver: 1}
	}
	save := func(a *asset, when time.Time, status string) {
		clock = when
		v, err := c.Save(ctx, a.ref, a.ver, map[string]any{"name": a.label, "status": status}, opt)
		demo.Must(err)
		a.ver = v
	}

	pump1, pump2, pump3, pump4, pump5 := create("pump-1"), create("pump-2"), create("pump-3"), create("pump-4"), create("pump-5")
	save(pump1, day(time.January, 15), "active")
	save(pump1, day(time.February, 10), "maintenance")
	save(pump1, day(time.March, 5), "active")

	save(pump2, day(time.January, 20), "active")
	clock = day(time.February, 12)
	_, err := c.Delete(ctx, pump2.ref, pump2.ver, opt)
	demo.Must(err)

	save(pump3, day(time.March, 3), "active") // first seen by xoluver in March

	save(pump4, day(time.January, 10), "active")
	save(pump4, day(time.January, 25), "fault")
	save(pump4, day(time.February, 20), "active")

	// pump-5 is never saved through xoluver.

	ends := []time.Time{
		time.Date(2026, time.January, 31, 23, 59, 59, 0, time.UTC),
		time.Date(2026, time.February, 28, 23, 59, 59, 0, time.UTC),
		time.Date(2026, time.March, 31, 23, 59, 59, 0, time.UTC),
	}
	fmt.Printf("%-8s", "asset")
	for _, e := range ends {
		fmt.Printf("%-14s", e.Format("Jan 2"))
	}
	fmt.Println()
	for _, a := range []*asset{pump1, pump2, pump3, pump4, pump5} {
		fmt.Printf("%-8s", a.label)
		for _, e := range ends {
			fmt.Printf("%-14s", cell(c.AsOf(ctx, a.ref, e)))
		}
		fmt.Println()
	}

	hits, falls := c.IndexStats()
	fmt.Printf("\n%d of %d lookups were answered from the index; %d left it\n", hits, hits+falls, falls)
	fmt.Println("pump-5 was never saved through xoluver, so the index holds nothing for it, and on tenant")
	fmt.Println("routes xoluver will not scan: it says it cannot answer instead of guessing.")
}

// cell turns one AsOf answer into a report cell. The three kinds of "no" are
// different statements and stay different in the report.
func cell(rec xoluver.VersionRecord, err error) string {
	var deleted *xoluver.ErrDeletedAsOf
	var before *xoluver.ErrBeforeHistory
	switch {
	case err == nil:
		return fmt.Sprint(rec.Snapshot["status"])
	case errors.As(err, &deleted):
		return "deleted" // proved by a tombstone: it did not exist then
	case errors.As(err, &before):
		return "no record" // the history starts later; it may have existed
	case errors.Is(err, xoluver.ErrTenantOQL):
		return "not indexed"
	}
	demo.Must(err)
	return ""
}
