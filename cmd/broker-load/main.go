// Command broker-load is a concurrent load generator for the managed-key broker.
// Each virtual caller has its own ed25519 identity and signs in-process (no
// per-request process spawn), so it can drive real pressure. Reports status-code
// tallies, throughput, and latency percentiles.
//
//	broker-load -broker http://127.0.0.1:8099 -app io.pilot.partner \
//	  -path /find-email -callers 200 -per 10
package main

import (
	"bytes"
	"crypto/ed25519"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pilot-protocol/app-template/internal/broker"
)

func main() {
	base := flag.String("broker", "http://127.0.0.1:8099", "broker base URL")
	app := flag.String("app", "", "app id, e.g. io.pilot.partner")
	path := flag.String("path", "/find-email", "method path under the app")
	callers := flag.Int("callers", 100, "distinct concurrent callers (each its own identity)")
	per := flag.Int("per", 1, "requests per caller")
	body := flag.String("body", "{}", "request body")
	flag.Parse()
	if *app == "" {
		fmt.Fprintln(os.Stderr, "broker-load: -app is required")
		os.Exit(2)
	}

	url := *base + "/" + *app + *path
	reqPath := "/" + *app + *path
	client := &http.Client{Timeout: 30 * time.Second}

	var wg sync.WaitGroup
	var codes sync.Map // int -> *int64
	var lat sync.Map   // per-request latency (ns), collected lock-free-ish
	latencies := make([]time.Duration, 0, *callers**per)
	var latMu sync.Mutex
	bump := func(code int) {
		v, _ := codes.LoadOrStore(code, new(int64))
		atomic.AddInt64(v.(*int64), 1)
	}
	_ = lat

	start := time.Now()
	for c := 0; c < *callers; c++ {
		_, priv, _ := ed25519.GenerateKey(nil)
		wg.Add(1)
		go func(priv ed25519.PrivateKey) {
			defer wg.Done()
			for i := 0; i < *per; i++ {
				b := []byte(*body)
				hdr := broker.Sign(priv, "POST", reqPath, b, time.Now())
				req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
				for k, v := range hdr {
					req.Header.Set(k, v)
				}
				req.Header.Set("Content-Type", "application/json")
				t0 := time.Now()
				resp, err := client.Do(req)
				d := time.Since(t0)
				if err != nil {
					bump(0)
					continue
				}
				resp.Body.Close()
				bump(resp.StatusCode)
				latMu.Lock()
				latencies = append(latencies, d)
				latMu.Unlock()
			}
		}(priv)
	}
	wg.Wait()
	elapsed := time.Since(start)

	total := *callers * *per
	fmt.Printf("requests=%d callers=%d per=%d elapsed=%s throughput=%.0f req/s\n",
		total, *callers, *per, elapsed.Round(time.Millisecond), float64(total)/elapsed.Seconds())
	codes.Range(func(k, v any) bool {
		fmt.Printf("  status %d: %d\n", k.(int), atomic.LoadInt64(v.(*int64)))
		return true
	})
	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		p := func(q float64) time.Duration { return latencies[int(float64(len(latencies)-1)*q)] }
		fmt.Printf("  latency p50=%s p95=%s p99=%s max=%s\n",
			p(0.50).Round(time.Millisecond), p(0.95).Round(time.Millisecond),
			p(0.99).Round(time.Millisecond), latencies[len(latencies)-1].Round(time.Millisecond))
	}
}
