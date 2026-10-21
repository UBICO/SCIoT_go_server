// Package config legge il file di configurazione del server (config.json).
//
// Il file ha una sezione per ogni componente:
//
//	{
//	  "server": {"heartbeat_interval_ms": 1000},
//	  "models": [ {...registry.ModelConfig...} ]
//	}
//
// Questo package conosce il formato del file e lo traduce nelle Config dei
// singoli package (server.Config, registry.Config). I package di dominio
// restano ignari del file JSON.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/load"
	"github.com/UBICO/SCIoT_go_server/internal/registry"
	"github.com/UBICO/SCIoT_go_server/internal/server"
	"github.com/UBICO/SCIoT_go_server/internal/stream"
	"github.com/UBICO/SCIoT_go_server/internal/worker"
)

// Limiti accettati per l'intervallo di heartbeat. Sotto i 100 ms il traffico
// di controllo diventa paragonabile a quello dei dati; sopra i 60 s il server
// reagirebbe troppo lentamente ai cambi di carico.
const (
	MinHeartbeatInterval = 100 * time.Millisecond
	MaxHeartbeatInterval = 60 * time.Second

	// Stesso range per il campionamento del carico del server.
	MinLoadSampleInterval = 100 * time.Millisecond
	MaxLoadSampleInterval = 60 * time.Second
)

// File è il contenuto completo di config.json.
type File struct {
	Server Server                 `json:"server"`
	Models []registry.ModelConfig `json:"models"`

	// Worker e Stream sono opzionali: senza la sezione "worker" il server
	// gira solo con il control plane (nessun data plane, tutti LOCAL).
	Worker *Worker `json:"worker,omitempty"`
	Stream Stream  `json:"stream"`

	// Record è opzionale: senza la sezione il server non registra nulla.
	Record *Record `json:"record,omitempty"`
}

// Record è la sezione "record": dove scrivere la registrazione degli eventi
// (internal/record, docs/RECORDING.md). Un file per ogni avvio del server.
type Record struct {
	Dir string `json:"dir"`
}

// Worker è la sezione "worker": il processo che esegue i segmenti TFLite.
//
//	"worker": {
//	  "command": ["uv", "run", "--project", "worker", "sciot-worker"],
//	  "address": "127.0.0.1:50051",
//	  "slots": 2, "threads": 6,
//	  "startup_timeout_ms": 120000
//	}
//
// Con "command" vuoto il worker è esterno: avviato da altri su "address".
type Worker struct {
	Command          []string `json:"command,omitempty"`
	Address          string   `json:"address"`
	Slots            int      `json:"slots"`
	Threads          int      `json:"threads"`
	StartupTimeoutMS *int     `json:"startup_timeout_ms,omitempty"`
}

// Stream è la sezione "stream": i limiti del data plane. Tutto opzionale.
type Stream struct {
	MaxInflight       *int `json:"max_inflight,omitempty"`
	DefaultDeadlineMS *int `json:"default_deadline_ms,omitempty"`
	MaxMessageBytes   *int `json:"max_message_bytes,omitempty"`
}

// Server è la sezione "server". Ogni campo è opzionale: se manca vale il
// default di server.DefaultConfig(). Per distinguere "assente" da "0"
// (che sarebbe un valore non valido) il campo è un puntatore.
type Server struct {
	HeartbeatIntervalMS *int  `json:"heartbeat_interval_ms,omitempty"`
	Verbose             *bool `json:"verbose,omitempty"`
	Admin               *bool `json:"admin,omitempty"`

	// Campionamento del carico della macchina del server (internal/load).
	LoadSampleIntervalMS *int     `json:"load_sample_interval_ms,omitempty"`
	LoadCPUEWMAAlpha     *float64 `json:"load_cpu_ewma_alpha,omitempty"`
}

