package api

import (
	"fmt"
)

// I metodi di un tipo devono stare nello STESSO package del tipo: per questo
// la validazione di Telemetry è qui e non in internal/server.

// Valid riporta se m è una modalità conosciuta.
// Esempio completo: receiver valore (m ExecutionMode), perché non modifica m.
func (m ExecutionMode) Valid() bool {
	// switch in Go non ha "fallthrough" implicito: niente break necessari,
	// e un case può elencare più valori.
	switch m {
	case ModeLocal, ModeSplit:
		return true
	default:
		return false
	}
}

// Validate controlla che i valori della telemetria siano plausibili.
// Restituisce nil se è tutto valido, altrimenti un errore che descrive il
// PRIMO campo non valido (con il nome JSON, così il client capisce quale).
func (t Telemetry) Validate() error {
	if t.InferenceTimeMS < 0 {
		return fmt.Errorf("inference_time_ms must be >= 0, got %v", t.InferenceTimeMS)
	}

	if t.CPULoad != nil && (*t.CPULoad < 0 || *t.CPULoad > 1) {
		return fmt.Errorf("cpu_load out of range [0, 1], got %v", *t.CPULoad)
	}

	if t.RTTMS != nil && *t.RTTMS < 0 {
		return fmt.Errorf("rtt_ms must be >= 0, got %v", *t.RTTMS)
	}

	if t.MemoryAvailableBytes != nil && *t.MemoryAvailableBytes < 0 {
		return fmt.Errorf("memory_available_bytes must be >= 0, got %v", *t.MemoryAvailableBytes)
	}

	return nil
}
