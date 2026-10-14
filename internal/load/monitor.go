// Package load campiona il carico della macchina su cui gira il server.
//
// È l'equivalente Go di EdgeLoadMonitor del server Python: una goroutine
// dedicata legge le metriche a intervalli fissi e pubblica uno snapshot,
// che il resto del server legge senza mai interrogare il sistema operativo
// sul percorso di una richiesta.
//
// Ogni metrica è opzionale: nil = "non misurata" (lettura fallita o non
// supportata), che è diverso da 0. Stessa regola di LoadSnapshot in Python.
package load

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"time"
)

var errNoValue = errors.New("no value returned")

// Reader legge le metriche grezze della macchina.
//
// È un'interfaccia piccola apposta: HostReader (gopsutil) è l'implementazione
// reale, e i test ne usano una finta che restituisce valori decisi dal test.
type Reader interface {
	CPUPercent() (float64, error) // uso CPU 0..100 dalla chiamata precedente
	CPUCount() (int, error)
	LoadAvg1() (float64, error)
	Memory() (total, available uint64, err error)
}

// Snapshot è una lettura del carico in un istante.
//
// `omitzero` (Go ≥ 1.24) omette il campo dal JSON quando vale il suo valore
// zero. Per i puntatori equivale a omitempty, ma funziona anche con le
// struct come time.Time, dove omitempty non ha effetto.
type Snapshot struct {
	CPUPercent           *float64  `json:"cpu_percent,omitzero"` // smussata con EWMA
	CPUCount             *int      `json:"cpu_count,omitzero"`
	LoadAvg1Min          *float64  `json:"load_avg_1_min,omitzero"`
	MemoryTotalBytes     *uint64   `json:"memory_total_bytes,omitzero"`
	MemoryAvailableBytes *uint64   `json:"memory_available_bytes,omitzero"`
	Goroutines           int       `json:"goroutines"`
	SampledAt            time.Time `json:"sampled_at,omitzero"` // zero = mai campionato
}

// Config contiene i parametri del campionamento.
type Config struct {
	Interval     time.Duration // tempo tra due campioni
	CPUEWMAAlpha float64       // peso del campione nuovo, 0..1
}

// DefaultConfig restituisce gli stessi valori del server Python.
func DefaultConfig() Config {
	return Config{Interval: time.Second, CPUEWMAAlpha: 0.35}
}

// Monitor campiona periodicamente il carico e conserva l'ultimo snapshot.
type Monitor struct {
	reader Reader
	cfg    Config

	// Sostituibili nei test (stesso schema di device.Store.now).
	now        func() time.Time
	goroutines func() int

	// mu protegge i due campi sotto: Sample li scrive dalla goroutine di
	// campionamento, Snapshot li legge dagli handler HTTP.
	mu      sync.Mutex
	current Snapshot
	cpuEWMA *float64 // nil finché non c'è stato un campione CPU valido
}

// NewMonitor crea un Monitor. Non avvia il campionamento: serve Run.
func NewMonitor(r Reader, cfg Config) *Monitor {
	return &Monitor{
		reader:     r,
		cfg:        cfg,
		now:        time.Now,
		goroutines: runtime.NumGoroutine,
	}
}

// ewma combina il valore precedente con un nuovo campione:
//
//	nessun precedente → raw
//	altrimenti        → alpha*raw + (1-alpha)*prev
//
// Stessa formula di EdgeLoadMonitor._smoothed_cpu_percent in Python.
func ewma(prev *float64, raw, alpha float64) float64 {

	if prev == nil {
		return raw
	}

	return alpha*raw + (1-alpha)*(*prev)
}

// Sample legge le metriche, aggiorna lo snapshot corrente e lo restituisce.
func (m *Monitor) Sample() Snapshot {
	snap := Snapshot{Goroutines: m.goroutines(), SampledAt: m.now()}

	cpuRaw, cpuErr := m.reader.CPUPercent()

	if n, err := m.reader.CPUCount(); err == nil {
		snap.CPUCount = ptr(n)
	}
	if l, err := m.reader.LoadAvg1(); err == nil {
		snap.LoadAvg1Min = ptr(l)
	}
	if total, avail, err := m.reader.Memory(); err == nil {
		snap.MemoryTotalBytes = ptr(total)
		snap.MemoryAvailableBytes = ptr(avail)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if cpuErr == nil {
		v := ewma(m.cpuEWMA, cpuRaw, m.cfg.CPUEWMAAlpha)
		m.cpuEWMA = &v
		snap.CPUPercent = &v
	}
	m.current = snap
	return snap
}

// Snapshot restituisce l'ultimo campione. Prima del primo Sample è uno
// Snapshot vuoto (SampledAt zero).
func (m *Monitor) Snapshot() Snapshot {

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.current
}

// Run campiona subito e poi ogni cfg.Interval, finché ctx non viene
// cancellato. È pensato per girare in una goroutine: go m.Run(ctx).
func (m *Monitor) Run(ctx context.Context) {
	m.Sample()
	ticker := time.NewTicker(m.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Sample()
		}

	}

}

// ptr restituisce un puntatore a una copia di v.
func ptr[T any](v T) *T { return &v }
