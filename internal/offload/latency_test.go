package offload

import (
	"math"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/costmodel"
)

// pred è una previsione con i soli campi che l'algoritmo usa.
func pred(split int, p95, fps float64) costmodel.Prediction {
	return costmodel.Prediction{Split: split, P95MS: p95, TotalMS: p95, FramesPerSec: fps, Rho: 0.3}
}

// preds: modello di 3 segmenti; LOCAL 60 ms a 16 frame/s. In ordine di
// split: −1, 0, 1, LOCAL.
func preds() []costmodel.Prediction {
	local := pred(2, 60, 16)
	local.Local = true
	return []costmodel.Prediction{pred(-1, 50, 30), pred(0, 40, 25), pred(1, 45, 20), local}
}

func onSplit(k int, age time.Duration) State {
	return State{DeviceID: "d", Last: 2, PolicyAge: age, Policy: api.Policy{Mode: api.ModeSplit, SplitPoint: ip(k)}}
}

// calm: σ nullo e campioni sufficienti per ogni split point.
func calm() ([]float64, []uint64) { return make([]float64, 4), []uint64{5, 5, 5, 5} }

func TestLatencyMovesToTheBest(t *testing.T) {
	l := NewLatency(DefaultLatencyConfig())
	sigma, n := calm()
	// Da split 1 (45 ms) a split 0 (40 ms): 5 ms di guadagno contro un
	// margine di max(2, 4.5) = 4.5 ms, ma con σ = 1 su entrambi (√2 ≈ 1.4)
	// il margine diventa 5.9: resta.
	sigma[1], sigma[2] = 1, 1
	if d := l.choose(onSplit(1, time.Minute), preds(), sigma, n); d.Changes(onSplit(1, 0).Policy, 0) || d.Reason != ReasonHysteresis {
		t.Errorf("guadagno sotto il margine: %+v", d)
	}
	// Da split −1 (50 ms): 10 ms di guadagno, margine ≈ 6.4 ms: cambia.
	d := l.choose(onSplit(-1, time.Minute), preds(), sigma, n)
	if d.Mode != api.ModeSplit || d.SplitPoint == nil || *d.SplitPoint != 0 || d.Reason != ReasonChange {
		t.Errorf("guadagno oltre il margine: %+v", d)
	}
	// Già sul migliore.
	if d := l.choose(onSplit(0, time.Minute), preds(), sigma, n); d.Reason != ReasonBest || d.Changes(onSplit(0, 0).Policy, 0) {
		t.Errorf("sul migliore: %+v", d)
	}
}

func TestLatencyMinHold(t *testing.T) {
	l := NewLatency(DefaultLatencyConfig())
	sigma, n := calm()
	if d := l.choose(onSplit(-1, 5*time.Second), preds(), sigma, n); d.Reason != ReasonMinHold || d.Changes(onSplit(-1, 0).Policy, 0) {
		t.Errorf("policy di 5 s: %+v; attesa la durata minima", d)
	}
}

func TestLatencyConstraints(t *testing.T) {
	l := NewLatency(DefaultLatencyConfig())
	ps := preds()
	local := ps[3]
	// Split 0 più lento di LOCAL in frame/s: escluso.
	slow := ps[1]
	slow.FramesPerSec = 10
	// Split 0 con il 20% dei frame rifiutati: escluso.
	rejected := ps[1]
	rejected.RejectedPerSec = 5
	missing := ps[1]
	missing.Missing = []string{"service"}
	for name, p := range map[string]costmodel.Prediction{"lento": slow, "rifiuti": rejected, "mancante": missing} {
		if l.feasible(p, local) {
			t.Errorf("%s: ammesso", name)
		}
	}
	if !l.feasible(ps[1], local) || !l.feasible(local, local) {
		t.Error("split 0 e LOCAL devono essere ammessi")
	}
	ps[1] = rejected
	if best, ok := l.best(ps, local); !ok || best != 2 {
		t.Errorf("migliore senza split 0 = %d, %v; atteso split 1 (indice 2)", best, ok)
	}
}

func TestLatencyLeavesAnInfeasibleSplit(t *testing.T) {
	l := NewLatency(DefaultLatencyConfig())
	sigma, n := calm()
	ps := preds()
	// Split 1 (45 ms) con troppi rifiuti, split 0 (44 ms): guadagno di 1 ms,
	// sotto il margine, ma l'attuale non rispetta i vincoli.
	ps[2].RejectedPerSec = 10
	ps[1].P95MS = 44
	d := l.choose(onSplit(1, time.Minute), ps, sigma, n)
	if d.Reason != ReasonInfeasible || d.SplitPoint == nil || *d.SplitPoint != 0 {
		t.Errorf("attuale non ammesso: %+v", d)
	}
}

