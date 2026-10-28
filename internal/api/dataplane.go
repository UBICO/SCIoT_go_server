package api

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
)

// ---------------------------------------------------------------------------
// Data plane (docs/PROTOCOL.md, "Data plane")
// ---------------------------------------------------------------------------

// Header HTTP dell'upgrade a WebSocket.
const (
	HeaderDevice  = "X-SCIoT-Device"
	HeaderSession = "X-SCIoT-Session"
)

// Valori del campo "type" dei messaggi sullo stream.
const (
	MsgHello  = "hello"
	MsgFrame  = "frame"
	MsgResult = "result"
	MsgError  = "error"
	MsgPolicy = "policy"
	// Misura della banda in LOCAL (docs/PROTOCOL.md, "Probe").
	MsgProbeRequest = "probe_request" // server → device
	MsgProbe        = "probe"         // device → server, binario
)

// Codici di chiusura dello stream (docs/PROTOCOL.md, "Closing").
const (
	CloseSessionExpired = 4001 // il device si è registrato di nuovo, o la sessione non esiste più
	CloseReplaced       = 4002 // sostituito da uno stream più recente dello stesso device
)

// EncodingFloat32 è l'unico encoding dei tensori accettato in v1.
const EncodingFloat32 = "float32"

// Codici di errore per singolo frame (messaggio "error").
const (
	ErrCodeServerBusy        = "server_busy"
	ErrCodeDeadlineExceeded  = "deadline_exceeded"
	ErrCodeInflightLimit     = "inflight_limit"
	ErrCodeInvalidFrame      = "invalid_frame"
	ErrCodeWorkerUnavailable = "worker_unavailable"
)

// ResultSchema identifica lo schema dei risultati.
const ResultSchema = "inference-result.v1"

// SegmentSize dice quanti byte di activation il server si aspetta per uno split point.
type SegmentSize struct {
	SplitPoint int `json:"split_point"`
	InputBytes int `json:"input_bytes"`
}

// Hello è il primo messaggio del server dopo l'upgrade.
type Hello struct {
	Type            string        `json:"type"`
	MaxInflight     int           `json:"max_inflight"`
	MaxMessageBytes int           `json:"max_message_bytes"`
	Policy          Policy        `json:"policy"`
	Segments        []SegmentSize `json:"segments"`
}

// FrameHeader è l'header JSON di un messaggio "frame".
type FrameHeader struct {
	Type          string    `json:"type"`
	SequenceID    uint64    `json:"sequence_id"`
	PolicyVersion uint64    `json:"policy_version"`
	SplitPoint    int       `json:"split_point"`
	Encoding      string    `json:"encoding"`
	CapturedAt    time.Time `json:"captured_at,omitzero"`
	DeadlineMS    *float64  `json:"deadline_ms,omitempty"`
	WantRaw       bool      `json:"want_raw,omitempty"`
}

// Box è un riquadro normalizzato in [0, 1]; X, Y è l'angolo in alto a sinistra.
type Box struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// Detection è un oggetto riconosciuto.
type Detection struct {
	Label string  `json:"label"`
	Score float64 `json:"score"`
	Box   Box     `json:"box"`
}

// Timing contiene i tempi di un'inferenza. I campi del server ci sono solo in SPLIT.
type Timing struct {
	DeviceMS *float64 `json:"device_ms,omitempty"`
	// ReceiveMS: dall'inizio all'ultimo byte del frame, letti dal server. Non
	// conta l'attesa del primo byte (latenza) e può essere sottostimato se i
	// byte erano già nel buffer TCP (docs/RECORDING.md, "Network").
	ReceiveMS   *float64  `json:"receive_ms,omitempty"`
	QueueWaitMS *float64  `json:"queue_wait_ms,omitempty"`
	ServerMS    *float64  `json:"server_ms,omitempty"`
	LayerMS     []float64 `json:"layer_ms,omitempty"`
}

// InferenceResult è lo schema "inference-result.v1": lo stesso per LOCAL e
// SPLIT, per lo stream e (in futuro) per MQTT.
type InferenceResult struct {
	Schema              string        `json:"schema"`
	Type                string        `json:"type,omitempty"` // "result" sullo stream
	DeviceID            string        `json:"device_id"`
	SessionID           string        `json:"session_id"`
	SequenceID          uint64        `json:"sequence_id"`
	ModelKey            string        `json:"model_key"`
	Mode                ExecutionMode `json:"mode"`
	SplitPoint          *int          `json:"split_point"` // null in LOCAL
	PolicyVersion       uint64        `json:"policy_version"`
	ServerPolicyVersion *uint64       `json:"server_policy_version,omitempty"`
	ComputedBy          string        `json:"computed_by"` // "device" | "server"
	CapturedAt          time.Time     `json:"captured_at,omitzero"`
	Timing              Timing        `json:"timing"`
	Detections          []Detection   `json:"detections"`
}

