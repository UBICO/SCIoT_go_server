package api

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestBinaryRoundTrip(t *testing.T) {
	header := []byte(`{"type":"frame","sequence_id":1}`)
	payload := bytes.Repeat([]byte{0xAB}, 1000)

	msg := EncodeBinary(header, payload)

	if len(msg) != 4+len(header)+len(payload) {
		t.Fatalf("len = %d", len(msg))
	}
	if n := binary.LittleEndian.Uint32(msg[:4]); int(n) != len(header) {
		t.Fatalf("header_length = %d, want %d (little-endian)", n, len(header))
	}
	h, p, err := DecodeBinary(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h, header) || !bytes.Equal(p, payload) {
		t.Errorf("round trip errato: header %q, payload %d byte", h, len(p))
	}
}

func TestBinaryEmptyPayload(t *testing.T) {
	_, p, err := DecodeBinary(EncodeBinary([]byte(`{}`), nil))
	if err != nil || len(p) != 0 {
		t.Errorf("payload vuoto: %d byte, %v", len(p), err)
	}
}

func TestDecodeBinaryMalformed(t *testing.T) {
	withLength := func(n uint32, rest int) []byte {
		b := make([]byte, 4+rest)
		binary.LittleEndian.PutUint32(b, n)
		return b
	}
	cases := map[string][]byte{
		"vuoto":                  {},
		"meno di 4 byte":         {1, 0},
		"header di lunghezza 0":  withLength(0, 10),
		"header oltre il limite": withLength(MaxHeaderBytes+1, MaxHeaderBytes+1),
		"header troncato":        withLength(100, 10),
		"lunghezza enorme":       withLength(math.MaxUint32, 10), // nessun overflow, nessun panic
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := DecodeBinary(msg); !errors.Is(err, ErrMalformedMessage) {
				t.Errorf("err = %v, want ErrMalformedMessage", err)
			}
		})
	}
}

func TestFrameHeaderValidate(t *testing.T) {
	sizes := map[int]int{-1: 1228800, 0: 3276800, 4: 819200}
	expected := func(sp int) (int, bool) { n, ok := sizes[sp]; return n, ok }
	deadline := func(v float64) *float64 { return &v }
	valid := FrameHeader{Type: MsgFrame, SequenceID: 7, SplitPoint: 4, Encoding: EncodingFloat32}

	cases := []struct {
		name    string
		mutate  func(*FrameHeader)
		payload int
		wantErr string // sottostringa attesa nell'errore, "" = valido
	}{
		{"valido", func(*FrameHeader) {}, 819200, ""},
		{"input grezzo (split -1)", func(h *FrameHeader) { h.SplitPoint = -1 }, 1228800, ""},
		{"con deadline", func(h *FrameHeader) { h.DeadlineMS = deadline(45) }, 819200, ""},
		{"tipo sbagliato", func(h *FrameHeader) { h.Type = "result" }, 819200, "type"},
		{"sequence_id zero", func(h *FrameHeader) { h.SequenceID = 0 }, 819200, "sequence_id"},
		{"encoding sconosciuto", func(h *FrameHeader) { h.Encoding = "float16" }, 819200, "encoding"},
		{"split point sconosciuto", func(h *FrameHeader) { h.SplitPoint = 3 }, 819200, "split_point"},
		{"payload della dimensione sbagliata", func(*FrameHeader) {}, 819199, "819200"},
		{"deadline zero", func(h *FrameHeader) { h.DeadlineMS = deadline(0) }, 819200, "deadline_ms"},
		{"deadline infinita", func(h *FrameHeader) { h.DeadlineMS = deadline(math.Inf(1)) }, 819200, "deadline_ms"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := valid
			tc.mutate(&h)
			err := h.Validate(tc.payload, expected)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("errore inatteso: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want un errore che contenga %q", err, tc.wantErr)
			}
		})
	}
}

func TestInferenceResultJSON(t *testing.T) {
	sp, spv, srv := 5, uint64(3), 7.3
	r := InferenceResult{
		Schema: ResultSchema, Type: MsgResult, DeviceID: "web_96", SessionID: "S", SequenceID: 1042,
		ModelKey: "yolo", Mode: ModeSplit, SplitPoint: &sp, PolicyVersion: 2,
		ServerPolicyVersion: &spv, ComputedBy: "server",
		CapturedAt: time.Date(2026, 10, 19, 10, 15, 3, 120e6, time.UTC),
		Timing:     Timing{ServerMS: &srv, LayerMS: []float64{1.1}},
		Detections: []Detection{{Label: "stop", Score: 0.9, Box: Box{0.1, 0.2, 0.3, 0.4}}},
	}
	got, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema":"inference-result.v1","type":"result","device_id":"web_96","session_id":"S",` +
		`"sequence_id":1042,"model_key":"yolo","mode":"SPLIT","split_point":5,"policy_version":2,` +
		`"server_policy_version":3,"computed_by":"server","captured_at":"2026-10-19T10:15:03.12Z",` +
		`"timing":{"server_ms":7.3,"layer_ms":[1.1]},` +
		`"detections":[{"label":"stop","score":0.9,"box":{"x":0.1,"y":0.2,"w":0.3,"h":0.4}}]}`
	if string(got) != want {
		t.Errorf("JSON\n got: %s\nwant: %s", got, want)
	}

	// LOCAL: split_point presente e null, niente campi del server.
	local, _ := json.Marshal(InferenceResult{Schema: ResultSchema, Mode: ModeLocal, ComputedBy: "device"})
	if !strings.Contains(string(local), `"split_point":null`) || strings.Contains(string(local), "server_policy_version") {
		t.Errorf("risultato LOCAL: %s", local)
	}
}
