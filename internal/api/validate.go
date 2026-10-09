package api

import (
	"errors"
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
	// TODO(2): sostituisci questo controllo con le regole della finestra.
	//   Sono regole TRA CAMPI: la validità di uno dipende dal valore di un altro.
	//   - inference_count >= 0
	//   - se inference_count == 0: inference_time_ms e inference_time_max_ms
	//     devono essere assenti (nil)
	//   - se inference_count > 0: entrambi presenti, e
	//     0 <= inference_time_ms <= inference_time_max_ms
	//     (la media non può superare il massimo)
	//   Suggerimento: uno switch senza espressione è un if/else-if più leggibile:
	//     switch {
	//     case t.InferenceCount < 0:
	//         return ...
	//     case t.InferenceCount == 0 && (... != nil || ... != nil):
	//         return ...
	//     }
	//   Le regole sugli altri campi (cpu_load, ...) restano come sono.
	switch {
	case t.InferenceCount < 0:
		return fmt.Errorf("inference_count must be >= 0, got %v", t.InferenceCount)

	case t.InferenceCount == 0 && (t.InferenceTimeMS != nil || t.InferenceTimeMaxMS != nil):
		return errors.New("inference_time_ms and inference_time_max_ms must be absent when inference_count is 0")

	case t.InferenceCount > 0 && (t.InferenceTimeMS == nil || t.InferenceTimeMaxMS == nil):
		return errors.New("inference_time_ms and inference_time_max_ms are required when inference_count > 0")

	case t.InferenceCount > 0 && (*t.InferenceTimeMS < 0 || *t.InferenceTimeMS > *t.InferenceTimeMaxMS):
		return fmt.Errorf("inference_time_ms %v must be in [0, inference_time_max_ms %v]", *t.InferenceTimeMS, *t.InferenceTimeMaxMS)
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
