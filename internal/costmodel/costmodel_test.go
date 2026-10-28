package costmodel

import (
	"math"
	"slices"
	"testing"

	"github.com/UBICO/SCIoT_go_server/internal/netmeasure"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func f(v float64) *float64 { return &v }

func TestErlangC(t *testing.T) {
	cases := []struct {
		c    int
		a    float64
		want float64
	}{
		{1, 0.5, 0.5}, // M/M/1: la probabilità di attesa è ρ
		{1, 0.9, 0.9},
		{2, 1, 1.0 / 3},
		{3, 2, 4.0 / 9},
		{2, 0, 0},           // nessun carico
		{2, 2, 1},           // instabile: tutti aspettano
		{2, 3, 1},           // oltre la capacità
		{10, 8, 0.40918015}, // tabelle di Erlang C
	}
	for _, tc := range cases {
		if got := ErlangC(tc.c, tc.a); math.Abs(got-tc.want) > 1e-7 {
			t.Errorf("ErlangC(%d, %v) = %v, atteso %v", tc.c, tc.a, got, tc.want)
		}
	}
	if !math.IsNaN(ErlangC(0, 1)) || !math.IsNaN(ErlangC(2, -1)) {
		t.Error("c < 1 o a < 0: atteso NaN")
	}
}

func TestWaitMMc(t *testing.T) {
	// M/M/1: Wq = ρ·S / (1 − ρ). λ = 25/s, S = 20 ms: ρ = 0.5, Wq = 20 ms.
	if w, ok := WaitMMc(1, 25, 0.02); !ok || !near(w, 0.02) {
		t.Errorf("M/M/1: %v, %v; attesi 0.02 s", w, ok)
	}
	// Con due slot lo stesso carico per slot aspetta meno: C = 1/3,
	// Wq = (1/3)·S/(2 − 1) = 6.67 ms.
	if w, ok := WaitMMc(2, 50, 0.02); !ok || !near(w, 0.02/3) {
		t.Errorf("M/M/2: %v, %v; attesi %v s", w, ok, 0.02/3)
	}
	if _, ok := WaitMMc(2, 100, 0.02); ok {
		t.Error("λ·S = c: la coda è instabile, ok deve essere false")
	}
}

func TestWaitGGc(t *testing.T) {
	mmc, _ := WaitMMc(2, 50, 0.02)
	if w, ok := WaitGGc(2, 50, 0.02, 1, 1); !ok || !near(w, mmc) {
		t.Errorf("ca = cs = 1: %v, %v; atteso M/M/c %v", w, ok, mmc)
	}
	if w, ok := WaitGGc(2, 50, 0.02, 1, 0); !ok || !near(w, mmc/2) {
		t.Errorf("servizio costante: %v, %v; attesa la metà di M/M/c %v", w, ok, mmc/2)
	}
	if _, ok := WaitGGc(2, 100, 0.02, 1, 0); ok {
		t.Error("coda instabile: ok deve essere false")
	}
	if w, ok := WaitGGc(2, 50, 0.02, -1, 0); ok || !math.IsNaN(w) {
		t.Errorf("ca negativo: %v, %v; attesi NaN, false", w, ok)
	}
}

// inputs: modello con 3 segmenti (0..2), device che li ha misurati tutti,
// rete 8 Mbit/s (1e6 B/s) con 2 ms di latenza, server con 2 slot vuoti.
func inputs() Inputs {
	bw, lat := 1e6, 2.0
	return Inputs{
		DeviceMS:   []*float64{f(10), f(20), f(30)},
		Network:    &netmeasure.State{BandwidthBps: &bw, LatencyMS: &lat},
		InputBytes: map[int]int{-1: 50_000, 0: 20_000, 1: 10_000},
		ServiceMS:  map[int]float64{-1: 12, 0: 8, 1: 4},
		Queue:      Queue{Slots: 2, CA: 1, CS: 1},
	}
}

func TestPredictLocal(t *testing.T) {
	p := Predict(inputs(), 2, 2)
	if !p.Local || p.DeviceMS != 60 || p.TotalMS != 60 || p.UploadMS != 0 || p.WaitMS != 0 || len(p.Missing) != 0 {
		t.Errorf("LOCAL = %+v", p)
	}
	if !near(p.FramesPerSec, 1000.0/60) {
		t.Errorf("frame/s = %v, attesi %v", p.FramesPerSec, 1000.0/60)
	}
}

func TestPredictSplit(t *testing.T) {
	in := inputs()
	p := Predict(in, 1, 2)
	// Device 0..1 = 30 ms; invio 2 + 10 KB a 1e6 B/s = 12 ms; servizio 4 ms.
	if p.Local || p.DeviceMS != 30 || p.UploadMS != 12 || p.ServiceMS != 4 || len(p.Missing) != 0 {
		t.Fatalf("split 1 = %+v", p)
	}
	// Ritmo: limitato dal prefisso (30 ms) → 33.3 frame/s; carico
	// 33.3 × 4 ms = 0.133 erlang su 2 slot.
	lambda := 1000.0 / 30
	if !near(p.FramesPerSec, lambda) || !near(p.Rho, lambda*0.004/2) {
		t.Errorf("frame/s %v, ρ %v", p.FramesPerSec, p.Rho)
	}
	wait, _ := WaitGGc(2, lambda, 0.004, 1, 1)
	if !near(p.WaitMS, wait*1000) || !near(p.TotalMS, 30+12+4+wait*1000) {
		t.Errorf("attesa %v, totale %v", p.WaitMS, p.TotalMS)
	}
}

func TestPredictCountsTheOthers(t *testing.T) {
	in := inputs()
	alone := Predict(in, 1, 2)
	in.Queue.OthersLambda = 300          // frame/s degli altri device
	in.Queue.OthersErlangs = 300 * 0.004 // con S = 4 ms: 1.2 erlang
	busy := Predict(in, 1, 2)
	if !(busy.WaitMS > alone.WaitMS) || !near(busy.Rho, (1.2+1000.0/30*0.004)/2) {
		t.Errorf("con gli altri: attesa %v (da solo %v), ρ %v", busy.WaitMS, alone.WaitMS, busy.Rho)
	}
}

func TestPredictSaturated(t *testing.T) {
	in := inputs()
	// Split −1: il device non calcola niente; rete non limitante, latenza 0:
	// il device produrrebbe frame senza limite, il server satura.
	zero := 0.0
	in.Network = &netmeasure.State{LatencyMS: &zero, NotLimiting: true}
	p := Predict(in, -1, 2)
	if !p.Saturated || !math.IsInf(p.WaitMS, 1) || !math.IsInf(p.TotalMS, 1) {
		t.Errorf("split −1 senza limiti = %+v: attesa una coda satura", p)
	}
	// Carico oltre gli slot anche con un ritmo finito.
	in = inputs()
	in.Queue.OthersLambda, in.Queue.OthersErlangs = 500, 2
	if p := Predict(in, 1, 2); !p.Saturated || p.Rho < 1 {
		t.Errorf("altri già a 2 erlang su 2 slot = %+v: attesa saturazione", p)
	}
}

func TestPredictMissingInputs(t *testing.T) {
	in := inputs()
	in.DeviceMS[1] = nil
	in.Network = nil
	delete(in.ServiceMS, 1)
	p := Predict(in, 1, 2)
	for _, want := range []string{"device segment 1", "network", "service"} {
		if !slices.Contains(p.Missing, want) {
			t.Errorf("Missing = %v, manca %q", p.Missing, want)
		}
	}
	if p := Predict(Inputs{InputBytes: map[int]int{1: 1}}, 1, 2); !slices.Contains(p.Missing, "slots") {
		t.Errorf("senza slot: Missing = %v", p.Missing)
	}
}
