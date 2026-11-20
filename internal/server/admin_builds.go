package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/UBICO/SCIoT_go_server/internal/api"
)

// Build dei modelli dalla dashboard (docs/ADMIN_API.md, docs/MODELS.md):
//
//	GET    /api/v1/admin/models/builds        i job, dal più recente
//	POST   /api/v1/admin/models/builds        upload (multipart) e avvio
//	GET    /api/v1/admin/models/builds/{id}   stato e log (?since=N)
//	DELETE /api/v1/admin/models/builds/{id}   annulla il job in corso
//
// Il build è sciot-model build in un sottoprocesso, nella cartella da cui
// gira il server (come il worker). Il comando è fisso (buildCommand), mai
// dalla config: gli unici argomenti sono i file caricati e opzioni validate.
// Un job alla volta (la conversione TensorFlow è pesante e falserebbe le
// misure di un esperimento sulla stessa macchina), in memoria: un riavvio
// perde lo storico dei job, non i modelli costruiti.

// Codici di errore dei build.
const (
	errCodeBuildRunning = "build_running" // 409: c'è già un build in corso
	errCodeModelExists  = "model_exists"  // 409: models_dir/<key> esiste già
	errCodeTooLarge     = "too_large"     // 413: upload oltre maxBuildUpload
	errCodeNotRunning   = "not_running"   // 409: annullare un build finito
)

// Limiti.
const (
	// buildUploadTimeout: il ReadTimeout della porta admin (10 s) chiuderebbe
	// un upload grande; per questa richiesta si allunga.
	buildUploadTimeout = 15 * time.Minute
	// maxBuildMemory: la parte dell'upload tenuta in memoria; il resto va in
	// file temporanei (ParseMultipartForm).
	maxBuildMemory    = 32 << 20
	maxBuildSamples   = 10
	maxBuildSegments  = 64
	maxBuildLogLines  = 10000 // poi "… log truncated"
	maxBuildJobs      = 20    // storico in memoria
	buildKillGrace    = 5 * time.Second
	buildLogTruncated = "… log truncated"
)

// maxBuildUpload: tutto l'upload (due zip più le immagini). Gli export
// SavedModel sono nell'ordine delle decine di MB. Una variabile solo per i
// test (il 413 senza caricare 1 GiB).
var maxBuildUpload int64 = 1 << 30

// buildCommand è il comando del build; gli argomenti si aggiungono in coda.
// Una variabile e non una costante solo per i test (uno script finto).
var buildCommand = []string{"uv", "run", "--project", "worker", "sciot-model", "build"}

var (
	// modelKey: la chiave del modello, che diventa anche il nome della
	// cartella: niente "/" né "..".
	modelKey = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	// cutTensor: il nome di un tensore per --cut.
	cutTensor = regexp.MustCompile(`^[A-Za-z0-9_./:;-]{1,256}$`)
	// modelHashLine: la riga del build con l'hash ("model_hash <64 hex>").
	modelHashLine = regexp.MustCompile(`^model_hash ([0-9a-f]{32,64})$`)
)

type buildState string

const (
	buildRunning   buildState = "running"
	buildSucceeded buildState = "succeeded"
	buildFailed    buildState = "failed"
	buildCancelled buildState = "cancelled"
)

// buildJob è un build. I campi sotto mu cambiano mentre il processo gira.
type buildJob struct {
	ID        string
	ModelKey  string
	Output    string // models_dir/<key>, come andrà in config.json (tflite_dir)
	Who       string
	StartedAt time.Time
	workDir   string // gli upload; si cancella a fine job

	mu         sync.Mutex
	state      buildState
	finishedAt *time.Time
	exitCode   *int
	modelHash  string
	log        []string
	partial    []byte // l'ultima riga, senza ancora '\n'
	cancelled  bool   // annullato dall'admin (o dallo shutdown)
	cmd        *exec.Cmd
	done       chan struct{} // chiuso quando il processo è uscito e il job è chiuso
}

