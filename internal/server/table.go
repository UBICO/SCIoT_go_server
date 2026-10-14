package server

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/load"
)

// ---------------------------------------------------------------------------
// Tabella degli heartbeat (modalità verbose)
// ---------------------------------------------------------------------------
//
// Una riga per ogni heartbeat accettato: è l'equivalente della tabella per
// inferenza del server Python, ma ogni riga riassume la finestra di
// inferenze dall'heartbeat precedente.

const (
	tableTimeFormat   = "15:04:05.000" // layout Go: si scrive con la data di riferimento
	deviceColumnWidth = 16
	tableHeaderRepeat = 20 // ripeti l'intestazione ogni N righe, come il Python
)

// tableHeader definisce le colonne e la loro larghezza. formatRow deve
// produrre righe con le STESSE larghezze, separate da " | ".
var tableHeader = fmt.Sprintf(
	"%-12s | %-*s | %-5s | %-11s | %4s | %5s | %14s | %13s | %5s | %8s | %11s | %11s | %10s | %9s | %12s",
	"Time", deviceColumnWidth, "Device", "Mode", "Ver app/srv", "Inf", "Inf/s",
	"Comp mean (ms)", "Comp max (ms)", "CPU %", "Mem (MB)",
	"HB RTT (ms)", "HB gap (ms)", "HB req (B)",
	"Srv CPU %", "Srv mem (MB)",
)

// heartbeatRow contiene i valori di una riga.
type heartbeatRow struct {
	at             time.Time
	deviceID       string
	appliedMode    api.ExecutionMode // modalità che il client dichiara di applicare
	appliedVersion uint64            // versione che il client dichiara di applicare
	serverVersion  uint64            // versione della policy sul server
	telemetry      api.Telemetry
	sinceLastSeen  time.Duration // "HB gap": tempo dal contatto precedente
	requestBytes   int64         // "HB req": byte del corpo dell'heartbeat
	serverLoad     load.Snapshot // carico del server al momento dell'heartbeat
}

// ---------------------------------------------------------------------------
// Conteggio dei byte del corpo
// ---------------------------------------------------------------------------

// countingReader avvolge un io.ReadCloser e conta i byte letti.
//
// io.Reader è l'interfaccia più importante della libreria standard: un solo
// metodo, Read(p []byte) (n int, err error). Chi la implementa può essere
// passato a QUALUNQUE funzione che legge (json.Decoder, io.Copy, ...).
// Avvolgere un Reader in un altro che aggiunge un comportamento (qui:
// contare) è il pattern "decorator" tipico di Go: MaxBytesReader fa lo stesso.
type countingReader struct {
	r io.ReadCloser
	n int64
}

// Read legge dal reader avvolto e accumula il numero di byte letti.
func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Close chiude il reader avvolto. Insieme a Read, fa di *countingReader un
// io.ReadCloser: può quindi prendere il posto di r.Body.
func (c *countingReader) Close() error {
	return c.r.Close()
}

// ---------------------------------------------------------------------------
// Formattazione
// ---------------------------------------------------------------------------

// formatOpt formatta un valore opzionale: "N/A" se p è nil, altrimenti
// (*p * scale) con il numero di decimali indicato.
//
//	formatOpt(nil, 1, 2)            → "N/A"
//	formatOpt(&v /*0.42*/, 100, 1)  → "42.0"
func formatOpt(p *float64, scale float64, decimals int) string {
	if p == nil {
		return "N/A"
	}
	return fmt.Sprintf("%.*f", decimals, *p*scale)
}

// formatRow costruisce la riga di tabella per r (senza "\n" finale).
func formatRow(r heartbeatRow) string {
	device := r.deviceID
	if len(device) > deviceColumnWidth {
		device = device[:deviceColumnWidth]
	}

	var infPerSec float64
	if r.sinceLastSeen > 0 {
		infPerSec = float64(r.telemetry.InferenceCount) / r.sinceLastSeen.Seconds()
	}

	var memMiB *float64
	if m := r.telemetry.MemoryAvailableBytes; m != nil {
		v := float64(*m) / (1 << 20)
		memMiB = &v
	}

	var srvMemMiB *float64
	if m := r.serverLoad.MemoryAvailableBytes; m != nil {
		v := float64(*m) / (1 << 20)
		srvMemMiB = &v
	}

	gapMS := float64(r.sinceLastSeen) / float64(time.Millisecond)

	return fmt.Sprintf(
		"%-12s | %-*s | %-5s | %-11s | %4d | %5.1f | %14s | %13s | %5s | %8s | %11s | %11.0f | %10d | %9s | %12s",
		r.at.Format(tableTimeFormat),
		deviceColumnWidth, device,
		r.appliedMode,
		fmt.Sprintf("%d/%d", r.appliedVersion, r.serverVersion),
		r.telemetry.InferenceCount,
		infPerSec,
		formatOpt(r.telemetry.InferenceTimeMS, 1, 2),
		formatOpt(r.telemetry.InferenceTimeMaxMS, 1, 2),
		formatOpt(r.telemetry.CPULoad, 100, 1),
		formatOpt(memMiB, 1, 0),
		formatOpt(r.telemetry.RTTMS, 1, 2),
		gapMS,
		r.requestBytes,
		formatOpt(r.serverLoad.CPUPercent, 1, 1),
		formatOpt(srvMemMiB, 1, 0),
	)
}

// ---------------------------------------------------------------------------
// Stampa
// ---------------------------------------------------------------------------

// heartbeatTable scrive le righe su w, ripetendo l'intestazione.
//
// Gli handler HTTP girano in goroutine diverse: senza mutex due heartbeat
// simultanei potrebbero leggere e incrementare rows insieme (data race) e
// intrecciare intestazione e righe.
type heartbeatTable struct {
	mu   sync.Mutex
	w    io.Writer // os.Stdout in produzione, un bytes.Buffer nei test
	rows int
}

func newHeartbeatTable(w io.Writer) *heartbeatTable {
	return &heartbeatTable{w: w}
}

// print scrive una riga, preceduta dall'intestazione ogni tableHeaderRepeat righe.
func (t *heartbeatTable) print(r heartbeatRow) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.rows%tableHeaderRepeat == 0 {
		fmt.Fprintln(t.w)
		fmt.Fprintln(t.w, tableHeader)
		fmt.Fprintln(t.w, strings.Repeat("-", len(tableHeader)))
	}
	fmt.Fprintln(t.w, formatRow(r))
	t.rows++
}
