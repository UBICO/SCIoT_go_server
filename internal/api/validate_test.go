package api

import (
	"strings"
	"testing"
)

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

func TestExecutionModeValid(t *testing.T) {
	for _, m := range []ExecutionMode{ModeLocal, ModeSplit} {
		if !m.Valid() {
			t.Errorf("%q deve essere valida", m)
		}
	}
	for _, m := range []ExecutionMode{"", "local", "REMOTE"} {
		if m.Valid() {
			t.Errorf("%q non deve essere valida", m)
		}
	}
}

func TestTelemetryValidate(t *testing.T) {
	cases := []struct {
		name    string
		tel     Telemetry
		wantErr bool
	}{
		{"finestra vuota", Telemetry{InferenceCount: 0}, false},
		{"finestra piena", Telemetry{InferenceCount: 47, InferenceTimeMS: f64(21.3), InferenceTimeMaxMS: f64(38.9)}, false},
		{"media uguale al massimo", Telemetry{InferenceCount: 1, InferenceTimeMS: f64(20), InferenceTimeMaxMS: f64(20)}, false},
		{"tutto valido", Telemetry{InferenceCount: 1, InferenceTimeMS: f64(0), InferenceTimeMaxMS: f64(0), CPULoad: f64(1), RTTMS: f64(0), MemoryAvailableBytes: i64(0)}, false},
		{"count negativo", Telemetry{InferenceCount: -1}, true},
		{"count 0 con media", Telemetry{InferenceCount: 0, InferenceTimeMS: f64(20)}, true},
		{"count 0 con massimo", Telemetry{InferenceCount: 0, InferenceTimeMaxMS: f64(20)}, true},
		{"count > 0 senza media", Telemetry{InferenceCount: 3, InferenceTimeMaxMS: f64(20)}, true},
		{"count > 0 senza massimo", Telemetry{InferenceCount: 3, InferenceTimeMS: f64(20)}, true},
		{"media negativa", Telemetry{InferenceCount: 3, InferenceTimeMS: f64(-1), InferenceTimeMaxMS: f64(20)}, true},
		{"media sopra il massimo", Telemetry{InferenceCount: 3, InferenceTimeMS: f64(30), InferenceTimeMaxMS: f64(20)}, true},
		{"cpu_load sopra 1", Telemetry{CPULoad: f64(1.01)}, true},
		{"cpu_load negativo", Telemetry{CPULoad: f64(-0.1)}, true},
		{"rtt negativo", Telemetry{RTTMS: f64(-3)}, true},
		{"memoria negativa", Telemetry{MemoryAvailableBytes: i64(-1)}, true},
		{"segmenti validi", Telemetry{SegmentMS: []*float64{f64(0.5), f64(0), nil}, SegmentCount: []int{3, 1, 0}}, false},
		{"segment_ms senza segment_count", Telemetry{SegmentMS: []*float64{f64(1)}}, true},
		{"segment_count senza segment_ms", Telemetry{SegmentCount: []int{1}}, true},
		{"lunghezze diverse", Telemetry{SegmentMS: []*float64{f64(1), f64(2)}, SegmentCount: []int{1}}, true},
		{"segmento eseguito senza media", Telemetry{SegmentMS: []*float64{nil}, SegmentCount: []int{2}}, true},
		{"media di un segmento non eseguito", Telemetry{SegmentMS: []*float64{f64(1)}, SegmentCount: []int{0}}, true},
		{"conteggio negativo", Telemetry{SegmentMS: []*float64{nil}, SegmentCount: []int{-1}}, true},
		{"media negativa di un segmento", Telemetry{SegmentMS: []*float64{f64(-1)}, SegmentCount: []int{1}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.tel.Validate()
			if err != nil && tc.wantErr && strings.Contains(tc.name, "segment") && !strings.Contains(err.Error(), "segment_") {
				t.Errorf("il messaggio deve nominare il campo JSON: %v", err)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func intp(v int) *int { return &v }

func TestSetPolicyRequestValidate(t *testing.T) {
	model := ModelInfo{ModelKey: "fomo_96x96", NumLayers: 59, LastOffloadingLayer: 58}
	cases := []struct {
		name    string
		req     SetPolicyRequest
		wantErr bool
	}{
		{"local", SetPolicyRequest{Mode: ModeLocal}, false},
		{"split all'input: tutto sul server", SetPolicyRequest{Mode: ModeSplit, SplitPoint: intp(-1)}, false},
		{"split al primo layer", SetPolicyRequest{Mode: ModeSplit, SplitPoint: intp(0)}, false},
		{"al server solo l'ultimo segmento", SetPolicyRequest{Mode: ModeSplit, SplitPoint: intp(57)}, false},
		{"split all'ultimo layer: niente per il server", SetPolicyRequest{Mode: ModeSplit, SplitPoint: intp(58)}, true},
		{"local con split_point", SetPolicyRequest{Mode: ModeLocal, SplitPoint: intp(3)}, true},
		{"split senza split_point", SetPolicyRequest{Mode: ModeSplit}, true},
		{"split sotto -1", SetPolicyRequest{Mode: ModeSplit, SplitPoint: intp(-2)}, true},
		{"split oltre il modello", SetPolicyRequest{Mode: ModeSplit, SplitPoint: intp(59)}, true},
		{"mode sconosciuto", SetPolicyRequest{Mode: "REMOTE"}, true},
		{"split con finestra", SetPolicyRequest{Mode: ModeSplit, SplitPoint: intp(3), MaxInflight: intp(2)}, false},
		{"finestra zero", SetPolicyRequest{Mode: ModeSplit, SplitPoint: intp(3), MaxInflight: intp(0)}, true},
		{"local con finestra", SetPolicyRequest{Mode: ModeLocal, MaxInflight: intp(2)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.req.Validate(model); (err != nil) != tc.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
