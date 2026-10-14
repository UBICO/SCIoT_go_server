package main

import (
	"context"
	"encoding/binary"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/spike/ml-runtime/workerpb"
)

func floats(vs ...float32) []byte {
	b := make([]byte, 4*len(vs))
	for i, v := range vs {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(v))
	}
	return b
}

func TestMaxAbsDiff(t *testing.T) {
	d, err := maxAbsDiff(floats(1, 2, 3), floats(1, 2.5, 2))
	if err != nil || d != 1 {
		t.Errorf("maxAbsDiff = %v, %v; want 1", d, err)
	}
	if _, err := maxAbsDiff(floats(1), floats(1, 2)); err == nil {
		t.Error("lunghezze diverse devono dare errore")
	}
}

func TestPercentile(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	// Stesso metodo di gen_inputs.py: indice round(q/100 * 9).
	cases := map[float64]float64{0: 1, 50: 6, 95: 10, 100: 10}
	for q, want := range cases {
		if got := percentile(values, q); got != want {
			t.Errorf("percentile(%v) = %v, want %v", q, got, want)
		}
	}
	if got := percentile([]float64{42}, 95); got != 42 {
		t.Errorf("un solo valore: %v", got)
	}
}

func TestFillStats(t *testing.T) {
	samples := []sample{
		{latency: 10 * time.Millisecond, queueWait: 0},
		{latency: 20 * time.Millisecond, queueWait: 4},
		{latency: 30 * time.Millisecond, queueWait: 8},
		{latency: 40 * time.Millisecond, queueWait: 0},
	}
	var res Result
	fillStats(&res, samples, 2*time.Second)

	if res.ThroughputRPS != 2 {
		t.Errorf("throughput = %v, want 2 (4 richieste in 2 s)", res.ThroughputRPS)
	}
	if res.MeanMS != 25 || res.QueueWaitMS != 3 {
		t.Errorf("mean = %v, queue = %v; want 25, 3", res.MeanMS, res.QueueWaitMS)
	}
	// Ordinate: 10 20 30 40 → p50 indice round(1.5)=2 → 30; p95 indice 3 → 40.
	if res.P50MS != 30 || res.P95MS != 40 {
		t.Errorf("p50 = %v, p95 = %v", res.P50MS, res.P95MS)
	}
}

// TestRunConcurrency usa un call finto (nessun worker): verifica che run
// esegua esattamente n richieste, ognuna con un indice diverso, con al
// massimo `concurrency` richieste contemporanee.
func TestRunConcurrency(t *testing.T) {
	const n, concurrency = 50, 4
	expected := floats(1, 2)

	var mu sync.Mutex
	seen := map[int]bool{}
	inflight, peak := 0, 0

	call := func(_ context.Context, i int) (*workerpb.RunResponse, error) {
		mu.Lock()
		if seen[i] {
			t.Errorf("indice %d eseguito due volte", i)
		}
		seen[i] = true
		inflight++
		peak = max(peak, inflight)
		mu.Unlock()

		time.Sleep(2 * time.Millisecond)

		mu.Lock()
		inflight--
		mu.Unlock()
		return &workerpb.RunResponse{Output: expected, QueueWaitMs: 1}, nil
	}

	samples, elapsed, maxDiff := run(call, concurrency, n, expected)

	if len(samples) != n || len(seen) != n {
		t.Fatalf("campioni %d, indici %d; want %d", len(samples), len(seen), n)
	}
	if peak > concurrency || peak < 2 {
		t.Errorf("picco di richieste parallele %d, want 2..%d", peak, concurrency)
	}
	if maxDiff != 0 || elapsed <= 0 {
		t.Errorf("maxDiff = %v, elapsed = %v", maxDiff, elapsed)
	}
	for _, s := range samples {
		if s.latency < 2*time.Millisecond || s.queueWait != 1 {
			t.Fatalf("campione errato: %+v", s)
		}
	}
}
