package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeConfig scrive content in un config.json temporaneo e ne restituisce il path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	path := writeConfig(t, `{
		"server": {"heartbeat_interval_ms": 500},
		"models": [{
			"model_key": "fomo_96x96",
			"tflite_dir": "/models/fomo_96x96",
			"last_offloading_layer": 58,
			"input_size_bytes": 18432
		}]
	}`)

	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.Server.HeartbeatIntervalMS == nil || *f.Server.HeartbeatIntervalMS != 500 {
		t.Errorf("heartbeat_interval_ms = %v", f.Server.HeartbeatIntervalMS)
	}
	if rc := f.RegistryConfig(); len(rc.Models) != 1 || rc.Models[0].TFLiteDir != "/models/fomo_96x96" {
		t.Errorf("RegistryConfig = %+v", rc)
	}
}

func TestLoadWithoutServerSection(t *testing.T) {
	// Il config.json della fase precedente (solo "models") resta valido.
	f, err := Load(writeConfig(t, `{"models": []}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.Server.HeartbeatIntervalMS != nil {
		t.Errorf("heartbeat_interval_ms deve essere nil se assente")
	}
}

func TestLoadUnknownField(t *testing.T) {
	path := writeConfig(t, `{"server": {"heartbeat_interval": 500}, "models": []}`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load deve rifiutare la chiave sconosciuta \"heartbeat_interval\"")
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "non-esiste.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want os.ErrNotExist avvolto", err)
	}
}

func ms(v int) *int { return &v }

func TestServerConfig(t *testing.T) {
	cases := []struct {
		name    string
		ms      *int
		want    time.Duration
		wantErr bool
	}{
		{"assente → default 1s", nil, time.Second, false},
		{"500 ms", ms(500), 500 * time.Millisecond, false},
		{"minimo", ms(100), 100 * time.Millisecond, false},
		{"massimo", ms(60_000), time.Minute, false},
		{"zero", ms(0), 0, true},
		{"negativo", ms(-1), 0, true},
		{"sotto il minimo", ms(99), 0, true},
		{"sopra il massimo", ms(60_001), 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := File{Server: Server{HeartbeatIntervalMS: tc.ms}}
			cfg, err := f.ServerConfig()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("errore atteso, got cfg %+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("ServerConfig: %v", err)
			}
			if cfg.HeartbeatInterval != tc.want {
				t.Errorf("HeartbeatInterval = %v, want %v", cfg.HeartbeatInterval, tc.want)
			}
			// Gli altri campi devono restare ai default.
			if cfg.MaxBodyBytes == 0 {
				t.Error("MaxBodyBytes azzerato: parti da server.DefaultConfig()")
			}
		})
	}
}
