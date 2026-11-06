package costmodel

import "testing"

// Fase 3.8: gli altri device come client della simulazione.

func TestClientFrom(t *testing.T) {
	in := inputs()
	// Split 1 di 3 segmenti: prefisso 10+20, suffisso 30, invio 2 ms di
	// latenza + 10 KB a 1e6 B/s, S = 4, ciclo 2, ritorno 2 (latenza).
	c, ok := ClientFrom(in, 1, 3, 2)
	want := Client{P: 30, X: 30, U: 12, A: 2, S: 4, DownMS: 2, Window: 3}
	if !ok || c != want {
		t.Errorf("ClientFrom = %+v, %v; atteso %+v", c, ok, want)
	}
	if _, ok := ClientFrom(in, 2, 3, 2); ok {
		t.Error("LOCAL (split = last) accettato: non carica il server")
	}
	if _, ok := ClientFrom(in, 1, 0, 2); ok {
		t.Error("finestra 0 accettata")
	}
	noSeg := inputs()
	noSeg.DeviceMS[2] = nil
	if _, ok := ClientFrom(noSeg, 1, 3, 2); ok {
		t.Error("segmento del suffisso sconosciuto accettato")
	}
	noCycle := inputs()
	noCycle.CycleMS = nil
	if _, ok := ClientFrom(noCycle, 1, 3, 2); ok {
		t.Error("ciclo sconosciuto accettato")
	}
	noService := inputs()
	delete(noService.ServiceMS, 1)
	if _, ok := ClientFrom(noService, 1, 3, 2); ok {
		t.Error("S sconosciuta accettata")
	}
}

// crowded: split 1 in localhost con S = 30 ms su 2 slot e suffisso 40 ms
// (la scadenza). Un device da solo invia un frame ogni ~32 ms (ciclo 2 +
// prefisso 30): circa 0.94 erlang, il server basta. Tre device uguali
// chiedono ~2.8 erlang: troppo.
func crowded() Inputs {
	in := localhost(inputs())
	in.ServiceMS[1] = 30
	in.DeviceMS[2] = f(40)
	in.Queue.CS = 0
	return in
}

func TestSimOtherClientsShareTheServer(t *testing.T) {
	in := crowded()
	alone := Predict(in, 1, 2)
	if alone.RejectedPerSec != 0 || alone.ServerShare != 1 {
		t.Fatalf("da solo = %+v: attesi tutti i frame al server", alone)
	}
	c, ok := ClientFrom(in, 1, 3, 2)
	if !ok {
		t.Fatal("ClientFrom sugli ingressi di crowded")
	}
	in.Queue.Clients = []Client{c, c}
	p := Predict(in, 1, 2)
	// Gli altri inviano appena possono: la coda è piena e il device viene
	// rifiutato.
	if !(p.RejectedPerSec > 0) || !(p.ServerShare < 1) || !(p.Rho > alone.Rho) {
		t.Errorf("con due client uguali = %+v: attesi rifiuti e ρ più alto", p)
	}
	// I tre sono uguali: il server ne serve tanti frame quanti del device,
	// quindi ρ ≈ 3 × frame serviti del device × S / slot.
	if want := 3 * p.ServerFramesPerSec * 0.030 / 2; !within(p.Rho, want, 0.15) {
		t.Errorf("ρ = %v, atteso ≈ %v (gli altri come il device)", p.Rho, want)
	}
	// Riproducibile anche con i client.
	if q := Predict(in, 1, 2); q.P95MS != p.P95MS || q.ServerFramesPerSec != p.ServerFramesPerSec {
		t.Errorf("stessi ingressi, risultati diversi: %+v / %+v", p, q)
	}
}

func TestSimClientsReplaceTheStream(t *testing.T) {
	// Con i client il flusso al ritmo misurato non si usa: un OthersLambda
	// enorme non cambia nulla (i due carichi non si sommano).
	in := crowded()
	c, _ := ClientFrom(in, 1, 3, 2)
	in.Queue.Clients = []Client{c}
	a := Predict(in, 1, 2)
	in.Queue.OthersLambda, in.Queue.OthersErlangs = 50, 1.5
	b := Predict(in, 1, 2)
	if a.ServerFramesPerSec != b.ServerFramesPerSec || a.P95MS != b.P95MS {
		t.Errorf("con i client il flusso conta: %+v / %+v", a, b)
	}
}
