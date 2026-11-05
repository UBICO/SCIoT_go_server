package replay

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/costmodel"
	"github.com/UBICO/SCIoT_go_server/internal/offload"
)

// Verifica della previsione con le misure del client (Fase 3.8): per ogni
// heartbeat di un device in SPLIT, la previsione della simulazione per la
// coppia (split, finestra) applicata contro ciò che il client ha misurato
// nella stessa finestra (il CSV dei frame, docs/CLIENT.md). Il server non
// vede i frame finiti sul device; il client sì: qui entrano tutti.

// ClientFrame è una riga del CSV dei frame del client
// (client/sciot_client/split_pipeline.py, FrameRecord): solo le colonne
// usate qui.
type ClientFrame struct {
	At        time.Time // captured_at: quando il frame è stato acquisito (UTC)
	LatencyMS float64   // dall'acquisizione al risultato
	// Outcome: "result" (risposta del server), "local" (LOCAL), oppure
	// "local (<motivo>)" per un frame SPLIT finito sul device: server_busy e
	// deadline_exceeded (rifiutati o scaduti sul server), window_full.
	Outcome string
}

// Esiti del CSV del client.
const (
	outcomeResult     = "result"
	outcomeServerBusy = "local (server_busy)"
	outcomeExpired    = "local (deadline_exceeded)"
	outcomeWindowFull = "local (window_full)"
)

// ReadClientFrames legge il CSV dei frame di un device.
func ReadClientFrames(r io.Reader) ([]ClientFrame, error) {
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("replay: CSV del client: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("replay: CSV del client vuoto")
	}
	col := map[string]int{"captured_at": -1, "latency_ms": -1, "outcome": -1}
	for i, name := range rows[0] {
		if _, ok := col[name]; ok {
			col[name] = i
		}
	}
	for name, i := range col {
		if i < 0 {
			return nil, fmt.Errorf("replay: CSV del client senza la colonna %s", name)
		}
	}
	out := make([]ClientFrame, 0, len(rows)-1)
	for n, row := range rows[1:] {
		at, err := time.Parse(time.RFC3339Nano, row[col["captured_at"]])
		if err != nil {
			return nil, fmt.Errorf("replay: CSV del client, riga %d: captured_at: %w", n+2, err)
		}
		lat, err := strconv.ParseFloat(row[col["latency_ms"]], 64)
		if err != nil {
			return nil, fmt.Errorf("replay: CSV del client, riga %d: latency_ms: %w", n+2, err)
		}
		out = append(out, ClientFrame{At: at, LatencyMS: lat, Outcome: row[col["outcome"]]})
	}
	return out, nil
}

// ClientWindow è ciò che il client ha misurato in una finestra.
type ClientWindow struct {
	Frames int
	// FPS: frame al secondo; MeanMS e P95MS: latenza di tutti i frame (P95
	// con costmodel.Percentile, lo stesso metodo della simulazione);
	// ServerShare: quota con esito "result"; RejectedPerSec: rifiutati o
	// scaduti al secondo (server_busy + deadline_exceeded, come
	// Prediction.RejectedPerSec); WindowFullPerSec: finestra piena al secondo.
	FPS, MeanMS, P95MS, ServerShare, RejectedPerSec, WindowFullPerSec float64
}

// measureWindow misura i frame acquisiti in (from, to].
func measureWindow(frames []ClientFrame, from, to time.Time) ClientWindow {
	var lat []float64
	var sum float64
	var result, rejected, full int
	for _, f := range frames {
		if !f.At.After(from) || f.At.After(to) {
			continue
		}
		lat = append(lat, f.LatencyMS)
		sum += f.LatencyMS
		switch f.Outcome {
		case outcomeResult:
			result++
		case outcomeServerBusy, outcomeExpired:
			rejected++
		case outcomeWindowFull:
			full++
		}
	}
	if len(lat) == 0 {
		return ClientWindow{}
	}
	n, sec := float64(len(lat)), to.Sub(from).Seconds()
	return ClientWindow{
		Frames:           len(lat),
		FPS:              n / sec,
		MeanMS:           sum / n,
		P95MS:            costmodel.Percentile(lat, 0.95),
		ServerShare:      float64(result) / n,
		RejectedPerSec:   float64(rejected) / sec,
		WindowFullPerSec: float64(full) / sec,
	}
}

// CheckWindow è il confronto in una finestra di heartbeat.
type CheckWindow struct {
	DeviceID string
	TMS      float64              // t_ms dell'heartbeat
	Pred     costmodel.Prediction // per la coppia applicata
	Meas     ClientWindow
}

