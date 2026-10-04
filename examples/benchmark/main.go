// Copyright (c) 2026 haitch
// Licensed under the Apache License, Version 2.0
// https://www.apache.org/licenses/LICENSE-2.0

// Command benchmark measures what the two optional primitives cost, so the
// trade-offs in docs/FSM_AND_TS.md can be checked on your own hardware instead
// of taken on trust:
//
//  1. write latency: a plain Save, a Save with an /fsm walk (with and without a
//     guard), and a Save with the /ts version index
//  2. AsOf latency as the whole history type grows (index against scan)
//  3. AsOf latency as one entity's history grows (index against scan)
//  4. write throughput with several writers
//
// It needs a xolu that serves /ts on tenant routes AND the ordinary routes (the
// default tenant mode does both), with /v2 enabled:
//
//	XOLU_TIMESERIES_ENABLED=true XOLU_TENANT_AUTO_REGISTER=true XOLU_API_V2_ENABLED=true XOLU_AUTH_TYPE=none ./xolu
//	XOLU_URL=http://localhost:9093 go run ./examples/benchmark
//
// The scan needs OQL, which works only on the ordinary routes, and the index
// needs tenant routes, so the two sides run on different routes of one server.
// Every number depends on the machine, so read ratios, not milliseconds.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ha1tch/xoluver"
)

var (
	flagURL      = flag.String("url", env("XOLU_URL", "http://localhost:9093"), "xolu base URL")
	flagTenant   = flag.String("tenant", env("XOLU_TENANT", "acme"), "tenant for the routes that need one")
	flagSections = flag.String("sections", "1,2,3,4", "which sections to run")
	flagSaves    = flag.Int("saves", 200, "section 1: timed saves per configuration")
	flagCalls    = flag.Int("calls", 100, "sections 2 and 3: timed AsOf calls per cell")
	flagRows     = flag.String("rows", "0,1000,10000", "section 2: total history rows in the type")
	flagVersions = flag.String("versions", "10,100,1000", "section 3: versions of the one entity")
	flagEntity   = flag.Int("entity", 20, "section 2: versions of the entity measured")
	flagWorkers  = flag.Int("workers", 4, "section 4: concurrent writers")
	flagPer      = flag.Int("per", 50, "section 4: saves per writer")
	flagSnapshot = flag.Int("snapshot", 500, "bytes of padding in filler history rows")
	flagRounds   = flag.Int("rounds", 3, "sections 1 and 4: rounds, each configuration run once per round in rotating order")
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchmark:", err)
		os.Exit(1)
	}
}

// ---- plumbing ---------------------------------------------------------------

type counting struct {
	rt http.RoundTripper
	n  int64
}

func (c *counting) RoundTrip(r *http.Request) (*http.Response, error) {
	atomic.AddInt64(&c.n, 1)
	return c.rt.RoundTrip(r)
}

// api is a plain HTTP caller that can address either the ordinary routes
// (tenant "") or a tenant's routes.
type api struct{ tenant string }

func (a api) route(p string) string {
	if a.tenant == "" {
		return p
	}
	for _, prefix := range []string{"/api/v1/", "/api/v2/"} {
		if strings.HasPrefix(p, prefix) {
			return prefix + "tenant/" + a.tenant + "/" + strings.TrimPrefix(p, prefix)
		}
	}
	return p
}

func (a api) call(method, p string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(*flagURL, "/")+a.route(p), body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s %s: HTTP %d: %.200s", method, p, resp.StatusCode, data)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (a api) create(typ string, doc map[string]any) int {
	var out struct {
		ID int `json:"id"`
	}
	must(a.call("POST", "/api/v1/"+typ, doc, &out))
	return out.ID
}

func newClient(a api, ct *counting) *xoluver.Client {
	c := xoluver.New(*flagURL)
	c.Tenant = a.tenant
	if ct != nil {
		c.HTTP = &http.Client{Transport: ct, Timeout: 120 * time.Second}
	}
	return c
}

var seq int64

func uniq(prefix string) string {
	return prefix + strconv.FormatInt(time.Now().UnixNano()/1000, 36) + strconv.FormatInt(atomic.AddInt64(&seq, 1), 36)
}

