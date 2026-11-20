package record

import (
	"os"
	"runtime/metrics"
	"time"
)

// RuntimeInterval è ogni quanto il server registra un evento runtime. Fisso:
// 10 s danno abbastanza punti anche in un run di 90 s, e in 8 ore sono 2880
// righe, niente rispetto ai frame.
const RuntimeInterval = 10 * time.Second

// SampleRuntime legge le risorse del processo. Devices lo riempie chi chiama
// (il record non conosce lo store dei device).
//
// Usa runtime/metrics e non runtime.ReadMemStats, che ferma tutte le
// goroutine per tutta la lettura.
func SampleRuntime() Runtime {
	samples := []metrics.Sample{
		{Name: "/sched/goroutines:goroutines"},
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/total:bytes"},
	}
	metrics.Read(samples)
	var r Runtime
	r.Goroutines = int(samples[0].Value.Uint64())
	r.HeapBytes = samples[1].Value.Uint64()
	r.GoMemoryBytes = samples[2].Value.Uint64()
	// Le voci di /dev/fd sono i descrittori aperti (macOS e Linux). ReadDir
	// apre la cartella stessa, che compare nell'elenco: se ne toglie una. Se
	// non si legge resta nil (null nel JSON): 0 sarebbe un valore.
	if entries, err := os.ReadDir("/dev/fd"); err == nil {
		n := len(entries) - 1
		r.OpenFiles = &n
	}
	return r
}
