// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

// Command quickstart walks through xoluver against a running xolu server:
// save versions, hit a conflict, restore, label, and check the history.
//
//	XOLU_URL=http://localhost:9090 go run ./examples/quickstart
//
// The server needs XOLU_API_V2_ENABLED=true (labels use /meta).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/ha1tch/xoluver"
)

func main() {
	url := os.Getenv("XOLU_URL")
	if url == "" {
		url = "http://localhost:9090"
	}
	ctx := context.Background()
	c := xoluver.New(url)

	// 1. Create an entity the normal xolu way. xoluver does not create entities;
	//    it versions the ones that exist. A new entity starts at version 1.
	id := createAsset(url, map[string]any{"name": "pump-1", "site": "north"})
	ref := xoluver.EntityRef{Type: "asset", ID: id}
	fmt.Printf("created asset %d at version 1\n", id)

	// 2. Save a change. You pass the version you read (1) and the complete new
	//    document. The first save also records version 1 as a baseline.
	v, err := c.Save(ctx, ref, 1, map[string]any{"name": "pump-1b", "site": "north"},
		xoluver.SaveOptions{SavedBy: "alice", Reason: "renamed"})
	check(err)
	fmt.Printf("saved: now at version %d\n", v)

	// 3. Save again, building on the version just returned.
	v, err = c.Save(ctx, ref, v, map[string]any{"name": "pump-1b", "site": "south"},
		xoluver.SaveOptions{SavedBy: "bob", Reason: "moved"})
	check(err)
	fmt.Printf("saved: now at version %d\n", v)

	// 4. Someone still working from version 1 is rejected, and nothing is written.
	_, err = c.Save(ctx, ref, 1, map[string]any{"name": "stale edit"}, xoluver.SaveOptions{SavedBy: "carol"})
	var conflict *xoluver.ErrVersionConflict
	if errors.As(err, &conflict) {
		fmt.Printf("carol's save was rejected: the current version is %d\n", conflict.Current)
	} else {
		log.Fatalf("expected a conflict, got %v", err)
	}

	// 5. Read the history, newest first.
	fmt.Println("history:")
	list, err := c.ListVersions(ctx, ref, 0)
	check(err)
	for _, s := range list {
		fmt.Printf("  v%d  %-8s by %-5s %s\n", s.Version, s.ChangeKind, s.SavedBy, s.Reason)
	}

	// 6. Restore version 1. This is a new save (version 4) with the old content.
	v, err = c.Restore(ctx, ref, 1, v, xoluver.SaveOptions{SavedBy: "alice", Reason: "undo"})
	check(err)
	fmt.Printf("restored version 1 as version %d\n", v)

	// 7. Name a version, and read it back.
	check(c.SetLabel(ctx, ref, "published", 2, "alice"))
	labels, err := c.Labels(ctx, ref)
	check(err)
	for _, l := range labels {
		fmt.Printf("label %q points at version %d\n", l.Name, l.Version)
	}

	// 8. Check that the history is consistent.
	rep, err := c.CheckHistory(ctx, ref)
	check(err)
	fmt.Printf("history ok: %v (versions %v)\n", rep.OK(), rep.Versions)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// createAsset creates an entity with a plain xolu POST and returns its id.
func createAsset(baseURL string, doc map[string]any) int {
	body, _ := json.Marshal(doc)
	resp, err := http.Post(baseURL+"/api/v1/asset", "application/json", bytes.NewReader(body))
	check(err)
	defer resp.Body.Close()
	var out struct {
		ID int `json:"id"`
	}
	check(json.NewDecoder(resp.Body).Decode(&out))
	if out.ID == 0 {
		log.Fatalf("create failed: HTTP %d", resp.StatusCode)
	}
	return out.ID
}
