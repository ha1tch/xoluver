// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

// Command contention checks the claim that the version index stays exact when
// several writers collide. Four workers increment one counter, retrying when a
// rival saves first. Every collision is a /commit that xolu rejects, and xolu
// writes the index events before the SQLite side: if a rejected commit left its
// events behind, the index would hold more events than the entity has versions.
// It does not, and AsOf at each version's own instant returns that version.
//
// It needs a xolu with /ts on tenant routes (see examples/timeindex).
//
//	XOLU_URL=http://localhost:9091 XOLU_TENANT=acme go run ./examples/contention
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"github.com/ha1tch/xoluver"
	"github.com/ha1tch/xoluver/examples/internal/demo"
)

const workers, each = 4, 5

func main() {
	ctx := context.Background()
	c, _ := demo.NewIndexedClient(ctx)
	id := demo.CreateAsset(map[string]any{"name": "counter", "counter": 0})
	ref := xoluver.EntityRef{Type: "asset", ID: id}

	var retries int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				atomic.AddInt64(&retries, int64(increment(ctx, c, ref, fmt.Sprintf("worker%d", w))))
			}
		}(w)
	}
	wg.Wait()

	doc, version := demo.Load(id)
	fmt.Printf("%d workers made %d increments each; counter = %v, version = %d\n", workers, each, doc["counter"], version)
	fmt.Printf("rejected commits that were retried: %d (varies from run to run)\n", retries)

	// Every rejected commit wrote index events first and had them undone. If any
	// had been left behind, the index would hold more events than versions.
	rep, err := c.CheckIndex(ctx, ref)
	demo.Must(err)
	fmt.Printf("index: %d events for %d versions, check ok = %v\n", rep.Events, version, rep.OK())

	// AsOf at each version's own instant must return that version. (Version 1 is
	// the baseline, stamped with the same instant as version 2, so the newer one
	// answers for that instant: start at version 2.)
	latest := map[string]int{} // saved_at -> the newest version stamped with it
	rows := map[int]xoluver.VersionRecord{}
	for v := 1; v <= version; v++ {
		rec, err := c.GetVersion(ctx, ref, v)
		demo.Must(err)
		rows[v] = rec
		if v > latest[rec.SavedAt] {
			latest[rec.SavedAt] = v
		}
	}
	exact := 0
	for v := 2; v <= version; v++ {
		t, err := xoluver.ParseTime(rows[v].SavedAt)
		demo.Must(err)
		got, err := c.AsOf(ctx, ref, t)
		want := latest[rows[v].SavedAt]
		n, _ := got.Snapshot["counter"].(json.Number).Int64()
		if err == nil && got.Version == want && int(n) == want-1 {
			exact++
		}
	}
	fmt.Printf("AsOf at a version's own instant returned that version, with its counter: %d of %d\n", exact, version-1)
	hits, falls := c.IndexStats()
	fmt.Printf("answered from the index: %d, left it: %d\n", hits, falls)
}

// increment adds 1 to the counter and returns how many times it had to retry.
func increment(ctx context.Context, c *xoluver.Client, ref xoluver.EntityRef, by string) (retries int) {
	for {
		doc, version := demo.Load(ref.ID)
		n, _ := doc["counter"].(json.Number).Int64()
		doc["counter"] = n + 1
		_, err := c.Save(ctx, ref, version, doc, xoluver.SaveOptions{SavedBy: by})
		var conflict *xoluver.ErrVersionConflict
		switch {
		case err == nil:
			return retries
		case errors.As(err, &conflict):
			retries++
		default:
			log.Fatal(err)
		}
	}
}
