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
		{"solo obbligatorio", Telemetry{InferenceTimeMS: 41.5}, false},
		{"tutto valido", Telemetry{InferenceTimeMS: 0, CPULoad: f64(1), RTTMS: f64(0), MemoryAvailableBytes: i64(0)}, false},
		{"inference negativo", Telemetry{InferenceTimeMS: -1}, true},
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
