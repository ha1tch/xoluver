// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

// Command timeindex shows the optional version index: AsOf answered from /ts
// events instead of a scan, and what happens to history that existed before the
// index did.
//
// It needs a xolu server that serves /ts on tenant routes, for example
//
//	XOLU_TIMESERIES_ENABLED=true XOLU_TENANT_AUTO_REGISTER=true XOLU_API_V2_ENABLED=true XOLU_AUTH_TYPE=none ./xolu
//	XOLU_URL=http://localhost:9091 XOLU_TENANT=acme go run ./examples/timeindex
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ha1tch/xoluver"
	"github.com/ha1tch/xoluver/examples/internal/demo"
)

func main() {
	if demo.Tenant() == "" {
		os.Setenv("XOLU_TENANT", "acme") // /ts is served only on tenant routes
	}
	ctx := context.Background()
	opt := xoluver.SaveOptions{SavedBy: "alice"}
	pause := func() { time.Sleep(15 * time.Millisecond) }

	// Define the index once: a timeline with two dimensions (entity id, version)
	// and no expiry. Then tell the client which entity type to index.
	timeline := 20000 + int(time.Now().UnixNano()/int64(time.Millisecond)%30000)
	c := demo.NewClient()
	demo.Must(c.DefineTimeIndex(ctx, timeline))
	demo.Must(c.UseTimeIndex("asset", timeline))
	fmt.Printf("index timeline %d defined for tenant %q\n\n", timeline, demo.Tenant())

	// New history is indexed in the same commit as its rows.
	id := demo.CreateAsset(map[string]any{"name": "pump-1"})
	ref := xoluver.EntityRef{Type: "asset", ID: id}
	pause()
	v, err := c.Save(ctx, ref, 1, map[string]any{"name": "pump-1b"}, opt)
	demo.Must(err)
	pause()
	afterFirst := time.Now()
	pause()
	_, err = c.Save(ctx, ref, v, map[string]any{"name": "pump-1c"}, opt)
	demo.Must(err)
	pause()
	afterSecond := time.Now()

	show(ctx, c, ref, "after the first save", afterFirst)
	show(ctx, c, ref, "after the second save", afterSecond)
	hits, falls := c.IndexStats()
	fmt.Printf("answered from the index: %d of %d calls (%d fell back)\n", hits, hits+falls, falls)
	rep, err := c.CheckIndex(ctx, ref)
	demo.Must(err)
	fmt.Printf("index check: ok=%v, %d events\n\n", rep.OK(), rep.Events)

	// History written before the index existed is not indexed. AsOf does not
	// guess from a partial index, and on tenant routes it cannot scan.
	old := demo.CreateAsset(map[string]any{"name": "pump-2"})
	oldRef := xoluver.EntityRef{Type: "asset", ID: old}
	plain := demo.NewClient() // a client with no index writes history only
	pause()
	_, err = plain.Save(ctx, oldRef, 1, map[string]any{"name": "pump-2b"}, opt)
	demo.Must(err)
	pause()
	afterOld := time.Now()

	fmt.Printf("asset %d was saved before the index existed\n", old)
	show(ctx, c, oldRef, "after its first save", afterOld)
	rep, err = c.CheckIndex(ctx, oldRef)
	demo.Must(err)
	fmt.Printf("index check: ok=%v, missing versions %v\n", rep.OK(), rep.Missing)
	n, err := c.RebuildIndex(ctx, oldRef)
	demo.Must(err)
	fmt.Printf("rebuilt: wrote %d events\n", n)
	show(ctx, c, oldRef, "after its first save", afterOld)
}

func show(ctx context.Context, c *xoluver.Client, ref xoluver.EntityRef, label string, t time.Time) {
	rec, err := c.AsOf(ctx, ref, t)
	switch {
	case err == nil:
		fmt.Printf("as of %-24s name = %q (version %d)\n", label+":", rec.Snapshot["name"], rec.Version)
	case errors.Is(err, xoluver.ErrTenantOQL):
		fmt.Printf("as of %-24s no answer: the index does not cover this entity, and xoluver will not guess\n", label+":")
	default:
		demo.Must(err)
	}
}
