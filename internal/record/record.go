// Package record scrive gli eventi del server in un file JSON Lines (un
// oggetto JSON per riga), per valutare offline la Fase 3: rieseguire un
// algoritmo sugli stessi dati, confrontare previsioni e misure, fare grafici.
//
// Il formato è descritto in docs/RECORDING.md (schema sciot-record.v1).
//
// La scrittura non rallenta mai le richieste: Record mette l'evento in un
// channel e una goroutine dedicata lo scrive. Se il channel è pieno (disco
// lento) l'evento viene scartato e contato, non si aspetta.
package record

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Schema è il nome e la versione del formato, nella riga "start".
const Schema = "sciot-record.v1"

// Options configura il Recorder.
type Options struct {
	Buffer        int           // eventi in attesa di scrittura prima di scartarli
	FlushInterval time.Duration // ogni quanto il buffer del file va su disco
}

// DefaultOptions: 4096 eventi sono qualche secondo di traffico di molti
// device; un flush al secondo limita a un secondo la perdita per un crash.
func DefaultOptions() Options {
	return Options{Buffer: 4096, FlushInterval: time.Second}
}

// Stats sono i contatori del Recorder, esposti in /api/v1/server/status.
type Stats struct {
	File    string `json:"file"`
	Written uint64 `json:"written"`
	Dropped uint64 `json:"dropped"`
}

// Recorder scrive eventi in un file. Un *Recorder nil è valido e non fa
// nulla: chi registra non deve controllare se la registrazione è attiva.
type Recorder struct {
	path  string
	start time.Time        // per t_ms; contiene la lettura monotona dell'orologio
	now   func() time.Time // sostituibile nei test

	file *os.File
	w    *bufio.Writer
	opts Options

	// mu protegge closed e la chiusura di ch: Record e Close possono essere
	// chiamati da goroutine diverse (vedi TODO(1) e TODO(3)).
	mu     sync.RWMutex
	closed bool
	ch     chan Event
	done   chan struct{} // chiuso quando la goroutine di scrittura è terminata

	written atomic.Uint64
	dropped atomic.Uint64
}

// Open crea dir (se serve) e il file sciot-<ora di avvio>.jsonl, scrive la
// riga "start" e avvia la goroutine di scrittura.
func Open(dir string, start Start, opts Options) (*Recorder, error) {
	now := time.Now()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("record: %w", err)
	}
	// Ora locale con i secondi: un file per avvio, ordinabile per nome.
	path := filepath.Join(dir, "sciot-"+now.Format("20060102-150405")+".jsonl")
	// O_EXCL: due server avviati nello stesso secondo non scrivono nello
	// stesso file.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("record: %w", err)
	}
	r := &Recorder{
		path: path, start: now, now: time.Now,
		file: file, w: bufio.NewWriter(file), opts: opts,
		ch: make(chan Event, opts.Buffer), done: make(chan struct{}),
	}
	// La riga start si scrive subito, prima della goroutine: è sempre la prima.
	start.Schema = Schema
	r.stamp(&start)
	if err := r.write(&start); err != nil {
		file.Close()
		return nil, err
	}
	go r.run()
	return r, nil
}

// stamp imposta kind, ts e t_ms dell'evento. Va chiamata da Record, cioè
// quando l'evento ACCADE, non quando viene scritto (anche un secondo dopo).
func (r *Recorder) stamp(e Event) {
	now := r.now()
	h := e.header()
	h.Kind = e.kind()
	h.TS = now.UTC()
	// Sub usa la lettura monotona se entrambi i tempi la hanno: non salta
	// con le correzioni NTP dell'orologio di sistema.
	h.TMS = float64(now.Sub(r.start).Microseconds()) / 1000
}

// write codifica e scrive un evento (una riga). Solo dalla goroutine di
// scrittura, o prima che parta (Open) o dopo che è finita (Close).
func (r *Recorder) write(e Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("record: %w", err)
	}
	data = append(data, '\n')
	if _, err := r.w.Write(data); err != nil {
		return fmt.Errorf("record: %w", err)
	}
	r.written.Add(1)
	return nil
}

// Record registra un evento senza bloccare. Sicura da più goroutine e dopo
// Close (l'evento viene ignorato).
func (r *Recorder) Record(e Event) {
	if r == nil {
		return
	}
	// Il tempo è quello dell'evento, non della scrittura.
	r.stamp(e)
	// RLock: più Record insieme vanno bene, ma nessuno deve inviare su ch
	// mentre Close lo chiude (invio su channel chiuso = panic).
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return
	}
	select {
	case r.ch <- e:
	default: // buffer pieno: scarta e conta
		r.dropped.Add(1)
	}
}

// run è la goroutine di scrittura: scrive gli eventi di ch e svuota il buffer
// del file ogni FlushInterval. Termina quando ch viene chiuso e svuotato.
func (r *Recorder) run() {
	defer close(r.done)
	ticker := time.NewTicker(r.opts.FlushInterval)
	defer ticker.Stop()
	// Un errore (disco pieno) si logga una volta sola, non a ogni evento.
	var writeFailed, flushFailed bool
	for {
		select {
		case e, ok := <-r.ch:
			// Un channel chiuso consegna prima tutti gli eventi rimasti e solo
			// dopo ok == false: nessun evento accettato da Record va perso.
			if !ok {
				return
			}
			if err := r.write(e); err != nil && !writeFailed {
				writeFailed = true
				log.Printf("%v", err)
			}
		case <-ticker.C:
			if err := r.w.Flush(); err != nil && !flushFailed {
				flushFailed = true
				log.Printf("record: flush: %v", err)
			}
		}
	}
}

// Close ferma la registrazione: scrive gli eventi in attesa e la riga "end",
// svuota il buffer e chiude il file. Si può chiamare più volte.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	// Il Lock aspetta che finiscano i Record in corso (che hanno l'RLock).
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	close(r.ch)
	r.mu.Unlock()

	// La goroutine ha scritto tutto ed è terminata: da qui write è sicura.
	<-r.done

	// Written è il valore prima della riga end (che non conta se stessa).
	end := &End{Written: r.written.Load(), Dropped: r.dropped.Load()}
	r.stamp(end)
	firstErr := r.write(end)
	if err := r.w.Flush(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("record: %w", err)
	}
	if err := r.file.Close(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("record: %w", err)
	}
	return firstErr
}

// Stats restituisce i contatori.
func (r *Recorder) Stats() Stats {
	if r == nil {
		return Stats{}
	}
	return Stats{File: r.path, Written: r.written.Load(), Dropped: r.dropped.Load()}
}
