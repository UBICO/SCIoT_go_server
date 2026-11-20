package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/device"
	"github.com/UBICO/SCIoT_go_server/internal/record"
)

func TestRuntimeLoop(t *testing.T) {
	rec, err := record.Open(t.TempDir(), record.Start{}, record.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	devices := device.NewStore()
	devices.Register("a", "python", api.ModelInfo{}, api.Capabilities{})
	devices.Register("b", "python", api.ModelInfo{}, api.Capabilities{})

	const every = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runtimeLoop(ctx, rec, devices, every)
		close(done)
	}()
	time.Sleep(5*every + every/2)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runtimeLoop did not return after the context was canceled")
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(rec.Stats().File)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var got []record.Runtime
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e record.Runtime
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if e.Kind == "runtime" {
			got = append(got, e)
		}
	}
	// One at once plus one per tick: about 6; at least 3 even on a slow CI.
	if len(got) < 3 {
		t.Fatalf("%d runtime events in %v with every = %v, want at least 3", len(got), 5*every, every)
	}
	if got[0].TMS > float64(every.Milliseconds()) {
		t.Errorf("first runtime event at t_ms %.1f: want one at once, before the first tick (%v)", got[0].TMS, every)
	}
	for i, e := range got {
		if e.Devices != 2 {
			t.Errorf("event %d: devices = %d, want 2", i, e.Devices)
		}
		if e.Goroutines < 1 || e.HeapBytes == 0 {
			t.Errorf("event %d: goroutines %d, heap_bytes %d: not sampled", i, e.Goroutines, e.HeapBytes)
		}
	}
}
