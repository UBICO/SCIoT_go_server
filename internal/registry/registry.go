// Package registry conosce i modelli serviti dal server e li indicizza per
// model_hash, lo stesso hash che il client calcola sui propri segmenti TFLite.
//
// Semantica dell'hash (identica al server/client Python):
// MD5 del contenuto di tutti i file *.tflite della cartella, concatenati in
// ordine di nome.
package registry

import (
	"cmp"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/UBICO/SCIoT_go_server/internal/api"
)

// ---------------------------------------------------------------------------
// Configurazione (file JSON)
// ---------------------------------------------------------------------------

// ModelConfig è la voce di un modello nel file di configurazione.
type ModelConfig struct {
	ModelKey            string `json:"model_key"`
	TFLiteDir           string `json:"tflite_dir"` // cartella con i segmenti *.tflite
	LastOffloadingLayer int    `json:"last_offloading_layer"`
	InputSizeBytes      int    `json:"input_size_bytes"`
}

// Config è il contenuto dell'intero file:
//
//	{"models": [ {...ModelConfig...}, ... ]}
type Config struct {
	Models []ModelConfig `json:"models"` // []T è una "slice": la lista di Go
}

// errNoTFLiteFiles è un "errore sentinella": un valore fisso che chi chiama
// può riconoscere con errors.Is(err, errNoTFLiteFiles).
var errNoTFLiteFiles = errors.New("no .tflite files found")

// ---------------------------------------------------------------------------
// Hash dei segmenti
// ---------------------------------------------------------------------------

// hashTFLiteDir calcola l'MD5 (in esadecimale) dei file *.tflite di dir.
func hashTFLiteDir(dir string) (string, error) {
	// Glob restituisce i path già ordinati per nome, come sorted() in Python.
	files, err := filepath.Glob(filepath.Join(dir, "*.tflite"))
	if err != nil {
		return "", fmt.Errorf("glob %s: %w", dir, err)
	}
	if len(files) == 0 {
		return "", fmt.Errorf("%s: %w", dir, errNoTFLiteFiles)
	}

	// Un hash.Hash è anche un io.Writer: addFile ci copia dentro i file.
	h := md5.New()
	for _, path := range files {
		if err := addFile(h, path); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// addFile scrive il contenuto del file in h.
//
// È una funzione separata per un motivo preciso: defer scatta alla fine della
// FUNZIONE, non del ciclo. Se hashTFLiteDir aprisse i file nel ciclo con
// defer f.Close(), resterebbero tutti aperti fino alla fine. Qui invece ogni
// file viene chiuso appena addFile ritorna.
func addFile(h hash.Hash, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	// io.Copy legge a blocchi: il file non viene caricato tutto in memoria.
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// Registry associa model_hash → informazioni sul modello.
//
// Viene costruito una volta all'avvio e poi solo letto. Per questo non serve
// un mutex: letture concorrenti di una map sono sicure finché nessuno scrive.
type Registry struct {
	byHash map[string]api.ModelInfo // minuscolo: non accessibile da fuori
}

// New calcola l'hash di ogni modello in cfg e costruisce il Registry.
// Restituisce un puntatore (*Registry): il valore viene condiviso, non copiato.
func New(cfg Config) (*Registry, error) {
	byHash := make(map[string]api.ModelInfo)
	for _, m := range cfg.Models {
		h, err := hashTFLiteDir(m.TFLiteDir)
		if err != nil {
			return nil, fmt.Errorf("model %q: %w", m.ModelKey, err)
		}
		// Due modelli con gli stessi segmenti sarebbero ambigui.
		if existing, ok := byHash[h]; ok {
			return nil, fmt.Errorf("models %q and %q have the same segments",
				existing.ModelKey, m.ModelKey)
		}
		byHash[h] = api.ModelInfo{
			ModelKey:            m.ModelKey,
			NumLayers:           m.LastOffloadingLayer + 1, // stessa regola del server Python
			LastOffloadingLayer: m.LastOffloadingLayer,
			InputSizeBytes:      m.InputSizeBytes,
		}
	}
	return &Registry{byHash: byHash}, nil
}

// Lookup restituisce il modello associato a hash e true, oppure il valore
// zero di api.ModelInfo e false se l'hash non è noto.
func (r *Registry) Lookup(hash string) (api.ModelInfo, bool) {
	info, ok := r.byHash[hash]
	return info, ok
}

// Len restituisce il numero di modelli registrati.
func (r *Registry) Len() int {
	return len(r.byHash)
}

// Models restituisce tutti i modelli con il loro hash, ordinati per
// model_key. L'ordine di iterazione di una map in Go è volutamente casuale:
// senza ordinamento la risposta HTTP cambierebbe a ogni chiamata.
func (r *Registry) Models() []api.ModelEntry {
	entries := make([]api.ModelEntry, 0, len(r.byHash))
	for hash, info := range r.byHash {
		entries = append(entries, api.ModelEntry{ModelHash: hash, ModelInfo: info})
	}
	slices.SortFunc(entries, func(a, b api.ModelEntry) int {
		return cmp.Compare(a.ModelKey, b.ModelKey)
	})
	return entries
}
