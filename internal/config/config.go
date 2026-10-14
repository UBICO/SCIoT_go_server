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
	"fmt"
	"os"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/load"
	"github.com/UBICO/SCIoT_go_server/internal/registry"
	"github.com/UBICO/SCIoT_go_server/internal/server"
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
		return File{}, fmt.Errorf("decode config: %w", err)
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
