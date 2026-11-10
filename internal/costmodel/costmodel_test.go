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

// inputs: modello con 3 segmenti (0..2), device che li ha misurati tutti con
// A = 2 ms di ciclo, finestra di 3 frame, rete 8 Mbit/s (1e6 B/s) con 2 ms di
// latenza, server con 2 slot vuoti.
func inputs() Inputs {
	bw, lat := 1e6, 2.0
	return Inputs{
		DeviceMS:    []*float64{f(10), f(20), f(30)},
		Network:     &netmeasure.State{BandwidthBps: &bw, LatencyMS: &lat},
		InputBytes:  map[int]int{-1: 50_000, 0: 20_000, 1: 10_000},
		ServiceMS:   map[int]float64{-1: 12, 0: 8, 1: 4},
		Queue:       Queue{Slots: 2, CA: 1, CS: 1},
		MaxInflight: 3,
		CycleMS:     f(2),
	}
}

// localhost: invio istantaneo (rete non limitante, latenza 0).
func localhost(in Inputs) Inputs {
	zero := 0.0
	in.Network = &netmeasure.State{LatencyMS: &zero, NotLimiting: true}
	return in
}

func TestPercentile(t *testing.T) {
	v := []float64{5, 1, 4, 2, 3}
	if got := Percentile(v, 0.95); got != 5 {
		t.Errorf("p95 di 1..5 = %v, atteso 5", got)
	}
	if got := Percentile(v, 0.5); got != 3 {
		t.Errorf("p50 di 1..5 = %v, atteso 3", got)
	}
	if got := Percentile(v, 0); got != 1 {
		t.Errorf("p0 = %v, atteso il minimo", got)
	}
	if v[0] != 5 || Percentile(nil, 0.95) != 0 {
		t.Error("non deve ordinare l'originale; senza valori 0")
	}
}

func TestPredictLocal(t *testing.T) {
	p := Predict(inputs(), 2, 2)
	if !p.Local || p.DeviceMS != 60 || p.TotalMS != 60 || p.LocalMS != 60 || p.UploadMS != 0 || p.WaitMS != 0 ||
		p.ServerShare != 0 || len(p.Missing) != 0 {
		t.Errorf("LOCAL = %+v", p)
	}
	// Ciclo: A + tutto il modello = 62 ms.
	if !near(p.FramesPerSec, 1000.0/62) {
		t.Errorf("frame/s = %v, attesi %v", p.FramesPerSec, 1000.0/62)
	}
}

// within confronta a meno di una tolleranza relativa: la simulazione conta
// frame interi in 5 s virtuali.
func within(got, want, rel float64) bool { return math.Abs(got-want) <= rel*math.Abs(want) }

func TestSimAllToServer(t *testing.T) {
	in := inputs()
	in.Queue.CS = 0 // servizio costante: risultato esatto
	p := Predict(in, 1, 2)
	// Invio in background: ciclo A + prefisso = 2 + 30 = 32 ms → 31.2 frame/s,
	// tutti al server. Il link (10 ms a frame) si libera prima del frame dopo,
	// 3 frame in volo bastano, S = 4 ms non si accoda.
	if p.Bound != BoundDevice || p.ServerShare != 1 || !within(p.FramesPerSec, 1000.0/32, 0.02) ||
		p.RejectedPerSec != 0 || p.WindowFullPerSec != 0 || p.WaitMS != 0 {
		t.Errorf("split 1 = %+v: attesi tutti i frame al server", p)
	}
	// Latenza: prefisso 30 + invio 12 + S 4 + ritorno 2 (latenza di rete).
	if !within(p.ServerMS, 48, 1e-9) || p.TotalMS != p.ServerMS || !within(p.P95MS, 48, 1e-9) {
		t.Errorf("latenza %v, totale %v, p95 %v; attesi 48 ms", p.ServerMS, p.TotalMS, p.P95MS)
	}
}

