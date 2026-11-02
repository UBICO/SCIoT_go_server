package offload

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/costmodel"
	"github.com/UBICO/SCIoT_go_server/internal/netmeasure"
)

func f(v float64) *float64 { return &v }
func ip(v int) *int        { return &v }

func state(policy api.Policy) State {
	bw, lat := 1e6, 2.0
	return State{
		DeviceID: "d", ModelKey: "m", Last: 2, Policy: policy,
		Inputs: costmodel.Inputs{
			DeviceMS:   []*float64{f(10), f(20), f(30)},
			Network:    &netmeasure.State{BandwidthBps: &bw, LatencyMS: &lat},
			InputBytes: map[int]int{-1: 50_000, 0: 20_000, 1: 10_000},
			ServiceMS:  map[int]float64{-1: 12, 0: 8, 1: 4},
			Queue:      costmodel.Queue{Slots: 2, CA: 1, CS: 1},
		},
	}
}

func TestStaticKeepsThePolicy(t *testing.T) {
	for _, p := range []api.Policy{
		{Mode: api.ModeLocal, PolicyVersion: 1},
		{Mode: api.ModeSplit, SplitPoint: ip(1), PolicyVersion: 3},
	} {
		s := state(p)
		d := Static{}.Decide(time.Unix(0, 0), s)
		if d.Changes(p) || d.Mode != p.Mode || d.Reason != "static" {
			t.Errorf("policy %+v: decisione %+v", p, d)
		}
		if len(d.Predictions) != s.Last+2 || d.Predictions[0].Split != -1 || !d.Predictions[s.Last+1].Local {
			t.Errorf("previsioni %+v: attese −1..Last", d.Predictions)
		}
	}
}

func TestChanges(t *testing.T) {
	local := api.Policy{Mode: api.ModeLocal}
	split1 := api.Policy{Mode: api.ModeSplit, SplitPoint: ip(1)}
	cases := []struct {
		d    Decision
		p    api.Policy
		want bool
	}{
		{Decision{Mode: api.ModeLocal}, local, false},
		{Decision{Mode: api.ModeSplit, SplitPoint: ip(1)}, local, true},
		{Decision{Mode: api.ModeSplit, SplitPoint: ip(1)}, split1, false},
		{Decision{Mode: api.ModeSplit, SplitPoint: ip(0)}, split1, true},
		{Decision{Mode: api.ModeLocal}, split1, true},
	}
	for _, c := range cases {
		if got := c.d.Changes(c.p); got != c.want {
			t.Errorf("%+v su %+v: Changes = %v, atteso %v", c.d, c.p, got, c.want)
		}
	}
}

func TestOthers(t *testing.T) {
	// 2 slot al 60%: 1.2 erlang, 100 frame/s ammessi. Il device in SPLIT
	// invia 20 frame/s con S = 10 ms: 0.2 erlang suoi.
	l, e := Others(100, 0.6, 2, 20, 10, true)
	if math.Abs(l-80) > 1e-9 || math.Abs(e-1.0) > 1e-9 {
		t.Errorf("in SPLIT: %v frame/s, %v erlang; attesi 80 e 1.0", l, e)
	}
	// In LOCAL il device non carica il server: tutto è degli altri.
	l, e = Others(100, 0.6, 2, 20, 10, false)
	if l != 100 || math.Abs(e-1.2) > 1e-9 {
		t.Errorf("in LOCAL: %v, %v; attesi 100 e 1.2", l, e)
	}
	// Finestre diverse: la differenza non scende sotto zero.
	if l, e := Others(10, 0.05, 2, 20, 10, true); l != 0 || e != 0 {
		t.Errorf("sotto zero: %v, %v; attesi 0 e 0", l, e)
	}
}

func TestFramesPerSec(t *testing.T) {
	if got := FramesPerSec(50, 1000); got != 50 {
		t.Errorf("50 in 1 s = %v", got)
	}
	if got := FramesPerSec(10, 500); got != 20 {
		t.Errorf("10 in 0.5 s = %v", got)
	}
	if FramesPerSec(10, 0) != 0 || FramesPerSec(0, 1000) != 0 || FramesPerSec(-1, 1000) != 0 {
		t.Error("finestra vuota o senza durata: atteso 0")
	}
}

func TestCycleMS(t *testing.T) {
	// Modello di 3 segmenti, SPLIT a 1: 20 frame in 1 s; 4 finiti sul device
	// (segmento 2 eseguito 4 volte), 16 inviati con 5 ms di invio.
	tel := api.Telemetry{
		InferenceCount: 20,
		SegmentMS:      []*float64{f(10), f(20), f(30)},
		SegmentCount:   []int{20, 20, 4},
	}
	// Calcolo 200 + 400 + 120 = 720 ms; invio 16 × 5 = 80; A = 200/20 = 10.
	if a, ok := CycleMS(tel, 1000, true, 2, 5, true); !ok || math.Abs(a-10) > 1e-9 {
		t.Errorf("SPLIT: %v, %v; attesi 10 ms", a, ok)
	}
	// In LOCAL nessun invio: (1000 − 720) / 20 = 14, anche senza la rete.
	if a, ok := CycleMS(tel, 1000, false, 2, 0, false); !ok || math.Abs(a-14) > 1e-9 {
		t.Errorf("LOCAL: %v, %v; attesi 14 ms", a, ok)
	}
	// Frame inviati e invio sconosciuto: A non è calcolabile.
	if _, ok := CycleMS(tel, 1000, true, 2, 0, false); ok {
		t.Error("invio sconosciuto: atteso ok = false")
	}
	// Un segmento eseguito senza tempo, o telemetria senza segmenti.
	bad := tel
	bad.SegmentMS = []*float64{f(10), nil, f(30)}
	if _, ok := CycleMS(bad, 1000, true, 2, 5, true); ok {
		t.Error("segmento senza tempo: atteso ok = false")
	}
	if _, ok := CycleMS(api.Telemetry{InferenceCount: 20}, 1000, false, 2, 0, false); ok {
		t.Error("senza segment_ms: atteso ok = false")
	}
	// Orologi diversi: mai sotto zero.
	if a, ok := CycleMS(tel, 500, false, 2, 0, false); !ok || a != 0 {
		t.Errorf("finestra più corta del calcolo: %v, %v; atteso 0", a, ok)
	}
}

func TestRecordPredictionsAreValidJSON(t *testing.T) {
	ps := []costmodel.Prediction{
		{Split: 1, TotalMS: 40, WaitMS: 1, Rho: 0.3},
		{Split: -1, TotalMS: math.Inf(1), WaitMS: math.Inf(1), Rho: math.Inf(1), Saturated: true},
	}
	rp := RecordPredictions(ps)
	if rp[0].TotalMS == nil || *rp[0].TotalMS != 40 || rp[1].TotalMS != nil || rp[1].WaitMS != nil || !rp[1].Saturated {
		t.Errorf("previsioni = %+v", rp)
	}
	if _, err := json.Marshal(rp); err != nil {
		t.Errorf("JSON: %v (un +Inf non è rappresentabile)", err)
	}
}
