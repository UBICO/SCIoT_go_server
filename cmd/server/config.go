package main

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/config"
	"github.com/UBICO/SCIoT_go_server/internal/dispatch"
	"github.com/UBICO/SCIoT_go_server/internal/load"
	"github.com/UBICO/SCIoT_go_server/internal/offload"
	"github.com/UBICO/SCIoT_go_server/internal/registry"
	"github.com/UBICO/SCIoT_go_server/internal/server"
	"github.com/UBICO/SCIoT_go_server/internal/stream"
	"github.com/UBICO/SCIoT_go_server/internal/worker"
)

// Validazione della configurazione: una sola funzione per l'avvio e per la
// dashboard (docs/ADMIN_API.md, config). parseConfig fa tutti i passi che
// all'avvio possono fallire, nello stesso ordine, ma non avvia nulla e non
// esce: restituisce un errore. main poi usa i valori di parsed per avviare.

// parsed è tutto quello che main ricava da config.json prima di avviare.
type parsed struct {
	reg        *registry.Registry
	server     server.Config
	segmentTau time.Duration
	load       load.Config
	algName    string
	alg        offload.Algorithm // nil per "static": nessuna decisione
	algCfg     offload.LatencyConfig
	dispatch   dispatch.Config
	worker     worker.Options
	dataPlane  bool // c'è la sezione "worker"
	stream     stream.Config
	recordDir  string
	recording  bool // c'è la sezione "record"
}

// parseConfig controlla f come all'avvio e ne ricava i valori per main.
//
// Ogni passo che fallisce restituisce l'errore con il prefisso che main
// usava con log.Fatalf ("server config: …"): il messaggio nel log
// all'avvio e nella dashboard è lo stesso.
//
// Niente effetti: si può chiamare mentre il server gira. registry.New
// legge i modelli (e ne calcola l'hash) ma non scrive; offload.New crea
// l'algoritmo senza avviarlo; RecordDir legge solo il percorso. Restano in
// main gli avvii: monitor, Supervisor, Recorder.
func parseConfig(f config.File) (parsed, error) {
	var p parsed
	var err error

	// Stesso ordine di main prima del refactoring. Ogni errore con %w (non
	// %v): tiene l'errore originale dentro, per errors.Is/As.
	if p.reg, err = registry.New(f.RegistryConfig()); err != nil {
		return parsed{}, fmt.Errorf("build registry: %w", err)
	}
	if p.server, err = f.ServerConfig(); err != nil {
		return parsed{}, fmt.Errorf("server config: %w", err)
	}
	if p.segmentTau, err = f.DeviceSegmentTau(); err != nil {
		return parsed{}, fmt.Errorf("server config: %w", err)
	}
	if p.load, err = f.LoadConfig(); err != nil {
		return parsed{}, fmt.Errorf("load monitor config: %w", err)
	}
	if p.algName, p.algCfg, err = f.OffloadConfig(); err != nil {
		return parsed{}, fmt.Errorf("offload config: %w", err)
	}
	// "static" non ha un algoritmo: p.alg resta nil, nessuna decisione.
	if p.algName != (offload.Static{}).Name() {
		if p.alg, err = offload.New(p.algName, p.algCfg); err != nil {
			return parsed{}, fmt.Errorf("offload: %w", err)
		}
	}
	if p.dispatch, err = f.DispatchConfig(); err != nil {
		return parsed{}, fmt.Errorf("dispatch config: %w", err)
	}
	if p.worker, p.dataPlane, err = f.WorkerOptions(p.reg.Models()); err != nil {
		return parsed{}, fmt.Errorf("worker config: %w", err)
	}
	if p.stream, err = f.StreamConfig(); err != nil {
		return parsed{}, fmt.Errorf("stream config: %w", err)
	}
	if p.recordDir, p.recording, err = f.RecordDir(); err != nil {
		return parsed{}, fmt.Errorf("record config: %w", err)
	}
	return p, nil
}

// checkConfig dice se il testo next partirebbe al prossimo avvio: gli stessi
// controlli dell'avvio (config.Parse, parseConfig), più la regola della
// dashboard: worker.command non cambia rispetto a current, il file in uso.
//
// Perché: worker.command è una riga di comando che il server esegue. Se la
// dashboard potesse cambiarla, chi ha il token admin potrebbe far eseguire
// qualunque comando sulla macchina. Si cambia solo a mano, sul server.
//
// Se current non si legge (il file è stato rotto a mano), il comando in uso
// vale "nessuno": next è accettato solo senza worker.command. Un file rotto
// con un comando si sistema a mano.
func checkConfig(current, next []byte) error {
	// Gli errori di Parse e parseConfig hanno già il loro prefisso.
	f, err := config.Parse(next)
	if err != nil {
		return err
	}
	if _, err := parseConfig(f); err != nil {
		return err
	}

	// Il comando in uso: se current non si legge vale "nessuno" (nil).
	var curCmd, nextCmd []string
	if cur, err := config.Parse(current); err == nil && cur.Worker != nil {
		curCmd = cur.Worker.Command
	}
	if f.Worker != nil {
		nextCmd = f.Worker.Command
	}
	if !slices.Equal(curCmd, nextCmd) {
		return errors.New("worker.command cannot be changed from the dashboard: edit config.json on the server")
	}
	return nil
}
