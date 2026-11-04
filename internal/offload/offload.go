// Package offload contiene gli algoritmi di offloading: a ogni heartbeat di
// un device decidono la sua policy (LOCAL o SPLIT a uno split point) dallo
// stato che il server misura. Lo stesso codice gira dal vivo nel server
// (Fase 3.6) e nel replay di una registrazione (cmd/sciot-replay algo).
//
// Un algoritmo si adatta alle metriche che esistono: non ne chiede di nuove
// e non cambia come si misurano.
package offload

import (
	"fmt"
	"math"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/costmodel"
	"github.com/UBICO/SCIoT_go_server/internal/record"
)

// State è ciò che un algoritmo sa di un device al suo heartbeat.
type State struct {
	DeviceID string
	ModelKey string
	Last     int // indice dell'ultimo segmento: SPLIT a −1..Last−1, LOCAL = Last

	// Policy è quella attuale del server per il device; PolicyAge da quando
	// è in vigore (per la durata minima, Fase 3.6).
	Policy    api.Policy
	PolicyAge time.Duration

	// FramesPerSec sono i frame che il device ha completato al secondo nella
	// finestra dell'heartbeat (telemetria).
	FramesPerSec float64

	// MaxWindow è il massimo della finestra del device (max_inflight
	// dell'hello, stream.max_inflight); 0 = sconosciuto (senza stream).
	MaxWindow int

	// Inputs sono gli ingressi del modello di costo, con Queue che descrive
	// il server SENZA questo device (Others).
	Inputs costmodel.Inputs
}

// Split è lo split point della policy: Last per LOCAL.
func (s State) Split() int {
	if s.Policy.Mode == api.ModeSplit && s.Policy.SplitPoint != nil {
		return *s.Policy.SplitPoint
	}
	return s.Last
}

// Window è la finestra della policy: 0 in LOCAL, MaxWindow se la policy non
// ne ha una.
func (s State) Window() int {
	if s.Policy.Mode != api.ModeSplit {
		return 0
	}
	return window(s.Policy.MaxInflight, s.MaxWindow)
}

// window è la finestra effettiva: w, o max se w è nil.
func window(w *int, max int) int {
	if w == nil {
		return max
	}
	return *w
}

// Decision è la scelta di un algoritmo.
type Decision struct {
	// Mode, SplitPoint e MaxInflight (la finestra, nil = il massimo) della
	// policy scelta; la versione la assegna il server quando la applica.
	Mode        api.ExecutionMode
	SplitPoint  *int
	MaxInflight *int
	Reason      string
	// Predictions sono le previsioni con cui ha scelto, una per coppia
	// (split point, finestra) e una per LOCAL (PredictAll), se l'algoritmo ne
	// usa.
	Predictions []costmodel.Prediction
	// Pending è il carico prenotato da altri device e non ancora visibile
	// nella misura, aggiunto agli altri prima di prevedere (reserve.go).
	Pending Load
}

// Split è lo split point della decisione: last per LOCAL.
func (d Decision) Split(last int) int {
	if d.Mode == api.ModeSplit && d.SplitPoint != nil {
		return *d.SplitPoint
	}
	return last
}

// Window è la finestra della decisione: 0 in LOCAL, maxWindow se nil.
func (d Decision) Window(maxWindow int) int {
	if d.Mode != api.ModeSplit {
		return 0
	}
	return window(d.MaxInflight, maxWindow)
}

// Changes dice se la decisione cambia la policy attuale: modo, split point o
// finestra. maxWindow è il massimo della finestra: una finestra nil vale
// maxWindow, quindi nil e maxWindow sono la stessa finestra.
func (d Decision) Changes(current api.Policy, maxWindow int) bool {
	if d.Mode != current.Mode {
		return true
	}
	if d.Mode != api.ModeSplit {
		return false
	}
	if d.SplitPoint == nil || current.SplitPoint == nil || *d.SplitPoint != *current.SplitPoint {
		return true
	}
	return window(d.MaxInflight, maxWindow) != window(current.MaxInflight, maxWindow)
}

