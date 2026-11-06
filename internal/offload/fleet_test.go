package offload

import (
	"fmt"
	"testing"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/costmodel"
)

func split(k, w int) api.Policy {
	return api.Policy{Mode: api.ModeSplit, SplitPoint: ip(k), MaxInflight: ip(w)}
}

var local = api.Policy{Mode: api.ModeLocal}

// member: lo stato di un device con gli ingressi completi per ClientFrom.
func memberState(id string, p api.Policy) State {
	st := state(p)
	st.DeviceID, st.MaxWindow = id, 3
	st.Inputs.CycleMS = f(2)
	st.Inputs.MaxInflight = st.Window()
	return st
}

func TestFleetClients(t *testing.T) {
	fl := NewFleet(3 * time.Second)
	t0 := time.Unix(1000, 0)
	fl.Update(t0, memberState("a", split(1, 2)))
	fl.Update(t0, memberState("b", split(0, 3)))
	fl.Update(t0, memberState("c", local))

	cs, ok := fl.Clients(t0, "a")
	// Per a: solo b (c è in LOCAL, a è lui stesso). b a split 0: prefisso 10.
	if !ok || len(cs) != 1 || cs[0].P != 10 || cs[0].Window != 3 {
		t.Fatalf("altri di a = %+v, %v; atteso b a split 0, finestra 3", cs, ok)
	}
	cs, _ = fl.Clients(t0, "c")
	if len(cs) != 2 || cs[0].P != 30 || cs[1].P != 10 {
		t.Errorf("altri di c = %+v; attesi a poi b (ordine per id)", cs)
	}
	if cs, ok := fl.Clients(t0, "z"); !ok || len(cs) != 2 {
		t.Errorf("device sconosciuto: %d altri, %v", len(cs), ok)
	}
}

func TestFleetIncompleteFallsBack(t *testing.T) {
	fl := NewFleet(3 * time.Second)
	t0 := time.Unix(1000, 0)
	fl.Update(t0, memberState("a", split(1, 2)))
	b := memberState("b", split(1, 2))
	b.Inputs.CycleMS = nil // ciclo sconosciuto
	fl.Update(t0, b)
	if cs, ok := fl.Clients(t0, "a"); ok || cs != nil {
		t.Errorf("con un altro incompleto: %+v, %v; atteso nil, false (ripiego)", cs, ok)
	}
	if _, ok := fl.Clients(t0, "b"); !ok {
		t.Error("per b l'unico altro (a) è completo")
	}
}

func TestFleetExpiresAndForgets(t *testing.T) {
	fl := NewFleet(3 * time.Second)
	t0 := time.Unix(1000, 0)
	fl.Update(t0, memberState("a", split(1, 2)))
	fl.Update(t0, memberState("b", split(1, 2)))
	if cs, _ := fl.Clients(t0.Add(3*time.Second), "a"); len(cs) != 1 {
		t.Errorf("a 3 s: %d altri, atteso 1 (non ancora scaduto)", len(cs))
	}
	if cs, _ := fl.Clients(t0.Add(3*time.Second+time.Millisecond), "a"); len(cs) != 0 {
		t.Errorf("oltre 3 s: %d altri, atteso 0", len(cs))
	}
	fl.Update(t0, memberState("c", split(1, 2)))
	fl.Forget("c")
	if cs, _ := fl.Clients(t0, "a"); len(cs) != 0 {
		t.Errorf("dopo Forget: %d altri", len(cs))
	}
}

func TestFleetSetPolicy(t *testing.T) {
	fl := NewFleet(3 * time.Second)
	t0 := time.Unix(1000, 0)
	fl.Update(t0, memberState("b", local))
	if cs, _ := fl.Clients(t0, "a"); len(cs) != 0 {
		t.Fatalf("b in LOCAL: %d altri", len(cs))
	}
	// Il server porta b a SPLIT 1/2: chi decide dopo lo vede subito.
	fl.SetPolicy("b", split(1, 2))
	cs, _ := fl.Clients(t0, "a")
	if len(cs) != 1 || cs[0].P != 30 || cs[0].Window != 2 {
		t.Errorf("dopo SetPolicy: %+v; atteso b a split 1, finestra 2", cs)
	}
	// Senza finestra nella policy: il massimo.
	fl.SetPolicy("b", api.Policy{Mode: api.ModeSplit, SplitPoint: ip(1)})
	if cs, _ := fl.Clients(t0, "a"); len(cs) != 1 || cs[0].Window != 3 {
		t.Errorf("policy senza finestra: %+v; attesa la finestra massima 3", cs)
	}
	fl.SetPolicy("ghost", split(1, 2)) // sconosciuto: nessun effetto
	if cs, _ := fl.Clients(t0, "a"); len(cs) != 1 {
		t.Errorf("SetPolicy di uno sconosciuto ha aggiunto un device: %d altri", len(cs))
	}
}

// BenchmarkPredictAllClients: il costo di una decisione (tutte le coppie)
// con n altri device simulati come client.
func BenchmarkPredictAllClients(b *testing.B) {
	for _, n := range []int{0, 1, 3, 7} {
		b.Run(fmt.Sprintf("others=%d", n), func(b *testing.B) {
			st := memberState("a", split(1, 2))
			st.Inputs.ServiceMS[1] = 30
			other := memberState("o", split(1, 2))
			c, ok := clientOf(other)
			if n > 0 && !ok {
				b.Skip("ClientFrom non ancora scritto")
			}
			for range n {
				st.Inputs.Queue.Clients = append(st.Inputs.Queue.Clients, c)
			}
			for b.Loop() {
				PredictAll(st)
			}
		})
	}
}

func clientOf(st State) (costmodel.Client, bool) {
	return costmodel.ClientFrom(st.Inputs, st.Split(), st.Window(), st.Last)
}
