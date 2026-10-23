package server

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/load"
	"github.com/UBICO/SCIoT_go_server/internal/netmeasure"
)

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }
func u64(v uint64) *uint64   { return &v }

var rowTime = time.Date(2026, 10, 10, 14, 3, 7, 123_000_000, time.UTC)

// fullRow ha tutti i campi presenti.
var fullRow = heartbeatRow{
	at:             rowTime,
	deviceID:       "web_192",
	appliedMode:    api.ModeLocal,
	appliedVersion: 1,
	serverVersion:  1,
	telemetry: api.Telemetry{
		InferenceCount:       16,
		InferenceTimeMS:      f64(61.824),
		InferenceTimeMaxMS:   f64(74.1),
		CPULoad:              f64(0.38),
		RTTMS:                f64(1.214),
		MemoryAvailableBytes: i64(8517640192),
	},
	sinceLastSeen: 1001 * time.Millisecond,
	requestBytes:  178,
	serverLoad:    load.Snapshot{CPUPercent: f64(23.44), MemoryAvailableBytes: u64(4096 << 20)},
	network:       &netmeasure.State{BandwidthBps: f64(12.5e6), LatencyMS: f64(1.234)},
}

// emptyRow: nessuna inferenza, nessun opzionale, device id lungo, gap 0.
var emptyRow = heartbeatRow{
	at:             rowTime,
	deviceID:       "a-very-long-device-identifier",
	appliedMode:    api.ModeLocal,
	appliedVersion: 2,
	serverVersion:  3,
	telemetry:      api.Telemetry{InferenceCount: 0},
	requestBytes:   95,
}

const (
	wantFullRow  = "14:03:07.123 | web_192          | LOCAL | 1/1         |   16 |  16.0 |          61.82 |         74.10 |  38.0 |     8123 |        1.21 |        1001 |        178 |      23.4 |         4096 |       100.0 |      1.23"
	wantEmptyRow = "14:03:07.123 | a-very-long-devi | LOCAL | 2/3         |    0 |   0.0 |            N/A |           N/A |   N/A |      N/A |         N/A |           0 |         95 |       N/A |          N/A |         N/A |       N/A"
)

// notLimitingRow: la banda misurata c'è ma la rete non limita; ping assente.
func notLimitingRow() heartbeatRow {
	r := emptyRow
	r.network = &netmeasure.State{BandwidthBps: f64(1e9), NotLimiting: true}
	return r
}

const wantNotLimitingRow = "14:03:07.123 | a-very-long-devi | LOCAL | 2/3         |    0 |   0.0 |            N/A |           N/A |   N/A |      N/A |         N/A |           0 |         95 |       N/A |          N/A |  not limit. |       N/A"

// --- countingReader ----------------------------------------------------------

func TestCountingReader(t *testing.T) {
	data := strings.Repeat("x", 10_000)
	c := &countingReader{r: io.NopCloser(strings.NewReader(data))}

	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) || c.n != int64(len(data)) {
		t.Errorf("letti %d byte, contati %d, want %d", len(got), c.n, len(data))
	}
}

func TestCountingReaderCountsBytesReturnedWithError(t *testing.T) {
	// iotest.DataErrReader restituisce l'ultimo blocco di dati INSIEME a
	// io.EOF: è il caso "n > 0 e err != nil" che Read deve contare.
	c := &countingReader{r: io.NopCloser(iotest.DataErrReader(strings.NewReader("hello")))}
	if _, err := io.ReadAll(c); err != nil {
		t.Fatal(err)
	}
	if c.n != 5 {
		t.Errorf("n = %d, want 5", c.n)
	}
}

// --- formatOpt -----------------------------------------------------------------

func TestFormatOpt(t *testing.T) {
	cases := []struct {
		name     string
		p        *float64
		scale    float64
		decimals int
		want     string
	}{
		{"nil", nil, 1, 2, "N/A"},
		{"zero non è N/A", f64(0), 1, 2, "0.00"},
		{"scala percentuale", f64(0.42), 100, 1, "42.0"},
		{"zero decimali", f64(1234.6), 1, 0, "1235"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatOpt(tc.p, tc.scale, tc.decimals); got != tc.want {
				t.Errorf("formatOpt = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- formatRow -----------------------------------------------------------------

func TestFormatRow(t *testing.T) {
	cases := []struct {
		name string
		row  heartbeatRow
		want string
	}{
		{"tutti i campi", fullRow, wantFullRow},
		{"finestra vuota, opzionali assenti, id lungo", emptyRow, wantEmptyRow},
		{"rete non limitante", notLimitingRow(), wantNotLimitingRow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatRow(tc.row)
			if got != tc.want {
				t.Errorf("riga errata\n got: %q\nwant: %q", got, tc.want)
			}
			// Le colonne devono essere allineate all'intestazione.
			if len(got) != len(tableHeader) {
				t.Errorf("lunghezza riga %d, intestazione %d", len(got), len(tableHeader))
			}
		})
	}
}

// --- heartbeatTable --------------------------------------------------------------

func TestTableRepeatsHeader(t *testing.T) {
	var buf bytes.Buffer
	table := newHeartbeatTable(&buf)
	for range tableHeaderRepeat + 1 {
		table.print(fullRow)
	}

	out := buf.String()
	if n := strings.Count(out, tableHeader); n != 2 {
		t.Errorf("intestazioni = %d, want 2 (riga 1 e riga %d)", n, tableHeaderRepeat+1)
	}
	if n := strings.Count(out, wantFullRow); n != tableHeaderRepeat+1 {
		t.Errorf("righe = %d, want %d", n, tableHeaderRepeat+1)
	}
	if !strings.Contains(out, strings.Repeat("-", len(tableHeader))) {
		t.Error("manca la riga di separazione sotto l'intestazione")
	}
}

func TestTableConcurrentPrint(t *testing.T) {
	// Con go test -race: senza mutex il race detector segnala rows.
	var buf bytes.Buffer
	table := newHeartbeatTable(&buf)
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { table.print(fullRow) })
	}
	wg.Wait()
	if n := strings.Count(buf.String(), wantFullRow); n != 50 {
		t.Errorf("righe = %d, want 50", n)
	}
}
