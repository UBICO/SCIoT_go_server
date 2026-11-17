package offload

import (
	"cmp"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/costmodel"
	"github.com/UBICO/SCIoT_go_server/internal/ewma"
)

// Latency sceglie lo split point con la latenza prevista più bassa (p95 o
// media, LatencyConfig.Objective), tra quelli che rispettano i vincoli, e lo
// cambia solo quando conviene davvero: dopo una durata minima e con un
// guadagno oltre un margine che cresce vicino alla saturazione e con il
// rumore delle previsioni (docs/OFFLOAD.md).
//
// Sceglie la coppia (split point, finestra): la finestra, allo split point
// scelto, è quella con più frame/s entro P95Slack dal migliore (Fase 4.2).
//
// Ricorda, per device e per candidato, il rumore delle previsioni: va
// creato una volta e usato per tutti i device (è sicuro da più goroutine).
type Latency struct {
	cfg LatencyConfig

	mu    sync.Mutex
	noise map[string]map[candidate]*noise // device → candidato → rumore
}

// candidate è una coppia (split point, finestra); finestra 0 per LOCAL.
type candidate struct{ split, window int }

// LatencyConfig sono i parametri di Latency (sezione "offload" di
// config.json).
type LatencyConfig struct {
	// Objective è il valore confrontato: ObjectiveP95 o ObjectiveMean.
	Objective string
	// MinFPSRatio: uno split point è ammesso solo se il device fa almeno i
	// frame al secondo di LOCAL per questo fattore. MaxRejectShare: e se la
	// quota dei suoi frame rifiutati dal server non supera questa.
	MinFPSRatio, MaxRejectShare float64
	// P95Slack (Fase 4.2): allo split point del migliore, la finestra con
	// più frame/s tra quelle con valore ≤ (1 + P95Slack) × il migliore; più
	// frame/s vuol dire almeno MarginRel in più. 0 = la finestra del valore
	// più basso, come prima. Solo la finestra: lo split point resta deciso
	// dal valore (docs/OFFLOAD.md, "Window").
	P95Slack float64
	// MinHold: nessun cambio prima che la policy sia in vigore da tanto.
	MinHold time.Duration
	// Margine: max(MarginMinMS, MarginRel × valore attuale) + KNoise · σ.
	MarginMinMS, MarginRel, KNoise float64
	// NoiseTau è la costante di tempo della stima del rumore σ.
	NoiseTau time.Duration
}

// Obiettivi di LatencyConfig.Objective.
const (
	ObjectiveP95  = "p95"
	ObjectiveMean = "mean"
)

// DefaultLatencyConfig restituisce i valori di partenza.
func DefaultLatencyConfig() LatencyConfig {
	return LatencyConfig{
		Objective:   ObjectiveP95,
		MinFPSRatio: 1, MaxRejectShare: 0.1, P95Slack: 0.25,
		MinHold:     10 * time.Second,
		MarginMinMS: 2, MarginRel: 0.1, KNoise: 1,
		NoiseTau: 10 * time.Second,
	}
}

// minNoiseSamples: con meno differenze tra previsioni consecutive σ non è
// affidabile e lo split point non può sostituire quello in uso (stessa
// regola dei 3 campioni di not_limiting).
const minNoiseSamples = 3

// Motivi di una decisione (Decision.Reason, evento decision).
const (
	ReasonChange     = "latency"         // cambio: il migliore batte l'attuale oltre il margine
	ReasonFPS        = "fps"             // cambio di finestra: più frame/s con il valore entro P95Slack
	ReasonInfeasible = "infeasible"      // cambio: l'attuale non rispetta i vincoli
	ReasonBest       = "best"            // l'attuale è già il migliore
	ReasonHysteresis = "hysteresis"      // il migliore non batte l'attuale oltre il margine
	ReasonMinHold    = "min_hold"        // la policy è in vigore da meno di MinHold
	ReasonUnknown    = "current_unknown" // previsione dell'attuale con ingressi mancanti
	ReasonNoChoice   = "no_candidate"    // nessuno split point ammesso
	ReasonFewSamples = "few_samples"     // il migliore ha troppo poche previsioni per σ
)

// NewLatency crea l'algoritmo.
func NewLatency(cfg LatencyConfig) *Latency {
	return &Latency{cfg: cfg, noise: map[string]map[candidate]*noise{}}
}

func (l *Latency) Name() string { return "latency" }

// Forget dimentica un device (fine della sessione).
func (l *Latency) Forget(deviceID string) {
	l.mu.Lock()
	delete(l.noise, deviceID)
	l.mu.Unlock()
}