// Algorithm decide la policy di un device.
type Algorithm interface {
	Name() string
	Decide(now time.Time, s State) Decision
}

// PredictAll prevede il costo di ogni coppia (split point, finestra): split
// −1..Last−1, finestra MaxWindow..1, e per ultimo LOCAL (Window 0).
//
// L'ordine conta: a parità di valore l'algoritmo sceglie la previsione che
// viene dopo (latency.go, best), cioè lo split più alto e, a pari split, la
// finestra più piccola: meno carico sul server.
//
// Con MaxWindow sconosciuto (0) una sola previsione per split, con finestra
// 0: il modello di costo la segna come mancante ("max_inflight").
func PredictAll(s State) []costmodel.Prediction {
	var out []costmodel.Prediction
	lowest := min(s.MaxWindow, 1) // 0 solo con MaxWindow sconosciuto: una sola finestra, la 0
	for k := -1; k < s.Last; k++ {
		for w := s.MaxWindow; w >= lowest; w-- {
			in := s.Inputs
			in.MaxInflight = w
			p := costmodel.Predict(in, k, s.Last)
			p.Window = w
			out = append(out, p)
		}
	}
	return append(out, costmodel.Predict(s.Inputs, s.Last, s.Last))
}

// IndexOf è l'indice della previsione per lo split point split e la
// finestra window (ignorata per LOCAL); −1 se non c'è.
func IndexOf(preds []costmodel.Prediction, split, window int) int {
	for i, p := range preds {
		if p.Split == split && (p.Local || p.Window == window) {
			return i
		}
	}
	return -1
}

// Names sono gli algoritmi disponibili (offload.algorithm in config.json).
func Names() []string { return []string{Static{}.Name(), "latency"} }

// New crea l'algoritmo name; cfg vale per "latency".
func New(name string, cfg LatencyConfig) (Algorithm, error) {
	switch name {
	case Static{}.Name():
		return Static{}, nil
	case "latency":
		return NewLatency(cfg), nil
	}
	return nil, fmt.Errorf("unknown algorithm %q (known: %v)", name, Names())
}

// Forgetter è un algoritmo che ricorda qualcosa dei device: il server lo
// avvisa quando una sessione finisce.
type Forgetter interface {
	Forget(deviceID string)
}

// Static è l'algoritmo di riferimento: non cambia mai la policy. Le
// previsioni servono al replay, per confrontarlo con gli altri.
type Static struct{}

func (Static) Name() string { return "static" }

func (Static) Decide(now time.Time, s State) Decision {
	return Decision{
		Mode:        s.Policy.Mode,
		SplitPoint:  s.Policy.SplitPoint,
		MaxInflight: s.Policy.MaxInflight,
		Reason:      "static",
		Predictions: PredictAll(s),
	}
}

// Others è il carico del server senza il device: dal carico totale misurato
// (utilization ρ su slots slot) si toglie il suo contributo, i suoi frame al
// secondo per il suo S, se è in SPLIT.
//
//   - totalLambda: frame ammessi al secondo dal dispatcher (tutti i device);
//   - rho, slots: utilizzo misurato e slot;
//   - fps: frame al secondo del device; serviceMS: S del suo split point;
//   - inSplit: il device è in SPLIT (in LOCAL non carica il server).
//
// Restituisce frame al secondo ed erlang degli altri, mai sotto zero: le
// misure vengono da finestre diverse e la differenza può uscire negativa.
func Others(totalLambda, rho float64, slots int, fps, serviceMS float64, inSplit bool) (lambda, erlangs float64) {
	lambda, erlangs = totalLambda, rho*float64(slots)
	if inSplit {
		lambda -= fps
		erlangs -= fps * serviceMS / 1000
	}
	return max(0, lambda), max(0, erlangs)
}

