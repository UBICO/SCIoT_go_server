// Package device mantiene in memoria lo stato dei device registrati.
//
// A differenza del Registry (costruito all'avvio e poi solo letto), lo Store
// viene modificato mentre il server gira, da più richieste HTTP in parallelo:
// ogni accesso alla map deve quindi essere protetto da un mutex.
package device

import (
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/ewma"
)

// Device è lo stato di un device registrato.
//
// Non ha tag JSON: è un tipo interno del server, non un messaggio del
// protocollo. Le risposte HTTP usano i tipi di internal/api.
type Device struct {
	ID           string
	Type         string // "python", "esp32", ...
	SessionID    string // cambia ad ogni registrazione
	Model        api.ModelInfo
	Capabilities api.Capabilities
	Policy       api.Policy
	// PolicySince è quando la policy attuale è entrata in vigore (per la
	// durata minima dell'algoritmo di offloading).
	PolicySince time.Time
	// Pinned: la policy l'ha decisa l'admin; l'algoritmo non la cambia finché
	// l'admin non la rilascia (Unpin). Vale per la sessione.
	Pinned       bool
	RegisteredAt time.Time
	LastSeen     time.Time

	// AppliedPolicyVersion è la versione che il client ha dichiarato di
	// applicare nell'ultimo heartbeat (0 = nessun heartbeat ancora).
	AppliedPolicyVersion uint64
	// LastTelemetry è l'ultimo campione ricevuto. Contiene puntatori (campi
	// opzionali): una copia del Device li condivide con lo Store. È sicuro
	// solo perché nessuno modifica una Telemetry dopo averla ricevuta.
	LastTelemetry api.Telemetry
	// Segments è la stima del tempo del device per ogni segmento del
	// modello, in ms, dai segment_ms degli heartbeat (vuota per un segmento
	// mai eseguito). nil fino al primo heartbeat che li porta; si azzera con
	// una nuova registrazione. Come LastTelemetry, una copia la condivide con
	// lo Store: per questo Heartbeat ne crea sempre una nuova (mai in place).
	Segments []ewma.Stat
}

// DefaultSegmentTau è la costante di tempo delle stime per segmento: un
// campione per heartbeat (1 s), dieci heartbeat di memoria.
const DefaultSegmentTau = 10 * time.Second

// ErrSessionExpired indica che device o sessione non sono (più) validi:
// device mai registrato, server riavviato (lo Store è in memoria) oppure
// session_id di una registrazione precedente.
var ErrSessionExpired = errors.New("session expired")

// ErrDeviceNotFound: SetPolicy su un device mai registrato.
var ErrDeviceNotFound = errors.New("device not found")

// ErrInvalidPolicy avvolge l'errore di validazione di una policy, così chi
// chiama può distinguerlo con errors.Is senza conoscerne il testo.
var ErrInvalidPolicy = errors.New("invalid policy")

// ErrInvalidTelemetry: telemetria valida in sé ma non per il modello della
// sessione (per esempio segment_ms di lunghezza diversa dai segmenti).
var ErrInvalidTelemetry = errors.New("invalid telemetry")

// Store contiene i device registrati, indicizzati per ID.
type Store struct {
	// mu protegge devices. Convenzione Go: il mutex si dichiara subito
	// sopra i campi che protegge.
	//
	// Uno Store non va mai copiato: la copia avrebbe un mutex diverso e
	// la protezione salterebbe. Per questo si usa sempre *Store (puntatore)
	// e go vet segnala le copie accidentali.
	mu      sync.Mutex
	devices map[string]Device

	// now restituisce l'ora corrente. In produzione è time.Now; nei test
	// lo sostituiamo con un orologio finto per avere tempi prevedibili.
	// In Go le funzioni sono valori: si possono salvare in un campo.
	now func() time.Time

	segmentTau time.Duration // vedi SetSegmentTau
}

// NewStore crea uno Store vuoto.
func NewStore() *Store {
	// mu non va inizializzato: il valore zero di sync.Mutex è pronto all'uso.
	return &Store{
		devices:    make(map[string]Device),
		now:        time.Now,
		segmentTau: DefaultSegmentTau,
	}
}

// SetSegmentTau cambia la costante di tempo delle stime per segmento
// (server.device_segment_tau_ms). Va chiamata prima di servire richieste.
func (s *Store) SetSegmentTau(tau time.Duration) { s.segmentTau = tau }

