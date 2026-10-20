package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/UBICO/SCIoT_go_server/internal/api"
)

// ManifestFile è il nome del manifest nella cartella di un modello.
const ManifestFile = "manifest.json"

// ManifestVersion è l'unica versione del formato che il server conosce
// (sciot-model build, docs/MODELS.md).
const ManifestVersion = 2

// Manifest è la parte di manifest.json che serve al server. Le altre chiavi
// (tool, source, cuts, verification) restano nel file per le persone e per
// i build successivi: json.Unmarshal ignora i campi che la struct non ha.
type Manifest struct {
	FormatVersion int `json:"format_version"`
	// Quantization è null per un modello float32. È un json.RawMessage, cioè
	// il testo JSON così com'è: il server oggi deve solo sapere se è null,
	// e un formato futuro non va interpretato a metà.
	Quantization json.RawMessage   `json:"quantization"`
	Input        api.TensorSpec    `json:"input"`
	Output       api.TensorSpec    `json:"output"`
	Segments     []ManifestSegment `json:"segments"`
	Postprocess  api.Postprocess   `json:"postprocess"`
	ModelHash    string            `json:"model_hash"`
}

// ManifestSegment è un segmento del modello.
type ManifestSegment struct {
	Index int    `json:"index"`
	File  string `json:"file"`
}

// readManifest legge e decodifica <dir>/manifest.json.
func readManifest(dir string) (Manifest, error) {
	path := filepath.Join(dir, ManifestFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, fmt.Errorf("%s not found: build the model with sciot-model build (docs/MODELS.md)", path)
	}
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// validateManifest controlla che il manifest descriva davvero i file di dir.
// hash è il model_hash ricalcolato sui file (hashTFLiteDir).
func validateManifest(m Manifest, dir, hash string) error {
	if m.FormatVersion != ManifestVersion {
		return fmt.Errorf("manifest format_version is %d, this server supports %d", m.FormatVersion, ManifestVersion)
	}
	if q := string(m.Quantization); len(m.Quantization) != 0 && q != "null" {
		return fmt.Errorf("quantized models are not supported yet (quantization: %s)", q)
	}
	if len(m.Segments) == 0 {
		return errors.New("manifest has no segments")
	}
	inManifest := make(map[string]bool, len(m.Segments))
	for i, s := range m.Segments {
		if s.Index != i {
			return fmt.Errorf("manifest segment %d (%s) has index %d: indexes must be 0, 1, 2, ... in order", i, s.File, s.Index)
		}
		// Un file elencato due volte passerebbe tutti i controlli sui file e
		// sull'hash, ma num_layers conterebbe un segmento che non esiste.
		if inManifest[s.File] {
			return fmt.Errorf("manifest lists %s twice", s.File)
		}
		inManifest[s.File] = true
	}

	paths, err := filepath.Glob(filepath.Join(dir, "*.tflite"))
	if err != nil {
		return err
	}
	onDisk := make(map[string]bool, len(paths))
	for _, p := range paths {
		name := filepath.Base(p)
		onDisk[name] = true
		if !inManifest[name] {
			return fmt.Errorf("%s is in %s but not in the manifest: leftover from an older build?", name, dir)
		}
	}
	for _, s := range m.Segments {
		if !onDisk[s.File] {
			return fmt.Errorf("segment %s listed in the manifest is missing from %s", s.File, dir)
		}
	}

	if m.ModelHash != hash {
		return fmt.Errorf("segments do not match the manifest (hash %s, manifest %s): files from different builds?", hash, m.ModelHash)
	}
	if m.Postprocess.Decoder == "" || len(m.Postprocess.Labels) == 0 {
		return errors.New("manifest postprocess is empty: decoder and labels are required")
	}
	if len(m.Input.Shape) == 0 || m.Input.DType == "" {
		return fmt.Errorf("manifest input is empty (shape %v, dtype %q)", m.Input.Shape, m.Input.DType)
	}
	return nil
}

// PostprocessOverride è la sezione "postprocess" di un modello in
// config.json: cambia solo le soglie, per un esperimento. Decoder e label
// descrivono il modello e non si cambiano qui.
type PostprocessOverride struct {
	ScoreThreshold *float64 `json:"score_threshold,omitempty"`
	IoUThreshold   *float64 `json:"iou_threshold,omitempty"`
	MaxDetections  *int     `json:"max_detections,omitempty"`
}

// apply restituisce p con i valori dell'override. o può essere nil.
func (o *PostprocessOverride) apply(p api.Postprocess) (api.Postprocess, error) {
	if o == nil {
		return p, nil
	}
	if o.ScoreThreshold != nil {
		if v := *o.ScoreThreshold; v <= 0 || v > 1 {
			return p, fmt.Errorf("postprocess.score_threshold must be in (0, 1], got %v", v)
		}
		p.ScoreThreshold = *o.ScoreThreshold
	}
	if o.IoUThreshold != nil {
		if p.IoUThreshold == nil {
			return p, fmt.Errorf("postprocess.iou_threshold: decoder %s has no NMS", p.Decoder)
		}
		if v := *o.IoUThreshold; v <= 0 || v > 1 {
			return p, fmt.Errorf("postprocess.iou_threshold must be in (0, 1], got %v", v)
		}
		v := *o.IoUThreshold
		p.IoUThreshold = &v
	}
	if o.MaxDetections != nil {
		if *o.MaxDetections < 1 {
			return p, fmt.Errorf("postprocess.max_detections must be >= 1, got %d", *o.MaxDetections)
		}
		p.MaxDetections = *o.MaxDetections
	}
	return p, nil
}
