package replay

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// recording: un heartbeat con 2 slot, poi n frame eseguiti, uno ogni gapMS a
// partire da startMS, con servizio serviceMS e attesa waitMS. Gli esiti sono
// scritti in ordine inverso: l'ordine di arrivo va ricostruito.
func recording(startMS, gapMS float64, n int, serviceMS, waitMS float64) string {
	var b strings.Builder
	b.WriteString(`{"kind":"start","t_ms":0}` + "\n")
	b.WriteString(`{"kind":"frame","t_ms":1,"outcome":"result","timing":{"server_ms":1,"queue_wait_ms":0}}` + "\n") // prima degli slot: ignorato
	b.WriteString(`{"kind":"heartbeat","t_ms":2,"queue":{"slots":2}}` + "\n")
	for i := n - 1; i >= 0; i-- {
		server := serviceMS + waitMS
		arrival := startMS + float64(i)*gapMS
		fmt.Fprintf(&b, `{"kind":"frame","t_ms":%g,"outcome":"result","timing":{"server_ms":%g,"queue_wait_ms":%g}}`+"\n",
			arrival+server, server, waitMS)
	}
	b.WriteString(`{"kind":"frame","t_ms":9000,"outcome":"server_busy"}` + "\n") // non eseguito: ignorato
	return b.String()
}

func TestQueueWindowsMeasures(t *testing.T) {
	// 50 frame nella finestra [0, 5) s, uno ogni 100 ms: λ = 10/s.
	ws, err := QueueWindows(strings.NewReader(recording(0, 100, 50, 10, 1)), 5, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 {
		t.Fatalf("finestre = %+v, attesa 1", ws)
	}
	w := ws[0]
	if w.Frames != 50 || w.Slots != 2 || !near(w.Lambda, 10) || !near(w.ServiceMS, 10) || !near(w.MeasuredMS, 1) {
		t.Errorf("finestra = %+v", w)
	}
	// Arrivi regolari e servizio costante: ca = cs = 0, quindi G/G/c prevede
	// attesa nulla; M/M/c no.
	if !near(w.CA, 0) || !near(w.CS, 0) || !near(w.GGcMS, 0) || !(w.MMcMS > 0) {
		t.Errorf("ca %v cs %v GGc %v MMc %v", w.CA, w.CS, w.GGcMS, w.MMcMS)
	}
	if !near(w.Rho, 10*0.010/2) {
		t.Errorf("rho = %v, atteso 0.05", w.Rho)
	}
}

func TestQueueWindowsSplitsAndFilters(t *testing.T) {
	// 80 frame ogni 100 ms da 0: [0,5) ne ha 50, [5,10) 30. Con min 40 resta
	// solo la prima.
	ws, err := QueueWindows(strings.NewReader(recording(0, 100, 80, 10, 1)), 5, 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].StartS != 0 || ws[0].Frames != 50 {
		t.Errorf("finestre = %+v", ws)
	}
	ws, _ = QueueWindows(strings.NewReader(recording(0, 100, 80, 10, 1)), 5, 20)
	if len(ws) != 2 || ws[1].StartS != 5 || ws[1].Frames != 30 {
		t.Errorf("finestre = %+v", ws)
	}
}

func TestQueueWindowsUnstable(t *testing.T) {
	// 2 slot, S = 300 ms, λ = 10/s: ρ = 1.5, la previsione non è finita.
	ws, err := QueueWindows(strings.NewReader(recording(0, 100, 50, 300, 50)), 5, 20)
	if err != nil {
		t.Fatal(err)
	}
	if w := ws[0]; w.Rho < 1 || !math.IsInf(w.MMcMS, 1) || !math.IsInf(w.GGcMS, 1) {
		t.Errorf("finestra instabile = %+v", w)
	}
	b := Buckets(ws)
	if last := b[len(b)-1]; last.Windows != 1 || !near(last.MeasuredMS, 50) {
		t.Errorf("fascia instabile = %+v", last)
	}
}

func TestBuckets(t *testing.T) {
	ws := []QueueWindow{
		{Rho: 0.1, MeasuredMS: 1, MMcMS: 2, GGcMS: 1},
		{Rho: 0.3, MeasuredMS: 3, MMcMS: 4, GGcMS: 3},
		{Rho: 0.9, MeasuredMS: 10, MMcMS: 20, GGcMS: 12},
	}
	b := Buckets(ws)
	if b[0].Windows != 2 || !near(b[0].MeasuredMS, 2) || !near(b[0].MMcMS, 3) || b[2].Windows != 1 || b[1].Windows != 0 {
		t.Errorf("fasce = %+v", b)
	}
}

func TestBadLine(t *testing.T) {
	if _, err := QueueWindows(strings.NewReader("{\"kind\":\n"), 5, 1); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("err = %v: atteso un errore con il numero di riga", err)
	}
}
