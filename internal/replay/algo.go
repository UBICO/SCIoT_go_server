package replay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/costmodel"
	"github.com/UBICO/SCIoT_go_server/internal/offload"
	"github.com/UBICO/SCIoT_go_server/internal/record"
)

// Il replay degli algoritmi è a ciclo aperto: le decisioni non cambiano ciò
// che la registrazione dice dopo (un device spostato dall'algoritmo resta,
// nei dati, dove era). Confronta gli algoritmi decisione per decisione; il
// loro effetto vero lo misura il benchmark con il sistema vero (Fase 3.9).

// Step è una decisione dell'algoritmo a un heartbeat registrato.
type Step struct {
	Heartbeat record.Heartbeat
	State     offload.State
	Decision  offload.Decision
	// PredictedActualMS è la latenza prevista dal modello per la policy che
	// il device applicava, PredictedChosenMS per quella scelta (NaN se il
	// modello non la sa prevedere: ingressi mancanti o coda satura).
	// MeasuredActualMS è quella misurata per la policy applicata nella
	// finestra dell'heartbeat (NaN senza misure).
	PredictedActualMS, PredictedChosenMS, MeasuredActualMS float64
}

// Algo riesegue alg su ogni heartbeat di r (con i campi dello stato: dalla
// Fase 3.5) e restituisce le decisioni in ordine.
func Algo(r io.Reader, alg offload.Algorithm) ([]Step, error) {
	hbs, frames, err := readForAlgo(r)
	if err != nil {
		return nil, err
	}
	since := map[string]float64{}  // device → t_ms da cui vale la policy
	version := map[string]uint64{} // device → ultima versione vista
	var out []Step
	for _, hb := range hbs {
		if hb.NumSegments == 0 {
			continue // registrazione precedente alla Fase 3.5: stato incompleto
		}
		if v, ok := version[hb.DeviceID]; !ok || v != hb.ServerPolicy.PolicyVersion {
			version[hb.DeviceID] = hb.ServerPolicy.PolicyVersion
			since[hb.DeviceID] = hb.TMS
		}
		st := stateOf(hb, hb.TMS-since[hb.DeviceID])
		d := alg.Decide(hb.TS, st)
		preds := d.Predictions
		if preds == nil {
			preds = offload.PredictAll(st)
		}
		step := Step{Heartbeat: hb, State: st, Decision: d,
			PredictedActualMS: predicted(preds, st.Split()),
			PredictedChosenMS: predicted(preds, splitOf(d, st.Last)),
			MeasuredActualMS:  measured(hb, frames[hb.DeviceID]),
		}
		out = append(out, step)
	}
	return out, nil
}

// stateOf costruisce lo stato di un device da un suo heartbeat registrato.
func stateOf(hb record.Heartbeat, policyAgeMS float64) offload.State {
	st := offload.State{
		DeviceID:     hb.DeviceID,
		ModelKey:     hb.ModelKey,
		Last:         hb.NumSegments - 1,
		Policy:       hb.ServerPolicy,
		PolicyAge:    time.Duration(policyAgeMS * float64(time.Millisecond)),
		FramesPerSec: offload.FramesPerSec(hb.Telemetry.InferenceCount, hb.SinceLastSeenMS),
	}
	in := costmodel.Inputs{
		DeviceMS:   make([]*float64, len(hb.DeviceSegments)),
		Network:    hb.Network,
		InputBytes: map[int]int{},
		ServiceMS:  map[int]float64{},
	}
	for i, e := range hb.DeviceSegments {
		if e != nil {
			m := e.Mean
			in.DeviceMS[i] = &m
		}
	}
	for _, s := range hb.InputBytes {
		in.InputBytes[s.SplitPoint] = s.InputBytes
	}
	var cs, weight float64
	for _, s := range hb.Service {
		in.ServiceMS[s.SplitPoint] = s.MeanMS
		if s.MeanMS > 0 {
			cs += s.StdMS / s.MeanMS * float64(s.Samples)
			weight += float64(s.Samples)
		}
	}
	if q := hb.Queue; q != nil {
		in.Queue.Slots = q.Slots
		if weight > 0 {
			in.Queue.CS = cs / weight // cs medio, pesato sui campioni
		}
		if q.InterarrivalMS != nil && q.InterarrivalStdMS != nil && *q.InterarrivalMS > 0 {
			in.Queue.CA = *q.InterarrivalStdMS / *q.InterarrivalMS
		}
		rho := 0.0
		if q.Utilization != nil {
			rho = *q.Utilization
		}
		split := st.Split()
		inSplit := split < st.Last
		in.Queue.OthersLambda, in.Queue.OthersErlangs = offload.Others(
			q.AdmittedPerSec, rho, q.Slots, st.FramesPerSec, in.ServiceMS[split], inSplit)
	}
	st.Inputs = in
	return st
}

func splitOf(d offload.Decision, last int) int {
	if d.Mode == api.ModeSplit && d.SplitPoint != nil {
		return *d.SplitPoint
	}
	return last
}

// predicted è la latenza prevista allo split point split (NaN se non
// prevedibile).
func predicted(ps []costmodel.Prediction, split int) float64 {
	for _, p := range ps {
		if p.Split == split {
			if len(p.Missing) > 0 || p.Saturated || math.IsInf(p.TotalMS, 0) {
				return math.NaN()
			}
			return p.TotalMS
		}
	}
	return math.NaN()
}

