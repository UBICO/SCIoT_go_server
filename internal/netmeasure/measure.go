// Package netmeasure misura la rete tra un device e il server, dal server:
// quanto dura la ricezione di un frame (banda) e quanto un ping (latenza).
//
// La misura nasce sul server, che è chi decide la policy: il device non deve
// riportare nulla. Un solo orologio per ogni durata: niente sincronizzazione.
//
//	tempo di invio di un frame ≈ latenza (RTT/2) + byte / banda
//
// Il limite (docs/RECORDING.md, "Network"): il server vede i byte quando li
// LEGGE, non quando arrivano. Se si sono accumulati nel buffer TCP del kernel
// prima della lettura, la ricezione sembra più corta e la banda più alta del
// vero. Per questo ogni campione dice se la lettura ha davvero aspettato la
// rete: solo quelli entrano nella stima della banda.
//
// La compressione WebSocket deve restare disattivata (default della
// libreria): con la compressione i byte sulla rete non sarebbero più quelli
// del messaggio e byte / tempo non sarebbe la banda.
package netmeasure

import (
	"io"
	"time"
)

// WaitThreshold separa una lettura che ha preso byte già presenti nel buffer
// (ritorna in pochi microsecondi: è una copia in memoria) da una che ha
// aspettato che arrivassero dalla rete.
const WaitThreshold = 200 * time.Microsecond

// ValidWaitedShare è la quota minima di byte arrivati da letture che hanno
// aspettato perché il campione misuri la rete e non il buffer.
const ValidWaitedShare = 0.9

// Sample è la ricezione di un messaggio.
type Sample struct {
	Bytes   int           // byte del messaggio (payload WebSocket)
	Receive time.Duration // dalla lettura dell'inizio del messaggio all'ultimo byte
	// WaitedBytes sono i byte arrivati da letture che hanno aspettato la rete.
	WaitedBytes int
	// Valid: il campione misura la rete (WaitedBytes/Bytes >= ValidWaitedShare).
	Valid bool
}

// BandwidthBps è la banda del campione, in byte al secondo; 0 senza durata.
func (s Sample) BandwidthBps() float64 {
	if s.Receive <= 0 {
		return 0
	}
	return float64(s.Bytes) / s.Receive.Seconds()
}

// ReadChunk è la dimensione di ogni Read. La libreria riempie p per intero
// (io.ReadFull), quindi ogni Read restituisce ReadChunk byte (meno solo alla
// fine del messaggio) e la sua durata dice se li ha aspettati. Deve essere
// fissa e grande rispetto a un segmento TCP (~1,5 KB):
//   - buffer piccoli (io.ReadAll comincia da 512 B) spezzano ogni blocco
//     arrivato dalla rete in più Read: la prima aspetta, le altre trovano i
//     byte nel buffer, e anche una rete lenta sembrerebbe "già nel buffer";
//   - buffer enormi (io.ReadAll li fa crescere fino a MB) mescolano in una
//     sola Read byte aspettati e byte già presenti.
//
// 64 KiB: a 1 Gbit/s arrivano in ~0,5 ms (sopra WaitThreshold); dal buffer
// del kernel si copiano in decine di µs.
const ReadChunk = 64 << 10

// timedReader cronometra ogni Read e conta i byte arrivati aspettando.
type timedReader struct {
	r      io.Reader
	now    func() time.Time
	total  int
	waited int
}

func (t *timedReader) Read(p []byte) (int, error) {
	start := t.now()
	n, err := t.r.Read(p)
	d := t.now().Sub(start)
	// Una lettura da 0 byte non aggiunge nulla a nessuno dei due contatori.
	if n > 0 {
		t.total += n
		if d >= WaitThreshold {
			t.waited += n
		}
	}
	return n, err
}

// ReadMessage legge il resto di un messaggio WebSocket da r e ne misura la
// ricezione. started è l'istante in cui la libreria ha restituito r, cioè
// quando è arrivato l'inizio del messaggio (ws.Reader).
//
// La lettura va fatta tutta qui: è ciò che viene cronometrato.
func ReadMessage(r io.Reader, started time.Time, now func() time.Time) ([]byte, Sample, error) {
	tr := &timedReader{r: r, now: now}
	var data []byte
	chunk := make([]byte, ReadChunk)
	for {
		n, err := tr.Read(chunk)
		data = append(data, chunk[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, Sample{}, err
		}
	}
	return data, sampleOf(tr.total, tr.waited, now().Sub(started)), nil
}

// sampleOf costruisce il campione e ne decide la validità.
func sampleOf(total, waited int, receive time.Duration) Sample {
	// Un messaggio vuoto non è mai valido; float64 per evitare la divisione intera.
	valid := total > 0 && float64(waited)/float64(total) >= ValidWaitedShare
	return Sample{Bytes: total, Receive: receive, WaitedBytes: waited, Valid: valid}
}
