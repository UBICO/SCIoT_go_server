package replay

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/offload"
)

// algoRecording: un device in SPLIT a 1 (modello di 3 segmenti), due
// heartbeat a 1 s di distanza, due frame eseguiti nella seconda finestra; più
// un heartbeat di una registrazione vecchia (senza num_segments).
const algoRecording = `{"kind":"start","t_ms":0}
{"kind":"heartbeat","ts":"2026-10-28T10:00:00Z","t_ms":500,"device_id":"old","server_policy":{"mode":"LOCAL","split_point":null,"policy_version":1},"telemetry":{"inference_count":1}}
{"kind":"heartbeat","ts":"2026-10-28T10:00:01Z","t_ms":1000,"device_id":"d","applied":{"mode":"SPLIT","policy_version":2},"server_policy":{"mode":"SPLIT","split_point":1,"policy_version":2},"telemetry":{"inference_count":20,"inference_time_ms":30,"inference_time_max_ms":35,"segment_ms":[10,20,null],"segment_count":[20,20,0]},"since_last_seen_ms":1000,"queue":{"slots":2,"running":0,"queued":0,"offered_per_s":100,"admitted_per_s":100,"utilization":0.6,"predicted_wait_ms":0,"interarrival_ms":10,"interarrival_std_ms":5},"network":{"bandwidth_bps":1000000,"latency_ms":2,"not_limiting":false,"valid_samples":5,"discarded_samples":0},"device_segments":[{"mean":10,"std":0,"n":3,"age_ms":0},{"mean":20,"std":0,"n":3,"age_ms":0},{"mean":30,"std":0,"n":3,"age_ms":0}],"model_key":"m","num_segments":3,"service":[{"split_point":1,"mean_ms":4,"std_ms":1,"samples":10},{"split_point":-1,"mean_ms":12,"std_ms":3,"samples":10}],"input_bytes":[{"split_point":-1,"input_bytes":50000},{"split_point":0,"input_bytes":20000},{"split_point":1,"input_bytes":10000}],"max_inflight":3}
{"kind":"frame","t_ms":1500,"device_id":"d","outcome":"result","receive_ms":10,"timing":{"server_ms":5}}
{"kind":"frame","t_ms":1800,"device_id":"d","outcome":"result","receive_ms":14,"timing":{"server_ms":7}}
{"kind":"frame","t_ms":1900,"device_id":"d","outcome":"server_busy","receive_ms":99}
{"kind":"heartbeat","ts":"2026-10-28T10:00:02Z","t_ms":2000,"device_id":"d","applied":{"mode":"SPLIT","policy_version":2},"server_policy":{"mode":"SPLIT","split_point":1,"policy_version":2},"telemetry":{"inference_count":20,"inference_time_ms":30,"inference_time_max_ms":35,"segment_ms":[10,20,null],"segment_count":[20,20,0]},"since_last_seen_ms":1000,"queue":{"slots":2,"running":0,"queued":0,"offered_per_s":100,"admitted_per_s":100,"utilization":0.6,"predicted_wait_ms":0},"network":{"bandwidth_bps":1000000,"latency_ms":2,"not_limiting":false,"valid_samples":5,"discarded_samples":0},"device_segments":[{"mean":10,"std":0,"n":3,"age_ms":0},{"mean":20,"std":0,"n":3,"age_ms":0},{"mean":30,"std":0,"n":3,"age_ms":0}],"model_key":"m","num_segments":3,"service":[{"split_point":1,"mean_ms":4,"std_ms":1,"samples":10}],"input_bytes":[{"split_point":1,"input_bytes":10000}],"max_inflight":3}
`

func TestAlgoBuildsTheState(t *testing.T) {
	steps, err := Algo(strings.NewReader(algoRecording), offload.Static{})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("passi = %d, attesi 2 (l'heartbeat vecchio va saltato)", len(steps))
	}
	st := steps[0].State
	if st.Last != 2 || st.Split() != 1 || st.FramesPerSec != 20 || st.Inputs.InputBytes[1] != 10000 || st.Inputs.ServiceMS[-1] != 12 {
		t.Errorf("stato = %+v", st)
	}
	if *st.Inputs.DeviceMS[2] != 30 || *st.Inputs.DeviceMS[1] != 20 {
		t.Errorf("tempi del device = %v", st.Inputs.DeviceMS)
	}
	// A: 1000 ms − 20×(10+20) di calcolo − 20 frame inviati × 12 ms di invio
	// (latenza 2 + 10 KB a 1e6 B/s), su 20 frame = 8 ms.
	if st.Inputs.MaxInflight != 3 || st.Inputs.CycleMS == nil || math.Abs(*st.Inputs.CycleMS-8) > 1e-9 {
		t.Errorf("finestra %d, ciclo %v", st.Inputs.MaxInflight, st.Inputs.CycleMS)
	}
	q := st.Inputs.Queue
	// Altri: 100 − 20 frame/s; 0.6×2 − 20×4 ms = 1.12 erlang. ca = 5/10. cs
	// medio pesato: (1/4 + 3/12)/2 = 0.25.
	if q.Slots != 2 || math.Abs(q.OthersLambda-80) > 1e-9 || math.Abs(q.OthersErlangs-1.12) > 1e-9 ||
		math.Abs(q.CA-0.5) > 1e-9 || math.Abs(q.CS-0.25) > 1e-9 {
		t.Errorf("coda = %+v", q)
	}
	if st.PolicyAge != 0 || steps[1].State.PolicyAge.Milliseconds() != 1000 {
		t.Errorf("età della policy: %v, %v; attese 0 e 1 s", st.PolicyAge, steps[1].State.PolicyAge)
	}
}

func TestAlgoMeasuresTheActualPolicy(t *testing.T) {
	steps, _ := Algo(strings.NewReader(algoRecording), offload.Static{})
	// Prima finestra: nessun frame eseguito.
	if !math.IsNaN(steps[0].MeasuredActualMS) {
		t.Errorf("prima finestra misurata = %v, attesa NaN", steps[0].MeasuredActualMS)
	}
	// Seconda: prefisso 30 + latenza 2 + media di (10+5, 14+7) = 50.
	if got := steps[1].MeasuredActualMS; math.Abs(got-50) > 1e-9 {
		t.Errorf("misurata = %v, attesa 50", got)
	}
	// La prima ha tutti gli ingressi per split 1: prevista e uguale per la
	// policy applicata e per quella scelta (static non cambia).
	if p := steps[0].PredictedActualMS; math.IsNaN(p) || p != steps[0].PredictedChosenMS {
		t.Errorf("prevista: %v / %v", p, steps[0].PredictedChosenMS)
	}
}

func TestSummarizeAndDecisionEvents(t *testing.T) {
	steps, _ := Algo(strings.NewReader(algoRecording), offload.Static{})
	sums := Summarize(steps)
	if len(sums) != 1 || sums[0].DeviceID != "d" || sums[0].Heartbeats != 2 || sums[0].Differ != 0 ||
		sums[0].Changes != 0 || sums[0].NMeasured != 1 || sums[0].MeasuredActualMS != 50 {
		t.Errorf("riassunto = %+v", sums)
	}
	ev := DecisionEvent(steps[0], "static")
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if ev.Kind != "decision" || ev.Changed || ev.Mode != api.ModeSplit || !strings.Contains(string(data), `"predictions"`) {
		t.Errorf("evento = %s", data)
	}
}