func TestLatencyNeedsSamples(t *testing.T) {
	l := NewLatency(DefaultLatencyConfig())
	sigma, _ := calm()
	if d := l.choose(onSplit(-1, time.Minute), preds(), sigma, []uint64{5, 2, 5, 5}); d.Reason != ReasonFewSamples {
		t.Errorf("migliore con 2 campioni: %+v", d)
	}
}

func TestLatencyUnknownCurrent(t *testing.T) {
	l := NewLatency(DefaultLatencyConfig())
	sigma, n := calm()
	ps := preds()
	ps[0].Missing = []string{"cycle"}
	if d := l.choose(onSplit(-1, time.Minute), ps, sigma, n); d.Reason != ReasonUnknown || d.Changes(onSplit(-1, 0).Policy, 0) {
		t.Errorf("attuale sconosciuto: %+v", d)
	}
}

func TestLatencyGoesLocal(t *testing.T) {
	l := NewLatency(DefaultLatencyConfig())
	sigma, n := calm()
	ps := preds()
	for i := range 3 {
		ps[i].P95MS = 90 // il server è diventato lento
	}
	d := l.choose(onSplit(0, time.Minute), ps, sigma, n)
	if d.Mode != api.ModeLocal || d.SplitPoint != nil || d.Reason != ReasonChange {
		t.Errorf("tutti gli split più lenti di LOCAL: %+v", d)
	}
}

func TestMargin(t *testing.T) {
	l := NewLatency(DefaultLatencyConfig())
	cur, cand := pred(1, 50, 20), pred(0, 40, 25)
	// base max(2, 5) = 5; ρ 0.3 sotto la soglia 0.8: fattore 1; + 1 × σ 2.
	if got, want := l.margin(cur, cand, 2), 5.0+2; math.Abs(got-want) > 1e-9 {
		t.Errorf("sotto la soglia: margine %v, atteso %v", got, want)
	}
	// ρ 0.9: × (1 + (0.9 − 0.8)/0.1) = × 2.
	cand.Rho = 0.9
	if got := l.margin(cur, cand, 0); math.Abs(got-10) > 1e-9 {
		t.Errorf("ρ 0.9: margine %v, atteso 10", got)
	}
	// ρ oltre il limite: non infinito.
	cand.Rho = 1
	if got := l.margin(cur, cand, 0); math.IsInf(got, 0) || math.Abs(got-5*(1+0.19/0.01)) > 1e-9 {
		t.Errorf("ρ = 1: margine %v", got)
	}
	// Soglia 0: il termine di coda parte da subito.
	cfg := DefaultLatencyConfig()
	cfg.QueueRhoFrom = 0
	cand.Rho = 0.3
	if got := NewLatency(cfg).margin(cur, cand, 0); math.Abs(got-5*(1+0.3/0.7)) > 1e-9 {
		t.Errorf("soglia 0: margine %v", got)
	}
	// Base minima con valori piccoli.
	if got := l.margin(pred(1, 5, 20), pred(0, 4, 25), 0); math.Abs(got-2) > 1e-9 {
		t.Errorf("base minima: %v", got)
	}
}

func TestObjectiveMean(t *testing.T) {
	cfg := DefaultLatencyConfig()
	cfg.Objective = ObjectiveMean
	l := NewLatency(cfg)
	ps := preds()
	// Per il p95 vince split 0 (40), per la media split 1.
	ps[2].TotalMS = 30
	if best, _ := l.best(ps, ps[3]); best != 2 {
		t.Errorf("con la media: migliore %d, atteso 2", best)
	}
}

func TestNoiseIgnoresASingleStep(t *testing.T) {
	l := NewLatency(DefaultLatencyConfig())
	at := time.Unix(1000, 0)
	feed := func(v float64) ([]float64, []uint64) {
		at = at.Add(time.Second)
		p := pred(0, v, 25)
		return l.observe(at, "d", []costmodel.Prediction{p})
	}
	// Valori costanti, poi un salto netto da 40 a 60 e di nuovo costanti.
	for range 10 {
		feed(40)
	}
	for range 3 {
		feed(60)
	}
	sigma, n := feed(60)
	if n[0] != 13 {
		t.Errorf("campioni %d, attesi 13 differenze", n[0])
	}
	// Una deviazione standard dei valori darebbe ~9 ms; il rumore attorno
	// all'andamento resta molto più piccolo del salto.
	if !(sigma[0] < 5) {
		t.Errorf("σ dopo un salto = %v: il salto non è rumore", sigma[0])
	}
	// Un rumore vero alterna i valori: σ cresce.
	for i := range 20 {
		feed(60 + 5*float64(i%2*2-1))
	}
	if sigma, _ := feed(55); !(sigma[0] > 3) {
		t.Errorf("σ con ±5 ms di rumore = %v", sigma[0])
	}
	// Una previsione mancante interrompe la serie.
	l.observe(at, "d", []costmodel.Prediction{{Split: 0, Missing: []string{"cycle"}}})
	_, before := feed(60)
	_, after := feed(60)
	if after[0] != before[0]+1 {
		t.Errorf("dopo un buco: %d → %d campioni", before[0], after[0])
	}
	l.Forget("d")
	if _, n := feed(60); n[0] != 0 {
		t.Errorf("dopo Forget: %d campioni", n[0])
	}
}

