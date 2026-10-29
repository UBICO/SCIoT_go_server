// Package offload contiene gli algoritmi di offloading: a ogni heartbeat di
// un device decidono la sua policy (LOCAL o SPLIT a uno split point) dallo
// stato che il server misura. Lo stesso codice gira dal vivo nel server
// (Fase 3.6) e nel replay di una registrazione (cmd/sciot-replay algo).
//
// Un algoritmo si adatta alle metriche che esistono: non ne chiede di nuove
// e non cambia come si misurano.
package offload

import (
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

// Decision è la scelta di un algoritmo.
type Decision struct {
	// Mode e SplitPoint della policy scelta; la versione la assegna il
	// server quando la applica.
	Mode       api.ExecutionMode
	SplitPoint *int
	Reason     string
	// Predictions sono le previsioni con cui ha scelto, una per split point
	// (−1..Last, Last = LOCAL), se l'algoritmo ne usa.
	Predictions []costmodel.Prediction
}

// Changes dice se la decisione cambia la policy attuale.
func (d Decision) Changes(current api.Policy) bool {
	if d.Mode != current.Mode {
		return true
	}
	if d.Mode != api.ModeSplit {
		return false
	}
	return d.SplitPoint == nil || current.SplitPoint == nil || *d.SplitPoint != *current.SplitPoint
}

// Algorithm decide la policy di un device.
type Algorithm interface {
	Name() string
	Decide(now time.Time, s State) Decision
}

// PredictAll prevede il costo di ogni split point −1..Last (Last = LOCAL).
func PredictAll(s State) []costmodel.Prediction {
	out := make([]costmodel.Prediction, 0, s.Last+2)
	for k := -1; k <= s.Last; k++ {
		out = append(out, costmodel.Predict(s.Inputs, k, s.Last))
	}
	return out
}

// Static è l'algoritmo di riferimento: non cambia mai la policy. Le
// previsioni servono al replay, per confrontarlo con gli altri.
type Static struct{}

func (Static) Name() string { return "static" }

func (Static) Decide(now time.Time, s State) Decision {
	return Decision{
		Mode:        s.Policy.Mode,
		SplitPoint:  s.Policy.SplitPoint,
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
			SplitPoint: p.Split, Local: p.Local,
			TotalMS: finite(p.TotalMS), DeviceMS: p.DeviceMS, UploadMS: p.UploadMS,
			WaitMS: finite(p.WaitMS), ServiceMS: p.ServiceMS, Rho: finite(p.Rho),
			Saturated: p.Saturated, Missing: p.Missing,
		}
	}
	return out
}
