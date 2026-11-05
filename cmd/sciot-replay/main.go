// sciot-replay rilegge una registrazione del server (docs/RECORDING.md) per
// verificare offline i modelli e gli algoritmi della Fase 3.
//
//	sciot-replay queue [-window 5s] [-min-frames 20] record.jsonl
//	sciot-replay algo [-algorithm static|latency] [-config config.json] [-out decisions.jsonl] record.jsonl
//	sciot-replay check [-warmup 20s] [-min-frames 10] [-margin-rel 0.1] record.jsonl frames_<device>.csv...
//
// queue: per ogni finestra, attesa in coda misurata contro quella prevista da
// M/M/c (Erlang C) e G/G/c (Allen–Cunneen), con λ, S, c, ca e cs misurati
// nella stessa finestra; poi un riassunto per fascia di utilizzo ρ.
//
// algo: riesegue un algoritmo di offloading su ogni heartbeat registrato
// (con lo stato che il server aveva in quell'istante) e riassume per device
// decisioni, cambi di policy e latenze previste e misurate. A ciclo aperto:
// le decisioni non cambiano i dati che seguono.
//
// check: per ogni heartbeat di un device in SPLIT, la previsione della
// simulazione per la coppia applicata contro le misure del client (il CSV dei
// frame, uno per device: il device è nel nome, frames_<device>.csv); errori
// mediani per fascia di ρ previsto e queue_rho_from ricavato con margin_rel.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/config"
	"github.com/UBICO/SCIoT_go_server/internal/costmodel"
	"github.com/UBICO/SCIoT_go_server/internal/offload"
	"github.com/UBICO/SCIoT_go_server/internal/replay"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "queue":
		fs := flag.NewFlagSet("queue", flag.ExitOnError)
		window := fs.Duration("window", 5*time.Second, "length of a window")
		minFrames := fs.Int("min-frames", 20, "skip windows with fewer served frames")
		fs.Parse(os.Args[2:])
		err = queue(os.Stdout, oneFile(fs), window.Seconds(), *minFrames)
	case "algo":
		fs := flag.NewFlagSet("algo", flag.ExitOnError)
		name := fs.String("algorithm", "static", "algorithm: "+strings.Join(offload.Names(), ", "))
		cfgPath := fs.String("config", "", "config.json whose \"offload\" section sets the algorithm's parameters (default: the defaults)")
		out := fs.String("out", "", "also write every decision as a JSON line (decision events)")
		fs.Parse(os.Args[2:])
		cfg := offload.DefaultLatencyConfig()
		if *cfgPath != "" {
			f, err := config.Load(*cfgPath)
			if err == nil {
				_, cfg, err = f.OffloadConfig()
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "algo: %v\n", err)
				os.Exit(2)
			}
		}
		alg, newErr := offload.New(*name, cfg)
		if newErr != nil {
			fmt.Fprintf(os.Stderr, "algo: %v\n", newErr)
			os.Exit(2)
		}
		err = algo(os.Stdout, oneFile(fs), alg, *out)
	case "check":
		fs := flag.NewFlagSet("check", flag.ExitOnError)
		warmup := fs.Duration("warmup", 20*time.Second, "skip each device's first heartbeats")
		minFrames := fs.Int("min-frames", 10, "skip windows with fewer client frames")
		marginRel := fs.Float64("margin-rel", offload.DefaultLatencyConfig().MarginRel, "offload.margin_rel: the error the margin's base covers")
		fs.Parse(os.Args[2:])
		if fs.NArg() < 2 {
			fmt.Fprintln(os.Stderr, "check: a recording and at least one frames_<device>.csv")
			os.Exit(2)
		}
		err = check(os.Stdout, fs.Arg(0), fs.Args()[1:], *warmup, *minFrames, *marginRel)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:\n  sciot-replay queue [-window 5s] [-min-frames 20] record.jsonl\n  sciot-replay algo [-algorithm static|latency] [-config config.json] [-out decisions.jsonl] record.jsonl\n  sciot-replay check [-warmup 20s] [-min-frames 10] [-margin-rel 0.1] record.jsonl frames_<device>.csv...")
	os.Exit(2)
}

func oneFile(fs *flag.FlagSet) string {
	if fs.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "%s: one recording file\n", fs.Name())
		os.Exit(2)
	}
	return fs.Arg(0)
}

func queue(out io.Writer, path string, windowS float64, minFrames int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	ws, err := replay.QueueWindows(f, windowS, minFrames)
	if err != nil {
		return err
	}
	if len(ws) == 0 {
		return fmt.Errorf("no window with at least %d served frames", minFrames)
	}
	fmt.Fprintf(out, "%8s %6s %5s %8s %8s %6s %5s %5s %12s %10s %10s\n",
		"start_s", "frames", "slots", "lambda/s", "S_ms", "rho", "ca", "cs", "measured_ms", "MMc_ms", "GGc_ms")
	for _, w := range ws {
		fmt.Fprintf(out, "%8.1f %6d %5d %8.1f %8.2f %6.3f %5.2f %5.2f %12.2f %10.2f %10.2f\n",
			w.StartS, w.Frames, w.Slots, w.Lambda, w.ServiceMS, w.Rho, w.CA, w.CS, w.MeasuredMS, w.MMcMS, w.GGcMS)
	}
	fmt.Fprintf(out, "\n%-16s %8s %12s %10s %10s\n", "rho", "windows", "measured_ms", "MMc_ms", "GGc_ms")
	for _, b := range replay.Buckets(ws) {
		if b.Windows > 0 {
			fmt.Fprintf(out, "%-16s %8d %12.2f %10.2f %10.2f\n", b.Label, b.Windows, b.MeasuredMS, b.MMcMS, b.GGcMS)
		}
	}
	return nil
}