// Decide implementa Algorithm. Gli altri device in SPLIT sono simulati come
// client alla policy che il server ha appena dato loro (Fleet.SetPolicy):
// un cambio concesso è già nella decisione successiva degli altri, prima che
// il dispatcher lo misuri. Nel ripiego (Queue.Clients vuoto) gli altri sono
// il carico misurato (docs/OFFLOAD.md, "Other devices").
func (l *Latency) Decide(now time.Time, s State) Decision {
	preds := PredictAll(s)
	sigma, samples := l.observe(now, s.DeviceID, preds)
	return l.choose(s, preds, sigma, samples)
}

// choose è la decisione sulle previsioni preds (PredictAll: una per coppia
// split point e finestra, LOCAL per ultima), con σ e il numero di campioni
// di ogni previsione.
func (l *Latency) choose(s State, preds []costmodel.Prediction, sigma []float64, samples []uint64) Decision {
	keep := Decision{Mode: s.Policy.Mode, SplitPoint: s.Policy.SplitPoint, MaxInflight: s.Policy.MaxInflight, Predictions: preds}
	cur := IndexOf(preds, s.Split(), s.Window())
	local := preds[len(preds)-1]
	// Senza previsione per l'attuale (una finestra oltre il massimo, dopo un
	// cambio di config) è come una previsione con ingressi mancanti.
	if cur < 0 || len(preds[cur].Missing) > 0 {
		keep.Reason = ReasonUnknown
		return keep
	}
	if !l.canChange(s.PolicyAge) {
		keep.Reason = ReasonMinHold
		return keep
	}
	best, ok := l.best(preds, local)
	if !ok {
		keep.Reason = ReasonNoChoice
		return keep
	}
	// La finestra con più frame/s allo split point del migliore (P95Slack);
	// = best se nessuna ne dà abbastanza di più.
	win := l.fastestWindow(preds, local, best)
	if win == cur {
		keep.Reason = ReasonBest
		return keep
	}
	// L'attuale non rispetta i vincoli: si cambia senza margine (la durata
	// minima vale comunque).
	if !l.feasible(preds[cur], local) {
		return l.move(preds, win, ReasonInfeasible)
	}
	if samples[win] < minNoiseSamples {
		keep.Reason = ReasonFewSamples
		return keep
	}
	// σ della differenza tra le due previsioni: i due rumori si sommano.
	sd := math.Hypot(sigma[cur], sigma[best])
	margin := l.margin(preds[cur], preds[best], sd)
	// Allo split point del migliore l'isteresi è sulla finestra. Entro lo
	// slack dal migliore (più il margine) si cambia solo per abbastanza
	// frame/s in più; oltre, per il valore.
	//
	// Vale anche con win == best (campagna 4.2, R1): a finestra 2 (p95 175)
	// con la 1 a 140, 175/140 = 1,26 è fuori slack e fastestWindow dà la 1;
	// l'isteresi sul valore (35 ms > margine 18) riportava a finestra 1, dove
	// il p95 previsto tornava a 150 e dopo 10 s si tornava alla 2. Con
	// win == best moreFPS(best, cur) è falso se best ha meno frame/s: si resta.
	// Con P95Slack 0 resta la regola di prima.
	if l.cfg.P95Slack > 0 && sameSplit(preds[cur], preds[best]) {
		inSlack := l.value(preds[cur]) <= (1+l.cfg.P95Slack)*l.value(preds[best])+margin
		switch {
		case !inSlack:
			return l.move(preds, win, ReasonChange)
		case l.moreFPS(preds[win], preds[cur]):
			return l.move(preds, win, ReasonFPS)
		}
		keep.Reason = ReasonHysteresis
		return keep
	}
	// Su un altro split point, o con P95Slack 0: l'isteresi sul valore,
	// contro il migliore.
	if l.value(preds[cur])-l.value(preds[best]) <= margin {
		keep.Reason = ReasonHysteresis
		return keep
	}
	return l.move(preds, win, ReasonChange)
}

// fastestWindow restituisce, tra le previsioni ammesse allo split point di best
// (non LOCAL) con valore ≤ (1 + P95Slack) × value(best), quella con più
// frame/s: in ordine di valore crescente, una sostituisce la scelta solo se
// ne ha almeno (1 + MarginRel) volte i frame/s. Con P95Slack 0, o best
// LOCAL, è best.
func (l *Latency) fastestWindow(preds []costmodel.Prediction, local costmodel.Prediction, best int) int {
	if l.cfg.P95Slack <= 0 || preds[best].Local {
		return best
	}
	limit := (1 + l.cfg.P95Slack) * l.value(preds[best])
	var cands []int
	for i, p := range preds {
		if v := l.value(p); i != best && sameSplit(p, preds[best]) && v <= limit && l.feasible(p, local) {
			cands = append(cands, i) // v NaN: il confronto è falso, esclusa
		}
	}
	slices.SortFunc(cands, func(a, b int) int {
		if c := cmp.Compare(l.value(preds[a]), l.value(preds[b])); c != 0 {
			return c
		}
		return cmp.Compare(b, a)
	})
	chosen := best
	for _, i := range cands {
		if l.moreFPS(preds[i], preds[chosen]) {
			chosen = i
		}
	}
	return chosen
}

