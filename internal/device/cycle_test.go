package device

import (
	"math"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
)

// window è la telemetria di una finestra del Raspberry Pi allo split −1
// (modello di 2 segmenti, il secondo è il suffisso da 636 ms): frames frame,
// di cui local finiti sul device.
func window(frames, local int) api.Telemetry {
	return api.Telemetry{
		InferenceCount: frames,
		SegmentMS:      []*float64{nil, ptr(636.0)},
		SegmentCount:   []int{0, local},
	}
}

func TestCycleIsTheRatioOfSums(t *testing.T) {
	t0 := time.Unix(1000, 0)
	tau := time.Hour // pesi quasi uguali: il rapporto delle somme semplici
	var c Cycle
	if _, ok := c.MS(); ok {
		t.Fatal("senza finestre: ok = false")
	}
	// Finestra 1: 4 frame, 1 locale → 1000 − 636 = 364 (A di finestra 91).
	// Finestra 2: 5 frame, 2 locali → 1000 − 1272 = −272 (A di finestra < 0).
	c = c.Update(window(4, 1), 1000, t0, tau)
	c = c.Update(window(5, 2), 1000, t0.Add(time.Second), tau)
	// Somme: (364 − 272) / 9 = 10.2 ms; la media delle A tagliate a 0 sarebbe 45.5.
	if a, ok := c.MS(); !ok || math.Abs(a-92.0/9) > 0.05 {
		t.Errorf("A = %v, %v; attesi %.1f ms (rapporto delle somme)", a, ok, 92.0/9)
	}
}

func TestCycleIgnoresUselessWindows(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var c Cycle
	c = c.Update(window(4, 1), 1000, t0, time.Hour)
	want, _ := c.MS()
	bad := window(4, 1)
	bad.SegmentMS = []*float64{nil, nil} // segmento eseguito senza tempo
	for name, u := range map[string]struct {
		t   api.Telemetry
		gap float64
	}{
		"nessun frame":         {window(0, 0), 1000},
		"durata nulla":         {window(4, 1), 0},
		"senza segment_ms":     {api.Telemetry{InferenceCount: 4}, 1000},
		"segmento senza tempo": {bad, 1000},
	} {
		if a, ok := c.Update(u.t, u.gap, t0.Add(time.Second), time.Hour).MS(); !ok || a != want {
			t.Errorf("%s: A = %v, %v; attesa invariata %v", name, a, ok, want)
		}
	}
}

func TestCycleExcludesTheClientWaits(t *testing.T) {
	// 4 frame, 1 locale, 300 ms di attesa per un posto nella finestra:
	// (1000 − 636 − 300) / 4 = 16 ms.
	w := window(4, 1)
	w.WaitMS = ptr(300.0)
	c := Cycle{}.Update(w, 1000, time.Unix(1000, 0), time.Hour)
	if a, ok := c.MS(); !ok || math.Abs(a-16) > 1e-9 {
		t.Errorf("A = %v, %v; attesi 16 ms", a, ok)
	}
}

func TestCycleNeverBelowZero(t *testing.T) {
	// Orologi diversi: più calcolo della durata in ogni finestra.
	c := Cycle{}.Update(window(2, 2), 1000, time.Unix(1000, 0), time.Hour)
	if a, ok := c.MS(); !ok || a != 0 {
		t.Errorf("A = %v, %v; atteso 0", a, ok)
	}
}