// Register registra (o ri-registra) un device e ne restituisce lo stato.
//
// Una ri-registrazione sostituisce completamente lo stato precedente:
// nuovo session_id, nuova policy, nuovo modello. È la stessa semantica di
// replace_registration nel server Python.
//
// Restituisce una COPIA del Device (valore, non puntatore): chi chiama può
// usarla liberamente senza mutex, e non può modificare lo Store per sbaglio.
func (s *Store) Register(id, deviceType string, model api.ModelInfo, caps api.Capabilities) Device {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	device := Device{
		ID:           id,
		Type:         deviceType,
		SessionID:    newSessionID(),
		Model:        model,
		Capabilities: caps,
		Policy: api.Policy{
			Mode:          api.ModeLocal,
			SplitPoint:    nil,
			PolicyVersion: 1,
		},
		PolicySince:  now,
		RegisteredAt: now,
		LastSeen:     now,
	}

	s.devices[id] = device

	return device
}

// Get restituisce lo stato del device e true, oppure Device{} e false.
func (s *Store) Get(id string) (Device, bool) {
	// Anche le letture vanno protette: lettura e scrittura in parallelo
	// sulla stessa map sono una data race.
	s.mu.Lock()
	defer s.mu.Unlock()
	device, ok := s.devices[id]
	return device, ok
}

// Len restituisce il numero di device registrati.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.devices)
}

// HeartbeatResult è l'esito di un heartbeat accettato.
type HeartbeatResult struct {
	Policy api.Policy
	// SinceLastSeen è il tempo trascorso dal contatto precedente (heartbeat
	// o registrazione): con un intervallo di 1 s dovrebbe valere ~1 s, e
	// scostamenti grandi indicano un client o una rete in difficoltà.
	SinceLastSeen time.Duration
	// Segments è la stima per segmento dopo questo heartbeat (ms); nil se il
	// device non ha mai inviato segment_ms.
	Segments []ewma.Stat
	// Model è il modello della sessione.
	Model api.ModelInfo
	// PolicySince e Pinned: vedi Device.
	PolicySince time.Time
	Pinned      bool
}

// Heartbeat registra un heartbeat e restituisce la policy corrente.
//
// Restituisce ErrSessionExpired se il device non esiste o se sessionID non
// coincide con la sessione attuale. In quel caso lo stato NON viene toccato:
// un heartbeat vecchio non deve aggiornare la sessione nuova.
func (s *Store) Heartbeat(id, sessionID string, appliedVersion uint64, t api.Telemetry) (HeartbeatResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.devices[id]

	if !ok || d.SessionID != sessionID {
		return HeartbeatResult{}, ErrSessionExpired
	}

	// Sotto il lock: il modello è quello della sessione attuale. La sessione
	// è già verificata sopra, quindi un device scaduto riceve session_expired.
	if t.SegmentMS != nil && len(t.SegmentMS) != d.Model.NumLayers {
		return HeartbeatResult{}, fmt.Errorf("%w: segment_ms has %d entries, the model has %d segments",
			ErrInvalidTelemetry, len(t.SegmentMS), d.Model.NumLayers)
	}

	now := s.now()
	since := now.Sub(d.LastSeen)
	d.LastSeen = now
	d.AppliedPolicyVersion = appliedVersion
	d.LastTelemetry = t
	if t.SegmentMS != nil {
		d.Segments = updateSegments(d.Segments, t.SegmentMS, now, s.segmentTau)
	}
	s.devices[id] = d
	return HeartbeatResult{Policy: d.Policy, SinceLastSeen: since, Segments: d.Segments, Model: d.Model,
		PolicySince: d.PolicySince, Pinned: d.Pinned}, nil
}

// newSessionID genera un identificatore casuale e non indovinabile.
//
// rand.Text (crypto/rand, Go ≥ 1.24) restituisce una stringa casuale
// di 26 caratteri con 128 bit di entropia. È crittograficamente sicura,
// a differenza di math/rand.
func newSessionID() string {
	return rand.Text()
}

