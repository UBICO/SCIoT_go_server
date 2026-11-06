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
	"slices"
	"strings"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
	"github.com/UBICO/SCIoT_go_server/internal/device"
	"github.com/UBICO/SCIoT_go_server/internal/dispatch"
	"github.com/UBICO/SCIoT_go_server/internal/load"
	"github.com/UBICO/SCIoT_go_server/internal/netmeasure"
	"github.com/UBICO/SCIoT_go_server/internal/offload"
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
	Worker   *Worker  `json:"worker,omitempty"`
	Stream   Stream   `json:"stream"`
	Dispatch Dispatch `json:"dispatch"`
	Offload  Offload  `json:"offload"`

	// Record è opzionale: senza la sezione il server non registra nulla.
	Record *Record `json:"record,omitempty"`
}

// Dispatch è la sezione "dispatch": la coda davanti al worker. Opzionale.
type Dispatch struct {
	// ServiceTauMS: costante di tempo delle stime del tempo di servizio S.
	ServiceTauMS *int `json:"service_tau_ms,omitempty"`
	// ProfileRuns: esecuzioni misurate del profilo di ogni modello quando il
	// worker diventa pronto; 0 = nessun profilo.
	ProfileRuns *int `json:"profile_runs,omitempty"`
	// ReprofileAgeMS: un modello con la stima di un segmento più vecchia di
	// così viene profilato di nuovo; 0 = mai.
	ReprofileAgeMS *int `json:"reprofile_age_ms,omitempty"`
}

// Offload è la sezione "offload": l'algoritmo che decide la policy dei
// device (internal/offload, docs/OFFLOAD.md). Opzionale: senza, "static".
type Offload struct {
	Algorithm      string   `json:"algorithm,omitempty"`
	Objective      string   `json:"objective,omitempty"`
	MinFPSRatio    *float64 `json:"min_fps_ratio,omitempty"`
	MaxRejectShare *float64 `json:"max_reject_share,omitempty"`
	MinHoldMS      *int     `json:"min_hold_ms,omitempty"`
	MarginMinMS    *float64 `json:"margin_min_ms,omitempty"`
	MarginRel      *float64 `json:"margin_rel,omitempty"`
	KQueue         *float64 `json:"k_queue,omitempty"`
	KNoise         *float64 `json:"k_noise,omitempty"`
	QueueRhoFrom   *float64 `json:"queue_rho_from,omitempty"`
	// Reserve: false spegne le prenotazioni (ablazione, Fase 3.8).
	Reserve *bool `json:"reserve,omitempty"`
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
	// PingIntervalMS: ogni quanto il server misura la latenza con un ping
	// WebSocket; 0 = mai.
	PingIntervalMS *int `json:"ping_interval_ms,omitempty"`
	// ProbeIntervalMS: dopo quanto senza misure di banda il server chiede un
	// probe al device; 0 = mai. ProbeBytes: la sua dimensione.
	ProbeIntervalMS *int `json:"probe_interval_ms,omitempty"`
	// ProbeUnknownIntervalMS: lo stesso, finché la banda del device non ha
	// una stima; 0 = sempre probe_interval_ms.
	ProbeUnknownIntervalMS *int `json:"probe_unknown_interval_ms,omitempty"`
	ProbeBytes             *int `json:"probe_bytes,omitempty"`
	// BandwidthTauMS: costante di tempo della stima della banda.
	BandwidthTauMS *int `json:"bandwidth_tau_ms,omitempty"`
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

	// DeviceSegmentTauMS: costante di tempo delle stime dei tempi del device
	// per segmento (segment_ms della telemetria).
	DeviceSegmentTauMS *int `json:"device_segment_tau_ms,omitempty"`
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
	if s.PingIntervalMS != nil {
		if v := *s.PingIntervalMS; v != 0 && (v < 100 || v > 60000) {
			return stream.Config{}, fmt.Errorf("stream.ping_interval_ms must be 0 or in [100, 60000], got %d", v)
		}
		cfg.PingInterval = time.Duration(*s.PingIntervalMS) * time.Millisecond
	}
	if s.ProbeIntervalMS != nil {
		if v := *s.ProbeIntervalMS; v != 0 && (v < 1000 || v > 3600000) {
			return stream.Config{}, fmt.Errorf("stream.probe_interval_ms must be 0 or in [1000, 3600000], got %d", v)
		}
		cfg.ProbeInterval = time.Duration(*s.ProbeIntervalMS) * time.Millisecond
	}
	if s.ProbeUnknownIntervalMS != nil {
		if v := *s.ProbeUnknownIntervalMS; v != 0 && (v < 1000 || v > 3600000) {
			return stream.Config{}, fmt.Errorf("stream.probe_unknown_interval_ms must be 0 or in [1000, 3600000], got %d", v)
		}
		cfg.ProbeUnknownInterval = time.Duration(*s.ProbeUnknownIntervalMS) * time.Millisecond
	}
	// Dopo max_message_bytes: il probe è un messaggio e deve starci.
	maxProbe := cfg.MaxMessageBytes - probeHeaderRoom
	if s.ProbeBytes != nil {
		if v := *s.ProbeBytes; v < netmeasure.ReadChunk || v > maxProbe {
			return stream.Config{}, fmt.Errorf("stream.probe_bytes must be in [%d, max_message_bytes - %d], got %d",
				netmeasure.ReadChunk, probeHeaderRoom, v)
		}
		cfg.ProbeBytes = *s.ProbeBytes
	} else {
		// Il default non deve impedire un max_message_bytes piccolo.
		cfg.ProbeBytes = min(cfg.ProbeBytes, maxProbe)
	}
	bw, err := tau("stream.bandwidth_tau_ms", s.BandwidthTauMS, cfg.BandwidthTau)
	if err != nil {
		return stream.Config{}, err
	}
	cfg.BandwidthTau = bw
	return cfg, nil
}