// Load legge e decodifica il file di configurazione.
// (Spostata qui da registry.LoadConfig: il file non riguarda più solo i modelli.)
func Load(path string) (File, error) {
	file, err := os.Open(path)
	if err != nil {
		return File{}, fmt.Errorf("%w", err)
	}
	defer file.Close()

	dec := json.NewDecoder(file)
	dec.DisallowUnknownFields()

	var f File
	if err := dec.Decode(&f); err != nil {
		return File{}, fmt.Errorf("decode config: %w%s", err, removedKeyHint(err))
	}
	return f, nil
}

// RegistryConfig restituisce la configurazione per registry.New.
func (f File) RegistryConfig() registry.Config {
	return registry.Config{Models: f.Models}
}

// ServerConfig restituisce la configurazione per server.New: i default di
// server.DefaultConfig() con sopra i valori presenti nel file.
func (f File) ServerConfig() (server.Config, error) {
	cfg := server.DefaultConfig()
	if f.Server.HeartbeatIntervalMS != nil {
		interval := time.Duration(*f.Server.HeartbeatIntervalMS) * time.Millisecond

		if interval < MinHeartbeatInterval || interval > MaxHeartbeatInterval {
			return server.Config{}, fmt.Errorf("heartbeat_interval_ms out of range [%v, %v], got %v", MinHeartbeatInterval, MaxHeartbeatInterval, interval)
		}
		cfg.HeartbeatInterval = interval
	}
	if f.Server.Verbose != nil {
		cfg.Verbose = *f.Server.Verbose
	}
	if f.Server.Admin != nil {
		cfg.Admin = *f.Server.Admin
	}
	return cfg, nil
}

// LoadConfig restituisce la configurazione del monitor di carico:
// load.DefaultConfig() con sopra i valori presenti nel file.
func (f File) LoadConfig() (load.Config, error) {

	cfg := load.DefaultConfig()
	if f.Server.LoadSampleIntervalMS != nil {
		interval := time.Duration(*f.Server.LoadSampleIntervalMS) * time.Millisecond

		if interval < MinLoadSampleInterval || interval > MaxLoadSampleInterval {
			return load.Config{}, fmt.Errorf("load_sample_interval_ms out of range [%v, %v], got %v", MinLoadSampleInterval, MaxLoadSampleInterval, interval)
		}
		cfg.Interval = interval
	}
	//   - load_sample_interval_ms, se presente: converti in Duration e
	//     verifica [MinLoadSampleInterval, MaxLoadSampleInterval]
	//   - load_cpu_ewma_alpha, se presente: deve stare in (0, 1].
	//     alpha = 0 significherebbe "ignora sempre i campioni nuovi": la media
	//     resterebbe ferma al primo valore, quindi va rifiutato.
	//   - errori con il nome JSON del campo, il valore e il range atteso.
	if f.Server.LoadCPUEWMAAlpha != nil {
		alpha := *f.Server.LoadCPUEWMAAlpha

		if alpha <= 0 || alpha > 1 {
			return load.Config{}, fmt.Errorf("load_cpu_ewma_alpha out of range (0, 1], got %v", alpha)
		}
		cfg.CPUEWMAAlpha = alpha
	}

	return cfg, nil
}

// Valori fissi del supervisore: non serve esporli in config.json.
const (
	DefaultWorkerStartupTimeout = 2 * time.Minute // import di TensorFlow + caricamento dei modelli
	workerMinBackoff            = time.Second
	workerMaxBackoff            = 30 * time.Second
	workerStopGrace             = 10 * time.Second
	workerHealthInterval        = 5 * time.Second
)