// machine defines a small lifecycle and starts one machine for the entity. With
// guard, "submit" is guarded by a T-SQL condition on its payload.
func (a api) machine(typ string, id int, guard bool) int {
	submit := map[string]any{"from": "Draft", "input": "submit", "to": "Review"}
	if guard {
		submit["guard"] = "payload.reviewer != ''"
	}
	def := map[string]any{
		"name": uniq("Bench"), "determinism": "strict", "initial": "Draft",
		"states": map[string]any{
			"Draft": map[string]any{"terminal": false}, "Review": map[string]any{"terminal": false}, "Retired": map[string]any{"terminal": true},
		},
		"transitions": []any{submit, map[string]any{"from": "Review", "input": "reject", "to": "Draft"}, map[string]any{"from": []any{"Draft", "Review"}, "input": "retire", "to": "Retired"}},
	}
	var d, m struct {
		ID int `json:"id"`
	}
	must(a.call("POST", "/api/v2/fsm/def", def, &d))
	must(a.call("POST", "/api/v2/fsm/machine", map[string]any{"definition": d.ID, "ref": fmt.Sprintf("%s:%d", typ, id)}, &m))
	return m.ID
}

// ---- statistics -------------------------------------------------------------

type stats struct {
	n                  int
	median, p95, total time.Duration
	reqs               float64
}

func summarize(ds []time.Duration, reqs int64) stats {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	var total time.Duration
	for _, d := range s {
		total += d
	}
	return stats{n: len(s), median: s[len(s)/2], p95: s[len(s)*95/100], total: total, reqs: float64(reqs) / float64(len(s))}
}

func ms(d time.Duration) string { return fmt.Sprintf("%.2f", float64(d)/float64(time.Millisecond)) }

func ratio(x, base time.Duration) string { return fmt.Sprintf("%.2fx", float64(x)/float64(base)) }

// ---- section 1: write latency -----------------------------------------------

type writeCfg struct {
	name   string
	tenant string
	index  bool
	walk   string // "", "plain" (no guard) or "guard"
}

// saveOnce saves a small document, alternating the walk input when there is one.
func saveOnce(ctx context.Context, c *xoluver.Client, ref xoluver.EntityRef, ver, i, machine int, walk string) (int, error) {
	opt := xoluver.SaveOptions{SavedBy: "bench"}
	if walk != "" {
		w := &xoluver.LifecycleWalk{MachineID: machine, Input: "reject"}
		if i%2 == 0 {
			w.Input = "submit"
			if walk == "guard" {
				w.Payload = map[string]any{"reviewer": "r1"}
			}
		}
		opt.Walk = w
	}
	return c.Save(ctx, ref, ver, map[string]any{"n": i}, opt)
}

// setup creates what a configuration needs: a client, an entity and, for walks, a machine.
func (cfg writeCfg) setup(ctx context.Context) (c *xoluver.Client, ref xoluver.EntityRef, machine int) {
	a := api{tenant: cfg.tenant}
	c = newClient(a, nil)
	typ := uniq("bw")
	if cfg.index {
		tl := 30000 + int(atomic.AddInt64(&seq, 1))%30000
		must(c.DefineTimeIndex(ctx, tl))
		must(c.UseTimeIndex(typ, tl))
	}
	id := a.create(typ, map[string]any{"n": 0})
	ref = xoluver.EntityRef{Type: typ, ID: id}
	if cfg.walk != "" {
		machine = a.machine(typ, id, cfg.walk == "guard")
	}
	return c, ref, machine
}

// runWrites performs one round of one write configuration on a fresh entity and
// returns the latency of each timed save.
func runWrites(ctx context.Context, cfg writeCfg) []time.Duration {
	c, ref, machine := cfg.setup(ctx)
	ver := 1
	var err error
	for w := 0; w < 10; w++ { // warm up
		ver, err = saveOnce(ctx, c, ref, ver, w, machine, cfg.walk)
		must(err)
	}
	ds := make([]time.Duration, 0, *flagSaves)
	for k := 0; k < *flagSaves; k++ {
		t0 := time.Now()
		ver, err = saveOnce(ctx, c, ref, ver, k+10, machine, cfg.walk)
		must(err)
		ds = append(ds, time.Since(t0))
	}
	return ds
}

