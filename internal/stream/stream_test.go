package stream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/UBICO/SCIoT_go_server/internal/api"
)

// --- fakes -----------------------------------------------------------------------

type fakeSessions struct {
	mu     sync.Mutex
	device string
	sess   string
	state  Session
}

func (f *fakeSessions) Lookup(deviceID, sessionID string) (Session, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if deviceID != f.device || sessionID != f.sess {
		return Session{}, false
	}
	return f.state, true
}

func (f *fakeSessions) setPolicyVersion(v uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.Policy.PolicyVersion = v
}

type fakeCatalog map[string][]api.SegmentSize

func (f fakeCatalog) Segments(model string) ([]api.SegmentSize, bool) {
	s, ok := f[model]
	return s, ok
}

const split4Bytes = 64 // tensori piccoli: il protocollo è lo stesso

var sizes = []api.SegmentSize{{SplitPoint: -1, InputBytes: 32}, {SplitPoint: 4, InputBytes: split4Bytes}}

type harness struct {
	srv      *httptest.Server
	sessions *fakeSessions
	exec     *FakeExecutor
	handler  *Handler
}

func newHarness(t *testing.T, exec *FakeExecutor, cfg Config) *harness {
	t.Helper()
	sessions := &fakeSessions{device: "web_96", sess: "S1", state: Session{
		ModelKey: "yolo", Policy: api.Policy{Mode: api.ModeSplit, SplitPoint: intp(4), PolicyVersion: 2},
	}}
	h := NewHandler(sessions, fakeCatalog{"yolo": sizes}, exec, cfg)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &harness{srv: srv, sessions: sessions, exec: exec, handler: h}
}

func intp(v int) *int { return &v }

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.MaxMessageBytes = 1 << 20
	return cfg
}

// dial apre lo stream come farebbe un device.
func (h *harness) dial(t *testing.T, device, session string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	header := http.Header{}
	if device != "" {
		header.Set(api.HeaderDevice, device)
	}
	if session != "" {
		header.Set(api.HeaderSession, session)
	}
	url := "ws" + strings.TrimPrefix(h.srv.URL, "http")
	return websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
}

// open apre uno stream valido e consuma il messaggio hello.
func (h *harness) open(t *testing.T) *websocket.Conn {
	t.Helper()
	ws, _, err := h.dial(t, "web_96", "S1")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	ws.SetReadLimit(1 << 20)
	var hello api.Hello
	readText(t, ws, &hello)
	return ws
}