// DispatchConfig restituisce i parametri del dispatcher:
// dispatch.DefaultConfig() con sopra i valori presenti nel file.
func (f File) DispatchConfig() (dispatch.Config, error) {
	cfg := dispatch.DefaultConfig()
	var err error
	cfg.ServiceTau, err = tau("dispatch.service_tau_ms", f.Dispatch.ServiceTauMS, cfg.ServiceTau)
	if err != nil {
		return cfg, err
	}
	if r := f.Dispatch.ProfileRuns; r != nil {
		// Oltre 100 il profilo occupa il worker per secondi senza migliorare la stima.
		if *r < 0 || *r > maxProfileRuns {
			return cfg, fmt.Errorf("dispatch.profile_runs must be in [0, %d], got %d", maxProfileRuns, *r)
		}
		cfg.ProfileRuns = *r
	}
	if a := f.Dispatch.ReprofileAgeMS; a != nil {
		if *a != 0 {
			if cfg.ReprofileAge, err = tau("dispatch.reprofile_age_ms", a, 0); err != nil {
				return cfg, err
			}
		} else {
			cfg.ReprofileAge = 0
		}
	}
	return cfg, nil
}

const maxProfileRuns = 100

// OffloadConfig restituisce l'algoritmo di offloading: nome e parametri,
// offload.DefaultLatencyConfig() con sopra i valori presenti nel file.
func (f File) OffloadConfig() (string, offload.LatencyConfig, error) {
	o, cfg := f.Offload, offload.DefaultLatencyConfig()
	// Le prenotazioni si spengono con la finestra su cui il dispatcher misura λ.
	if d, err := f.DispatchConfig(); err == nil {
		cfg.ReserveWindow = d.Window
	}
	name := o.Algorithm
	if name == "" {
		name = offload.Static{}.Name()
	}
	if !slices.Contains(offload.Names(), name) {
		return "", cfg, fmt.Errorf("offload.algorithm must be one of %v, got %q", offload.Names(), name)
	}
	switch o.Objective {
	case "":
	case offload.ObjectiveP95, offload.ObjectiveMean:
		cfg.Objective = o.Objective
	default:
		return "", cfg, fmt.Errorf("offload.objective must be %q or %q, got %q", offload.ObjectiveP95, offload.ObjectiveMean, o.Objective)
	}
	for _, v := range []struct {
		name   string
		in     *float64
		out    *float64
		lo, hi float64
	}{
		{"offload.min_fps_ratio", o.MinFPSRatio, &cfg.MinFPSRatio, 0, 10},
		{"offload.max_reject_share", o.MaxRejectShare, &cfg.MaxRejectShare, 0, 1},
		{"offload.margin_min_ms", o.MarginMinMS, &cfg.MarginMinMS, 0, 1000},
		{"offload.margin_rel", o.MarginRel, &cfg.MarginRel, 0, 1},
		{"offload.k_queue", o.KQueue, &cfg.KQueue, 0, 100},
		{"offload.k_noise", o.KNoise, &cfg.KNoise, 0, 100},
		{"offload.queue_rho_from", o.QueueRhoFrom, &cfg.QueueRhoFrom, 0, 0.99},
	} {
		if v.in == nil {
			continue
		}
		if *v.in < v.lo || *v.in > v.hi {
			return "", cfg, fmt.Errorf("%s must be in [%g, %g], got %g", v.name, v.lo, v.hi, *v.in)
		}
		*v.out = *v.in
	}
	if o.Reserve != nil {
		cfg.Reserve = *o.Reserve
	}
	if h := o.MinHoldMS; h != nil {
		if *h < 0 || time.Duration(*h)*time.Millisecond > maxTau {
			return "", cfg, fmt.Errorf("offload.min_hold_ms must be in [0, %d], got %d", maxTau.Milliseconds(), *h)
		}
		cfg.MinHold = time.Duration(*h) * time.Millisecond
	}
	return name, cfg, nil
}

// DeviceSegmentTau è la costante di tempo delle stime dei tempi del device
// per segmento (device.Store.SetSegmentTau).
func (f File) DeviceSegmentTau() (time.Duration, error) {
	return tau("server.device_segment_tau_ms", f.Server.DeviceSegmentTauMS, device.DefaultSegmentTau)
}

// Limiti delle costanti di tempo (internal/ewma): sotto i 100 ms una stima
// non media più niente, sopra l'ora non segue più il sistema.
const (
	minTau = 100 * time.Millisecond
	maxTau = time.Hour
)

// tau legge una costante di tempo in ms; def se assente.
func tau(name string, ms *int, def time.Duration) (time.Duration, error) {
	if ms == nil {
		return def, nil
	}
	d := time.Duration(*ms) * time.Millisecond
	if d < minTau || d > maxTau {
		return 0, fmt.Errorf("%s must be in [%d, %d], got %d", name, minTau.Milliseconds(), maxTau.Milliseconds(), *ms)
	}
	return d, nil
}

// probeHeaderRoom sono i byte lasciati all'header del messaggio probe (4 di
// lunghezza + il JSON, ~40) dentro max_message_bytes.
const probeHeaderRoom = 64

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