func algo(out io.Writer, path string, alg offload.Algorithm, decisionsPath string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	steps, err := replay.Algo(f, alg)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		return fmt.Errorf("no heartbeat with the algorithm's state (recordings from phase 3.5 on)")
	}
	if decisionsPath != "" {
		if err := writeDecisions(decisionsPath, steps, alg.Name()); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "algorithm %s, open loop (decisions do not change the data that follow)\n\n", alg.Name())
	fmt.Fprintf(out, "%-16s %10s %8s %8s %22s %22s %22s\n",
		"device", "heartbeats", "differ", "changes", "predicted_actual_ms", "predicted_chosen_ms", "measured_actual_ms")
	for _, s := range replay.Summarize(steps) {
		fmt.Fprintf(out, "%-16s %10d %8d %8d %22s %22s %22s\n", s.DeviceID, s.Heartbeats, s.Differ, s.Changes,
			mean(s.PredictedActualMS, s.NPredActual), mean(s.PredictedChosenMS, s.NPredChosen), mean(s.MeasuredActualMS, s.NMeasured))
	}
	return nil
}

// mean stampa una media con quanti heartbeat la compongono.
func mean(v float64, n int) string {
	if n == 0 || math.IsNaN(v) {
		return "—"
	}
	return fmt.Sprintf("%.2f (%d)", v, n)
}

func writeDecisions(path string, steps []replay.Step, name string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, s := range steps {
		if err := enc.Encode(replay.DecisionEvent(s, name)); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

func check(out io.Writer, recordPath string, csvPaths []string, warmup time.Duration, minFrames int, marginRel float64) error {
	clients := map[string][]replay.ClientFrame{}
	for _, path := range csvPaths {
		id, ok := strings.CutPrefix(strings.TrimSuffix(filepath.Base(path), ".csv"), "frames_")
		if !ok {
			return fmt.Errorf("%s: the name must be frames_<device>.csv", path)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		frames, err := replay.ReadClientFrames(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		clients[id] = frames
	}
	f, err := os.Open(recordPath)
	if err != nil {
		return err
	}
	defer f.Close()
	ws, err := replay.Check(f, clients, warmup, minFrames)
	if err != nil {
		return err
	}
	if len(ws) == 0 {
		return fmt.Errorf("no SPLIT heartbeat window with a complete prediction and at least %d client frames", minFrames)
	}
	fmt.Fprintf(out, "relative error (predicted − measured)/measured, median per band of predicted rho; %d windows\n\n", len(ws))
	fmt.Fprintf(out, "%-16s %8s %8s %8s %8s %8s %8s\n", "rho", "windows", "p95", "|p95|", "mean", "fps", "share")
	bs := replay.CheckBuckets(ws)
	for _, b := range bs {
		if b.Windows > 0 {
			fmt.Fprintf(out, "%-16s %8d %8s %8s %8s %8s %8s\n", b.Label, b.Windows, pct(b.P95), pct(b.AbsP95), pct(b.Mean), pct(b.FPS), pct(b.Share))
		}
	}
	// Dove vanno i frame che il server non serve: rifiutati o scaduti sul
	// server, oppure finiti sul device a finestra piena. Valori assoluti
	// (frame/s): un errore relativo non esiste dove il misurato è 0.
	var rejP, rejM, fullP, fullM []float64
	for _, w := range ws {
		rejP, rejM = append(rejP, w.Pred.RejectedPerSec), append(rejM, w.Meas.RejectedPerSec)
		fullP, fullM = append(fullP, w.Pred.WindowFullPerSec), append(fullM, w.Meas.WindowFullPerSec)
	}
	med := func(x []float64) float64 { return costmodel.Percentile(x, 0.5) }
	fmt.Fprintf(out, "\nframes/s not served, median of the windows:  %12s %10s\n", "predicted", "measured")
	fmt.Fprintf(out, "  refused or expired on the server           %12.1f %10.1f\n", med(rejP), med(rejM))
	fmt.Fprintf(out, "  window full (finished on the device)       %12.1f %10.1f\n", med(fullP), med(fullM))
	if rho, ok := replay.RhoFrom(bs, marginRel); ok {
		fmt.Fprintf(out, "\nqueue_rho_from = %.2f (first band with median |p95 error| > margin_rel %.2f)\n", rho, marginRel)
	} else {
		fmt.Fprintf(out, "\nqueue_rho_from: no band above margin_rel %.2f in these data\n", marginRel)
	}
	return nil
}

// pct stampa un errore relativo in percento.
func pct(v float64) string {
	if math.IsNaN(v) {
		return "—"
	}
	return fmt.Sprintf("%+.0f%%", 100*v)
}