func section1(ctx context.Context) {
	fmt.Printf("\n== 1. Write latency: %d timed saves per configuration per round, %d rounds pooled (ms; one writer; each save reads the entity and one history row, then commits)\n\n", *flagSaves, *flagRounds)
	cfgs := []writeCfg{
		{"plain Save", "", false, ""},
		{"  + /fsm walk, no guard", "", false, "plain"},
		{"  + /fsm walk, guarded", "", false, "guard"},
		{"tenant Save", *flagTenant, false, ""},
		{"  + /ts version index", *flagTenant, true, ""},
		{"  + index + guarded walk", *flagTenant, true, "guard"},
	}
	pool := make([][]time.Duration, len(cfgs))
	for round := 0; round < *flagRounds; round++ {
		for k := range cfgs {
			i := (k + round) % len(cfgs) // rotate the order so no configuration always runs first
			pool[i] = append(pool[i], runWrites(ctx, cfgs[i])...)
		}
	}
	fmt.Printf("%-28s %8s %8s  %s\n", "configuration", "median", "p95", "vs its baseline")
	var base, tbase, idx time.Duration
	for i, cfg := range cfgs {
		s := summarize(pool[i], 0)
		note := "(baseline)"
		switch i {
		case 0:
			base = s.median
		case 1, 2:
			note = ratio(s.median, base)
		case 3:
			tbase = s.median
		case 4:
			idx, note = s.median, ratio(s.median, tbase)
		case 5:
			note = ratio(s.median, tbase) + " of tenant, " + ratio(s.median, idx) + " of the index row"
		}
		fmt.Printf("%-28s %8s %8s  %s\n", cfg.name, ms(s.median), ms(s.p95), note)
	}
}

// ---- filler and entity builders ---------------------------------------------

// filler pads a history type with rows for other entities, 25 per commit, so the
// scan has something to scan. It writes through the same route as the entity.
type filler struct {
	a    api
	typ  string
	ent  string
	id   int
	ver  int
	rows int
	pad  string
}

func newFiller(a api, typ string) *filler {
	f := &filler{a: a, typ: typ, ent: typ + "fill", ver: 1, pad: strings.Repeat("x", *flagSnapshot)}
	f.id = a.create(f.ent, map[string]any{"n": 0})
	return f
}

func (f *filler) topUp(target int) {
	for f.rows < target {
		n := target - f.rows
		if n > 25 {
			n = 25
		}
		rows := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			k := f.rows + i
			rows = append(rows, map[string]any{"entity": f.typ + "_version", "id": 4_000_000_000_000 + k, "data": map[string]any{
				"entity_id": 2_000_000 + k%5000, "version": 1 + k%900, "snapshot": map[string]any{"pad": f.pad},
				"change_kind": "save", "saved_by": "filler", "saved_at": "2026-01-01T00:00:00.000000000Z",
			}})
		}
		req := map[string]any{"update": map[string]any{"entity": f.ent, "id": f.id, "version": f.ver, "data": map[string]any{"n": f.ver}}, "append": rows}
		must(f.a.call("POST", "/api/v1/commit", req, nil))
		f.ver++
		f.rows += n
	}
}

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// build saves an entity m times, one second apart on an explicit clock, so
// instants can be chosen inside its history. It returns the number of history
// rows written (one per version, plus the baseline).
func build(ctx context.Context, c *xoluver.Client, a api, typ string, m int) xoluver.EntityRef {
	id := a.create(typ, map[string]any{"n": 0})
	ref := xoluver.EntityRef{Type: typ, ID: id}
	clock := base
	c.Now = func() time.Time { return clock }
	ver := 1
	for i := 1; i <= m; i++ {
		clock = base.Add(time.Duration(i) * time.Second)
		v, err := c.Save(ctx, ref, ver, map[string]any{"n": i}, xoluver.SaveOptions{SavedBy: "bench"})
		must(err)
		ver = v
	}
	return ref
}

// timeAsOf runs AsOf at the given instants and reports latency and requests per call.
func timeAsOf(ctx context.Context, c *xoluver.Client, ct *counting, ref xoluver.EntityRef, instants []time.Time) stats {
	for i := 0; i < 3; i++ { // warm up
		_, err := c.AsOf(ctx, ref, instants[i%len(instants)])
		must(err)
	}
	ds := make([]time.Duration, 0, len(instants))
	before := atomic.LoadInt64(&ct.n)
	for _, t := range instants {
		t0 := time.Now()
		_, err := c.AsOf(ctx, ref, t)
		must(err)
		ds = append(ds, time.Since(t0))
	}
	return summarize(ds, atomic.LoadInt64(&ct.n)-before)
}