// SetPolicy sostituisce la policy del device con req, incrementandone la
// versione, e la blocca (Pinned): è la policy dell'admin. Il device la
// riceverà nella risposta al prossimo heartbeat.
//
// La validazione avviene sotto lo stesso lock della scrittura: se il device
// si ri-registra con un altro modello tra un controllo esterno e la
// scrittura, uno split_point valido per il vecchio modello potrebbe non
// esserlo per il nuovo.
//
// La policy vale per la sessione corrente: una ri-registrazione riparte da
// LOCAL v1.
func (s *Store) SetPolicy(id string, req api.SetPolicyRequest) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.devices[id]
	if !ok {
		return Device{}, ErrDeviceNotFound
	}
	d, err := s.setPolicyLocked(d, req)
	if err != nil {
		return Device{}, err
	}
	d.Pinned = true
	s.devices[id] = d
	return d, nil
}

// ErrPinned: l'algoritmo prova a cambiare la policy di un device bloccato
// dall'admin.
var ErrPinned = errors.New("policy pinned by the admin")

// ErrStalePolicy: la decisione è stata presa su una policy che nel frattempo
// è cambiata (per esempio l'admin l'ha impostata e poi rilasciata).
var ErrStalePolicy = errors.New("policy changed since the decision")

// SetAutoPolicy è il cambio di policy dell'algoritmo di offloading: come
// SetPolicy, ma solo per la sessione sessionID (una ri-registrazione nel
// frattempo l'ha resa vecchia: ErrSessionExpired), mai su un device
// bloccato (ErrPinned) e solo se la policy è ancora la versione basedOn su
// cui l'algoritmo ha deciso (ErrStalePolicy). La decisione e la scrittura non
// sono sotto lo stesso lock (e con le decisioni in una goroutine passa
// tempo tra le due): questi controlli impediscono che una decisione presa
// sullo stato vecchio sovrascriva quello nuovo.
func (s *Store) SetAutoPolicy(id, sessionID string, basedOn uint64, req api.SetPolicyRequest) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.devices[id]
	if !ok || d.SessionID != sessionID {
		return Device{}, ErrSessionExpired
	}
	if d.Pinned {
		return Device{}, ErrPinned
	}
	if d.Policy.PolicyVersion != basedOn {
		return Device{}, ErrStalePolicy
	}
	d, err := s.setPolicyLocked(d, req)
	if err != nil {
		return Device{}, err
	}
	s.devices[id] = d
	return d, nil
}

// Unpin restituisce il device all'algoritmo: la policy resta quella
// dell'admin finché l'algoritmo non decide altrimenti.
func (s *Store) Unpin(id string) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.devices[id]
	if !ok {
		return Device{}, ErrDeviceNotFound
	}
	d.Pinned = false
	s.devices[id] = d
	return d, nil
}

// setPolicyLocked valida req per il modello di d e ne fa la nuova policy, con
// la versione successiva. Va chiamata con s.mu acquisito; non scrive d.
func (s *Store) setPolicyLocked(d Device, req api.SetPolicyRequest) (Device, error) {
	if err := req.Validate(d.Model); err != nil {
		return Device{}, fmt.Errorf("%w: %v", ErrInvalidPolicy, err)
	}

	var splitPoint *int
	if req.SplitPoint != nil {
		// Copia del valore: lo Store non deve condividere memoria con la
		// richiesta del chiamante.
		sp := *req.SplitPoint
		splitPoint = &sp
	}
	var window *int
	if req.MaxInflight != nil {
		w := *req.MaxInflight
		window = &w
	}
	d.Policy = api.Policy{
		Mode:          req.Mode,
		SplitPoint:    splitPoint,
		PolicyVersion: d.Policy.PolicyVersion + 1,
		MaxInflight:   window,
	}
	d.PolicySince = s.now()
	return d, nil
}

// updateSegments restituisce una COPIA di old (o una stima nuova, se old è
// nil) con i segmenti eseguiti nella finestra aggiornati: un campione per
// segmento, la media della finestra, arrivato in at. I segmenti null non
// cambiano: la loro stima invecchia (Stat.Last).
func updateSegments(old []ewma.Stat, ms []*float64, at time.Time, tau time.Duration) []ewma.Stat {
	stats := make([]ewma.Stat, len(ms))
	copy(stats, old) // old ha la stessa lunghezza, o è nil: Heartbeat l'ha controllata
	for i, v := range ms {
		if v != nil {
			stats[i].Update(at, *v, tau)
		}
	}
	return stats
}