// moreFPS dice se a fa abbastanza frame/s più di b: almeno (1 + MarginRel)
// volte, oltre il rumore della simulazione (~3%).
func (l *Latency) moreFPS(a, b costmodel.Prediction) bool {
	return a.FramesPerSec > (1+l.cfg.MarginRel)*b.FramesPerSec
}

// sameSplit dice se a e b sono SPLIT allo stesso split point (finestre
// qualunque).
func sameSplit(a, b costmodel.Prediction) bool {
	return !a.Local && !b.Local && a.Split == b.Split
}

// move è il cambio alla previsione di indice i (l'ultima = LOCAL).
func (l *Latency) move(preds []costmodel.Prediction, i int, reason string) Decision {
	d := Decision{Mode: api.ModeLocal, Reason: reason, Predictions: preds}
	if p := preds[i]; !p.Local {
		split, window := p.Split, p.Window
		d.Mode, d.SplitPoint, d.MaxInflight = api.ModeSplit, &split, &window
	}
	return d
}

// value è il valore che l'obiettivo confronta.
func (l *Latency) value(p costmodel.Prediction) float64 {
	if l.cfg.Objective == ObjectiveMean {
		return p.TotalMS
	}
	return p.P95MS
}

// feasible dice se la previsione p rispetta i vincoli, con local la
// previsione LOCAL.
func (l *Latency) feasible(p, local costmodel.Prediction) bool {
	if len(p.Missing) > 0 || p.FramesPerSec < local.FramesPerSec*l.cfg.MinFPSRatio {
		return false
	}
	return p.FramesPerSec == 0 || p.RejectedPerSec/p.FramesPerSec <= l.cfg.MaxRejectShare
}

// best restituisce l'indice della previsione ammessa (feasible) con il valore
// più basso (value); ok = false se nessuna è ammessa. A pari valore, quella
// con l'indice più alto: più segmenti sul device, meno carico sul server.
func (l *Latency) best(preds []costmodel.Prediction, local costmodel.Prediction) (best int, ok bool) {
	bestV := math.Inf(1)
	for i, p := range preds {
		v := l.value(p)
		if math.IsNaN(v) || math.IsInf(v, 0) || !l.feasible(p, local) {
			continue
		}
		if v <= bestV {
			best, bestV, ok = i, v, true
		}
	}
	return best, ok
}

// margin è il guadagno minimo (ms) perché cand sostituisca cur, con sd il
// rumore della loro differenza:
//
//	max(MarginMinMS, MarginRel × value(cur)) + KNoise · sd
//
// Non dipende da ρ del candidato (Fase 4.3): il parametro resta perché il
// termine di coda ne era l'unico uso e TestMargin ne verifica l'indipendenza.
func (l *Latency) margin(cur, _ costmodel.Prediction, sd float64) float64 {
	return math.Max(l.cfg.MarginMinMS, l.cfg.MarginRel*l.value(cur)) + l.cfg.KNoise*sd
}

// canChange dice se una policy in vigore da age può già cambiare.
func (l *Latency) canChange(age time.Duration) bool {
	return age >= l.cfg.MinHold
}

// noise è il rumore delle previsioni di uno split point attorno al loro
// andamento: la variabilità delle differenze tra previsioni consecutive. Un
// salto di livello (le prestazioni cambiano) pesa una sola differenza; il
// rumore pesa sempre. Var(v_t − v_{t−1}) = 2σ² per un rumore indipendente,
// quindi σ = std(differenze) / √2.
type noise struct {
	last  float64 // ultimo valore; NaN = nessuno
	diffs ewma.Stat
}

// observe aggiorna il rumore di ogni split point con le previsioni nuove e
// restituisce σ e il numero di differenze, per indice di preds. Una
// previsione non disponibile (ingressi mancanti, valore non finito)
// interrompe la serie: la prossima riparte senza differenza.
func (l *Latency) observe(now time.Time, deviceID string, preds []costmodel.Prediction) ([]float64, []uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	byDevice := l.noise[deviceID]
	if byDevice == nil {
		byDevice = map[candidate]*noise{}
		l.noise[deviceID] = byDevice
	}
	sigma := make([]float64, len(preds))
	samples := make([]uint64, len(preds))
	for i, p := range preds {
		key := candidate{p.Split, p.Window}
		n := byDevice[key]
		if n == nil {
			n = &noise{last: math.NaN()}
			byDevice[key] = n
		}
		v := l.value(p)
		if len(p.Missing) > 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			n.last = math.NaN()
		} else {
			if !math.IsNaN(n.last) {
				n.diffs.Update(now, v-n.last, l.cfg.NoiseTau)
			}
			n.last = v
		}
		sigma[i], samples[i] = n.diffs.Std()/math.Sqrt2, n.diffs.N()
	}
	return sigma, samples
}