// measured è la latenza misurata nella finestra dell'heartbeat per la
// policy applicata: in LOCAL il tempo di calcolo del device; in SPLIT il
// prefisso del device più, per i frame del device eseguiti dal server nella
// finestra, latenza di rete + ricezione + tempo del server. I frame
// completati dal device (rifiutati) non contano: il server non li vede.
func measured(hb record.Heartbeat, frames []record.Frame) float64 {
	if hb.Telemetry.InferenceTimeMS == nil {
		return math.NaN()
	}
	device := *hb.Telemetry.InferenceTimeMS
	if hb.Applied.Mode != api.ModeSplit {
		return device
	}
	from := hb.TMS - hb.SinceLastSeenMS
	var sum float64
	var n int
	for _, f := range frames {
		if f.TMS > from && f.TMS <= hb.TMS && f.Outcome == record.OutcomeResult && f.Timing != nil && f.Timing.ServerMS != nil {
			sum += f.ReceiveMS + *f.Timing.ServerMS
			n++
		}
	}
	if n == 0 {
		return math.NaN()
	}
	latency := 0.0
	if hb.Network != nil && hb.Network.LatencyMS != nil {
		latency = *hb.Network.LatencyMS
	}
	return device + latency + sum/float64(n)
}

// readForAlgo legge gli heartbeat in ordine e i frame per device.
func readForAlgo(r io.Reader) ([]record.Heartbeat, map[string][]record.Frame, error) {
	var hbs []record.Heartbeat
	frames := map[string][]record.Frame{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for line := 1; sc.Scan(); line++ {
		var k struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(sc.Bytes(), &k); err != nil {
			return nil, nil, fmt.Errorf("line %d: %w", line, err)
		}
		switch k.Kind {
		case "heartbeat":
			var hb record.Heartbeat
			if err := json.Unmarshal(sc.Bytes(), &hb); err != nil {
				return nil, nil, fmt.Errorf("line %d: %w", line, err)
			}
			hbs = append(hbs, hb)
		case "frame":
			var f record.Frame
			if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
				return nil, nil, fmt.Errorf("line %d: %w", line, err)
			}
			frames[f.DeviceID] = append(frames[f.DeviceID], f)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	slices.SortStableFunc(hbs, func(a, b record.Heartbeat) int { return cmpFloat(a.TMS, b.TMS) })
	return hbs, frames, nil
}

// AlgoSummary riassume il replay di un device.
type AlgoSummary struct {
	DeviceID   string
	Heartbeats int
	// Differ: decisioni diverse dalla policy applicata; Changes: cambi di
	// policy che l'algoritmo avrebbe fatto (oscillazioni).
	Differ, Changes int
	// Medie sugli heartbeat in cui il valore esiste (n tra parentesi nel
	// riassunto stampato).
	PredictedActualMS, PredictedChosenMS, MeasuredActualMS float64
	NPredActual, NPredChosen, NMeasured                    int
}

// Summarize riassume le decisioni per device, in ordine di device.
func Summarize(steps []Step) []AlgoSummary {
	by := map[string]*AlgoSummary{}
	var ids []string
	last := map[string]offload.Decision{}
	for _, s := range steps {
		id := s.Heartbeat.DeviceID
		a := by[id]
		if a == nil {
			a = &AlgoSummary{DeviceID: id}
			by[id] = a
			ids = append(ids, id)
		}
		a.Heartbeats++
		if s.Decision.Changes(s.State.Policy) {
			a.Differ++
		}
		if prev, ok := last[id]; ok && (prev.Mode != s.Decision.Mode || !sameSplit(prev.SplitPoint, s.Decision.SplitPoint)) {
			a.Changes++
		}
		last[id] = s.Decision
		addIfFinite(&a.PredictedActualMS, &a.NPredActual, s.PredictedActualMS)
		addIfFinite(&a.PredictedChosenMS, &a.NPredChosen, s.PredictedChosenMS)
		addIfFinite(&a.MeasuredActualMS, &a.NMeasured, s.MeasuredActualMS)
	}
	slices.Sort(ids)
	out := make([]AlgoSummary, 0, len(ids))
	for _, id := range ids {
		a := by[id]
		div(&a.PredictedActualMS, a.NPredActual)
		div(&a.PredictedChosenMS, a.NPredChosen)
		div(&a.MeasuredActualMS, a.NMeasured)
		out = append(out, *a)
	}
	return out
}

func sameSplit(a, b *int) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

func addIfFinite(sum *float64, n *int, v float64) {
	if !math.IsNaN(v) && !math.IsInf(v, 0) {
		*sum += v
		*n++
	}
}

func div(sum *float64, n int) {
	if n > 0 {
		*sum /= float64(n)
	} else {
		*sum = math.NaN()
	}
}

// DecisionEvent è la decisione come riga della registrazione (stesso
// formato che il server scriverà dal vivo).
func DecisionEvent(s Step, algorithm string) *record.Decision {
	d := &record.Decision{
		DeviceID: s.Heartbeat.DeviceID, Algorithm: algorithm, Current: s.State.Policy,
		Mode: s.Decision.Mode, SplitPoint: s.Decision.SplitPoint,
		Changed: s.Decision.Changes(s.State.Policy), Reason: s.Decision.Reason,
		Predictions: offload.RecordPredictions(s.Decision.Predictions),
	}
	d.Kind, d.TS, d.TMS = "decision", s.Heartbeat.TS, s.Heartbeat.TMS
	return d
}