// FramesPerSec sono i frame al secondo di una finestra: count inferenze in
// gapMS millisecondi (since_last_seen_ms). 0 se la finestra non ha durata.
func FramesPerSec(count int, gapMS float64) float64 {
	if gapMS <= 0 || count <= 0 {
		return 0
	}
	return float64(count) / (gapMS / 1000)
}

// CycleMS è A, il tempo di un ciclo del device oltre al calcolo dei segmenti
// e all'invio, dalla finestra di un heartbeat (gapMS ms, telemetria t):
//
//	A = (gapMS − Σ segment_ms·segment_count − inviati·uploadMS) / inference_count
//
// Il calcolo viene dai tempi per segmento e non da inference_time_ms, che
// non contiene i suffissi dei frame finiti sul device. In SPLIT ogni frame
// finito sul device esegue l'ultimo segmento: inviati = inference_count −
// segment_count[last]; in LOCAL nessuno. uploadMS è l'invio allo split point
// applicato (ok = false se serve e non è noto). Mai sotto zero: finestra e
// tempi vengono da orologi diversi.
//
// shortcut: uploadMS è la stima della rete, non l'invio misurato di quei
// frame; misurarlo nel client se su rete vera A risulta rumoroso.
func CycleMS(t api.Telemetry, gapMS float64, split bool, last int, uploadMS float64, uploadOK bool) (float64, bool) {
	n := t.InferenceCount
	if n <= 0 || gapMS <= 0 || len(t.SegmentMS) != last+1 || len(t.SegmentCount) != last+1 {
		return 0, false
	}
	var compute float64
	for i, c := range t.SegmentCount {
		if c == 0 {
			continue
		}
		if t.SegmentMS[i] == nil {
			return 0, false
		}
		compute += *t.SegmentMS[i] * float64(c)
	}
	sent := 0
	if split {
		sent = max(0, n-t.SegmentCount[last])
	}
	if sent > 0 && !uploadOK {
		return 0, false
	}
	return max(0, (gapMS-compute-float64(sent)*uploadMS)/float64(n)), true
}

// DecisionRecord è la decisione d sullo stato s come evento della
// registrazione (senza intestazione: la mette il Recorder, o il replay con
// l'istante dell'heartbeat).
func DecisionRecord(s State, d Decision, algorithm string) *record.Decision {
	return &record.Decision{
		DeviceID: s.DeviceID, Algorithm: algorithm, Current: s.Policy,
		Mode: d.Mode, SplitPoint: d.SplitPoint, MaxInflight: d.MaxInflight,
		Changed: d.Changes(s.Policy, s.MaxWindow), Reason: d.Reason,
		PendingPerSec: d.Pending.PerSec, PendingErlangs: d.Pending.Erlangs,
		Predictions: RecordPredictions(d.Predictions),
	}
}

// RecordPredictions converte le previsioni per la registrazione: il JSON non
// rappresenta +Inf, quindi un valore infinito diventa null (Saturated lo
// dice).
func RecordPredictions(ps []costmodel.Prediction) []record.Prediction {
	finite := func(v float64) *float64 {
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return nil
		}
		return &v
	}
	out := make([]record.Prediction, len(ps))
	for i, p := range ps {
		out[i] = record.Prediction{
			SplitPoint: p.Split, Window: p.Window, Local: p.Local,
			TotalMS: finite(p.TotalMS), DeviceMS: p.DeviceMS, UploadMS: p.UploadMS,
			WaitMS: finite(p.WaitMS), ServiceMS: p.ServiceMS, SuffixMS: p.SuffixMS,
			ServerMS: finite(p.ServerMS), LocalMS: p.LocalMS,
			FramesPerSec: p.FramesPerSec, ServerFramesPerSec: p.ServerFramesPerSec,
			ServerShare: p.ServerShare, Bound: p.Bound, P95MS: p.P95MS,
			RejectedPerSec: p.RejectedPerSec, WindowFullPerSec: p.WindowFullPerSec, Rho: finite(p.Rho),
			Saturated: p.Saturated, Missing: p.Missing,
		}
	}
	return out
}