// WorkerOptions restituisce le opzioni del supervisore e true, oppure false
// se la sezione "worker" manca (data plane disabilitato).
//
// known sono i modelli del registry (registry.Models()): il worker carica gli
// stessi modelli del file, e il supervisore ne verifica gli hash.
func (f File) WorkerOptions(known []api.ModelEntry) (worker.Options, bool, error) {
	if f.Worker == nil {
		return worker.Options{}, false, nil
	}
	w := f.Worker
	if w.Address == "" {
		return worker.Options{}, false, fmt.Errorf("worker.address is required")
	}
	// Per un worker esterno (command vuoto) slots e threads li decide chi lo
	// avvia: si validano solo quando li passa il supervisore.
	if len(w.Command) > 0 {
		if w.Slots < 1 {
			return worker.Options{}, false, fmt.Errorf("worker.slots must be >= 1, got %d", w.Slots)
		}
		if w.Threads < 1 {
			return worker.Options{}, false, fmt.Errorf("worker.threads must be >= 1, got %d", w.Threads)
		}
	}
	startupTimeout := DefaultWorkerStartupTimeout
	if w.StartupTimeoutMS != nil {
		if *w.StartupTimeoutMS < 1 {
			return worker.Options{}, false, fmt.Errorf("worker.startup_timeout_ms must be >= 1, got %d", *w.StartupTimeoutMS)
		}
		startupTimeout = time.Duration(*w.StartupTimeoutMS) * time.Millisecond
	}

	models := make([]worker.ModelArg, len(f.Models))
	for i, m := range f.Models {
		models[i] = worker.ModelArg{Key: m.ModelKey, Dir: m.TFLiteDir}
	}
	return worker.Options{
		Command:        w.Command,
		Address:        w.Address,
		Models:         models,
		Slots:          w.Slots,
		Threads:        w.Threads,
		Known:          known,
		StartupTimeout: startupTimeout,
		MinBackoff:     workerMinBackoff,
		MaxBackoff:     workerMaxBackoff,
		StopGrace:      workerStopGrace,
		HealthInterval: workerHealthInterval,
	}, true, nil
}

// StreamConfig restituisce i limiti dello stream: stream.DefaultConfig() con
// sopra i valori presenti nel file.
func (f File) StreamConfig() (stream.Config, error) {
	cfg := stream.DefaultConfig()
	s := f.Stream
	if s.MaxInflight != nil {
		if *s.MaxInflight < 1 {
			return stream.Config{}, fmt.Errorf("stream.max_inflight must be >= 1, got %d", *s.MaxInflight)
		}
		cfg.MaxInflight = *s.MaxInflight
	}
	if s.DefaultDeadlineMS != nil {
		if *s.DefaultDeadlineMS < 1 {
			return stream.Config{}, fmt.Errorf("stream.default_deadline_ms must be >= 1, got %d", *s.DefaultDeadlineMS)
		}
		cfg.DefaultDeadline = time.Duration(*s.DefaultDeadlineMS) * time.Millisecond
	}
	if s.MaxMessageBytes != nil {
		if *s.MaxMessageBytes < 1<<10 || *s.MaxMessageBytes > worker.MaxMessageBytes {
			return stream.Config{}, fmt.Errorf("stream.max_message_bytes must be in [1024, %d], got %d",
				worker.MaxMessageBytes, *s.MaxMessageBytes)
		}
		cfg.MaxMessageBytes = *s.MaxMessageBytes
	}
	return cfg, nil
}

// removedKeys sono chiavi di models[] tolte quando il server ha cominciato a
// leggere il manifest.json del modello: un config.json vecchio fallisce con
// "unknown field", e il suggerimento spiega perché.
var removedKeys = map[string]string{
	"last_offloading_layer": "it is now the number of segments in manifest.json",
	"input_size_bytes":      "the model input now comes from manifest.json",
}

func removedKeyHint(err error) string {
	for key, why := range removedKeys {
		if strings.Contains(err.Error(), fmt.Sprintf("unknown field %q", key)) {
			return fmt.Sprintf(" (%s was removed: %s; see docs/MODELS.md)", key, why)
		}
	}
	return ""
}

// RecordDir restituisce la cartella della registrazione e true, oppure false
// se la sezione "record" manca. "~/" all'inizio indica la home dell'utente.
func (f File) RecordDir() (string, bool, error) {
	if f.Record == nil {
		return "", false, nil
	}
	dir := f.Record.Dir
	if dir == "" {
		return "", false, errors.New("record.dir is required")
	}
	if rest, ok := strings.CutPrefix(dir, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false, fmt.Errorf("record.dir: %w", err)
		}
		dir = filepath.Join(home, rest)
	}
	return dir, true, nil
}