// buildView è un job nel JSON. Log solo nel GET di un job.
type buildView struct {
	ID         string     `json:"id"`
	ModelKey   string     `json:"model_key"`
	Output     string     `json:"output"`
	Who        string     `json:"who"`
	State      buildState `json:"state"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	ModelHash  string     `json:"model_hash,omitempty"`
	LogLines   int        `json:"log_lines"`
	// Log: le righe da since in poi; Next: il since della prossima richiesta.
	Log  []string `json:"log,omitempty"`
	Next int      `json:"next,omitempty"`
}

func (j *buildJob) view(since int, withLog bool) buildView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := buildView{
		ID: j.ID, ModelKey: j.ModelKey, Output: j.Output, Who: j.Who,
		State: j.state, StartedAt: j.StartedAt, FinishedAt: j.finishedAt,
		ExitCode: j.exitCode, ModelHash: j.modelHash, LogLines: len(j.log),
	}
	if withLog {
		since = min(max(since, 0), len(j.log))
		v.Log = append([]string{}, j.log[since:]...)
		v.Next = len(j.log)
	}
	return v
}

// Write raccoglie l'output del processo (stdout e stderr) riga per riga: è
// l'io.Writer di cmd.Stdout e cmd.Stderr. exec copia l'output da due
// goroutine: Write va protetta con mu.
func (j *buildJob) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.partial = append(j.partial, p...)
	for {
		i := bytes.IndexByte(j.partial, '\n')
		if i < 0 {
			break
		}
		j.appendLine(string(bytes.TrimSuffix(j.partial[:i], []byte("\r"))))
		j.partial = j.partial[i+1:]
	}
	// Mai un errore: fermerebbe la copia e il processo resterebbe bloccato sulla pipe.
	return len(p), nil
}

// appendLine aggiunge una riga al log, con j.mu già acquisito. Oltre
// maxBuildLogLines il log si ferma con buildLogTruncated (una volta sola),
// ma model_hash si legge lo stesso: è nelle ultime righe.
func (j *buildJob) appendLine(line string) {
	if m := modelHashLine.FindStringSubmatch(line); m != nil {
		j.modelHash = m[1]
	}
	switch {
	case len(j.log) < maxBuildLogLines:
		j.log = append(j.log, line)
	case len(j.log) == maxBuildLogLines:
		j.log = append(j.log, buildLogTruncated)
	}
}

// builder tiene i job. Uno solo in corso alla volta.
type builder struct {
	dir     string   // models_dir
	command []string // buildCommand; i test lo cambiano
	now     func() time.Time

	mu      sync.Mutex
	jobs    []*buildJob // dal più vecchio, al massimo maxBuildJobs
	running *buildJob
	seq     int
}

// SetModelBuilds accende il build dalla dashboard: dir è models_dir. Va
// chiamata prima di AdminRoutes.
func (s *Server) SetModelBuilds(dir string) {
	s.builds = &builder{dir: dir, command: buildCommand, now: s.now}
}

// start avvia il processo di job con args (dopo buildCommand) e restituisce
// subito; una goroutine aspetta la fine e chiude il job.
//
// Il processo:
//   - cmd.Stdout e cmd.Stderr = job (Write sopra);
//   - ambiente del server più PYTHONUNBUFFERED=1: con l'output su una pipe
//     Python accumulerebbe tutto e il log arriverebbe solo alla fine;
//   - cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}: un gruppo di
//     processi suo, così cancel ferma anche i figli (uv lancia Python).
func (b *builder) start(job *buildJob, args []string) error {
	// Copia di b.command[1:]: l'append non deve toccare l'array di buildCommand.
	cmd := exec.Command(b.command[0], append(slices.Clone(b.command[1:]), args...)...)
	cmd.Stdout, cmd.Stderr = job, job
	cmd.Env = append(buildEnv(os.Environ()), "PYTHONUNBUFFERED=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	job.mu.Lock()
	job.cmd = cmd
	job.mu.Unlock()
	go func() { b.finish(job, cmd.Wait()) }()
	return nil
}

// buildEnv è env senza SCIOT_ADMIN_TOKEN: il build non lo usa, e un segreto
// non va dato a un processo che non ne ha bisogno.
func buildEnv(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		return strings.HasPrefix(kv, "SCIOT_ADMIN_TOKEN=")
	})
}

// finish chiude job dopo l'uscita del processo (err da cmd.Wait).
func (b *builder) finish(job *buildJob, err error) {
	// Prima gli effetti (cartella, b.running), poi lo stato: chi legge
	// "succeeded" può subito lanciare un altro build o guardare workDir.
	os.RemoveAll(job.workDir) // gli upload sono grandi: non restano
	b.mu.Lock()
	if b.running == job {
		b.running = nil
	}
	b.mu.Unlock()

	code := -1 // anche se ucciso da un segnale (ExitCode() dà -1)
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		code = 0
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	}
	job.mu.Lock()
	if len(job.partial) > 0 { // l'ultima riga, senza '\n' finale
		job.appendLine(string(bytes.TrimSuffix(job.partial, []byte("\r"))))
		job.partial = nil
	}
	switch {
	case job.cancelled:
		job.state = buildCancelled
	case code == 0 && job.modelHash != "":
		job.state = buildSucceeded
	default: // exit 0 senza hash: il modello non è stato scritto
		job.state = buildFailed
	}
	now := b.now()
	job.finishedAt, job.exitCode = &now, &code
	state := job.state
	job.mu.Unlock()
	close(job.done)
	log.Printf("model build %s %s: exit %d", job.ID, state, code)
}

// abort chiude come failed un job il cui processo non è partito (upload non
// salvato, comando non trovato): l'errore nel log, workDir cancellata.
func (b *builder) abort(job *buildJob, err error) {
	job.mu.Lock()
	job.appendLine("error: " + err.Error())
	now := b.now()
	job.state, job.finishedAt = buildFailed, &now
	job.mu.Unlock()
	if job.workDir != "" {
		os.RemoveAll(job.workDir)
	}
	b.mu.Lock()
	if b.running == job {
		b.running = nil
	}
	b.mu.Unlock()
	close(job.done)
	log.Printf("model build %s: %v", job.ID, err)
}

// cancel ferma il job in corso: SIGTERM al gruppo, SIGKILL dopo
// buildKillGrace se non è ancora uscito. Restituisce false se il job non è
// in corso.
func (b *builder) cancel(job *buildJob) bool {
	job.mu.Lock()
	if job.state != buildRunning || job.cmd == nil {
		job.mu.Unlock()
		return false
	}
	job.cancelled = true
	pid := job.cmd.Process.Pid
	job.mu.Unlock()
	// "-pid": tutto il gruppo (uv lancia Python).
	syscall.Kill(-pid, syscall.SIGTERM)
	go func() {
		select {
		case <-job.done:
		case <-time.After(buildKillGrace):
			syscall.Kill(-pid, syscall.SIGKILL)
		}
	}()
	return true
}

// close ferma il build in corso allo spegnimento del server e aspetta che
// sia uscito (un processo di conversione orfano terrebbe CPU e memoria).
func (b *builder) close() {
	b.mu.Lock()
	job := b.running
	b.mu.Unlock()
	if job != nil && b.cancel(job) {
		<-job.done
	}
}

// find restituisce il job id, o nil.
func (b *builder) find(id string) *buildJob {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, j := range b.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

// buildsEnabled risponde 404 se il build è spento (nessun models_dir).
func (s *Server) buildsEnabled(w http.ResponseWriter) bool {
	if s.builds == nil {
		writeError(w, http.StatusNotFound, errCodeNotFound, "model builds are not enabled: set models_dir in config.json")
		return false
	}
	return true
}

type adminBuildsResponse struct {
	Enabled   bool        `json:"enabled"`
	ModelsDir string      `json:"models_dir,omitempty"`
	Builds    []buildView `json:"builds"` // dal più recente, senza log
}

// handleAdminBuilds: GET /api/v1/admin/models/builds. Senza models_dir:
// 200 {"enabled": false} (la dashboard nasconde il pulsante).
func (s *Server) handleAdminBuilds(w http.ResponseWriter, _ *http.Request) {
	resp := adminBuildsResponse{Builds: []buildView{}}
	if b := s.builds; b != nil {
		resp.Enabled, resp.ModelsDir = true, b.dir
		b.mu.Lock()
		jobs := append([]*buildJob{}, b.jobs...)
		b.mu.Unlock()
		for i := len(jobs) - 1; i >= 0; i-- {
			resp.Builds = append(resp.Builds, jobs[i].view(0, false))
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAdminBuild: GET /api/v1/admin/models/builds/{id}?since=N.
func (s *Server) handleAdminBuild(w http.ResponseWriter, r *http.Request) {
	if !s.buildsEnabled(w) {
		return
	}
	job := s.builds.find(r.PathValue("id"))
	if job == nil {
		writeError(w, http.StatusNotFound, errCodeNotFound, "no such build")
		return
	}
	since := 0
	if q := r.URL.Query().Get("since"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, api.ErrCodeInvalidRequest, "since must be a non-negative integer")
			return
		}
		since = n
	}
	writeJSON(w, http.StatusOK, job.view(since, true))
}

// handleAdminBuildCancel: DELETE /api/v1/admin/models/builds/{id}.
//
//	202 → il processo riceve SIGTERM; lo stato diventa "cancelled" quando esce
//	409 not_running → il job è già finito
func (s *Server) handleAdminBuildCancel(w http.ResponseWriter, r *http.Request) {
	if !s.buildsEnabled(w) {
		return
	}
	job := s.builds.find(r.PathValue("id"))
	if job == nil {
		writeError(w, http.StatusNotFound, errCodeNotFound, "no such build")
		return
	}
	if !s.builds.cancel(job) {
		writeError(w, http.StatusConflict, errCodeNotRunning, "the build is not running")
		return
	}
	s.auditBuild(r, auditBuildCancel, job)
	writeJSON(w, http.StatusAccepted, job.view(0, false))
}

// saveUpload copia un file del multipart in path.
func saveUpload(fh *multipart.FileHeader, path string) error {
	src, err := fh.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

// handleAdminBuildStart: POST /api/v1/admin/models/builds, multipart:
//
//	model_key      testo, la chiave (modelKey)
//	savedmodel     file, l'export "TensorFlow SavedModel" (.zip)
//	cpp_export     file, l'export "C++ library" (.zip)
//	sample         file, 0..maxBuildSamples immagini per la verifica
//	max_segments   testo opzionale, 0..maxBuildSegments (0 = tutti i tagli)
//	cut            testo, ripetibile, un tensore per --cut (cutTensor)
//
//	202 → buildView del job avviato
//	400 invalid_request → campo mancante o non valido
//	409 build_running   → un altro build è in corso
//	409 model_exists    → models_dir/<model_key> esiste già (niente --overwrite)
//	413 too_large       → upload oltre 1 GiB
func (s *Server) handleAdminBuildStart(w http.ResponseWriter, r *http.Request) {
	if !s.buildsEnabled(w) {
		return
	}
	// L'upload è grande e lento: più del ReadTimeout della porta admin. Anche
	// la scrittura: WriteTimeout parte dagli header e scadrebbe prima della risposta.
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(buildUploadTimeout)
	for _, set := range []func(time.Time) error{rc.SetReadDeadline, rc.SetWriteDeadline} {
		if err := set(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			log.Printf("admin build: %v", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "cannot extend the upload timeout")
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBuildUpload)
	if err := r.ParseMultipartForm(maxBuildMemory); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, errCodeTooLarge, "upload larger than 1 GiB")
		} else {
			writeError(w, http.StatusBadRequest, api.ErrCodeInvalidRequest, "invalid multipart upload: "+err.Error())
		}
		return
	}
	defer r.MultipartForm.RemoveAll() // i file temporanei
	form := r.MultipartForm

	bad := func(msg string) { writeError(w, http.StatusBadRequest, api.ErrCodeInvalidRequest, msg) }
	key := ""
	if v := form.Value["model_key"]; len(v) == 1 {
		key = v[0]
	}
	if !modelKey.MatchString(key) {
		bad("model_key must match " + modelKey.String())
		return
	}
	if len(form.File["savedmodel"]) != 1 {
		bad("savedmodel: exactly one file is required")
		return
	}
	if len(form.File["cpp_export"]) != 1 {
		bad("cpp_export: exactly one file is required")
		return
	}
	samples := form.File["sample"]
	if len(samples) > maxBuildSamples {
		bad("sample: at most " + strconv.Itoa(maxBuildSamples) + " files")
		return
	}
	maxSegments := 0
	if v := form.Value["max_segments"]; len(v) > 0 && v[0] != "" {
		n, err := strconv.Atoi(v[0])
		if err != nil || n < 0 || n > maxBuildSegments {
			bad("max_segments must be an integer in [0, " + strconv.Itoa(maxBuildSegments) + "]")
			return
		}
		maxSegments = n
	}
	cuts := form.Value["cut"]
	for _, c := range cuts {
		if !cutTensor.MatchString(c) {
			bad("cut: invalid tensor name " + strconv.Quote(c))
			return
		}
	}

	b := s.builds
	output := filepath.Join(b.dir, key)
	if _, err := os.Stat(output); err == nil {
		writeError(w, http.StatusConflict, errCodeModelExists, "models_dir/"+key+" already exists")
		return
	}

	// Controllo e assegnazione di b.running sotto lo stesso lock: due POST insieme.
	b.mu.Lock()
	if b.running != nil {
		b.mu.Unlock()
		writeError(w, http.StatusConflict, errCodeBuildRunning, "another build is running")
		return
	}
	b.seq++
	job := &buildJob{
		ID: fmt.Sprintf("%s-%d", key, b.seq), ModelKey: key, Output: output,
		Who: actorOf(r), StartedAt: b.now(), state: buildRunning, done: make(chan struct{}),
	}
	b.running = job
	b.jobs = append(b.jobs, job)
	if len(b.jobs) > maxBuildJobs { // il più vecchio; quello in corso è l'ultimo
		b.jobs = slices.Delete(b.jobs, 0, len(b.jobs)-maxBuildJobs)
	}
	b.mu.Unlock()

	fail := func(err error) {
		b.abort(job, err)
		writeError(w, http.StatusInternalServerError, "internal_error", "cannot start the build: "+err.Error())
	}
	workDir, err := os.MkdirTemp("", "sciot-build-*")
	if err != nil {
		fail(err)
		return
	}
	job.workDir = workDir
	// Nomi fissi: quello originale arriva dal browser.
	savedModel := filepath.Join(workDir, "savedmodel.zip")
	cppExport := filepath.Join(workDir, "cpp_export.zip")
	if err := saveUpload(form.File["savedmodel"][0], savedModel); err != nil {
		fail(err)
		return
	}
	if err := saveUpload(form.File["cpp_export"][0], cppExport); err != nil {
		fail(err)
		return
	}
	args := []string{savedModel, "--ei-cpp-export", cppExport}
	for i, fh := range samples {
		ext := strings.ToLower(filepath.Ext(fh.Filename))
		if ext != ".jpg" && ext != ".jpeg" && ext != ".png" && ext != ".bmp" {
			ext = ".img"
		}
		path := filepath.Join(workDir, "sample-"+strconv.Itoa(i+1)+ext)
		if err := saveUpload(fh, path); err != nil {
			fail(err)
			return
		}
		args = append(args, "--sample", path)
	}
	for _, c := range cuts {
		args = append(args, "--cut", c)
	}
	if maxSegments > 0 {
		args = append(args, "--max-segments", strconv.Itoa(maxSegments))
	}
	args = append(args, "--output", output)
	if err := b.start(job, args); err != nil {
		fail(err)
		return
	}
	s.auditBuild(r, auditBuildStart, job)
	writeJSON(w, http.StatusAccepted, job.view(0, false))
}
