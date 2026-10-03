// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

// Command timemachine asks what an entity looked like at earlier instants. The
// three kinds of "no" are different statements, and AsOf keeps them apart:
// "no record then", "did not exist then (it had been deleted)", and "no history
// at all".
//
//	XOLU_URL=http://localhost:9090 go run ./examples/timemachine
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ha1tch/xoluver"
	"github.com/ha1tch/xoluver/examples/internal/demo"
)

func main() {
	ctx := context.Background()
	c := xoluver.New(demo.BaseURL())
	id := demo.CreateAsset(map[string]any{"name": "pump-1"})
	ref := xoluver.EntityRef{Type: "asset", ID: id}
	opt := xoluver.SaveOptions{SavedBy: "alice"}

	// Nothing has gone through xoluver yet, so there is no history to ask about.
	show(ctx, c, ref, "before any save", time.Now())

	// Remember an instant in front of each step. The pauses keep the instants
	// apart on a real clock.
	pause := func() { time.Sleep(15 * time.Millisecond) }
	beforeFirstSave := time.Now()
	pause()

	v, err := c.Save(ctx, ref, 1, map[string]any{"name": "pump-1b"}, opt)
	demo.Must(err)
	pause()
	afterFirst := time.Now()
	pause()

	v, err = c.Save(ctx, ref, v, map[string]any{"name": "pump-1c"}, opt)
	demo.Must(err)
	pause()
	afterSecond := time.Now()
	pause()

	_, err = c.Delete(ctx, ref, v, opt)
	demo.Must(err)
	pause()
	afterDelete := time.Now()

	fmt.Println()
	show(ctx, c, ref, "before the first save", beforeFirstSave)
	show(ctx, c, ref, "after the first save", afterFirst)
	show(ctx, c, ref, "after the second save", afterSecond)
	show(ctx, c, ref, "after the delete", afterDelete)
	show(ctx, c, ref, "an hour from now", time.Now().Add(time.Hour))

	// And right now: the entity does not exist.
	fmt.Println()
	_, err = c.Save(ctx, ref, v+1, map[string]any{"name": "x"}, opt)
	fmt.Printf("%-24s does not exist now: %v\n", "saving now:", errors.Is(err, xoluver.ErrEntityNotFound))
}

// show prints what the entity was at t, distinguishing the kinds of answer.
func show(ctx context.Context, c *xoluver.Client, ref xoluver.EntityRef, label string, t time.Time) {
	rec, err := c.AsOf(ctx, ref, t)
	var before *xoluver.ErrBeforeHistory
	var deleted *xoluver.ErrDeletedAsOf
	switch {
	case err == nil:
		fmt.Printf("%-24s name = %q (version %d)\n", label+":", rec.Snapshot["name"], rec.Version)
	case errors.As(err, &before):
		fmt.Printf("%-24s no record: history starts at version %d, and it cannot say whether the entity existed\n", label+":", before.SinceVersion)
	case errors.As(err, &deleted):
		fmt.Printf("%-24s did not exist: it had been deleted (version %d)\n", label+":", deleted.Version)
	case errors.Is(err, xoluver.ErrNoHistory):
		fmt.Printf("%-24s no history is recorded for the entity\n", label+":")
	default:
		demo.Must(err)
	}
}