// Fase 3.9, Raspberry Pi allo split −1: suffisso 636 ms, 1.2 MB in 85 ms
// sul link, S 50 ms. Il client manda due o tre frame in pochi ms (invio in
// background), il successivo trova la finestra piena e finisce in locale;
// intanto il secondo frame aspetta il primo sul link. Prima della correzione
// il client simulato restava fermo durante l'invio, la finestra non si
// riempiva mai e le finestre 2 e 3 erano uguali.
func TestSimBackgroundSendOnSlowDevice(t *testing.T) {
	bw, lat := 1228800/0.085, 2.0 // byte/s: 85 ms per 1.2 MB
	in := inputs()
	in.DeviceMS = []*float64{f(200), f(200), f(236)}
	in.Network = &netmeasure.State{BandwidthBps: &bw, LatencyMS: &lat}
	in.InputBytes[-1] = 1228800
	in.ServiceMS[-1] = 50
	in.Queue.CS = 0
	for _, c := range []struct {
		window       int
		fps, share   float64
		serverMS     float64 // media dei serviti
		windowFullPS float64
	}{
		// Ciclo: un frame a finestra piena (636) + A = 2 per frame. Serviti,
		// nati ogni 2 ms: link 85 + latenza 2 + S 50 + ritorno 2 = 139, più la
		// coda sul link (83 per il secondo, 166 per il terzo).
		{2, 3000.0 / 642, 2.0 / 3, (139 + 222) / 2.0, 1000.0 / 642},
		{3, 4000.0 / 644, 3.0 / 4, (139 + 222 + 305) / 3.0, 1000.0 / 644},
	} {
		in.MaxInflight = c.window
		p := Predict(in, -1, 2)
		// Frame interi in 5 s virtuali: un frame in più o in meno è 0.2 frame/s.
		if p.Bound != BoundWindow || math.Abs(p.FramesPerSec-c.fps) > 0.2 || !within(p.ServerShare, c.share, 0.03) ||
			!within(p.WindowFullPerSec, c.windowFullPS, 0.15) || p.RejectedPerSec != 0 {
			t.Errorf("finestra %d = %+v: attesi %.2f frame/s, quota %.2f", c.window, p, c.fps, c.share)
		}
		if !within(p.ServerMS, c.serverMS, 0.03) || !within(p.P95MS, 636, 0.01) {
			t.Errorf("finestra %d: serviti %.0f ms (attesi %.0f), p95 %.0f (atteso 636: i frame locali)",
				c.window, p.ServerMS, c.serverMS, p.P95MS)
		}
	}
}

// Lo stesso Raspberry Pi con la regola della finestra piena (capability
// wait_for_window): aspettare un posto (~150 ms) costa meno che finire il
// frame (636 ms), quindi a regime nessun frame finisce sul device. Il limite
// diventa il link, un frame ogni 85 ms; con la finestra 3 i frame aspettano
// di più sul link, a pari frame/s: la finestra 2 è meglio.
func TestSimSlowDeviceWaitsForAPlace(t *testing.T) {
	bw, lat := 1228800/0.085, 2.0
	in := inputs()
	in.DeviceMS = []*float64{f(200), f(200), f(236)}
	in.Network = &netmeasure.State{BandwidthBps: &bw, LatencyMS: &lat}
	in.InputBytes[-1] = 1228800
	in.ServiceMS[-1] = 50
	in.Queue.CS = 0
	in.WaitForWindow = true
	p95 := map[int]float64{}
	for _, window := range []int{2, 3} {
		in.MaxInflight = window
		p := Predict(in, -1, 2)
		if !within(p.FramesPerSec, 1000.0/85, 0.1) || p.WindowFullPerSec > 0.5 || p.RejectedPerSec != 0 {
			t.Errorf("finestra %d = %+v: attesi ~11.8 frame/s (link), nessun frame finito sul device", window, p)
		}
		if !(p.P95MS < 320) {
			t.Errorf("finestra %d: p95 %.0f ms, atteso sotto 320 (nessun frame da 636)", window, p.P95MS)
		}
		p95[window] = p.P95MS
	}
	if !(p95[3] > p95[2]) {
		t.Errorf("p95 finestra 3 %.0f, finestra 2 %.0f: con la 3 i frame aspettano di più sul link", p95[3], p95[2])
	}
}

func TestSimSelfQueueing(t *testing.T) {
	// Il caso misurato (docs/COSTMODEL.md): split −1 in localhost, S = 37 ms
	// su 2 slot, finestra di 3. Il terzo frame in volo trova gli slot occupati
	// dai frame dello stesso device: attesa prevista 37 + S 37 > scadenza 62,
	// rifiutato; finirlo sul device blocca il device per 62 ms.
	in := localhost(inputs())
	in.ServiceMS[-1] = 37
	in.DeviceMS = []*float64{f(20), f(20), f(22)} // suffisso 62 ms
	in.Queue.CS = 0
	p := Predict(in, -1, 2)
	if p.Bound != BoundDeadline || p.RejectedPerSec == 0 || p.WaitMS != 0 || !(p.ServerShare > 0.3 && p.ServerShare < 0.7) {
		t.Errorf("split −1 = %+v: attesi rifiuti sistematici e circa metà dei frame al server", p)
	}
	// Gli slot restano vuoti mentre il device finisce i rifiutati: il server
	// lavora molto sotto la sua capacità (2 / 37 ms ≈ 54 frame/s).
	if !(p.Rho < 0.5) || !(p.ServerFramesPerSec < 30) {
		t.Errorf("ρ %v, frame/s al server %v: attesa una capacità poco usata", p.Rho, p.ServerFramesPerSec)
	}
}

