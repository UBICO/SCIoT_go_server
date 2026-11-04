package stream

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/netmeasure"
	"github.com/UBICO/SCIoT_go_server/internal/record"
)

func TestProbeDue(t *testing.T) {
	t0 := time.Unix(1000, 0)
	const interval = 30 * time.Second
	cases := []struct {
		name                    string
		now, last, pendingSince time.Time
		want                    bool
	}{
		{"mai misurato", t0, time.Time{}, time.Time{}, true},
		{"misura recente", t0, t0.Add(-10 * time.Second), time.Time{}, false},
		{"misura vecchia", t0, t0.Add(-interval), time.Time{}, true},
		{"probe in attesa", t0, time.Time{}, t0.Add(-10 * time.Second), false},
		{"probe in attesa ma misura vecchia", t0, t0.Add(-time.Hour), t0.Add(-10 * time.Second), false},
		{"probe senza risposta da interval", t0, time.Time{}, t0.Add(-interval), true},
	}
	for _, c := range cases {
		if got := probeDue(c.now, c.last, c.pendingSince, interval); got != c.want {
			t.Errorf("%s: probeDue = %v, atteso %v", c.name, got, c.want)
		}
	}
}

func TestProbeInterval(t *testing.T) {
	const interval, unknown = 30 * time.Second, 5 * time.Second
	bw := 1e6
	if got := probeInterval(netmeasure.State{}, interval, unknown); got != unknown {
		t.Errorf("banda sconosciuta: %v, atteso %v", got, unknown)
	}
	// Anche "non limitante" senza banda: su un link che non aspetta mai, i
	// probe tengono viva la regola dei 3 campioni.
	if got := probeInterval(netmeasure.State{NotLimiting: true}, interval, unknown); got != unknown {
		t.Errorf("non limitante senza banda: %v, atteso %v", got, unknown)
	}
	if got := probeInterval(netmeasure.State{BandwidthBps: &bw}, interval, unknown); got != interval {
		t.Errorf("banda nota: %v, atteso %v", got, interval)
	}
	if got := probeInterval(netmeasure.State{}, interval, 0); got != interval {
		t.Errorf("unknown = 0: %v, atteso %v", got, interval)
	}
	if got := probeInterval(netmeasure.State{}, 2*time.Second, unknown); got != 2*time.Second {
		t.Errorf("mai più lungo di interval: %v", got)
	}
}

// probeHarness: device con la capability probe, probe rapidi e piccoli, niente
// ping (il client dei test non legge in background), registrazione accesa.
func probeHarness(t *testing.T) (*harness, *record.Recorder) {
	t.Helper()
	cfg := testConfig()
	cfg.PingInterval = 0
	cfg.ProbeInterval = 50 * time.Millisecond
	cfg.ProbeBytes = netmeasure.ReadChunk
	h := newHarness(t, &FakeExecutor{}, cfg)
	h.sessions.state.Probe = true
	rec, err := record.Open(t.TempDir(), record.Start{}, record.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	h.handler.SetRecorder(rec)
	return h, rec
}

func readProbeRequest(t *testing.T, ws *websocket.Conn) api.ProbeRequest {
	t.Helper()
	var req api.ProbeRequest
	readText(t, ws, &req)
	if req.Type != api.MsgProbeRequest || req.ProbeID == 0 {
		t.Fatalf("atteso un probe_request, ricevuto %+v", req)
	}
	return req
}

func sendProbe(t *testing.T, ws *websocket.Conn, id uint64, n int) {
	t.Helper()
	hdr, _ := json.Marshal(api.ProbeHeader{Type: api.MsgProbe, ProbeID: id})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageBinary, api.EncodeBinary(hdr, make([]byte, n))); err != nil {
		t.Fatalf("write probe: %v", err)
	}
}

func readError(t *testing.T, ws *websocket.Conn) api.StreamError {
	t.Helper()
	var e api.StreamError
	readText(t, ws, &e)
	if e.Type != api.MsgError {
		t.Fatalf("atteso un error, ricevuto %+v", e)
	}
	return e
}

func recordedProbes(t *testing.T, rec *record.Recorder) []record.Probe {
	t.Helper()
	rec.Close()
	f, err := os.Open(rec.Stats().File)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var probes []record.Probe
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var p record.Probe
		if err := json.Unmarshal(sc.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if p.Kind == "probe" {
			probes = append(probes, p)
		}
	}
	return probes
}

func TestProbeRequestedAndMeasured(t *testing.T) {
	h, rec := probeHarness(t)
	ws := h.open(t)

	// Nessun frame (LOCAL): la rete non è misurata, il server chiede un probe.
	req := readProbeRequest(t, ws)
	if req.Bytes != netmeasure.ReadChunk {
		t.Errorf("bytes = %d, attesi %d", req.Bytes, netmeasure.ReadChunk)
	}
	sendProbe(t, ws, req.ProbeID, req.Bytes)
	eventually(t, func() bool {
		n, _ := h.handler.Network("web_96")
		return n.Valid+n.Discarded == 1
	})

	// Lo stesso probe una seconda volta: è già chiuso.
	sendProbe(t, ws, req.ProbeID, req.Bytes)
	if e := readError(t, ws); e.Code != api.ErrCodeInvalidFrame {
		t.Errorf("probe ripetuto: codice %q, atteso invalid_frame", e.Code)
	}

	ws.CloseNow()
	probes := recordedProbes(t, rec)
	if len(probes) != 1 {
		t.Fatalf("probe registrati = %+v, atteso 1", probes)
	}
	if p := probes[0]; p.ProbeID != req.ProbeID || p.Bytes != req.Bytes || p.DeviceID != "web_96" || p.SessionID != "S1" {
		t.Errorf("evento probe = %+v", p)
	}
}

func TestProbeOfWrongSizeKeepsWaiting(t *testing.T) {
	h, _ := probeHarness(t)
	// Un solo probe_request in tutto il test: il primo controllo, dopo
	// probeCheckEvery, trova la rete mai misurata; il successivo tra un'ora.
	h.handler.cfg.ProbeInterval = time.Hour
	ws := h.open(t)

	req := readProbeRequest(t, ws)
	sendProbe(t, ws, req.ProbeID, req.Bytes-1)
	if e := readError(t, ws); e.Code != api.ErrCodeInvalidFrame {
		t.Errorf("probe corto: codice %q, atteso invalid_frame", e.Code)
	}
	// Il probe resta in attesa: la risposta giusta è ancora accettata.
	sendProbe(t, ws, req.ProbeID, req.Bytes)
	sendProbe(t, ws, req.ProbeID, req.Bytes) // ora è chiuso
	if e := readError(t, ws); e.Code != api.ErrCodeInvalidFrame {
		t.Errorf("probe ripetuto: codice %q, atteso invalid_frame", e.Code)
	}
}

func TestProbeNotSolicitedIsRejected(t *testing.T) {
	h := newHarness(t, &FakeExecutor{}, testConfig()) // senza capability: nessun probe chiesto
	ws := h.open(t)
	sendProbe(t, ws, 1, 10)
	if e := readError(t, ws); e.Code != api.ErrCodeInvalidFrame {
		t.Errorf("probe non richiesto: codice %q, atteso invalid_frame", e.Code)
	}
}

func TestNoProbeWithoutCapability(t *testing.T) {
	h, _ := probeHarness(t)
	h.sessions.state.Probe = false
	ws := h.open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, msg, err := ws.Read(ctx); err == nil {
		t.Errorf("senza capability nessun messaggio atteso, ricevuto %s", msg)
	}
}
