// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

// Command retry shows the standard way to update an entity that others may be
// changing too: read it, change what you read, save on top of the version you
// read, and start again if someone saved first. Nothing is ever lost.
//
//	XOLU_URL=http://localhost:9090 go run ./examples/retry
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
	c := xoluver.New(demo.BaseURL())
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
	fmt.Printf("%d workers made %d increments each (%d in total)\n", workers, each, workers*each)
	fmt.Printf("counter = %v, version = %d\n", doc["counter"], version)
	fmt.Printf("saves retried after a conflict: %d (varies from run to run)\n", retries)

	rep, err := c.CheckHistory(ctx, ref)
	demo.Must(err)
	fmt.Printf("history ok: %v (%d versions)\n", rep.OK(), len(rep.Versions))
}

// increment adds 1 to the counter and returns how many times it had to retry.
func increment(ctx context.Context, c *xoluver.Client, ref xoluver.EntityRef, by string) (retries int) {
	for {
		doc, version := demo.Load(ref.ID) // the document and the version it is at
		n, _ := doc["counter"].(json.Number).Int64()
		doc["counter"] = n + 1 // change the document you read, then save it all back

		_, err := c.Save(ctx, ref, version, doc, xoluver.SaveOptions{SavedBy: by})
		var conflict *xoluver.ErrVersionConflict
		switch {
		case err == nil:
			return retries
		case errors.As(err, &conflict):
			retries++ // someone saved first: read again and redo the change
		default:
			log.Fatal(err)
		}
	}
}