func TestSimWindowFull(t *testing.T) {
	// Finestra di 1 frame con un servizio lungo e la scadenza larga: nessun
	// rifiuto, ma il device trova spesso la finestra piena.
	in := localhost(inputs())
	in.MaxInflight = 1
	in.ServiceMS[-1] = 30
	in.Queue.CS = 0
	p := Predict(in, -1, 2)
	if p.Bound != BoundWindow || p.WindowFullPerSec == 0 || p.RejectedPerSec != 0 || !(p.ServerShare < 1) {
		t.Errorf("finestra 1 = %+v: attesa la finestra piena come causa", p)
	}
}

func TestSimCountsTheOthers(t *testing.T) {
	in := inputs()
	alone := Predict(in, 1, 2)
	in.Queue.OthersLambda = 300          // frame/s degli altri device
	in.Queue.OthersErlangs = 300 * 0.004 // con S = 4 ms: 1.2 erlang
	busy := Predict(in, 1, 2)
	if !(busy.WaitMS > alone.WaitMS) || !(busy.Rho > alone.Rho+0.5) || busy.Saturated {
		t.Errorf("con gli altri: attesa %v (da solo %v), ρ %v (da solo %v)", busy.WaitMS, alone.WaitMS, busy.Rho, alone.Rho)
	}
}

func TestSimSaturated(t *testing.T) {
	// Gli altri chiedono più dei 2 slot (2.4 erlang): la loro coda cresce, il
	// server rifiuta i frame del device, che li finisce tutti da sé.
	in := inputs()
	in.Queue.OthersLambda, in.Queue.OthersErlangs = 600, 2.4
	p := Predict(in, 1, 2)
	if !p.Saturated || p.ServerShare != 0 || p.Bound != BoundDeadline || !math.IsNaN(p.ServerMS) || math.IsNaN(p.TotalMS) {
		t.Errorf("altri a 2.4 erlang su 2 slot = %+v: attesi saturazione e tutto sul device", p)
	}
}

func TestSimIsReproducible(t *testing.T) {
	in := inputs()
	in.Queue.OthersLambda, in.Queue.OthersErlangs = 200, 0.8
	a, b := Predict(in, 0, 2), Predict(in, 0, 2)
	if a.TotalMS != b.TotalMS || a.P95MS != b.P95MS || a.ServerFramesPerSec != b.ServerFramesPerSec {
		t.Errorf("stessi ingressi, risultati diversi: %+v / %+v", a, b)
	}
}

func TestPredictMissingInputs(t *testing.T) {
	in := inputs()
	in.DeviceMS[1] = nil
	in.Network = nil
	delete(in.ServiceMS, 1)
	in.CycleMS = nil
	in.MaxInflight = 0
	p := Predict(in, 1, 2)
	for _, want := range []string{"device segment 1", "network", "service", "cycle", "max_inflight"} {
		if !slices.Contains(p.Missing, want) {
			t.Errorf("Missing = %v, manca %q", p.Missing, want)
		}
	}
	// Il suffisso è un ingresso: un segmento dopo lo split mai misurato.
	in = inputs()
	in.DeviceMS[2] = nil
	if p := Predict(in, 1, 2); !slices.Contains(p.Missing, "device segment 2") {
		t.Errorf("suffisso: Missing = %v", p.Missing)
	}
	if p := Predict(Inputs{InputBytes: map[int]int{1: 1}}, 1, 2); !slices.Contains(p.Missing, "slots") {
		t.Errorf("senza slot: Missing = %v", p.Missing)
	}
	// LOCAL senza i tempi del device e senza A (device tenuto in SPLIT
	// dall'admin, Fase 3.9): frame/s 0, non +Inf, che il JSON non accetta.
	if p := Predict(Inputs{DeviceMS: make([]*float64, 3)}, 2, 2); p.FramesPerSec != 0 || len(p.Missing) == 0 {
		t.Errorf("LOCAL senza tempi: frame/s %v, Missing %v", p.FramesPerSec, p.Missing)
	}
}
