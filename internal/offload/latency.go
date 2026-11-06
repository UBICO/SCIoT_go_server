package offload

import (
	"math"
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
// Sceglie la coppia (split point, finestra) e prenota il carico che concede
// (reserve.go): i device che decidono subito dopo lo vedono prima che il
// dispatcher lo misuri.
//
// Ricorda, per device e per candidato, il rumore delle previsioni: va
// creato una volta e usato per tutti i device (è sicuro da più goroutine).
type Latency struct {
	cfg LatencyConfig

	mu    sync.Mutex
	noise map[string]map[candidate]*noise // device → candidato → rumore

	// decideMu tiene insieme lettura delle prenotazioni, decisione e nuova
	// prenotazione: due heartbeat simultanei non vedono lo stesso carico
	// libero.
	decideMu sync.Mutex
	res      reservations
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
	// MinHold: nessun cambio prima che la policy sia in vigore da tanto.
	MinHold time.Duration
	// Margine: max(MarginMinMS, MarginRel × valore attuale) × (1 + KQueue ·
	// max(0, ρ' − QueueRhoFrom)/(1−ρ')) + KNoise · σ.
	MarginMinMS, MarginRel, KQueue, KNoise float64
	// QueueRhoFrom è da dove parte il termine di coda: sotto, il p95 della
	// simulazione è entro MarginRel da quello del client (Fase 3.8, sciot-replay
	// check, docs/OFFLOAD.md "Verification") e il margine non deve crescere.
	// 0 = da subito.
	QueueRhoFrom float64
	// NoiseTau è la costante di tempo della stima del rumore σ.
	NoiseTau time.Duration
	// Reserve: le prenotazioni sono attive (default). false le spegne, per
	// l'ablazione: con gli altri device simulati come client (Fase 3.8)
	// servono solo nel ripiego sul carico misurato.
	Reserve bool
	// ReserveWindow è la finestra su cui il dispatcher misura λ e ρ
	// (dispatch.Config.Window): una prenotazione si spegne in questo tempo.
	ReserveWindow time.Duration
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
		MinFPSRatio: 1, MaxRejectShare: 0.1,
		MinHold:     10 * time.Second,
		MarginMinMS: 2, MarginRel: 0.1, KQueue: 1, KNoise: 1, QueueRhoFrom: 0.8,
		NoiseTau: 10 * time.Second,
		Reserve:  true,
		// = dispatch.DefaultConfig().Window; cmd/server passa quella vera.
		ReserveWindow: 10 * time.Second,
	}
}

// minNoiseSamples: con meno differenze tra previsioni consecutive σ non è
// affidabile e lo split point non può sostituire quello in uso (stessa
// regola dei 3 campioni di not_limiting).
const minNoiseSamples = 3

// maxRho limita ρ' nel termine di coda del margine: a ρ' = 1 sarebbe infinito.
const maxRho = 0.99

// Motivi di una decisione (Decision.Reason, evento decision).
const (
	ReasonChange     = "latency"         // cambio: il migliore batte l'attuale oltre il margine
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
	return &Latency{cfg: cfg, noise: map[string]map[candidate]*noise{}, res: newReservations(cfg.ReserveWindow)}
}

func (l *Latency) Name() string { return "latency" }

// Forget dimentica un device (fine della sessione).
func (l *Latency) Forget(deviceID string) {
	l.mu.Lock()
	delete(l.noise, deviceID)
	l.mu.Unlock()
	l.decideMu.Lock()
	l.res.forget(deviceID)
	l.decideMu.Unlock()
}

// Decide implementa Algorithm. Un cambio prenota la differenza tra il
// carico della coppia scelta e quello dell'attuale. Il carico prenotato
// dagli altri device si aggiunge a quello misurato solo nel ripiego, quando
// gli altri non sono simulati come client (Queue.Clients vuoto): con i
// client un cambio concesso è già nella simulazione (Fleet.SetPolicy), e
// sommarlo al carico misurato cambierebbe solo il ρ di LOCAL.
func (l *Latency) Decide(now time.Time, s State) Decision {
	l.decideMu.Lock()
	defer l.decideMu.Unlock()
	var pend Load // zero con le prenotazioni spente o con i client
	// La prenotazione di un cambio (più sotto) resta: serve a chi decide dopo
	// nel ripiego.
	if l.cfg.Reserve && len(s.Inputs.Queue.Clients) == 0 {
		pend = l.res.pending(now, s.DeviceID)
	}
	s.Inputs.Queue.OthersLambda += pend.PerSec
	s.Inputs.Queue.OthersErlangs += pend.Erlangs

	preds := PredictAll(s)
	sigma, samples := l.observe(now, s.DeviceID, preds)
	d := l.choose(s, preds, sigma, samples)
	d.Pending = pend
	if l.cfg.Reserve && d.Changes(s.Policy, s.MaxWindow) {
		chosen := IndexOf(preds, d.Split(s.Last), d.Window(s.MaxWindow))
		cur := IndexOf(preds, s.Split(), s.Window())
		if chosen >= 0 && cur >= 0 {
			a, b := serverLoad(preds[chosen]), serverLoad(preds[cur])
			// shortcut: prenota anche se il server poi non applica il cambio
			// (device bloccato o ri-registrato nel frattempo, rari): si spegne
			// in ReserveWindow; se diventa frequente, prenotare dopo SetAutoPolicy.
			l.res.reserve(now, s.DeviceID, Load{PerSec: a.PerSec - b.PerSec, Erlangs: a.Erlangs - b.Erlangs})
		}
	}
	return d
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
	switch {
	case !ok:
		keep.Reason = ReasonNoChoice
		return keep
	case best == cur:
		keep.Reason = ReasonBest
		return keep
	}
	// L'attuale non rispetta i vincoli: si cambia senza margine (la durata
	// minima vale comunque).
	if !l.feasible(preds[cur], local) {
		return l.move(preds, best, ReasonInfeasible)
	}
	if samples[best] < minNoiseSamples {
		keep.Reason = ReasonFewSamples
		return keep
	}
	// σ della differenza tra le due previsioni: i due rumori si sommano.
	sd := math.Hypot(sigma[cur], sigma[best])
	if l.value(preds[cur])-l.value(preds[best]) <= l.margin(preds[cur], preds[best], sd) {
		keep.Reason = ReasonHysteresis
		return keep
	}
	return l.move(preds, best, ReasonChange)
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
//	max(MarginMinMS, MarginRel × value(cur)) × (1 + KQueue · max(0, ρ − QueueRhoFrom)/(1−ρ)) + KNoise · sd
//
// con ρ = cand.Rho, limitato a maxRho.
func (l *Latency) margin(cur, cand costmodel.Prediction, sd float64) float64 {
	rho := math.Min(cand.Rho, maxRho)
	queue := 1 + l.cfg.KQueue*math.Max(0, rho-l.cfg.QueueRhoFrom)/(1-rho)
	return math.Max(l.cfg.MarginMinMS, l.cfg.MarginRel*l.value(cur))*queue + l.cfg.KNoise*sd
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