func instantsIn(m, calls int) []time.Time {
	r := rand.New(rand.NewSource(7))
	out := make([]time.Time, calls)
	for i := range out {
		out[i] = base.Add(time.Duration(1+r.Int63n(int64(m))) * time.Second).Add(time.Duration(r.Int63n(int64(time.Second))))
	}
	return out
}

// pair builds the same entity on both sides: the ordinary routes without an index
// (AsOf scans) and the tenant routes with one (AsOf uses the index).
type pair struct {
	scanC, idxC     *xoluver.Client
	scanCt, idxCt   *counting
	scanRef, idxRef xoluver.EntityRef
	scanF, idxF     *filler
}

func newPair(ctx context.Context, typ string, m int, withFiller bool) *pair {
	p := &pair{scanCt: &counting{rt: http.DefaultTransport}, idxCt: &counting{rt: http.DefaultTransport}}
	plain, tenant := api{}, api{tenant: *flagTenant}
	p.scanC, p.idxC = newClient(plain, p.scanCt), newClient(tenant, p.idxCt)
	tl := 40000 + int(atomic.AddInt64(&seq, 1))%20000
	must(p.idxC.DefineTimeIndex(ctx, tl))
	must(p.idxC.UseTimeIndex(typ, tl))
	p.scanRef = build(ctx, p.scanC, plain, typ, m)
	p.idxRef = build(ctx, p.idxC, tenant, typ, m)
	if withFiller {
		p.scanF, p.idxF = newFiller(plain, typ), newFiller(tenant, typ)
	}
	return p
}

// ---- section 2: AsOf against the size of the whole history type -------------

func section2(ctx context.Context) {
	fmt.Printf("\n== 2. AsOf latency as the whole history type grows (entity of %d versions; %d calls per cell; filler rows carry %d bytes of snapshot)\n\n", *flagEntity, *flagCalls, *flagSnapshot)
	typ := uniq("bs")
	p := newPair(ctx, typ, *flagEntity, true)
	instants := instantsIn(*flagEntity, *flagCalls)
	fmt.Printf("%-12s | %-30s | %-30s | %s\n", "history rows", "scan (ordinary routes)", "index (tenant routes)", "scan / index")
	fmt.Printf("%-12s | %-9s %-9s %-10s | %-9s %-9s %-10s |\n", "in the type", "median", "p95", "requests", "median", "p95", "requests")
	for _, field := range strings.Split(*flagRows, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		must(err)
		p.scanF.topUp(n)
		p.idxF.topUp(n)
		s := timeAsOf(ctx, p.scanC, p.scanCt, p.scanRef, instants)
		x := timeAsOf(ctx, p.idxC, p.idxCt, p.idxRef, instants)
		hits, falls := p.idxC.IndexStats()
		if falls != 0 || hits == 0 {
			must(fmt.Errorf("the index client left the index %d times (hits %d): the comparison would be wrong", falls, hits))
		}
		fmt.Printf("%-12d | %-9s %-9s %-10.1f | %-9s %-9s %-10.1f | %s\n", n+*flagEntity+1, ms(s.median), ms(s.p95), s.reqs, ms(x.median), ms(x.p95), x.reqs, ratio(s.median, x.median))
	}
}

// ---- section 3: AsOf against the length of one entity's history -------------

func section3(ctx context.Context) {
	fmt.Printf("\n== 3. AsOf latency as one entity's history grows (no other rows in the type; %d calls per cell)\n\n", *flagCalls)
	fmt.Printf("%-10s | %-30s | %-30s | %s\n", "versions", "scan (ordinary routes)", "index (tenant routes)", "scan / index")
	fmt.Printf("%-10s | %-9s %-9s %-10s | %-9s %-9s %-10s |\n", "of entity", "median", "p95", "requests", "median", "p95", "requests")
	for _, field := range strings.Split(*flagVersions, ",") {
		m, err := strconv.Atoi(strings.TrimSpace(field))
		must(err)
		p := newPair(ctx, uniq("bv"), m, false)
		instants := instantsIn(m, *flagCalls)
		s := timeAsOf(ctx, p.scanC, p.scanCt, p.scanRef, instants)
		x := timeAsOf(ctx, p.idxC, p.idxCt, p.idxRef, instants)
		fmt.Printf("%-10d | %-9s %-9s %-10.1f | %-9s %-9s %-10.1f | %s\n", m, ms(s.median), ms(s.p95), s.reqs, ms(x.median), ms(x.p95), x.reqs, ratio(s.median, x.median))
	}
}