// Check confronta le previsioni con il client, heartbeat per heartbeat.
// clients: device → i suoi frame. Restano solo gli heartbeat dopo warmup
// (dal primo heartbeat del device), in SPLIT con la policy del server
// applicata, con una previsione completa (Missing vuoto) e almeno minFrames
// frame del client nella finestra.
//
// ρ della fascia è Pred.Rho, quello previsto: è il ρ che il margine
// dell'algoritmo legge (offload.Latency.margin), quindi l'asse su cui va
// scelto queue_rho_from.
func Check(record io.Reader, clients map[string][]ClientFrame, warmup time.Duration, minFrames int) ([]CheckWindow, error) {
	// Static non cambia la policy e restituisce le previsioni di ogni coppia.
	steps, err := Algo(record, offload.Static{})
	if err != nil {
		return nil, err
	}
	first := map[string]float64{} // device → t_ms del primo heartbeat
	var out []CheckWindow
	for _, s := range steps {
		hb := s.Heartbeat
		if _, ok := first[hb.DeviceID]; !ok {
			first[hb.DeviceID] = hb.TMS
		}
		frames, ok := clients[hb.DeviceID]
		if !ok || hb.TMS-first[hb.DeviceID] < float64(warmup.Milliseconds()) {
			continue
		}
		if hb.Applied.Mode != api.ModeSplit || hb.Applied.PolicyVersion != hb.ServerPolicy.PolicyVersion {
			continue
		}
		i := offload.IndexOf(s.Decision.Predictions, s.State.Split(), s.State.Window())
		if i < 0 || len(s.Decision.Predictions[i].Missing) > 0 || s.Decision.Predictions[i].Saturated ||
			math.IsInf(s.Decision.Predictions[i].P95MS, 0) || math.IsNaN(s.Decision.Predictions[i].P95MS) {
			continue // nessuna previsione finita da confrontare
		}
		to := hb.TS
		from := to.Add(-time.Duration(hb.SinceLastSeenMS * float64(time.Millisecond)))
		m := measureWindow(frames, from, to)
		if m.Frames < minFrames {
			continue
		}
		out = append(out, CheckWindow{DeviceID: hb.DeviceID, TMS: hb.TMS, Pred: s.Decision.Predictions[i], Meas: m})
	}
	return out, nil
}

// CheckBucket riassume le finestre di una fascia di ρ previsto.
type CheckBucket struct {
	Label   string
	Windows int
	// Mediane dell'errore relativo (previsto − misurato)/misurato: > 0 il
	// modello sovrastima. NaN senza finestre (o con misurato 0 per tutte).
	P95, Mean, FPS, Share float64
	// AbsP95 è la mediana di |errore relativo del p95|: la regola di
	// queue_rho_from (RhoFrom).
	AbsP95 float64
}

// CheckBuckets raggruppa le finestre per fascia di Pred.Rho (le fasce di
// Buckets: rhoBucket, rhoLabels).
func CheckBuckets(ws []CheckWindow) []CheckBucket {
	out := make([]CheckBucket, len(rhoLabels))
	// errs[k][m]: errori relativi della fascia k per la metrica m (P95, media, FPS, quota).
	errs := make([][4][]float64, len(rhoLabels))
	abs := make([][]float64, len(rhoLabels))
	for k := range out {
		out[k].Label = rhoLabels[k]
	}
	for _, w := range ws {
		k := rhoBucket(w.Pred.Rho)
		out[k].Windows++
		pairs := [4][2]float64{
			{w.Pred.P95MS, w.Meas.P95MS},
			{w.Pred.TotalMS, w.Meas.MeanMS},
			{w.Pred.FramesPerSec, w.Meas.FPS},
			{w.Pred.ServerShare, w.Meas.ServerShare},
		}
		for m, p := range pairs {
			if p[1] == 0 {
				continue
			}
			e := (p[0] - p[1]) / p[1]
			errs[k][m] = append(errs[k][m], e)
			if m == 0 {
				abs[k] = append(abs[k], math.Abs(e))
			}
		}
	}
	for k := range out {
		out[k].P95, out[k].Mean = median(errs[k][0]), median(errs[k][1])
		out[k].FPS, out[k].Share = median(errs[k][2]), median(errs[k][3])
		out[k].AbsP95 = median(abs[k])
	}
	return out
}

// median è la mediana (rango più vicino); NaN senza valori.
func median(x []float64) float64 {
	if len(x) == 0 {
		return math.NaN()
	}
	return costmodel.Percentile(x, 0.5)
}

// RhoFrom è queue_rho_from ricavato dalle fasce: il limite inferiore
// (rhoEdges) della prima fascia, dal basso, con AbsP95 > marginRel. Sotto, la
// parte base del margine (margin_rel × valore) copre già l'errore del
// modello; da lì in su serve il termine di coda. ok = false se nessuna fascia
// con finestre supera marginRel: i dati non dicono dove parte.
func RhoFrom(bs []CheckBucket, marginRel float64) (rho float64, ok bool) {
	for k, b := range bs {
		if !math.IsNaN(b.AbsP95) && b.AbsP95 > marginRel {
			return rhoEdges[k], true
		}
	}
	return 0, false
}
