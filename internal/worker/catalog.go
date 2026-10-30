package worker

import (
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/workerpb"
)

// Catalog contiene, per ogni modello servito dal data plane, i byte attesi a
// ogni split point. Implementa stream.Catalog.
//
// È sostituibile mentre il server gira (Replace): dopo un riavvio il worker
// viene descritto di nuovo. Le letture degli handler e le sostituzioni del
// supervisore avvengono in goroutine diverse, quindi c'è un RWMutex: molte
// letture in parallelo, una scrittura ogni tanto.
type Catalog struct {
	mu     sync.RWMutex
	models map[string][]api.SegmentSize
}

// NewCatalog crea un catalogo vuoto: nessun modello ha il data plane finché
// il worker non è stato descritto.
func NewCatalog() *Catalog {
	return &Catalog{models: map[string][]api.SegmentSize{}}
}

// Segments implementa stream.Catalog.
func (c *Catalog) Segments(modelKey string) ([]api.SegmentSize, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.models[modelKey]
	return s, ok
}

// Models restituisce le chiavi dei modelli disponibili, in ordine.
func (c *Catalog) Models() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Sorted(maps.Keys(c.models))
}

// Replace sostituisce tutto il contenuto (nil = nessun modello disponibile,
// per esempio mentre il worker è giù).
func (c *Catalog) Replace(models map[string][]api.SegmentSize) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if models == nil {
		models = map[string][]api.SegmentSize{}
	}
	c.models = models
}

// Mismatch descrive un modello che il worker serve in modo diverso da come lo
// conosce il registry: per quel modello il data plane resta disabilitato.
type Mismatch struct {
	ModelKey string
	Reason   string
}

func (m Mismatch) String() string { return fmt.Sprintf("model %q: %s", m.ModelKey, m.Reason) }

// BuildCatalog confronta la descrizione del worker con i modelli del
// registry e restituisce le dimensioni per i modelli coerenti.
//
// Per ogni split point k la dimensione attesa è l'input del segmento k+1:
//
//	split -1 (input del modello) → segmento 0
//	split  k                     → segmento k+1, per k = 0 .. last-1
//
// Lo split point "last" non esiste nel data plane: significa tutto LOCAL.
func BuildCatalog(desc *workerpb.DescribeResponse, known []api.ModelEntry) (map[string][]api.SegmentSize, []Mismatch) {
	registry := make(map[string]api.ModelEntry, len(known))
	for _, e := range known {
		registry[e.ModelKey] = e
	}

	models := map[string][]api.SegmentSize{}
	var mismatches []Mismatch
	// Un modello del registry che il worker non serve non è un Mismatch:
	// non avrà il data plane e i suoi device restano LOCAL.
	for _, m := range desc.GetModels() {
		key := m.GetModelKey()
		entry, ok := registry[key]
		switch {
		case !ok:
			mismatches = append(mismatches, Mismatch{key, "not in the server config"})
			continue
		case m.GetModelHash() != entry.ModelHash:
			mismatches = append(mismatches, Mismatch{key, fmt.Sprintf(
				"model hash differs: worker has %s, registry has %s", m.GetModelHash(), entry.ModelHash)})
			continue
		case len(m.GetSegments()) != entry.NumLayers:
			mismatches = append(mismatches, Mismatch{key, fmt.Sprintf(
				"worker has %d segments, registry expects %d", len(m.GetSegments()), entry.NumLayers)})
			continue
		}

		// Lo split k richiede l'input del segmento k+1; l'ultimo segmento è
		// raggiungibile solo come k = last-1. I segmenti sono già in ordine.
		segs := m.GetSegments()
		sizes := make([]api.SegmentSize, len(segs))
		for i, s := range segs {
			sizes[i] = api.SegmentSize{SplitPoint: i - 1, InputBytes: int(s.GetInputBytes())}
		}
		models[key] = sizes
	}
	return models, mismatches
}
