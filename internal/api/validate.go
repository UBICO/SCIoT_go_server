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
	// Regole della finestra di inferenza (tra campi). L'ordine dei case conta:
	// il terzo garantisce che i puntatori non siano nil prima che il quarto
	// li dereferenzi.
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

	return t.validateSegments()
}

// Validate controlla una richiesta di policy rispetto al modello del device:
// LOCAL senza split_point, SPLIT con -1 <= split_point <= last_offloading_layer-1.
//
// split_point è l'ultimo segmento eseguito dal device, come nei frame:
//   - -1: il device non esegue nulla e invia l'input del modello (tutto sul server);
//   - last_offloading_layer-1: al server resta solo l'ultimo segmento;
//   - last_offloading_layer non è uno SPLIT: al server non resterebbe nulla
//     (è LOCAL).
func (r SetPolicyRequest) Validate(m ModelInfo) error {
	switch r.Mode {
	case ModeLocal:
		if r.SplitPoint != nil {
			return fmt.Errorf("LOCAL policy must not have a split_point, got %d", *r.SplitPoint)
		}
		if r.MaxInflight != nil {
			return fmt.Errorf("LOCAL policy must not have a max_inflight, got %d", *r.MaxInflight)
		}
	case ModeSplit:
		if r.SplitPoint == nil {
			return errors.New("SPLIT policy requires a split_point")
		}
		if sp := *r.SplitPoint; sp < -1 || sp > m.LastOffloadingLayer-1 {
			return fmt.Errorf("split_point %d out of range [-1, %d] for model %q (%d would leave nothing to the server: use LOCAL)",
				sp, m.LastOffloadingLayer-1, m.ModelKey, m.LastOffloadingLayer)
		}
		// Il massimo è dello stream: lo controlla chi lo conosce (il server).
		if r.MaxInflight != nil && *r.MaxInflight < 1 {
			return fmt.Errorf("max_inflight must be >= 1, got %d", *r.MaxInflight)
		}
	default:
		return fmt.Errorf("invalid mode %q", r.Mode)
	}
	return nil
}

// validateSegments controlla segment_ms e segment_count tra loro. La
// lunghezza rispetto al modello la controlla device.Store.Heartbeat, che
// conosce il modello della sessione.
func (t Telemetry) validateSegments() error {
	// Entrambi assenti è valido (client che non li invia); uno solo no.
	if (t.SegmentMS == nil) != (t.SegmentCount == nil) {
		return errors.New("segment_ms and segment_count must be sent together")
	}
	if len(t.SegmentMS) != len(t.SegmentCount) {
		return fmt.Errorf("segment_ms has %d entries but segment_count has %d", len(t.SegmentMS), len(t.SegmentCount))
	}

	for i, ms := range t.SegmentMS {
		n := t.SegmentCount[i]
		switch {
		case n < 0:
			return fmt.Errorf("segment_count[%d] must be >= 0, got %d", i, n)
		case ms == nil && n > 0:
			return fmt.Errorf("segment_ms[%d] is null but segment_count[%d] is %d", i, i, n)
		case ms != nil && n == 0:
			return fmt.Errorf("segment_ms[%d] is %v but segment_count[%d] is 0", i, *ms, i)
		case ms != nil && *ms < 0:
			return fmt.Errorf("segment_ms[%d] must be >= 0, got %v", i, *ms)
		}
	}
	return nil
}
