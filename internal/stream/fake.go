package stream

import (
	"context"
	"sync"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
)

// FakeExecutor simula l'esecuzione di un frame: utile nei test e per
// sviluppare il client (2.6) prima che esista il worker vero (2.4).
type FakeExecutor struct {
	// Delay restituisce la durata simulata di un job (nil = nessuna attesa).
	Delay func(Job) time.Duration
	// Err, se non nil, viene restituito invece di un risultato.
	Err error
	// OutputBytes è la dimensione del tensore di output simulato.
	OutputBytes int
	// Detections restituite per ogni job.
	Detections []api.Detection
	// Block, se non nil, sospende Execute finché il canale non viene chiuso.
	Block chan struct{}
	// Admission, se non nil, è la previsione restituita con il risultato.
	Admission *Admission

	mu   sync.Mutex
	jobs []Job
}

// Execute registra il job e restituisce un risultato simulato.
func (f *FakeExecutor) Execute(ctx context.Context, job Job) (Outcome, error) {
	f.mu.Lock()
	f.jobs = append(f.jobs, job)
	f.mu.Unlock()

	if f.Block != nil {
		select {
		case <-f.Block:
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		}
	}
	if f.Delay != nil {
		select {
		case <-time.After(f.Delay(job)):
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		}
	}
	if f.Err != nil {
		return Outcome{}, f.Err
	}
	return Outcome{
		Output:      make([]byte, f.OutputBytes),
		Detections:  f.Detections,
		LayerMS:     []float64{1, 2},
		QueueWaitMS: 0.5,
		Admission:   f.Admission,
	}, nil
}

// Jobs restituisce una copia dei job ricevuti.
func (f *FakeExecutor) Jobs() []Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Job(nil), f.jobs...)
}
