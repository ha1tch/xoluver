// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

package xoluver_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ha1tch/xoluver"
)

// These examples are compiled with the tests but not run: they have no
// "Output:" comment because they need a live xolu server.

func ExampleNew() {
	c := xoluver.New("http://localhost:9090")
	ref := xoluver.EntityRef{Type: "asset", ID: 123}
	_, _ = c, ref
}

func ExampleClient_Save() {
	ctx := context.Background()
	c := xoluver.New("http://localhost:9090")
	ref := xoluver.EntityRef{Type: "asset", ID: 123}

	// 3 is the version you read; the document is the complete new content.
	newVersion, err := c.Save(ctx, ref, 3, map[string]any{"name": "pump-1b"},
		xoluver.SaveOptions{SavedBy: "alice", Reason: "renamed"})
	if err != nil {
		return
	}
	fmt.Println("now at version", newVersion)
}

func ExampleClient_Save_conflict() {
	ctx := context.Background()
	c := xoluver.New("http://localhost:9090")
	ref := xoluver.EntityRef{Type: "asset", ID: 123}

	_, err := c.Save(ctx, ref, 3, map[string]any{"name": "pump-1b"}, xoluver.SaveOptions{SavedBy: "alice"})

	var conflict *xoluver.ErrVersionConflict
	if errors.As(err, &conflict) {
		// Someone saved first. Nothing was written. Reload version
		// conflict.Current, redo your change on top of it, and save again.
		fmt.Println("stale; current version is", conflict.Current)
	}
}

func ExampleClient_ListVersions() {
	ctx := context.Background()
	c := xoluver.New("http://localhost:9090")
	ref := xoluver.EntityRef{Type: "asset", ID: 123}

	versions, _ := c.ListVersions(ctx, ref, 10) // newest first, at most 10
	for _, v := range versions {
		fmt.Println(v.Version, v.ChangeKind, v.SavedBy, v.SavedAt)
	}
}

func ExampleClient_GetVersion() {
	ctx := context.Background()
	c := xoluver.New("http://localhost:9090")
	ref := xoluver.EntityRef{Type: "asset", ID: 123}

	rec, err := c.GetVersion(ctx, ref, 2)
	if errors.Is(err, xoluver.ErrVersionNotFound) {
		fmt.Println("no such version")
		return
	}
	fmt.Println(rec.Snapshot["name"]) // the full document as it was at version 2
}

func ExampleClient_Restore() {
	ctx := context.Background()
	c := xoluver.New("http://localhost:9090")
	ref := xoluver.EntityRef{Type: "asset", ID: 123}

	// Put the content of version 1 back. History is not rewritten: this makes a
	// new version (here 5) whose content equals version 1.
	newVersion, _ := c.Restore(ctx, ref, 1, 4, xoluver.SaveOptions{SavedBy: "alice", Reason: "undo"})
	fmt.Println("restored as version", newVersion)
}

func ExampleClient_SetLabel() {
	ctx := context.Background()
	c := xoluver.New("http://localhost:9090")
	ref := xoluver.EntityRef{Type: "asset", ID: 123}

	_ = c.SetLabel(ctx, ref, "published", 2, "alice") // setting it again moves it
	labels, _ := c.Labels(ctx, ref)
	for _, l := range labels {
		fmt.Println(l.Name, "->", l.Version)
	}
}

func ExampleClient_SaveDetailed_lifecycle() {
	ctx := context.Background()
	c := xoluver.New("http://localhost:9090")
	ref := xoluver.EntityRef{Type: "asset", ID: 123}

	// Optional: move an /fsm machine in the same commit as the save. If the
	// transition is refused, nothing is saved.
	res, err := c.SaveDetailed(ctx, ref, 3, map[string]any{"name": "pump-1b"}, xoluver.SaveOptions{
		SavedBy: "alice",
		Walk:    &xoluver.LifecycleWalk{MachineID: 12, Input: "submit", Payload: map[string]any{"reviewer": "bob"}},
	})
	var rejected *xoluver.ErrLifecycleRejected
	if errors.As(err, &rejected) {
		fmt.Println("transition refused:", rejected.Message)
		return
	}
	fmt.Println(res.Version, res.Walk.Previous, "->", res.Walk.Current)
}

func ExampleClient_AsOf() {
	ctx := context.Background()
	c := xoluver.New("http://localhost:9090")
	ref := xoluver.EntityRef{Type: "asset", ID: 123}

	rec, err := c.AsOf(ctx, ref, time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC))

	var noRecord *xoluver.ErrBeforeHistory
	var deleted *xoluver.ErrDeletedAsOf
	switch {
	case err == nil:
		fmt.Println("then:", rec.Snapshot["name"], "(version", rec.Version, ")")
	case errors.As(err, &noRecord):
		// The history starts later. xoluver cannot say whether the entity existed.
		fmt.Println("no record before", noRecord.Since)
	case errors.As(err, &deleted):
		// A tombstone proves the entity was gone at that instant.
		fmt.Println("did not exist then; deleted at", deleted.DeletedAt)
	case errors.Is(err, xoluver.ErrNoHistory):
		fmt.Println("nothing is recorded for this entity")
	}
}

func ExampleClient_CheckHistory() {
	ctx := context.Background()
	c := xoluver.New("http://localhost:9090")
	ref := xoluver.EntityRef{Type: "asset", ID: 123}

	rep, _ := c.CheckHistory(ctx, ref)
	if !rep.OK() {
		fmt.Println("duplicates:", rep.Duplicates, "gaps:", rep.Gaps, "current row missing:", rep.CurrentMissing)
	}
}

func ExampleClient_Delete() {
	ctx := context.Background()
	c := xoluver.New("http://localhost:9090")
	ref := xoluver.EntityRef{Type: "asset", ID: 123}

	// Records a tombstone, then removes the entity. 3 is the version you read.
	res, err := c.Delete(ctx, ref, 3, xoluver.SaveOptions{SavedBy: "alice", Reason: "decommissioned"})

	var unfinished *xoluver.ErrDeleteIncomplete
	switch {
	case errors.As(err, &unfinished):
		// The deletion is recorded but the entity is still there. Call Delete again.
		fmt.Println("recorded at version", unfinished.Version, "- call Delete again")
	case errors.Is(err, xoluver.ErrEntityNotFound):
		fmt.Println("the entity does not exist now")
	case err == nil:
		fmt.Println("deleted; the tombstone is version", res.Version)
	}
}
