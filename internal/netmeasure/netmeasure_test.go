package netmeasure

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// fakeClock avanza solo quando lo dice il test: le durate sono esatte.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// chunkReader restituisce un blocco per Read e, prima di ogni blocco, fa
// avanzare l'orologio della durata indicata: simula letture che aspettano la
// rete (durata lunga) o che trovano i byte nel buffer (durata ~0).
type chunkReader struct {
	clock  *fakeClock
	chunks []string
	waits  []time.Duration
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	r.clock.t = r.clock.t.Add(r.waits[0])
	n := copy(p, r.chunks[0])
	if n < len(r.chunks[0]) {
		r.chunks[0] = r.chunks[0][n:]
		r.waits[0] = 0 // il resto del blocco è già arrivato
		return n, nil
	}
	r.chunks, r.waits = r.chunks[1:], r.waits[1:]
	return n, nil
}

func TestReadMessageAllWaited(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	started := clock.now()
	r := &chunkReader{clock: clock,
		chunks: []string{strings.Repeat("a", 1000), strings.Repeat("b", 1000)},
		waits:  []time.Duration{time.Millisecond, time.Millisecond},
	}
	data, s, err := ReadMessage(r, started, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 2000 || s.Bytes != 2000 || s.WaitedBytes != 2000 || !s.Valid {
		t.Errorf("len %d, sample %+v: attesi 2000 byte tutti aspettati, valido", len(data), s)
	}
	if s.Receive != 2*time.Millisecond {
		t.Errorf("Receive = %v, atteso 2ms", s.Receive)
	}
	if bw := s.BandwidthBps(); bw != 1e6 {
		t.Errorf("banda = %v B/s, attesa 1e6", bw)
	}
}

func TestReadMessageBufferedIsInvalid(t *testing.T) {
	// Il primo 50% aspetta la rete, il resto era già nel buffer.
	clock := &fakeClock{t: time.Unix(0, 0)}
	r := &chunkReader{clock: clock,
		chunks: []string{strings.Repeat("a", 500), strings.Repeat("b", 500)},
		waits:  []time.Duration{time.Millisecond, time.Microsecond},
	}
	_, s, err := ReadMessage(r, clock.now(), clock.now)
	if err != nil {
		t.Fatal(err)
	}
	if s.WaitedBytes != 500 || s.Valid {
		t.Errorf("sample %+v: attesi 500 byte aspettati su 1000, non valido", s)
	}
}

func TestReadMessageUsesFixedChunks(t *testing.T) {
	// Ogni Read riceve un buffer di esattamente ReadChunk byte, anche per un
	// messaggio di MB (io.ReadAll li farebbe crescere).
	msg := bytes.Repeat([]byte{7}, 3*ReadChunk+10)
	clock := &fakeClock{t: time.Unix(0, 0)}
	data, s, err := ReadMessage(&sizeReader{t: t, r: bytes.NewReader(msg)}, clock.now(), clock.now)
	if err != nil || !bytes.Equal(data, msg) || s.Bytes != len(msg) {
		t.Errorf("len %d, Bytes %d, err %v", len(data), s.Bytes, err)
	}
}

// sizeReader fallisce il test se riceve un buffer diverso da ReadChunk.
type sizeReader struct {
	t *testing.T
	r io.Reader
}

func (m *sizeReader) Read(p []byte) (int, error) {
	if len(p) != ReadChunk {
		m.t.Fatalf("Read con %d byte, attesi %d", len(p), ReadChunk)
	}
	return m.r.Read(p)
}

func TestReadMessageDataWithEOF(t *testing.T) {
	// Byte restituiti INSIEME a io.EOF: vanno contati.
	clock := &fakeClock{t: time.Unix(0, 0)}
	_, s, err := ReadMessage(iotest.DataErrReader(strings.NewReader("hello")), clock.now(), clock.now)
	if err != nil || s.Bytes != 5 {
		t.Errorf("Bytes %d, err %v: attesi 5", s.Bytes, err)
	}
}

func TestReadMessageError(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	_, _, err := ReadMessage(iotest.ErrReader(io.ErrUnexpectedEOF), clock.now(), clock.now)
	if err == nil {
		t.Error("atteso l'errore del reader")
	}
}

func TestSampleOf(t *testing.T) {
	cases := []struct {
		total, waited int
		valid         bool
	}{
		{0, 0, false},   // messaggio vuoto
		{100, 90, true}, // esattamente la soglia
		{100, 89, false},
		{100, 100, true},
	}
	for _, c := range cases {
		s := sampleOf(c.total, c.waited, time.Millisecond)
		if s.Valid != c.valid || s.Bytes != c.total || s.WaitedBytes != c.waited || s.Receive != time.Millisecond {
			t.Errorf("sampleOf(%d, %d) = %+v, valid atteso %v", c.total, c.waited, s, c.valid)
		}
	}
}

func valid(bytes int, d time.Duration) Sample {
	return Sample{Bytes: bytes, Receive: d, WaitedBytes: bytes, Valid: true}
}

func TestEstimatorBandwidthEWMA(t *testing.T) {
	e := NewEstimator()
	if s := e.Snapshot(); s.BandwidthBps != nil || s.LatencyMS != nil {
		t.Fatalf("stato iniziale %+v: attesi nil", s)
	}
	e.Observe(time.Time{}, valid(1000, time.Millisecond)) // 1e6 B/s
	first := e.Snapshot().BandwidthBps
	if first == nil || *first != 1e6 {
		t.Fatalf("primo campione: banda %v, attesa 1e6", first)
	}
	e.Observe(time.Time{}, valid(2000, time.Millisecond)) // 2e6 B/s
	got := *e.Snapshot().BandwidthBps
	want := BandwidthAlpha*2e6 + (1-BandwidthAlpha)*1e6
	if got != want {
		t.Errorf("banda %v, attesa %v", got, want)
	}
	if *first != 1e6 {
		t.Errorf("il puntatore del primo Snapshot è cambiato: %v", *first)
	}
}

func TestEstimatorDiscardedDoNotMoveBandwidth(t *testing.T) {
	e := NewEstimator()
	e.Observe(time.Time{}, valid(1000, time.Millisecond))
	e.Observe(time.Time{}, Sample{Bytes: 1000, Receive: time.Microsecond}) // dal buffer: 1e9 B/s
	s := e.Snapshot()
	if *s.BandwidthBps != 1e6 || s.Valid != 1 || s.Discarded != 1 {
		t.Errorf("stato %+v: banda 1e6, 1 valido, 1 scartato", s)
	}
}

func TestEstimatorNotLimiting(t *testing.T) {
	e := NewEstimator()
	discarded := Sample{Bytes: 1000, Receive: time.Microsecond}
	for range RecentWindow - 1 {
		e.Observe(time.Time{}, discarded)
	}
	if e.Snapshot().NotLimiting {
		t.Fatal("con meno di RecentWindow campioni non si può dire")
	}
	e.Observe(time.Time{}, discarded)
	if !e.Snapshot().NotLimiting {
		t.Fatal("RecentWindow campioni tutti scartati: rete non limitante")
	}
	// Ne bastano 2 validi su 20 (10%) perché la rete torni a limitare.
	e.Observe(time.Time{}, valid(1000, time.Millisecond))
	if !e.Snapshot().NotLimiting {
		t.Fatal("1 valido su 20 (5%) è ancora sotto NotLimitingShare")
	}
	e.Observe(time.Time{}, valid(1000, time.Millisecond))
	if e.Snapshot().NotLimiting {
		t.Fatal("2 validi su 20 (10%) non è sotto NotLimitingShare")
	}
	if s := e.Snapshot(); s.Valid+s.Discarded != RecentWindow+2 {
		t.Errorf("contati %d campioni, attesi %d", s.Valid+s.Discarded, RecentWindow+2)
	}
}

func TestEstimatorLatencyIsWindowedMin(t *testing.T) {
	e := NewEstimator()
	t0 := time.Unix(0, 0)
	e.ObservePing(t0, 10*time.Millisecond)
	e.ObservePing(t0.Add(time.Second), 4*time.Millisecond)
	e.ObservePing(t0.Add(2*time.Second), 300*time.Millisecond) // pong dietro la coda
	if got := *e.Snapshot().LatencyMS; got != 2 {
		t.Errorf("latenza %v ms, attesa 2 (metà del minimo)", got)
	}
	// Il minimo resta finché ha meno di LatencyWindow, anche con tanti ping in coda.
	for i := range 50 {
		e.ObservePing(t0.Add(time.Duration(3+i)*time.Second), 300*time.Millisecond)
	}
	if got := *e.Snapshot().LatencyMS; got != 2 {
		t.Errorf("dopo 53 s latenza %v ms, attesa ancora 2", got)
	}
	// Oltre LatencyWindow il minimo vecchio esce dalla finestra.
	e.ObservePing(t0.Add(time.Second+LatencyWindow+time.Millisecond), 8*time.Millisecond)
	if got := *e.Snapshot().LatencyMS; got != 4 {
		t.Errorf("latenza %v ms, attesa 4", got)
	}
}

func TestEstimatorKeepsTheLastPing(t *testing.T) {
	// Un ping arrivato dopo una lunga pausa non svuota la finestra.
	e := NewEstimator()
	t0 := time.Unix(0, 0)
	e.ObservePing(t0, 2*time.Millisecond)
	e.ObservePing(t0.Add(10*LatencyWindow), 6*time.Millisecond)
	if got := *e.Snapshot().LatencyMS; got != 3 {
		t.Errorf("latenza %v ms, attesa 3", got)
	}
}

func TestEstimatorLastSample(t *testing.T) {
	e := NewEstimator()
	if !e.LastSample().IsZero() {
		t.Fatal("senza campioni LastSample deve essere zero")
	}
	t1 := time.Unix(10, 0)
	e.Observe(t1, Sample{Bytes: 1, Receive: time.Microsecond}) // scartato: conta lo stesso
	if !e.LastSample().Equal(t1) {
		t.Errorf("LastSample = %v, atteso %v", e.LastSample(), t1)
	}
}

func TestUploadMS(t *testing.T) {
	lat, bw := 2.0, 1e6
	cases := []struct {
		name string
		s    State
		ms   float64
		ok   bool
	}{
		{"senza latenza", State{BandwidthBps: &bw}, 0, false},
		{"senza banda", State{LatencyMS: &lat}, 0, false},
		{"latenza + byte/banda", State{LatencyMS: &lat, BandwidthBps: &bw}, 2 + 10, true},
		{"rete non limitante", State{LatencyMS: &lat, NotLimiting: true}, 2, true},
	}
	for _, c := range cases {
		ms, ok := c.s.UploadMS(10_000)
		if ms != c.ms || ok != c.ok {
			t.Errorf("%s: (%v, %v), attesi (%v, %v)", c.name, ms, ok, c.ms, c.ok)
		}
	}
}
