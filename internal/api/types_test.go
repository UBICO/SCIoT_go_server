// I file che finiscono in _test.go contengono test e non vengono inclusi
// nel binario. Si eseguono con: go test ./...
package api

import (
	"encoding/json"
	"strings"
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
		"capabilities": {"probe": true}
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
		Capabilities:    Capabilities{Probe: true},
	}
	// Le struct composte solo da tipi confrontabili si possono confrontare con ==.
	if req != want {
		t.Errorf("decodifica errata\n got: %+v\nwant: %+v", req, want)
	}
}

func TestRegisterResponseJSON(t *testing.T) {
	iou := 0.2
	resp := RegisterResponse{
		SessionID: "s1",
		Model: ModelInfo{
			ModelKey:            "yolo_small_320",
			NumLayers:           8,
			LastOffloadingLayer: 7,
			Input:               TensorSpec{Shape: []int{1, 320, 320, 3}, DType: "float32"},
			Postprocess: Postprocess{Decoder: "yolo_pro", Labels: []string{"bicycle", "stop sign"},
				ScoreThreshold: 0.5, IoUThreshold: &iou, MaxDetections: 200},
		},
		Policy:              Policy{Mode: ModeLocal, PolicyVersion: 1},
		HeartbeatIntervalMS: 1000,
		DataPlane:           true,
	}
	want := `{"session_id":"s1",` +
		`"model":{"model_key":"yolo_small_320","num_layers":8,"last_offloading_layer":7,` +
		`"input":{"shape":[1,320,320,3],"dtype":"float32"},` +
		`"postprocess":{"decoder":"yolo_pro","labels":["bicycle","stop sign"],"score_threshold":0.5,"iou_threshold":0.2,"max_detections":200}},` +
		`"policy":{"mode":"LOCAL","split_point":null,"policy_version":1},` +
		`"heartbeat_interval_ms":1000,"data_plane":true}`

	got, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(got) != want {
		t.Errorf("JSON errato\n got: %s\nwant: %s", got, want)
	}
}

// decodeStrict decodifica come il server (server.decodeJSON): un campo
// sconosciuto è un errore.
func decodeStrict(body string) (RegisterRequest, error) {
	var req RegisterRequest
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	err := dec.Decode(&req)
	return req, err
}

func TestUnknownCapabilitiesAreIgnored(t *testing.T) {
	// Un client più nuovo del server: capability che il server non conosce.
	req, err := decodeStrict(`{"protocol_version": 1, "device_id": "d", "model_hash": "h",
		"capabilities": {"probe": true, "raw_input_split": true, "segment_dtypes": ["int8"]}}`)
	if err != nil {
		t.Fatalf("capability sconosciute devono essere ignorate: %v", err)
	}
	if !req.Capabilities.Probe {
		t.Errorf("Probe = false, atteso true: le capability note vanno comunque lette")
	}
}

func TestCapabilitiesStrictOutside(t *testing.T) {
	cases := map[string]string{
		"campo sconosciuto fuori da capabilities": `{"protocol_version": 1, "devce_id": "d"}`,
		"capabilities non è un oggetto":           `{"protocol_version": 1, "capabilities": 5}`,
		"capability di tipo sbagliato":            `{"protocol_version": 1, "capabilities": {"probe": "yes"}}`,
	}
	for name, body := range cases {
		if _, err := decodeStrict(body); err == nil {
			t.Errorf("%s: atteso un errore", name)
		}
	}
}

func TestMissingCapabilitiesMeanNo(t *testing.T) {
	req, err := decodeStrict(`{"protocol_version": 1, "device_id": "d", "model_hash": "h"}`)
	if err != nil || req.Capabilities.Probe {
		t.Errorf("senza capabilities: %+v, %v; atteso Probe false", req.Capabilities, err)
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
	iou := 0.2
	e := ModelEntry{
		ModelHash: "h1",
		ModelInfo: ModelInfo{
			ModelKey:            "yolo_small_320",
			NumLayers:           8,
			LastOffloadingLayer: 7,
			Input:               TensorSpec{Shape: []int{1, 320, 320, 3}, DType: "float32"},
			Postprocess: Postprocess{Decoder: "yolo_pro", Labels: []string{"bicycle", "stop sign"},
				ScoreThreshold: 0.5, IoUThreshold: &iou, MaxDetections: 200},
		},
	}
	got, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model_hash":"h1","model_key":"yolo_small_320","num_layers":8,"last_offloading_layer":7,` +
		`"input":{"shape":[1,320,320,3],"dtype":"float32"},` +
		`"postprocess":{"decoder":"yolo_pro","labels":["bicycle","stop sign"],"score_threshold":0.5,"iou_threshold":0.2,"max_detections":200}}`
	if string(got) != want {
		t.Errorf("JSON errato\n got: %s\nwant: %s", got, want)
	}
}

func TestPostprocessWithoutNMSOmitsIoU(t *testing.T) {
	got, err := json.Marshal(Postprocess{Decoder: "fomo", Labels: []string{"a"}, ScoreThreshold: 0.8, MaxDetections: 10})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"decoder":"fomo","labels":["a"],"score_threshold":0.8,"max_detections":10}`; string(got) != want {
		t.Errorf("JSON errato\n got: %s\nwant: %s", got, want)
	}
}