// ---- section 4: write throughput with several writers -----------------------

// runThroughput runs the writers of one configuration at once and returns the
// saves per second and every save's latency.
func runThroughput(ctx context.Context, cfg writeCfg) (float64, []time.Duration) {
	type worker struct {
		ref     xoluver.EntityRef
		machine int
	}
	a := api{tenant: cfg.tenant}
	c := newClient(a, nil)
	typ := uniq("bt")
	if cfg.index {
		tl := 50000 + int(atomic.AddInt64(&seq, 1))%10000
		must(c.DefineTimeIndex(ctx, tl))
		must(c.UseTimeIndex(typ, tl))
	}
	ws := make([]worker, *flagWorkers)
	for w := range ws {
		id := a.create(typ, map[string]any{"n": 0})
		ws[w] = worker{ref: xoluver.EntityRef{Type: typ, ID: id}}
		if cfg.walk != "" {
			ws[w].machine = a.machine(typ, id, cfg.walk == "guard")
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var all []time.Duration
	start := make(chan struct{})
	for _, w := range ws {
		wg.Add(1)
		go func(w worker) {
			defer wg.Done()
			<-start
			ver := 1
			local := make([]time.Duration, 0, *flagPer)
			for k := 0; k < *flagPer; k++ {
				t0 := time.Now()
				v, err := saveOnce(ctx, c, w.ref, ver, k, w.machine, cfg.walk)
				must(err)
				ver = v
				local = append(local, time.Since(t0))
			}
			mu.Lock()
			all = append(all, local...)
			mu.Unlock()
		}(w)
	}
	t0 := time.Now()
	close(start)
	wg.Wait()
	return float64(len(all)) / time.Since(t0).Seconds(), all
}

func section4(ctx context.Context) {
	fmt.Printf("\n== 4. Write throughput: %d writers, %d saves each, distinct entities (no conflicts), median of %d rounds\n\n", *flagWorkers, *flagPer, *flagRounds)
	cfgs := []writeCfg{
		{"plain Save", "", false, ""},
		{"  + guarded /fsm walk", "", false, "guard"},
		{"tenant Save", *flagTenant, false, ""},
		{"  + /ts version index", *flagTenant, true, ""},
		{"  + index + guarded walk", *flagTenant, true, "guard"},
	}
	rates := make([][]float64, len(cfgs))
	pool := make([][]time.Duration, len(cfgs))
	for round := 0; round < *flagRounds; round++ {
		for k := range cfgs {
			i := (k + round) % len(cfgs)
			rate, ds := runThroughput(ctx, cfgs[i])
			rates[i] = append(rates[i], rate)
			pool[i] = append(pool[i], ds...)
		}
	}
	median := func(xs []float64) float64 {
		s := append([]float64(nil), xs...)
		sort.Float64s(s)
		return s[len(s)/2]
	}
	fmt.Printf("%-28s %12s %14s\n", "configuration", "saves/second", "median save ms")
	var plainBase, tenantBase float64
	for i, cfg := range cfgs {
		rate := median(rates[i])
		note := ""
		switch i {
		case 0:
			plainBase = rate
		case 1:
			note = fmt.Sprintf("  %.2fx of plain", rate/plainBase)
		case 2:
			tenantBase = rate
		default:
			note = fmt.Sprintf("  %.2fx of tenant", rate/tenantBase)
		}
		fmt.Printf("%-28s %12.0f %14s%s\n", cfg.name, rate, ms(summarize(pool[i], 0).median), note)
	}
}

func main() {
	flag.Parse()
	ctx := context.Background()
	fmt.Printf("xoluver benchmark: %s/%s, %d CPU, %s, server %s, tenant %q\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version(), *flagURL, *flagTenant)
	// Warm the server up with a throwaway round, so the first measurement does not pay for it.
	runWrites(ctx, writeCfg{"warm-up", "", false, ""})
	runWrites(ctx, writeCfg{"warm-up", *flagTenant, false, ""})
	sections := map[string]func(context.Context){"1": section1, "2": section2, "3": section3, "4": section4}
	for _, s := range strings.Split(*flagSections, ",") {
		if f, ok := sections[strings.TrimSpace(s)]; ok {
			f(ctx)
		}
	}
	fmt.Println("\nThe numbers depend on this machine and on running the client and the server together. Compare the ratios.")
}
