// I file che finiscono in _test.go contengono test e non vengono inclusi
// nel binario. Si eseguono con: go test ./...
package api

import (
	"encoding/json"
	"testing"
)

// Ogni funzione TestXxx(t *testing.T) è un test.
// t.Errorf segnala un fallimento e continua; t.Fatalf segnala e si ferma.

func TestPolicyJSON(t *testing.T) {
	five := 5

	// "Table-driven test": una slice di casi, ciascuno con input e risultato
	// atteso, e un unico ciclo che li verifica tutti. È lo stile più comune
	// nei test Go.
	cases := []struct {
		name   string
		policy Policy
		want   string
	}{
		{
			name:   "local con split_point null",
			policy: Policy{Mode: ModeLocal, SplitPoint: nil, PolicyVersion: 1},
			want:   `{"mode":"LOCAL","split_point":null,"policy_version":1}`,
		},
		{
			name:   "split al layer 5",
			policy: Policy{Mode: ModeSplit, SplitPoint: &five, PolicyVersion: 2},
			want:   `{"mode":"SPLIT","split_point":5,"policy_version":2}`,
		},
	}

	for _, tc := range cases {
		// t.Run crea un sotto-test con un nome: nell'output vedi quale caso fallisce.
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.policy)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("JSON errato\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func TestRegisterRequestDecode(t *testing.T) {
	body := `{
		"protocol_version": 1,
		"device_id": "web_192",
		"device_type": "python",
		"model_hash": "abc123",
		"capabilities": {"raw_input_split": true}
	}`

	var req RegisterRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	want := RegisterRequest{
		ProtocolVersion: 1,
		DeviceID:        "web_192",
		DeviceType:      "python",
		ModelHash:       "abc123",
		Capabilities:    Capabilities{RawInputSplit: true},
	}
	// Le struct composte solo da tipi confrontabili si possono confrontare con ==.
	if req != want {
		t.Errorf("decodifica errata\n got: %+v\nwant: %+v", req, want)
	}
}

func TestRegisterResponseJSON(t *testing.T) {
	resp := RegisterResponse{
		SessionID: "s1",
		Model: ModelInfo{
			ModelKey:            "fomo_96x96",
			NumLayers:           59,
			LastOffloadingLayer: 58,
			InputSizeBytes:      18432,
		},
		Policy:              Policy{Mode: ModeLocal, PolicyVersion: 1},
		HeartbeatIntervalMS: 1000,
	}
	want := `{"session_id":"s1",` +
		`"model":{"model_key":"fomo_96x96","num_layers":59,"last_offloading_layer":58,"input_size_bytes":18432},` +
		`"policy":{"mode":"LOCAL","split_point":null,"policy_version":1},` +
		`"heartbeat_interval_ms":1000}`

	got, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(got) != want {
		t.Errorf("JSON errato\n got: %s\nwant: %s", got, want)
	}
}

func TestErrorResponseJSON(t *testing.T) {
	resp := ErrorResponse{Error: ErrorDetail{Code: ErrCodeModelNotFound, Message: "unknown model"}}
	want := `{"error":{"code":"model_not_found","message":"unknown model"}}`

	got, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(got) != want {
		t.Errorf("JSON errato\n got: %s\nwant: %s", got, want)
	}
}

func TestTelemetryOmitsMissingOptionals(t *testing.T) {
	// Finestra senza inferenze: restano solo i campi obbligatori.
	got, err := json.Marshal(Telemetry{InferenceCount: 0})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"inference_count":0}`; string(got) != want {
		t.Errorf("JSON errato\n got: %s\nwant: %s", got, want)
	}
}

func TestHeartbeatRequestDecode(t *testing.T) {
	body := `{
		"device_id": "web_96",
		"session_id": "S1",
		"policy_version": 3,
		"mode": "SPLIT",
		"telemetry": {
			"inference_count": 47,
			"inference_time_ms": 41.5,
			"inference_time_max_ms": 60,
			"cpu_load": 0,
			"memory_available_bytes": 812345344
		}
	}`

	var req HeartbeatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if req.DeviceID != "web_96" || req.SessionID != "S1" || req.PolicyVersion != 3 || req.Mode != ModeSplit {
		t.Errorf("campi errati: %+v", req)
	}
	tel := req.Telemetry
	if tel.InferenceCount != 47 || tel.InferenceTimeMS == nil || *tel.InferenceTimeMS != 41.5 ||
		tel.InferenceTimeMaxMS == nil || *tel.InferenceTimeMaxMS != 60 {
		t.Errorf("metriche di inferenza errate: %+v", tel)
	}
	// cpu_load: 0 è stato INVIATO → puntatore non nil che punta a 0.
	if tel.CPULoad == nil || *tel.CPULoad != 0 {
		t.Errorf("CPULoad = %v, want puntatore a 0", tel.CPULoad)
	}
	// rtt_ms non è stato inviato → nil.
	if tel.RTTMS != nil {
		t.Errorf("RTTMS = %v, want nil", *tel.RTTMS)
	}
	if tel.MemoryAvailableBytes == nil || *tel.MemoryAvailableBytes != 812345344 {
		t.Errorf("MemoryAvailableBytes = %v", tel.MemoryAvailableBytes)
	}
}

func TestHeartbeatResponseJSON(t *testing.T) {
	got, err := json.Marshal(HeartbeatResponse{Policy: Policy{Mode: ModeLocal, PolicyVersion: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"policy":{"mode":"LOCAL","split_point":null,"policy_version":1}}`; string(got) != want {
		t.Errorf("JSON errato\n got: %s\nwant: %s", got, want)
	}
}

func TestModelEntryJSONIsFlat(t *testing.T) {
	// L'embedding promuove i campi di ModelInfo allo stesso livello di model_hash.
	e := ModelEntry{
		ModelHash: "h1",
		ModelInfo: ModelInfo{ModelKey: "fomo_96x96", NumLayers: 59, LastOffloadingLayer: 58, InputSizeBytes: 18432},
	}
	got, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model_hash":"h1","model_key":"fomo_96x96","num_layers":59,"last_offloading_layer":58,"input_size_bytes":18432}`
	if string(got) != want {
		t.Errorf("JSON errato\n got: %s\nwant: %s", got, want)
	}
}
