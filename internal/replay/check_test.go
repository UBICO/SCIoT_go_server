package replay

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/costmodel"
)

// clientCSV: le colonne del client (FrameRecord), in quell'ordine.
const clientCSV = `t_s,captured_at,seq,policy_version,mode,split,acq_ms,dev_ms,srv_ms,recv_ms,net_ms,rtt_ms,sent_bytes,detections,latency_ms,outcome
0.1,2026-10-28T10:00:00.100Z,,0,LOCAL,,0,70,,,,,0,1,75,local
0.4,2026-10-28T10:00:00.400Z,1,2,SPLIT,1,0,30,5,10,2,,10000,1,50,result
0.9,2026-10-28T10:00:00.900Z,2,2,SPLIT,1,0,30,,,,,10000,1,90,local (server_busy)
1.5,2026-10-28T10:00:01.500Z,3,2,SPLIT,1,0,30,,,,,0,1,60,local (window_full)
`

func TestReadClientFrames(t *testing.T) {
	fs, err := ReadClientFrames(strings.NewReader(clientCSV))
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 4 {
		t.Fatalf("frame = %d, attesi 4", len(fs))
	}
	want := time.Date(2026, 10, 28, 10, 0, 0, 400*int(time.Millisecond), time.UTC)
	if f := fs[1]; !f.At.Equal(want) || f.LatencyMS != 50 || f.Outcome != outcomeResult {
		t.Errorf("frame 1 = %+v", f)
	}
	if fs[2].Outcome != outcomeServerBusy {
		t.Errorf("esito = %q", fs[2].Outcome)
	}
	if _, err := ReadClientFrames(strings.NewReader("t_s,captured_at\n0.1,2026-10-28T10:00:00Z\n")); err == nil {
		t.Error("CSV senza latency_ms e outcome accettato")
	}
	if _, err := ReadClientFrames(strings.NewReader(strings.Replace(clientCSV, ",50,result", ",x,result", 1))); err == nil {
		t.Error("latency_ms non numerica accettata")
	}
}

func TestMeasureWindow(t *testing.T) {
	fs, err := ReadClientFrames(strings.NewReader(clientCSV))
	if err != nil || len(fs) != 4 {
		t.Fatalf("ReadClientFrames: %d frame, %v", len(fs), err)
	}
	from := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	m := measureWindow(fs, from, from.Add(time.Second)) // i primi tre
	if m.Frames != 3 || !near(m.FPS, 3) || !near(m.MeanMS, 215.0/3) || m.P95MS != 90 {
		t.Errorf("finestra = %+v", m)
	}
	if !near(m.ServerShare, 1.0/3) || !near(m.RejectedPerSec, 1) || m.WindowFullPerSec != 0 {
		t.Errorf("esiti = %+v", m)
	}
	// Estremi: (from, to]. Un frame acquisito esattamente a to entra.
	at := fs[3].At
	if m := measureWindow(fs, fs[2].At, at); m.Frames != 1 || !near(m.WindowFullPerSec, 1/0.6) {
		t.Errorf("frame a to: %+v", m)
	}
	if m := measureWindow(fs, at, at.Add(time.Second)); m.Frames != 0 {
		t.Errorf("frame a from: %+v", m)
	}
}

func TestCheckJoinsRecordAndClient(t *testing.T) {
	fs, err := ReadClientFrames(strings.NewReader(clientCSV))
	if err != nil {
		t.Fatal(err)
	}
	// algoRecording (algo_test.go): heartbeat di d a 10:00:01 e 10:00:02,
	// finestre di 1 s. Nella prima 3 frame, nella seconda 1 (< minFrames).
	ws, err := Check(strings.NewReader(algoRecording), map[string][]ClientFrame{"d": fs}, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].DeviceID != "d" || ws[0].TMS != 1000 || ws[0].Meas.Frames != 3 {
		t.Fatalf("finestre = %+v", ws)
	}
	if p := ws[0].Pred; p.Split != 1 || p.Window != 3 {
		t.Errorf("previsione per %d/%d, attesa 1/3", p.Split, p.Window)
	}
	// Con il warmup di 1 s la prima finestra (t = 0 dal primo heartbeat)
	// esce, la seconda non ha abbastanza frame.
	if ws, _ := Check(strings.NewReader(algoRecording), map[string][]ClientFrame{"d": fs}, time.Second, 2); len(ws) != 0 {
		t.Errorf("con warmup: %d finestre", len(ws))
	}
}

// win: una finestra con ρ previsto rho, p95 previsto pred e misurato meas.
func win(rho, pred, meas float64) CheckWindow {
	return CheckWindow{
		Pred: costmodel.Prediction{Rho: rho, P95MS: pred, TotalMS: pred, FramesPerSec: 10, ServerShare: 0.5},
		Meas: ClientWindow{Frames: 10, P95MS: meas, MeanMS: meas, FPS: 10, ServerShare: 0},
	}
}

func TestCheckBucketsAndRhoFrom(t *testing.T) {
	ws := []CheckWindow{
		win(0.3, 105, 100), win(0.4, 95, 100), win(0.45, 100, 100), // errori +5%, −5%, 0
		win(0.6, 108, 100), win(0.7, 92, 100), // |8%|
		win(0.85, 130, 100), win(0.9, 60, 100), win(0.9, 125, 100), // 30%, −40%, 25%
	}
	bs := CheckBuckets(ws)
	if len(bs) != len(rhoLabels) {
		t.Fatalf("fasce = %d, attese %d", len(bs), len(rhoLabels))
	}
	if bs[0].Windows != 3 || !near(bs[0].P95, 0) || !near(bs[0].AbsP95, 0.05) || !near(bs[0].FPS, 0) {
		t.Errorf("fascia 0 = %+v", bs[0])
	}
	// Mediana con il rango più vicino (costmodel.Percentile): di [−0.4, 0.25, 0.3] è 0.25.
	if bs[2].Windows != 3 || !near(bs[2].P95, 0.25) || !near(bs[2].AbsP95, 0.3) {
		t.Errorf("fascia 2 = %+v", bs[2])
	}
	// ServerShare misurato 0: errore saltato, nessun valore.
	if !math.IsNaN(bs[0].Share) {
		t.Errorf("quota con misurato 0 = %v, attesa NaN", bs[0].Share)
	}
	if bs[3].Windows != 0 || !math.IsNaN(bs[3].AbsP95) {
		t.Errorf("fascia vuota = %+v", bs[3])
	}

	if rho, ok := RhoFrom(bs, 0.1); !ok || rho != 0.8 {
		t.Errorf("RhoFrom(0.1) = %v, %v; atteso 0.8", rho, ok)
	}
	if rho, ok := RhoFrom(bs, 0.06); !ok || rho != 0.5 {
		t.Errorf("RhoFrom(0.06) = %v, %v; atteso 0.5", rho, ok)
	}
	if _, ok := RhoFrom(bs, 0.5); ok {
		t.Error("RhoFrom(0.5): nessuna fascia supera il margine, atteso ok = false")
	}
}
