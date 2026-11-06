package server

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/record"
)

// jobOf: un heartbeat del device id, riconoscibile da seq (in t_ms).
func jobOf(id string, seq float64) decisionJob {
	return decisionJob{deviceID: id, hb: &record.Heartbeat{DeviceID: id, Header: record.Header{TMS: seq}}}
}

// TestDeciderCoalescesAndKeepsTheOrder: mentre un device è in decisione, gli
// heartbeat che arrivano non si accumulano; per ogni device si decide sul più
// recente, nell'ordine del primo arrivo.
func TestDeciderCoalescesAndKeepsTheOrder(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var ran []string
	d := newDecider(func(j decisionJob) {
		if j.deviceID == "a" && j.hb.TMS == 1 {
			<-release // la prima decisione resta in corso
		}
		mu.Lock()
		defer mu.Unlock()
		ran = append(ran, fmt.Sprintf("%s@%g", j.deviceID, j.hb.TMS))
	})
	defer d.close()

	d.submit(jobOf("a", 1))
	// Aspetta che la goroutine abbia preso a@1 (la coda torna vuota).
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		empty := len(d.order) == 0
		d.mu.Unlock()
		if empty {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("la goroutine non prende il primo heartbeat")
		}
		time.Sleep(time.Millisecond)
	}
	d.submit(jobOf("b", 1))
	d.submit(jobOf("a", 2))
	d.submit(jobOf("a", 3))
	d.submit(jobOf("b", 2))
	d.mu.Lock()
	queued := slices.Clone(d.order)
	d.mu.Unlock()
	if !slices.Equal(queued, []string{"b", "a"}) {
		t.Errorf("coda = %v, attesa [b a] (senza doppioni, ordine del primo arrivo)", queued)
	}
	close(release)
	d.wait()

	mu.Lock()
	defer mu.Unlock()
	want := []string{"a@1", "b@2", "a@3"}
	if !slices.Equal(ran, want) {
		t.Errorf("decisioni = %v, attese %v", ran, want)
	}
}

// TestDeciderClose: close ferma la goroutine e ritorna; dopo close submit non
// blocca.
func TestDeciderClose(t *testing.T) {
	d := newDecider(func(decisionJob) {})
	d.submit(jobOf("a", 1))
	d.wait()
	closed := make(chan struct{})
	go func() {
		d.close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("close non ritorna")
	}
	submitted := make(chan struct{})
	go func() {
		d.submit(jobOf("a", 2))
		d.submit(jobOf("b", 1))
		close(submitted)
	}()
	select {
	case <-submitted:
	case <-time.After(5 * time.Second):
		t.Fatal("submit dopo close blocca")
	}
}
