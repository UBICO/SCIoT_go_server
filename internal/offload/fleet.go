package offload

import (
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/costmodel"
)

// Fleet ricorda l'ultimo stato di ogni device, per simulare gli altri come
// client quando si prevede per uno (costmodel.Queue.Clients, Fase 3.8).
//
// Sta fuori dagli algoritmi: il server (decide) e il replay (Algo) lo
// aggiornano a ogni heartbeat, prima di Decide, così ogni algoritmo vede gli
// stessi ingressi, dal vivo e offline. Anche i device bloccati dall'admin ci
// sono: il loro carico conta come quello degli altri.
type Fleet struct {
	// Expire: un device senza heartbeat da più di tanto non è più un "altro"
	// (sessione scaduta, device spento senza ri-registrarsi).
	expire time.Duration

	mu      sync.Mutex
	devices map[string]member
}

// member è l'ultimo stato noto di un device e quando è stato visto.
type member struct {
	st   State
	seen time.Time
}

// NewFleet crea un Fleet che dimentica i device non visti da expire.
func NewFleet(expire time.Duration) *Fleet {
	return &Fleet{expire: expire, devices: map[string]member{}}
}

// Update registra lo stato st del device st.DeviceID, visto a now.
func (f *Fleet) Update(now time.Time, st State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.devices[st.DeviceID] = member{st, now}
}

// SetPolicy aggiorna la policy di un device appena cambiata dal server: chi
// decide dopo lo simula subito con la nuova, prima del suo prossimo
// heartbeat (l'effetto delle prenotazioni, Fase 3.7). Nessun effetto per un
// device sconosciuto.
func (f *Fleet) SetPolicy(deviceID string, p api.Policy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.devices[deviceID]
	if !ok {
		return
	}
	m.st.Policy = p
	m.st.Inputs.MaxInflight = m.st.Window()
	f.devices[deviceID] = m
}

// Forget toglie un device (nuova registrazione: lo stato vecchio non vale più).
func (f *Fleet) Forget(deviceID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.devices, deviceID)
}

// Clients restituisce gli altri device (tutti tranne deviceID) visti negli
// ultimi expire e in SPLIT, come client della simulazione, ognuno alla sua
// policy (costmodel.ClientFrom con split, finestra e ultimo segmento del suo
// stato). complete = false se uno di loro non ha ingressi completi: allora
// il chiamante non usa i client (ripiego sul carico misurato), perché
// simularne solo una parte sottostimerebbe gli altri. Toglie i device non
// visti da più di expire. Nessun altro in SPLIT: nil, true.
func (f *Fleet) Clients(now time.Time, deviceID string) (clients []costmodel.Client, complete bool) {
	type other struct {
		id string
		c  costmodel.Client
	}
	var others []other
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, m := range f.devices {
		if now.Sub(m.seen) > f.expire {
			delete(f.devices, id)
			continue
		}
		if id == deviceID || m.st.Split() >= m.st.Last {
			continue
		}
		c, ok := costmodel.ClientFrom(m.st.Inputs, m.st.Split(), m.st.Window(), m.st.Last)
		if !ok {
			return nil, false
		}
		others = append(others, other{id, c})
	}
	// Ordine per id: la simulazione dà lo stesso risultato con gli stessi ingressi.
	slices.SortFunc(others, func(a, b other) int { return strings.Compare(a.id, b.id) })
	for _, o := range others {
		clients = append(clients, o.c)
	}
	return clients, true
}
