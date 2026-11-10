package offload

import (
	"encoding/json"
	"math"
	"slices"
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
		if d.Changes(p, 0) || d.Mode != p.Mode || d.Reason != "static" {
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
		// Finestra, con massimo 3: nil vale 3.
		{Decision{Mode: api.ModeSplit, SplitPoint: ip(1), MaxInflight: ip(3)}, split1, false},
		{Decision{Mode: api.ModeSplit, SplitPoint: ip(1), MaxInflight: ip(2)}, split1, true},
		{Decision{Mode: api.ModeSplit, SplitPoint: ip(1)}, api.Policy{Mode: api.ModeSplit, SplitPoint: ip(1), MaxInflight: ip(2)}, true},
		{Decision{Mode: api.ModeSplit, SplitPoint: ip(1), MaxInflight: ip(2)}, api.Policy{Mode: api.ModeSplit, SplitPoint: ip(1), MaxInflight: ip(2)}, false},
	}
	for _, c := range cases {
		if got := c.d.Changes(c.p, 3); got != c.want {
			t.Errorf("%+v su %+v: Changes = %v, atteso %v", c.d, c.p, got, c.want)
		}
	}
}

func TestPredictAllWindows(t *testing.T) {
	s := state(api.Policy{Mode: api.ModeLocal})
	s.MaxWindow = 3
	s.Inputs.CycleMS = f(1)
	ps := PredictAll(s)
	// Split −1..1 × finestre 3, 2, 1, poi LOCAL.
	var got []candidate
	for _, p := range ps {
		got = append(got, candidate{p.Split, p.Window})
	}
	want := []candidate{{-1, 3}, {-1, 2}, {-1, 1}, {0, 3}, {0, 2}, {0, 1}, {1, 3}, {1, 2}, {1, 1}, {2, 0}}
	if !slices.Equal(got, want) {
		t.Fatalf("candidati %v, attesi %v", got, want)
	}
	if !ps[len(ps)-1].Local {
		t.Error("l'ultima previsione deve essere LOCAL")
	}
	// La finestra entra nella simulazione: allo split −1 con 2 slot, una
	// finestra 3 accoda il device dietro ai propri frame più di una 1.
	if !(ps[0].WaitMS >= ps[2].WaitMS) {
		t.Errorf("attesa con finestra 3 = %v, con 1 = %v", ps[0].WaitMS, ps[2].WaitMS)
	}
	if i := IndexOf(ps, 0, 2); i != 4 {
		t.Errorf("IndexOf(0, 2) = %d, atteso 4", i)
	}
	if i := IndexOf(ps, 2, 7); i != 9 {
		t.Errorf("IndexOf(LOCAL) = %d, atteso 9: la finestra non conta", i)
	}
	if i := IndexOf(ps, 0, 4); i != -1 {
		t.Errorf("IndexOf(finestra oltre il massimo) = %d", i)
	}
	// Senza massimo noto: una previsione per split, che lo segna mancante.
	s.MaxWindow = 0
	ps = PredictAll(s)
	if len(ps) != s.Last+2 || !slices.Contains(ps[0].Missing, "max_inflight") {
		t.Errorf("senza massimo: %d previsioni, mancanti %v", len(ps), ps[0].Missing)
	}
}

func ptr(v float64) *float64 { return &v }

func TestOthers(t *testing.T) {
	// 2 slot al 60%: 1.2 erlang, 100 frame/s ammessi. Il device elabora 20
	// frame/s (S = 10 ms), ma il dispatcher ne ha ammessi 14, per 0.15 erlang:
	// gli altri 6 sono stati rifiutati o non inviati (finestra piena).
	l, e := Others(100, 0.6, 2, ptr(14), ptr(0.15), 20, 10, true)
	if math.Abs(l-86) > 1e-9 || math.Abs(e-1.05) > 1e-9 {
		t.Errorf("con il carico del device: %v frame/s, %v erlang; attesi 86 e 1.05", l, e)
	}
	// Tornato in LOCAL da poco: i suoi frame sono ancora nella finestra.
	l, e = Others(100, 0.6, 2, ptr(5), ptr(0.05), 20, 10, false)
	if math.Abs(l-95) > 1e-9 || math.Abs(e-1.15) > 1e-9 {
		t.Errorf("in LOCAL con frame nella finestra: %v, %v; attesi 95 e 1.15", l, e)
	}
	// Mai sotto zero.
	if l, e := Others(10, 0.05, 2, ptr(12), ptr(0.2), 20, 10, true); l != 0 || e != 0 {
		t.Errorf("sotto zero: %v, %v; attesi 0 e 0", l, e)
	}

	// Ripiego (registrazioni senza i campi): la formula di prima.
	l, e = Others(100, 0.6, 2, nil, nil, 20, 10, true)
	if math.Abs(l-80) > 1e-9 || math.Abs(e-1.0) > 1e-9 {
		t.Errorf("ripiego in SPLIT: %v frame/s, %v erlang; attesi 80 e 1.0", l, e)
	}
	l, e = Others(100, 0.6, 2, nil, nil, 20, 10, false)
	if l != 100 || math.Abs(e-1.2) > 1e-9 {
		t.Errorf("ripiego in LOCAL: %v, %v; attesi 100 e 1.2", l, e)
	}
	if l, e := Others(10, 0.05, 2, nil, nil, 20, 10, true); l != 0 || e != 0 {
		t.Errorf("ripiego sotto zero: %v, %v; attesi 0 e 0", l, e)
	}
	// Uno solo dei due campi: ripiego (non si mescolano le due formule).
	l, _ = Others(100, 0.6, 2, ptr(14), nil, 20, 10, true)
	if math.Abs(l-80) > 1e-9 {
		t.Errorf("un campo solo: %v frame/s, atteso il ripiego 80", l)
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
