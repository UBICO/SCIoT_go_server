// Package registry conosce i modelli serviti dal server e li indicizza per
// model_hash, lo stesso hash che il client calcola sui propri segmenti TFLite.
//
// Semantica dell'hash (identica al server/client Python):
// MD5 del contenuto di tutti i file *.tflite della cartella, concatenati in
// ordine di nome.
package registry

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"

	"github.com/UBICO/sciot-go-server/internal/api"
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
	//   - files, err := filepath.Glob(filepath.Join(dir, "*.tflite"))
	//     Glob restituisce i path già ordinati per nome, come sorted() in Python.

	files, err := filepath.Glob(filepath.Join(dir, "*.tflite"))
	//   - se len(files) == 0 restituisci un errore che avvolge la sentinella:
	//     fmt.Errorf("%s: %w", dir, errNoTFLiteFiles)

	if err != nil {
		return "", fmt.Errorf("glob %s: %w", dir, err)
	}

	if len(files) == 0 {
		return "", fmt.Errorf("%s: %w", dir, errNoTFLiteFiles)
	}

	//   - h := md5.New()  → h è un hash.Hash, che è anche un io.Writer.
	//   - per ogni file (for _, path := range files { ... }) chiama
	//     addFile(h, path) e gestisci l'errore.
	//   - risultato: hex.EncodeToString(h.Sum(nil))
	//panic("TODO")
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
	//   - os.Open(path), errore con contesto.

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	//   - defer f.Close()
	defer f.Close()

	//   - io.Copy(h, f) copia il file nell'hasher a blocchi, senza caricarlo
	//     tutto in memoria. Restituisce (byte copiati, error): il primo valore
	//     non ci serve, scartalo con _.
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
	//   - crea la map: byHash := make(map[string]api.ModelInfo)
	byHash := make(map[string]api.ModelInfo)

	for _, m := range cfg.Models {
		h, err := hashTFLiteDir(m.TFLiteDir)
		//       * calcola l'hash di m.TFLiteDir; in caso di errore restituisci
		//         nil e fmt.Errorf("model %q: %w", m.ModelKey, err)
		//         (%q stampa la stringa tra virgolette)
		if err != nil {
			return nil, fmt.Errorf("model %q: %w", m.ModelKey, err)
		}

		//       * se l'hash è già presente nella map, errore: due modelli con gli
		//         stessi segmenti sono ambigui. Controllo "comma ok":
		//           if existing, ok := byHash[h]; ok { ... existing.ModelKey ... }

		if existing, ok := byHash[h]; ok {
			return nil, fmt.Errorf(
				"models %q and %q have the same segments",
				existing.ModelKey,
				m.ModelKey,
			)
		}
		//       * inserisci api.ModelInfo con NumLayers = m.LastOffloadingLayer + 1
		//         (stessa regola del server Python).

		byHash[h] = api.ModelInfo{
			ModelKey:            m.ModelKey,
			NumLayers:           m.LastOffloadingLayer + 1,
			LastOffloadingLayer: m.LastOffloadingLayer,
			InputSizeBytes:      m.InputSizeBytes,
		}
	}
	//   - per ogni m in cfg.Models:

	return &Registry{byHash: byHash}, nil
	//   - restituisci &Registry{byHash: byHash}, nil
	//panic("TODO")
}

// Lookup restituisce il modello associato a hash e true, oppure il valore
// zero di api.ModelInfo e false se l'hash non è noto.
func (r *Registry) Lookup(hash string) (api.ModelInfo, bool) {
	// (r *Registry) prima del nome rende Lookup un "metodo" di Registry:
	// si chiama come reg.Lookup("..."). r è l'equivalente di self.
	//
	// TODO(5): una riga, usando l'idioma "comma ok" della map.

	//panic("TODO")
	info, ok := r.byHash[hash]
	return info, ok
}

// Len restituisce il numero di modelli registrati.
func (r *Registry) Len() int {
	return len(r.byHash)
}
