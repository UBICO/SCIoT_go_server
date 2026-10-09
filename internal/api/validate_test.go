package api

import "testing"

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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.tel.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