// Fase 4.2: allo split point del migliore, la finestra con più frame/s tra
// quelle con p95 entro P95Slack. Raspberry Pi allo split −1 (replay della
// regola c): finestra 1 p95 140 a 7.4 frame/s, finestra 2 170 a 11.8,
// finestra 3 257 a 11.7; LOCAL 636 a 1.5. In ordine: w3, w2, w1, LOCAL.
func piWindows(w2p95, w2fps float64) []costmodel.Prediction {
	w := func(win int, p95, fps float64) costmodel.Prediction {
		p := pred(-1, p95, fps)
		p.Window = win
		return p
	}
	local := pred(2, 636, 1.5)
	local.Local = true
	return []costmodel.Prediction{w(3, 257, 11.7), w(2, w2p95, w2fps), w(1, 140, 7.4), local}
}

func onWindow(w int) State {
	return State{DeviceID: "d", Last: 2, PolicyAge: time.Minute, MaxWindow: 3,
		Policy: api.Policy{Mode: api.ModeSplit, SplitPoint: ip(-1), MaxInflight: ip(w)}}
}

func TestFastestWindow(t *testing.T) {
	l := NewLatency(DefaultLatencyConfig()) // P95Slack 0.25: fino a 175 ms
	cases := []struct {
		name       string
		p95, fps   float64
		slack      float64
		wantWindow int
	}{
		{"finestra 2 entro il 25% e +59% di frame/s", 170, 11.8, 0.25, 2},
		{"finestra 2 oltre il 25%", 180, 11.8, 0.25, 1},
		{"finestra 2 con meno del 10% di frame/s in più", 170, 8.0, 0.25, 1},
		{"P95Slack 0: il p95 più basso, come prima", 170, 11.8, 0, 1},
	}
	for _, tc := range cases {
		l.cfg.P95Slack = tc.slack
		ps := piWindows(tc.p95, tc.fps)
		best, ok := l.best(ps, ps[len(ps)-1])
		if !ok || ps[best].Window != 1 {
			t.Fatalf("%s: best = %d, %v; attesa la finestra 1 (p95 più basso)", tc.name, best, ok)
		}
		if got := ps[l.fastestWindow(ps, ps[len(ps)-1], best)].Window; got != tc.wantWindow {
			t.Errorf("%s: finestra %d, attesa %d", tc.name, got, tc.wantWindow)
		}
	}
}

func TestLatencyMovesToTheFasterWindow(t *testing.T) {
	l := NewLatency(DefaultLatencyConfig())
	sigma, n := calm()
	ps := piWindows(170, 11.8)
	// Da finestra 1: più frame/s entro il 25% → finestra 2, motivo fps.
	if d := l.choose(onWindow(1), ps, sigma, n); d.MaxInflight == nil || *d.MaxInflight != 2 || d.Reason != ReasonFPS {
		t.Errorf("da finestra 1: %+v; attesa la 2 per i frame/s", d)
	}
	// Sulla 2: è la scelta.
	if d := l.choose(onWindow(2), ps, sigma, n); d.Reason != ReasonBest {
		t.Errorf("da finestra 2: %+v; attesa best", d)
	}
	// Da finestra 3: p95 257 oltre 175 + margine → finestra 2, motivo latency.
	if d := l.choose(onWindow(3), ps, sigma, n); d.MaxInflight == nil || *d.MaxInflight != 2 || d.Reason != ReasonChange {
		t.Errorf("da finestra 3: %+v; attesa la 2 per il p95", d)
	}
	// Da LOCAL: lo split point lo decide il p95 (140 contro 636), la finestra i frame/s.
	if d := l.choose(State{DeviceID: "d", Last: 2, PolicyAge: time.Minute, MaxWindow: 3}, ps, sigma, n); d.Mode != api.ModeSplit || *d.MaxInflight != 2 || d.Reason != ReasonChange {
		t.Errorf("da LOCAL: %+v; attesa split −1 finestra 2", d)
	}
}

// Isteresi sulla finestra: entro P95Slack e senza abbastanza frame/s in più
// non si cambia, anche se un'altra finestra ha il p95 più basso.
func TestLatencyKeepsAWindowWithinSlack(t *testing.T) {
	l := NewLatency(DefaultLatencyConfig())
	sigma, n := calm()
	ps := piWindows(160, 11.8)
	ps[0].P95MS, ps[0].TotalMS, ps[0].FramesPerSec = 172, 172, 12.5 // w3: +6% di frame/s sulla 2
	if d := l.choose(onWindow(3), ps, sigma, n); d.Reason != ReasonHysteresis || d.Changes(onWindow(3).Policy, 3) {
		t.Errorf("da finestra 3 entro il 25%%: %+v; attesa hysteresis", d)
	}
}
