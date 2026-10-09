// Package device mantiene in memoria lo stato dei device registrati.
//
// A differenza del Registry (costruito all'avvio e poi solo letto), lo Store
// viene modificato mentre il server gira, da più richieste HTTP in parallelo:
// ogni accesso alla map deve quindi essere protetto da un mutex.
package device

import (
	"crypto/rand"
	"sync"
	"time"

	"github.com/UBICO/sciot-go-server/internal/api"
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
	RegisteredAt time.Time
	LastSeen     time.Time
}

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
}

// NewStore crea uno Store vuoto.
func NewStore() *Store {
	// restituisci un puntatore a uno Store con:
	//   - devices inizializzata con make(...). Una map nil si può leggere
	//     ma scriverci provoca un panic: va sempre creata.
	//   - now impostato a time.Now (senza parentesi: passi la funzione,
	//     non il risultato della chiamata).
	//   mu non va inizializzato: il valore zero di sync.Mutex è un mutex
	//   pronto all'uso e sbloccato.

	return &Store{
		devices: make(map[string]Device),
		now:     time.Now,
	}
}

// Register registra (o ri-registra) un device e ne restituisce lo stato.
//
// Una ri-registrazione sostituisce completamente lo stato precedente:
// nuovo session_id, nuova policy, nuovo modello. È la stessa semantica di
// replace_registration nel server Python.
//
// Restituisce una COPIA del Device (valore, non puntatore): chi chiama può
// usarla liberamente senza mutex, e non può modificare lo Store per sbaglio.
func (s *Store) Register(id, deviceType string, model api.ModelInfo, caps api.Capabilities) Device {
	//
	//   - s.mu.Lock() e subito defer s.mu.Unlock()
	//     Coppia Lock + defer Unlock: il mutex viene rilasciato anche se la
	//     funzione esce in anticipo o va in panic.
	//   - now := s.now()
	//   - costruisci il Device:
	//       * SessionID: newSessionID()
	//       * Policy: modalità ModeLocal, SplitPoint nil, PolicyVersion 1
	//         (ogni sessione parte in LOCAL, la policy è numerata per sessione)
	//       * RegisteredAt e LastSeen: now
	//   - salvalo nella map con chiave id (sovrascrive l'eventuale precedente)
	//   - restituiscilo
	//panic("TODO")
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
		RegisteredAt: now,
		LastSeen:     now,
	}

	s.devices[id] = device

	return device
}

// Get restituisce lo stato del device e true, oppure Device{} e false.
func (s *Store) Get(id string) (Device, bool) {
	// anche le letture vanno protette. Una lettura in parallelo
	//   a una scrittura sulla stessa map è una "data race": Go può
	//   interrompere il programma con "concurrent map read and map write".
	//   Poi usa il "comma ok" come in Registry.Lookup.
	//panic("TODO")
	s.mu.Lock()
	defer s.mu.Unlock()
	device, ok := s.devices[id]
	return device, ok
}

// Len restituisce il numero di device registrati.
func (s *Store) Len() int {
	// come Get, ma restituisce len(...) della map.
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.devices)
}

// newSessionID genera un identificatore casuale e non indovinabile.
//
// rand.Text (crypto/rand, Go ≥ 1.24) restituisce una stringa casuale
// di 26 caratteri con 128 bit di entropia. È crittograficamente sicura,
// a differenza di math/rand.
func newSessionID() string {
	return rand.Text()
}