// StreamError è il messaggio "error": un frame non è stato eseguito.
type StreamError struct {
	Type       string `json:"type"`
	SequenceID uint64 `json:"sequence_id"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

// PolicyMessage è il messaggio "policy": la policy del device è cambiata.
type PolicyMessage struct {
	Type   string `json:"type"`
	Policy Policy `json:"policy"`
}

// ProbeRequest è il messaggio "probe_request": il server chiede al device di
// inviare Bytes byte, per misurare la banda quando non arrivano frame.
type ProbeRequest struct {
	Type    string `json:"type"`
	ProbeID uint64 `json:"probe_id"`
	Bytes   int    `json:"bytes"`
}

// ProbeHeader è l'header del messaggio binario "probe" (device → server); il
// payload sono i Bytes richiesti, di contenuto qualunque (zeri). Il server lo
// misura come un frame e non lo esegue.
type ProbeHeader struct {
	Type    string `json:"type"`
	ProbeID uint64 `json:"probe_id"`
}

// ---------------------------------------------------------------------------
// Formato dei messaggi binari
// ---------------------------------------------------------------------------
//
//	┌─────────────────────┬──────────────────────┬──────────────────┐
//	│ header_length (u32, │ header JSON, UTF-8,  │ payload: tensore │
//	│ little-endian)      │ header_length byte   │ (resto)          │
//	└─────────────────────┴──────────────────────┴──────────────────┘

// headerLengthBytes è la dimensione del prefisso con la lunghezza dell'header.
const headerLengthBytes = 4

// MaxHeaderBytes limita l'header JSON: un header di 1 MB non è un header, è
// un errore (o un attacco). Un header reale è di ~150-400 byte.
const MaxHeaderBytes = 16 << 10

// ErrMalformedMessage indica un messaggio binario che non rispetta il formato.
var ErrMalformedMessage = errors.New("malformed binary message")

// EncodeBinary costruisce un messaggio binario da un header JSON già
// serializzato e dal payload.
func EncodeBinary(header, payload []byte) []byte {
	// Una sola allocazione della dimensione finale: il payload può essere di MB.
	msg := make([]byte, headerLengthBytes+len(header)+len(payload))
	binary.LittleEndian.PutUint32(msg[0:headerLengthBytes], uint32(len(header)))
	copy(msg[headerLengthBytes:], header)
	copy(msg[headerLengthBytes+len(header):], payload)
	return msg
}

// DecodeBinary separa un messaggio binario in header JSON e payload.
//
// Le due slice restituite PUNTANO dentro msg (nessuna copia): è voluto, il
// payload può essere di qualche MB.
func DecodeBinary(msg []byte) (header, payload []byte, err error) {
	if len(msg) < headerLengthBytes {
		return nil, nil, fmt.Errorf("%w: %d bytes, need at least %d for header_length",
			ErrMalformedMessage, len(msg), headerLengthBytes)
	}
	n := binary.LittleEndian.Uint32(msg[0:headerLengthBytes])
	// Il limite va controllato su n (uint32) prima di convertirlo in int e
	// sommarlo a 4: così non c'è overflow nemmeno su piattaforme a 32 bit.
	if n == 0 || n > MaxHeaderBytes {
		return nil, nil, fmt.Errorf("%w: header_length %d out of range [1, %d]",
			ErrMalformedMessage, n, MaxHeaderBytes)
	}
	end := headerLengthBytes + int(n)
	if len(msg) < end {
		return nil, nil, fmt.Errorf("%w: header_length %d but only %d bytes after the prefix",
			ErrMalformedMessage, n, len(msg)-headerLengthBytes)
	}
	return msg[headerLengthBytes:end], msg[end:], nil
}

// ---------------------------------------------------------------------------
// Validazione di un frame
// ---------------------------------------------------------------------------

// Validate controlla l'header di un frame e la dimensione del payload.
//
// expectedBytes restituisce i byte attesi per uno split point del modello del
// device, e false se lo split point non esiste.
func (h FrameHeader) Validate(payloadBytes int, expectedBytes func(splitPoint int) (int, bool)) error {
	if h.Type != MsgFrame {
		return fmt.Errorf("type must be %q, got %q", MsgFrame, h.Type)
	}
	if h.SequenceID == 0 {
		return errors.New("sequence_id must be > 0")
	}
	if h.Encoding != EncodingFloat32 {
		return fmt.Errorf("encoding must be %q, got %q", EncodingFloat32, h.Encoding)
	}
	want, ok := expectedBytes(h.SplitPoint)
	if !ok {
		return fmt.Errorf("unknown split_point %d", h.SplitPoint)
	}
	if payloadBytes != want {
		return fmt.Errorf("payload is %d bytes, split_point %d expects %d", payloadBytes, h.SplitPoint, want)
	}
	if d := h.DeadlineMS; d != nil && (math.IsNaN(*d) || math.IsInf(*d, 0) || *d <= 0) {
		return fmt.Errorf("deadline_ms must be a finite number > 0, got %v", *d)
	}
	return nil
}