func sendFrame(t *testing.T, ws *websocket.Conn, hdr api.FrameHeader, payload []byte) {
	t.Helper()
	data, _ := json.Marshal(hdr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageBinary, api.EncodeBinary(data, payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func frame(seq uint64) api.FrameHeader {
	return api.FrameHeader{Type: api.MsgFrame, SequenceID: seq, PolicyVersion: 2, SplitPoint: 4, Encoding: api.EncodingFloat32}
}

// read legge il prossimo messaggio: restituisce tipo, header/JSON e payload.
func read(t *testing.T, ws *websocket.Conn) (websocket.MessageType, []byte, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, msg, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ == websocket.MessageText {
		return typ, msg, nil
	}
	hdr, payload, err := api.DecodeBinary(msg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return typ, hdr, payload
}

func readText(t *testing.T, ws *websocket.Conn, v any) {
	t.Helper()
	typ, data, _ := read(t, ws)
	if typ != websocket.MessageText {
		t.Fatalf("atteso un messaggio di testo, ricevuto binario: %s", data)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}

func readResult(t *testing.T, ws *websocket.Conn) (api.InferenceResult, []byte) {
	t.Helper()
	typ, hdr, payload := read(t, ws)
	if typ != websocket.MessageBinary {
		t.Fatalf("atteso un risultato binario, ricevuto testo: %s", hdr)
	}
	var r api.InferenceResult
	if err := json.Unmarshal(hdr, &r); err != nil {
		t.Fatal(err)
	}
	return r, payload
}

// --- prima dell'upgrade ------------------------------------------------------------

func TestUpgradeErrors(t *testing.T) {
	h := newHarness(t, &FakeExecutor{}, testConfig())
	cases := []struct {
		name, device, session string
		want                  int
	}{
		{"header mancanti", "", "", http.StatusBadRequest},
		{"sessione sconosciuta", "web_96", "VECCHIA", http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, resp, err := h.dial(t, tc.device, tc.session)
			if err == nil || resp == nil || resp.StatusCode != tc.want {
				t.Fatalf("err %v, resp %v; want status %d", err, resp, tc.want)
			}
		})
	}
}

func TestUpgradeModelWithoutDataPlane(t *testing.T) {
	h := newHarness(t, &FakeExecutor{}, testConfig())
	h.handler.catalog = fakeCatalog{} // nessun modello disponibile
	_, resp, err := h.dial(t, "web_96", "S1")
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %v / %v", resp, err)
	}
}

// --- hello e risultati -------------------------------------------------------------

func TestHello(t *testing.T) {
	h := newHarness(t, &FakeExecutor{}, testConfig())
	ws, _, err := h.dial(t, "web_96", "S1")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	var hello api.Hello
	readText(t, ws, &hello)
	if hello.Type != api.MsgHello || hello.MaxInflight != 3 || hello.MaxMessageBytes != 1<<20 ||
		hello.Policy.PolicyVersion != 2 || len(hello.Segments) != 2 {
		t.Errorf("hello = %+v", hello)
	}
}

func TestFrameToResult(t *testing.T) {
	exec := &FakeExecutor{OutputBytes: 24, Detections: []api.Detection{{Label: "stop", Score: 0.9}}}
	h := newHarness(t, exec, testConfig())
	ws := h.open(t)

	captured := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	hdr := frame(1)
	hdr.CapturedAt = captured
	h.sessions.setPolicyVersion(3) // la policy cambia mentre il frame è in volo
	sendFrame(t, ws, hdr, make([]byte, split4Bytes))

	r, payload := readResult(t, ws)
	if r.Schema != api.ResultSchema || r.Type != api.MsgResult || r.SequenceID != 1 ||
		r.DeviceID != "web_96" || r.SessionID != "S1" || r.ModelKey != "yolo" ||
		r.Mode != api.ModeSplit || r.ComputedBy != "server" ||
		r.SplitPoint == nil || *r.SplitPoint != 4 || !r.CapturedAt.Equal(captured) {
		t.Errorf("risultato = %+v", r)
	}
	if r.PolicyVersion != 2 || r.ServerPolicyVersion == nil || *r.ServerPolicyVersion != 3 {
		t.Errorf("versioni: frame %d, server %v; want 2 e 3", r.PolicyVersion, r.ServerPolicyVersion)
	}
	if r.Timing.ServerMS == nil || r.Timing.QueueWaitMS == nil || *r.Timing.QueueWaitMS != 0.5 || len(r.Timing.LayerMS) != 2 {
		t.Errorf("timing = %+v", r.Timing)
	}
	if len(r.Detections) != 1 || r.Detections[0].Label != "stop" {
		t.Errorf("detections = %+v", r.Detections)
	}
	if len(payload) != 0 {
		t.Errorf("senza want_raw il payload deve essere vuoto, got %d byte", len(payload))
	}

	// Il job passato all'Executor.
	jobs := exec.Jobs()
	if len(jobs) != 1 || jobs[0].SplitPoint != 4 || len(jobs[0].Tensor) != split4Bytes || jobs[0].ModelKey != "yolo" {
		t.Errorf("job = %+v", jobs)
	}
}

func TestWantRaw(t *testing.T) {
	h := newHarness(t, &FakeExecutor{OutputBytes: 24}, testConfig())
	ws := h.open(t)
	hdr := frame(1)
	hdr.WantRaw = true
	sendFrame(t, ws, hdr, make([]byte, split4Bytes))
	if _, payload := readResult(t, ws); len(payload) != 24 {
		t.Errorf("payload = %d byte, want 24", len(payload))
	}
}

func TestDeadlinePassedToExecutor(t *testing.T) {
	exec := &FakeExecutor{}
	cfg := testConfig()
	cfg.DefaultDeadline = 700 * time.Millisecond
	h := newHarness(t, exec, cfg)
	fixed := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	h.handler.now = func() time.Time { return fixed }
	ws := h.open(t)

	sendFrame(t, ws, frame(1), make([]byte, split4Bytes)) // default
	readResult(t, ws)
	hdr := frame(2)
	d := 45.0
	hdr.DeadlineMS = &d
	sendFrame(t, ws, hdr, make([]byte, split4Bytes))
	readResult(t, ws)

	jobs := exec.Jobs()
	if want := fixed.Add(700 * time.Millisecond); !jobs[0].Deadline.Equal(want) {
		t.Errorf("deadline di default = %v, want %v", jobs[0].Deadline, want)
	}
	if want := fixed.Add(45 * time.Millisecond); !jobs[1].Deadline.Equal(want) {
		t.Errorf("deadline del frame = %v, want %v", jobs[1].Deadline, want)
	}
}

// --- asincronia ---------------------------------------------------------------------

func TestResultsOutOfOrder(t *testing.T) {
	// Il frame 1 è lento, il 2 veloce: il risultato del 2 arriva per primo.
	exec := &FakeExecutor{Delay: func(j Job) time.Duration {
		if j.Tensor[0] == 1 {
			return 200 * time.Millisecond
		}
		return 0
	}}
	h := newHarness(t, exec, testConfig())
	ws := h.open(t)

	slow := make([]byte, split4Bytes)
	slow[0] = 1
	sendFrame(t, ws, frame(1), slow)
	sendFrame(t, ws, frame(2), make([]byte, split4Bytes))

	first, _ := readResult(t, ws)
	second, _ := readResult(t, ws)
	if first.SequenceID != 2 || second.SequenceID != 1 {
		t.Errorf("ordine dei risultati %d, %d; want 2, 1", first.SequenceID, second.SequenceID)
	}
}

func TestInflightLimitAndDuplicates(t *testing.T) {
	block := make(chan struct{})
	cfg := testConfig()
	cfg.MaxInflight = 1
	h := newHarness(t, &FakeExecutor{Block: block}, cfg)
	ws := h.open(t)

	sendFrame(t, ws, frame(1), make([]byte, split4Bytes)) // resta in esecuzione
	sendFrame(t, ws, frame(1), make([]byte, split4Bytes)) // stesso sequence_id
	var dup api.StreamError
	readText(t, ws, &dup)
	if dup.Code != api.ErrCodeInvalidFrame || dup.SequenceID != 1 {
		t.Errorf("duplicato: %+v", dup)
	}

	sendFrame(t, ws, frame(2), make([]byte, split4Bytes)) // finestra piena
	var limit api.StreamError
	readText(t, ws, &limit)
	if limit.Code != api.ErrCodeInflightLimit || limit.SequenceID != 2 {
		t.Errorf("finestra: %+v", limit)
	}

	close(block) // il frame 1 termina e libera la finestra
	if r, _ := readResult(t, ws); r.SequenceID != 1 {
		t.Errorf("risultato %d, want 1", r.SequenceID)
	}
	sendFrame(t, ws, frame(3), make([]byte, split4Bytes))
	if r, _ := readResult(t, ws); r.SequenceID != 3 {
		t.Errorf("dopo il rilascio: risultato %d, want 3", r.SequenceID)
	}
}

// TestSlotFreedBeforeResult: un device con max_inflight 1 invia il frame
// successivo appena riceve il risultato. Il posto deve essere già libero, anche
// se la goroutine del frame precedente è lenta a terminare dopo l'invio
// (come su un runner di CI carico: era la causa di un test intermittente).
func TestSlotFreedBeforeResult(t *testing.T) {
	cfg := testConfig()
	cfg.MaxInflight = 1
	h := newHarness(t, &FakeExecutor{}, cfg)
	h.handler.afterSend = func() { time.Sleep(100 * time.Millisecond) }
	ws := h.open(t)

	for seq := uint64(1); seq <= 3; seq++ {
		sendFrame(t, ws, frame(seq), make([]byte, split4Bytes))
		if r, _ := readResult(t, ws); r.SequenceID != seq {
			t.Fatalf("risultato %d, want %d", r.SequenceID, seq)
		}
	}
}

// --- errori -------------------------------------------------------------------------

func TestInvalidFramesKeepStreamOpen(t *testing.T) {
	h := newHarness(t, &FakeExecutor{}, testConfig())
	ws := h.open(t)
	ctx := context.Background()

	ws.Write(ctx, websocket.MessageText, []byte(`{"type":"frame"}`))           // testo invece di binario
	ws.Write(ctx, websocket.MessageBinary, []byte{1, 2})                       // binario malformato
	sendFrame(t, ws, frame(5), make([]byte, split4Bytes-1))                    // dimensione sbagliata
	ws.Write(ctx, websocket.MessageBinary, api.EncodeBinary([]byte(`{`), nil)) // JSON rotto

	wantSeq := []uint64{0, 0, 5, 0}
	for i, seq := range wantSeq {
		var e api.StreamError
		readText(t, ws, &e)
		if e.Type != api.MsgError || e.Code != api.ErrCodeInvalidFrame || e.SequenceID != seq || e.Message == "" {
			t.Errorf("errore %d = %+v", i, e)
		}
	}
	// Lo stream è ancora aperto: un frame valido funziona.
	sendFrame(t, ws, frame(6), make([]byte, split4Bytes))
	if r, _ := readResult(t, ws); r.SequenceID != 6 {
		t.Errorf("dopo gli errori: risultato %d", r.SequenceID)
	}
}

func TestExecutorErrors(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{ErrBusy, api.ErrCodeServerBusy},
		{ErrDeadlineExceeded, api.ErrCodeDeadlineExceeded},
		{ErrWorkerUnavailable, api.ErrCodeWorkerUnavailable},
		{errors.New("grpc: connection refused"), api.ErrCodeWorkerUnavailable},
		{wrap(ErrBusy), api.ErrCodeServerBusy}, // errori avvolti con %w
	}
	for _, tc := range cases {
		t.Run(tc.code+"/"+tc.err.Error(), func(t *testing.T) {
			h := newHarness(t, &FakeExecutor{Err: tc.err}, testConfig())
			ws := h.open(t)
			sendFrame(t, ws, frame(9), make([]byte, split4Bytes))
			var e api.StreamError
			readText(t, ws, &e)
			if e.Code != tc.code || e.SequenceID != 9 {
				t.Errorf("errore = %+v, want %s", e, tc.code)
			}
		})
	}
}

func TestUnexpectedErrorDetailsStayOnServer(t *testing.T) {
	internal := errors.New("grpc: connection refused 127.0.0.1:50051")
	h := newHarness(t, &FakeExecutor{Err: internal}, testConfig())
	ws := h.open(t)
	sendFrame(t, ws, frame(4), make([]byte, split4Bytes))
	var e api.StreamError
	readText(t, ws, &e)
	if e.Code != api.ErrCodeWorkerUnavailable || strings.Contains(e.Message, "127.0.0.1") {
		t.Errorf("il device non deve ricevere i dettagli interni: %+v", e)
	}
}

func wrap(err error) error { return errors.Join(errors.New("queue"), err) }
