// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

// Command bypass shows what happens when something changes an entity without
// going through xoluver, and how CheckHistory reports it. The rule: change a
// versioned entity only through Save.
//
//	XOLU_URL=http://localhost:9090 go run ./examples/bypass
package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/ha1tch/xoluver"
	"github.com/ha1tch/xoluver/examples/internal/demo"
)

func main() {
	ctx := context.Background()
	c := xoluver.New(demo.BaseURL())
	id := demo.CreateAsset(map[string]any{"name": "pump-1"})
	ref := xoluver.EntityRef{Type: "asset", ID: id}

	report := func(title string) {
		rep, err := c.CheckHistory(ctx, ref)
		demo.Must(err)
		fmt.Printf("%-44s ok=%-5v entity at v%d, history has %v", title, rep.OK(), rep.CurrentVersion, rep.Versions)
		if rep.CurrentMissing {
			fmt.Print("  [no history row for the current version]")
		}
		if len(rep.Gaps) > 0 {
			fmt.Printf("  [missing versions %v]", rep.Gaps)
		}
		fmt.Println()
	}
	save := func(base int, name string) int {
		v, err := c.Save(ctx, ref, base, map[string]any{"name": name}, xoluver.SaveOptions{SavedBy: "alice"})
		demo.Must(err)
		return v
	}
	plainWrite := func(name string) { // a plain xolu PUT: no snapshot is written
		demo.Call(http.MethodPut, fmt.Sprintf("/api/v1/asset/%d", id), map[string]any{"name": name}, nil)
	}

	save(1, "pump-1b")
	report("after a normal save:")

	plainWrite("changed behind xoluver's back")
	report("after one plain write:")

	save(3, "saved again") // the next save records the plain write as its baseline
	report("after the next save (repaired):")

	plainWrite("write one") // version 5
	plainWrite("write two") // version 6
	save(6, "saved after two plain writes")
	report("after two plain writes and a save:")
}
