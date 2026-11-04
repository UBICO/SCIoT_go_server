package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/netmeasure"
	"github.com/UBICO/SCIoT_go_server/internal/record"
)

// Il probe misura la banda di un device che non invia frame (LOCAL): il
// server chiede "probe_request" con Config.ProbeBytes byte, il device
// risponde con un messaggio binario "probe" che readLoop misura come un
// frame (Observe) e handleProbe controlla. Non viene eseguito.
//
// La regola è della metrica, non dell'algoritmo: si chiede un probe solo se
// la misura è vecchia (nessun campione, valido o no, da ProbeInterval). In
// SPLIT i frame bastano e il probe non parte.
//
// Finché la banda non ha una stima l'intervallo è ProbeUnknownInterval. Su un
// link che non fa mai aspettare le letture (localhost, LAN veloce) nessun
// probe è un campione valido di banda: l'invio si conosce solo con "rete non
// limitante", che chiede 3 campioni in 60 s. Con un probe ogni 30 s non ci
// arriverebbe mai, e un device in LOCAL non saprebbe mai quanto costa
// inviare.

// probeCheckEvery è ogni quanto probeLoop guarda se serve un probe: la
// precisione della regola, non la frequenza dei probe.
const probeCheckEvery = time.Second

// probeLoop chiede un probe quando probeDue lo dice, finché ctx vive.
func (h *Handler) probeLoop(ctx context.Context, c *conn) {
	t := time.NewTicker(min(probeCheckEvery, h.cfg.ProbeInterval))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := h.now()
		interval := probeInterval(c.net.Snapshot(now), h.cfg.ProbeInterval, h.cfg.ProbeUnknownInterval)
		c.mu.Lock()
		due := probeDue(now, c.net.LastSample(), c.probeSentAt, interval)
		var id uint64
		if due {
			c.lastProbeID++
			id = c.lastProbeID
			c.probeID, c.probeSentAt = id, now
		}
		c.mu.Unlock()
		if !due {
			continue
		}
		req := api.ProbeRequest{Type: api.MsgProbeRequest, ProbeID: id, Bytes: h.cfg.ProbeBytes}
		if err := h.writeJSON(ctx, c, req); err != nil {
			if ctx.Err() == nil {
				log.Printf("stream: device=%s: write probe_request: %v", c.deviceID, err)
			}
			return // lo stream si sta chiudendo
		}
	}
}

// probeInterval è l'intervallo dei probe di un device con la rete st:
// unknown finché la banda non ha una stima (st.BandwidthBps nil), interval
// dopo. Mai più lungo di interval; unknown = 0 vuol dire sempre interval.
func probeInterval(st netmeasure.State, interval, unknown time.Duration) time.Duration {
	if unknown > 0 && st.BandwidthBps == nil {
		return min(unknown, interval)
	}
	return interval
}

// probeDue dice se chiedere un probe adesso.
//
//   - lastSample: quando è arrivato l'ultimo campione di rete (zero = mai);
//   - pendingSince: quando è partito il probe ancora senza risposta (zero =
//     nessuno in attesa);
//   - interval: Config.ProbeInterval, > 0.
func probeDue(now, lastSample, pendingSince time.Time, interval time.Duration) bool {
	if !pendingSince.IsZero() && now.Sub(pendingSince) < interval {
		return false
	}
	return lastSample.IsZero() || now.Sub(lastSample) >= interval
}

// handleProbe controlla la risposta a un probe_request. Il campione è già
// stato misurato da readLoop: qui si verifica che sia la risposta attesa e si
// registra l'evento.
func (h *Handler) handleProbe(ctx context.Context, c *conn, rawHeader []byte, payloadBytes int,
	sample netmeasure.Sample) {
	var hdr api.ProbeHeader
	if err := json.Unmarshal(rawHeader, &hdr); err != nil {
		h.sendError(ctx, c, 0, api.ErrCodeInvalidFrame, "invalid probe header: "+err.Error())
		return
	}
	var problem string
	c.mu.Lock()
	switch {
	case c.probeID == 0 || c.probeID != hdr.ProbeID:
		problem = fmt.Sprintf("unexpected probe_id %d", hdr.ProbeID)
	case payloadBytes != h.cfg.ProbeBytes:
		problem = fmt.Sprintf("probe of %d bytes, requested %d", payloadBytes, h.cfg.ProbeBytes)
	default:
		c.probeID, c.probeSentAt = 0, time.Time{}
	}
	c.mu.Unlock()
	// sendError scrive sulla rete: fuori dal mutex.
	if problem != "" {
		h.sendError(ctx, c, 0, api.ErrCodeInvalidFrame, problem)
		return
	}
	h.rec.Record(probeEvent(c, hdr.ProbeID, payloadBytes, sample))
}

// probeEvent costruisce l'evento di registrazione di un probe.
func probeEvent(c *conn, probeID uint64, bytes int, sample netmeasure.Sample) *record.Probe {
	var waitedShare float64
	if sample.Bytes > 0 {
		waitedShare = float64(sample.WaitedBytes) / float64(sample.Bytes)
	}
	return &record.Probe{
		DeviceID:           c.deviceID,
		SessionID:          c.sessionID,
		ProbeID:            probeID,
		Bytes:              bytes,
		ReceiveMS:          float64(sample.Receive) / float64(time.Millisecond),
		ReceiveWaitedShare: waitedShare,
		ReceiveValid:       sample.Valid,
	}
}
