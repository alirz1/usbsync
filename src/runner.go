package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Run struct {
	ID        string     `json:"id"`
	JobID     string     `json:"jobId"`
	JobName   string     `json:"jobName"`
	Direction string     `json:"direction"`
	DryRun    bool       `json:"dryRun"`
	Trigger   string     `json:"trigger"`
	State     string     `json:"state"` // queued running success warning failed cancelled
	Queued    time.Time  `json:"queued"`
	Start     *time.Time `json:"start,omitempty"`
	End       *time.Time `json:"end,omitempty"`

	// live progress
	Percent int    `json:"percent"`
	Bytes   int64  `json:"bytes"`
	Rate    string `json:"rate"`
	ETA     string `json:"eta"`
	ToCheck int    `json:"toCheck"`
	Total   int    `json:"total"`
	Current string `json:"current"`
	Pass    int    `json:"pass"`
	// Phase: "scanning" (working out what to copy) or "copying"
	Phase       string  `json:"phase"`
	Scanned     int     `json:"scanned"`     // items checked during the scan
	ToCopyBytes int64   `json:"toCopyBytes"` // from the pre-scan; 0 = unknown
	ToCopyFiles int     `json:"toCopyFiles"`
	FilesDone   int     `json:"filesDone"`
	CurSize     int64   `json:"curSize"`
	CurDone     int64   `json:"curDone"`
	Speed       float64 `json:"speed"`   // bytes/s, smoothed
	EtaSecs     int     `json:"etaSecs"` // -1 unknown
	Passes      int     `json:"passes"`

	// results (for a dry run: what would happen)
	NewFiles   int   `json:"newFiles"`
	NewBytes   int64 `json:"newBytes"`
	Updated    int   `json:"updated"`
	UpdBytes   int64 `json:"updBytes"`
	Deleted    int   `json:"deleted"`
	Unchanged  int   `json:"unchanged"`
	NewDirs    int   `json:"newDirs"`
	TotalBytes int64 `json:"totalBytes"` // size of every file the job covers
	DelBytes   int64 `json:"delBytes"`   // previews: size of the files that would be deleted
	ErrorCount int   `json:"errorCount"`
	Truncated  bool  `json:"truncated"`
	Paused     bool  `json:"paused"`

	FilesXfer int64  `json:"filesTransferred"`
	BytesXfer int64  `json:"bytesTransferred"`
	ExitCode  int    `json:"exitCode"`
	Message   string `json:"message"`
	Criteria  string `json:"criteria"`
	// Excluded lists every exclude rule in effect (system folders + the job's patterns)
	Excluded []string  `json:"excluded,omitempty"`
	Sources  []string  `json:"sources"`
	Dest     string    `json:"dest"`
	Pairs    []RunPair `json:"pairs,omitempty"`
	Errors   []string  `json:"errors,omitempty"`
	Notes    []string  `json:"notes,omitempty"`
	// Warn: things the user must know about this run (shown in the report)
	Warn    []string `json:"warn,omitempty"`
	Spec    *Job     `json:"spec,omitempty"`
	HasSpec bool     `json:"hasSpec"` // can be re-run
	// Recent: the last few changes found, for the live preview feed
	Recent []recentItem `json:"recent,omitempty"`

	job       *Job
	curRaw    string // current file as rsync names it (no pair prefix)
	curDest   string // destination folder of the current pair
	curPass   int
	cancelFn  func()
	cancelled bool
	pgid      int // process group of the rsync currently running for this run
}

type RunPair struct {
	Sources []string `json:"sources"`
	Dest    string   `json:"dest"`
}

var volPrefixRe = regexp.MustCompile(`^/volume\d+/`)

// pairLabel names a pair by its destination folder for the report.
func pairLabel(dest string, d *Drive) string {
	if d != nil && underOrEqual(dest, d.Mount) {
		rel := strings.TrimPrefix(strings.TrimPrefix(dest, d.Mount), "/")
		if rel == "" {
			return d.Name
		}
		return strings.ReplaceAll(rel, "/", "∕")
	}
	b := strings.ReplaceAll(volPrefixRe.ReplaceAllString(dest, ""), "/", "∕")
	if b == "" {
		return dest
	}
	return b
}

type recentItem struct {
	Status string `json:"s"`
	Path   string `json:"p"`
	Size   int64  `json:"z"`
}

type Runner struct {
	mu      sync.Mutex
	active  []*Run // running now (several can run at once)
	queue   []*Run
	history []*Run
	wake    chan struct{}
}

var runner = &Runner{wake: make(chan struct{}, 1)}

const maxHistory = 50

func runFile(id, ext string) string { return filepath.Join(dataDir, "logs", id+ext) }

func (r *Runner) loadHistory() {
	b, err := os.ReadFile(filepath.Join(dataDir, "history.json"))
	if err == nil {
		json.Unmarshal(b, &r.history)
		if len(r.history) > maxHistory {
			r.saveHistoryLocked() // trims older runs and their logs
		}
	}
}

func (r *Runner) saveHistoryLocked() {
	for len(r.history) > maxHistory {
		old := r.history[len(r.history)-1]
		os.Remove(runFile(old.ID, ".log"))
		os.Remove(runFile(old.ID, ".items"))
		r.history = r.history[:len(r.history)-1]
	}
	b, _ := json.Marshal(r.history)
	writeFileAtomic(filepath.Join(dataDir, "history.json"), b)
}

func (r *Runner) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Runner) enqueue(j *Job, dry bool, trigger string) (*Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if j.ID != "" {
		for _, a := range r.active {
			if a.JobID == j.ID && a.DryRun == dry {
				return nil, errors.New("This job is already running.")
			}
		}
		for _, q := range r.queue {
			if q.JobID == j.ID && q.DryRun == dry {
				return nil, errors.New("This job is already queued.")
			}
		}
	}
	if len(r.queue) >= 50 {
		return nil, errors.New("Too many queued runs.")
	}
	jc := *j
	spec := *j
	run := &Run{
		ID:    time.Now().Format("20060102-150405") + "-" + randHex(3),
		JobID: j.ID, JobName: j.Name, Direction: j.Direction, DryRun: dry, Trigger: trigger,
		State: "queued", Queued: time.Now(), job: &jc, Spec: &spec,
	}
	r.queue = append(r.queue, run)
	r.poke()
	cp := *run
	return &cp, nil
}

type pairPaths struct {
	pj   *Job
	srcs []string
	dest string
}

// resolveAll resolves every pair of a job on its drive.
func resolveAll(j *Job) (*Drive, []pairPaths, error) {
	j.ensurePairs()
	var drive *Drive
	var out []pairPaths
	for i := range j.Pairs {
		pj := j.pairJob(i)
		srcs, dest, d, err := resolveJob(pj)
		if err != nil {
			return nil, nil, err
		}
		drive = d
		out = append(out, pairPaths{pj, srcs, dest})
	}
	if len(out) == 0 {
		return nil, nil, errors.New("This job has no folders to sync.")
	}
	return drive, out, nil
}

func overlaps(a, b string) bool { return underOrEqual(a, b) || underOrEqual(b, a) }

// conflicts reports whether two runs touch the same folders in a way that
// could corrupt each other: one writing where the other reads or writes.
// Two previews never conflict (they don't write). Every pair counts.
func conflicts(a, b *Run) bool {
	if a.DryRun && b.DryRun {
		return false
	}
	_, ap, err1 := resolveAll(a.job)
	_, bp, err2 := resolveAll(b.job)
	if err1 != nil || err2 != nil {
		return false
	}
	for _, x := range ap {
		for _, y := range bp {
			if !a.DryRun && !b.DryRun && overlaps(x.dest, y.dest) {
				return true
			}
			if !a.DryRun {
				for _, s := range y.srcs {
					if overlaps(x.dest, s) {
						return true
					}
				}
			}
			if !b.DryRun {
				for _, s := range x.srcs {
					if overlaps(y.dest, s) {
						return true
					}
				}
			}
		}
	}
	return false
}

func maxParallel() int {
	n := getSettings().MaxParallel
	if n < 1 {
		n = 3
	}
	return min(n, 8)
}

// loop starts queued runs whenever there is a free slot and they don't
// conflict with what is already running (or with an earlier queued run).
func (r *Runner) loop() {
	for {
		r.mu.Lock()
		started := false
		limit := maxParallel()
		for i := 0; i < len(r.queue) && len(r.active) < limit; i++ {
			run := r.queue[i]
			var blocker *Run
			for _, a := range r.active {
				if conflicts(run, a) {
					blocker = a
					break
				}
			}
			if blocker == nil {
				for _, q := range r.queue[:i] { // keep order between overlapping jobs
					if conflicts(run, q) {
						blocker = q
						break
					}
				}
			}
			if blocker != nil {
				run.Message = fmt.Sprintf("Waiting for “%s” — it uses the same folders.", blocker.JobName)
				continue
			}
			r.queue = append(r.queue[:i], r.queue[i+1:]...)
			i--
			now := time.Now()
			run.State, run.Start, run.Message = "running", &now, ""
			r.active = append(r.active, run)
			started = true
			go r.runOne(run)
		}
		for _, q := range r.queue {
			if q.Message == "" && len(r.active) >= limit {
				q.Message = fmt.Sprintf("Waiting for a free slot (%d syncs at once).", limit)
			}
		}
		r.mu.Unlock()
		if !started {
			<-r.wake
		}
	}
}

func (r *Runner) runOne(run *Run) {
	r.execute(run)
	r.mu.Lock()
	end := time.Now()
	run.End = &end
	run.Current, run.Rate, run.ETA = "", "", ""
	run.Recent = nil
	for i, a := range r.active {
		if a == run {
			r.active = append(r.active[:i], r.active[i+1:]...)
			break
		}
	}
	r.history = append([]*Run{run}, r.history...)
	r.saveHistoryLocked()
	for _, q := range r.queue {
		q.Message = "" // re-evaluated by the loop
	}
	snap := *run
	r.mu.Unlock()
	r.poke()
	go afterRun(&snap)
}

func (r *Runner) cancel(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, q := range r.queue {
		if q.ID == id {
			r.queue = append(r.queue[:i], r.queue[i+1:]...)
			now := time.Now()
			q.State, q.End, q.Message = "cancelled", &now, "Removed from queue."
			r.history = append([]*Run{q}, r.history...)
			r.saveHistoryLocked()
			r.poke()
			return true
		}
	}
	for _, a := range r.active {
		if a.ID == id {
			a.cancelled = true
			if a.cancelFn != nil {
				a.cancelFn()
			}
			return true
		}
	}
	return false
}

func (r *Runner) shutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queue = nil
	for _, a := range r.active {
		a.cancelled = true
		a.Message = "Stopped because the package was stopped."
		if a.cancelFn != nil {
			a.cancelFn()
		}
	}
}

// waitIdle waits (bounded) for running syncs to be recorded after a cancel.
func (r *Runner) waitIdle(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		idle := len(r.active) == 0
		r.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (r *Runner) update(f func()) {
	r.mu.Lock()
	f()
	r.mu.Unlock()
}

func lite(run *Run) *Run {
	if run == nil {
		return nil
	}
	c := *run
	c.HasSpec = run.Spec != nil
	c.Errors = nil
	c.Spec = nil
	c.Notes = nil
	if c.End != nil {
		c.Recent = nil
	}
	return &c
}

func (r *Runner) statusJSON() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := []*Run{}
	for i, x := range r.history {
		if i >= maxHistory {
			break
		}
		h = append(h, lite(x))
	}
	q := []*Run{}
	for _, x := range r.queue {
		q = append(q, lite(x))
	}
	run := []*Run{}
	for _, x := range r.active {
		run = append(run, lite(x))
	}
	var first *Run
	if len(run) > 0 {
		first = run[0]
	}
	b, _ := json.Marshal(map[string]any{"active": first, "running": run, "queue": q, "history": h, "maxParallel": maxParallel(), "pending": pendingList()})
	return b
}

func (r *Runner) find(id string) *Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.active {
		if a.ID == id {
			c := *a
			return &c
		}
	}
	for _, x := range r.history {
		if x.ID == id {
			c := *x
			return &c
		}
	}
	return nil
}

// ---------- rsync output parsing ----------

var (
	progRe  = regexp.MustCompile(`^\s*([\d,]+)\s+(\d+)%\s+(\S+/s)\s+(\d+:\d{2}:\d{2})(?:\s+\(xfr#(\d+),\s*(?:to|ir)-chk=(\d+)/(\d+)\))?\s*$`)
	itemRe  = regexp.MustCompile(`^(\*deleting|[<>ch.*][fdLDS].{9}) +([\d,]+) (.+)$`)
	statRe  = regexp.MustCompile(`^(Number of regular files transferred|Total transferred file size): ([\d,]+)`)
	digitRe = regexp.MustCompile(`[^\d]`)
	errRe   = regexp.MustCompile(`(?i)^rsync(: | error:)|permission denied|no space left|input/output error|failed to|cannot `)
)

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(digitRe.ReplaceAllString(s, ""), 10, 64)
	return n
}

func splitCRLF(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// classify turns rsync's 11-character itemize code into a status and reasons.
// Layout: YXcstpoguax  (Y=update type, X=file type, then attribute flags)
func classify(code string) (status string, reasons []string) {
	if strings.HasPrefix(code, "*deleting") {
		return "delete", nil
	}
	if len(code) < 11 {
		return "unchanged", nil
	}
	act, typ, fl := code[0], code[1], code[2:]
	flag := func(i int, chars string) bool { return strings.ContainsRune(chars, rune(fl[i])) }
	if typ == 'd' {
		if fl[0] == '+' {
			return "newdir", nil
		}
		return "dir", nil
	}
	if fl[0] == '+' && (act == '>' || act == '<' || act == 'c') {
		return "new", nil
	}
	if act == '>' || act == '<' || act == 'c' {
		if flag(0, "c") {
			reasons = append(reasons, "content")
		}
		if flag(1, "s") {
			reasons = append(reasons, "size")
		}
		if flag(2, "tT") {
			reasons = append(reasons, "date")
		}
		if len(reasons) == 0 {
			reasons = append(reasons, "forced")
		}
		return "update", reasons
	}
	// '.' or 'h': no data transferred
	if flag(2, "tT") {
		reasons = append(reasons, "date")
	}
	if flag(3, "p") {
		reasons = append(reasons, "perms")
	}
	return "unchanged", reasons
}

var exitText = map[int]string{
	1: "rsync syntax or usage error", 2: "protocol incompatibility", 3: "errors selecting input/output files or folders",
	4: "requested action not supported", 5: "error starting client-server protocol", 10: "error in socket I/O",
	11: "error in file I/O (is the destination full or read-only?)", 12: "error in rsync data stream", 13: "errors with program diagnostics",
	14: "error in IPC code", 20: "rsync was stopped", 21: "some error returned by waitpid()", 22: "error allocating memory",
	25: "deletion limit reached", 30: "timeout in data send/receive", 35: "timeout waiting for daemon connection",
}

func severity(code int) int {
	switch code {
	case 0:
		return 0
	case 24:
		return 1
	case 23:
		return 2
	}
	return 3
}

// ---------- criteria ----------

func compareMode(o Options) string {
	if o.Compare != "" {
		return o.Compare
	}
	if o.Checksum {
		return "checksum"
	}
	return "quick"
}

func criteriaArgs(o Options, fstype string) []string {
	var a []string
	switch compareMode(o) {
	case "size":
		a = append(a, "--size-only")
	case "checksum":
		a = append(a, "--checksum")
	case "name":
		a = append(a, "--ignore-existing")
	case "always":
		a = append(a, "--ignore-times")
	}
	if o.Update {
		a = append(a, "--update")
	}
	if o.ExistingOnly {
		a = append(a, "--existing")
	}
	if o.MinSize != "" {
		a = append(a, "--min-size="+o.MinSize)
	}
	if o.MaxSize != "" {
		a = append(a, "--max-size="+o.MaxSize)
	}
	switch {
	case o.ModifyWindow != nil:
		a = append(a, "--modify-window="+strconv.Itoa(*o.ModifyWindow))
	case !isPosixFS(fstype):
		a = append(a, "--modify-window=2")
	}
	return a
}

func criteriaText(o Options, fstype string) string {
	var p []string
	switch compareMode(o) {
	case "size":
		p = append(p, "file name + size (dates ignored)")
	case "checksum":
		p = append(p, "content checksum")
	case "name":
		p = append(p, "file name only (existing files skipped)")
	case "always":
		p = append(p, "copy everything")
	default:
		p = append(p, "size + modified date")
	}
	if o.Update {
		p = append(p, "skip newer at destination")
	}
	if o.ExistingOnly {
		p = append(p, "update existing files only")
	}
	if o.MinSize != "" {
		p = append(p, "min "+o.MinSize+"B")
	}
	if o.MaxSize != "" {
		p = append(p, "max "+o.MaxSize+"B")
	}
	switch o.Modified.Mode {
	case "days":
		p = append(p, fmt.Sprintf("modified in last %d days", o.Modified.Days))
	case "since":
		p = append(p, "modified since "+o.Modified.Since)
	}
	if o.ModifyWindow != nil {
		p = append(p, fmt.Sprintf("±%ds time tolerance", *o.ModifyWindow))
	} else if !isPosixFS(fstype) {
		p = append(p, "±2s time tolerance (FAT/exFAT/NTFS)")
	}
	if o.Delete {
		p = append(p, "mirror (delete extras)")
	}
	return strings.Join(p, " · ")
}

func modifiedCutoff(m ModFilter) (time.Time, bool) {
	switch m.Mode {
	case "days":
		return time.Now().Add(-time.Duration(m.Days) * 24 * time.Hour), true
	case "since":
		t, err := time.ParseInLocation("2006-01-02", m.Since, time.Local)
		return t, err == nil
	}
	return time.Time{}, false
}

// ---------- execution ----------

type pass struct {
	srcs     []string
	listFile string
	count    int
	// set by execute for multi-pair runs
	pair   int
	dest   string
	args   []string // base args + this pair's unticked-item excludes
	prefix string   // shown before item names ("Pair 2 · TV/") when a job has several pairs
}

// planPasses returns one rsync pass normally. With a modified-date filter, it
// walks the sources and builds a --files-from list per parent folder.
func planPasses(run *Run, pj *Job, srcs []string, listBase int, logf func(string, ...any)) ([]pass, error) {
	o := pj.Options
	cutoff, ok := modifiedCutoff(o.Modified)
	if !ok {
		if pj.CopyMode == "contents" {
			// trailing slash = copy the folder's contents, not the folder
			return []pass{{srcs: []string{strings.TrimSuffix(srcs[0], "/") + "/"}}}, nil
		}
		return []pass{{srcs: srcs}}, nil
	}
	logf("Date filter: only files modified on or after %s", cutoff.Format("2006-01-02 15:04"))
	var order []string
	groups := map[string][]string{}
	for _, s := range srcs {
		p := filepath.Dir(s)
		if pj.CopyMode == "contents" {
			p = s
		}
		if _, seen := groups[p]; !seen {
			order = append(order, p)
		}
		groups[p] = append(groups[p], s)
	}
	var out []pass
	for i, parent := range order {
		lf := runFile(run.ID, fmt.Sprintf(".list%d", listBase+i))
		f, err := os.Create(lf)
		if err != nil {
			return nil, err
		}
		w := bufio.NewWriter(f)
		n := 0
		for _, s := range groups[parent] {
			filepath.WalkDir(s, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					if d != nil && d.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
				if d.IsDir() {
					if path != s && (hiddenDir(d.Name()) && o.SkipSystem) {
						return fs.SkipDir
					}
					return nil
				}
				if !d.Type().IsRegular() {
					return nil
				}
				info, err := d.Info()
				if err != nil || info.ModTime().Before(cutoff) {
					return nil
				}
				rel, _ := filepath.Rel(parent, path)
				w.WriteString(rel)
				w.WriteByte(0)
				n++
				return nil
			})
		}
		w.Flush()
		f.Close()
		logf("Pass %d: %d file(s) under %s match the date filter", i+1, n, parent)
		out = append(out, pass{srcs: []string{strings.TrimSuffix(parent, "/") + "/"}, listFile: lf, count: n})
	}
	return out, nil
}

func (r *Runner) execute(run *Run) {
	lf, err := os.Create(runFile(run.ID, ".log"))
	if err != nil {
		r.update(func() { run.State, run.Message = "failed", "Cannot write log: "+err.Error() })
		return
	}
	defer lf.Close()
	itemsF, _ := os.Create(runFile(run.ID, ".items"))
	itemsW := bufio.NewWriterSize(itemsF, 256*1024)
	defer func() {
		itemsW.Flush()
		itemsF.Close()
		for i := 0; i < 64; i++ {
			if os.Remove(runFile(run.ID, fmt.Sprintf(".list%d", i))) != nil && i > 0 {
				break
			}
		}
	}()

	logf := func(format string, a ...any) {
		fmt.Fprintf(lf, "[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
	}
	fail := func(msg string) {
		logf("ERROR: %s", msg)
		r.update(func() {
			run.State, run.Message = "failed", msg
			run.Errors = append(run.Errors, msg)
			run.ErrorCount++
		})
	}
	pkgUser := envOr("SYNOPKG_PKGNAME", "USBSync")
	permHint := fmt.Sprintf(" In DSM go to Control Panel › Shared Folder › (folder) › Edit › Permissions, choose \"System internal user\" and give %q Read/Write.", pkgUser)

	j := run.job
	normalizeOptions(&j.Options)
	kind := "Sync"
	if run.DryRun {
		kind = "Dry run"
	}
	logf("%s of job %q started (%s)", kind, j.Name, run.Trigger)

	drive, pairs, err := resolveAll(j)
	if err != nil {
		fail(err.Error())
		return
	}
	multi := len(pairs) > 1
	crit := criteriaText(j.Options, drive.FSType)
	var allSrcs []string
	var info []RunPair
	for _, pp := range pairs {
		allSrcs = append(allSrcs, pp.srcs...)
		info = append(info, RunPair{Sources: pp.srcs, Dest: pp.dest})
	}
	r.update(func() { run.Sources, run.Dest, run.Criteria, run.Pairs = allSrcs, pairs[0].dest, crit, info })
	logf("Drive: %s (%s, %s) mounted at %s", drive.Name, drive.Bus, drive.FSType, drive.Mount)
	logf("Criteria: %s", crit)
	if multi {
		logf("%d folder pairs, synced one after another", len(pairs))
	}
	pairTag := func(pi int) string {
		if multi {
			return fmt.Sprintf("Pair %d: ", pi+1)
		}
		return ""
	}

	for pi, pp := range pairs {
		label := pairTag(pi)
		for _, s := range pp.srcs {
			fi, err := os.Stat(s)
			if err != nil || !fi.IsDir() {
				fail(label + fmt.Sprintf("Source folder not found: %s", s))
				return
			}
			if !canRead(s) {
				fail(label + fmt.Sprintf("No read permission for %s.", s) + permHint)
				return
			}
			if underOrEqual(pp.dest, s) || underOrEqual(s, pp.dest) {
				fail(label + fmt.Sprintf("Source %s and destination %s overlap; pick folders that are not inside each other.", s, pp.dest))
				return
			}
		}
		if _, err := os.Stat(pp.dest); os.IsNotExist(err) {
			if !run.DryRun {
				if err := os.MkdirAll(pp.dest, 0o777); err != nil {
					fail(label + fmt.Sprintf("Cannot create destination %s: %v.", pp.dest, err) + permHint)
					return
				}
				logf("Created destination folder %s", pp.dest)
			}
		} else if !canWrite(pp.dest) && !run.DryRun {
			fail(label + fmt.Sprintf("No write permission for %s.", pp.dest) + permHint)
			return
		}
	}
	dest := pairs[0].dest // the drive-side checks below only need one destination

	rsyncBin, err := exec.LookPath("rsync")
	if err != nil {
		for _, c := range []string{"/usr/bin/rsync", "/bin/rsync"} {
			if _, e := os.Stat(c); e == nil {
				rsyncBin, err = c, nil
			}
		}
	}
	if err != nil {
		fail("rsync was not found on this NAS.")
		return
	}

	// -ii + a custom out-format reports every file, including unchanged ones,
	// with its size, so the report can show what was (or would be) skipped.
	o := j.Options
	timesOK := canSetTimes(dest, drive.Mount)
	if !timesOK {
		note := "This drive doesn't let USB Sync set file dates (DSM mounts NTFS/exFAT drives as root, and only a file's owner may change its date). " +
			"Copied files get the date they were written, so dates aren't preserved"
		switch compareMode(o) {
		case "quick":
			o.Compare = "size"
			note += ", and files are compared by name + size instead of size + date."
		default:
			note += "."
		}
		if o.Update {
			o.Update = false
			note += " “Don't overwrite newer files” was ignored because destination dates can't be trusted."
		}
		if o.ModifyWindow != nil {
			o.ModifyWindow = nil
		}
		logf("NOTE: %s", note)
		crit = criteriaText(o, drive.FSType)
		logf("Effective criteria: %s", crit)
		r.update(func() { run.Notes = append(run.Notes, note); run.Criteria = crit })
	}
	timesFlag := "-rt"
	if !timesOK {
		timesFlag = "-r"
	}
	base := []string{timesFlag, "-ii", "--out-format=%i %l %n", "--info=progress2", "--stats"}
	if !run.DryRun && compareMode(o) == "checksum" {
		// no pre-scan in checksum mode, so rsync's own % is shown; it needs
		// the full file list up front to be meaningful
		base = append(base, "--no-inc-recursive")
	}
	if rsyncAtLeast(rsyncBin, 3, 1) {
		base = append(base, "--outbuf=N") // never hold progress output back
	}
	if !run.DryRun {
		// keep a half-copied file if the sync is interrupted, so it can be resumed
		base = append(base, "--partial-dir="+partialDirName)
	}
	if isPosixFS(drive.FSType) {
		base = append(base, "-l", "-p")
	}
	base = append(base, criteriaArgs(o, drive.FSType)...)
	if o.Delete {
		base = append(base, "--delete")
	}
	if o.SkipSystem {
		// "-p" = perishable: still excluded everywhere, but ignored inside a
		// destination folder Mirror is deleting, so e.g. a movie folder's
		// @eaDir thumbnails don't stop the folder itself from being removed
		// ("cannot delete non-empty directory"). Unticked and unreadable
		// folders keep plain --exclude, so they stay fully protected.
		base = append(base, "--filter=-p @eaDir", "--filter=-p #recycle", "--filter=-p #snapshot", "--filter=-p .SynologyWorkingDirectory")
	}
	for _, e := range o.Excludes {
		base = append(base, "--filter=-p "+e) // perishable, as above
	}
	if len(o.Excludes) > 0 {
		r.update(func() { run.Excluded = append(run.Excluded, o.Excludes...) })
		logf("Excluding: %s", strings.Join(o.Excludes, ", "))
	}
	if run.DryRun {
		base = append(base, "--dry-run")
	}

	// Plan every pair: its own excludes for unticked items, its own passes.
	var passes []pass
	for pi, pp := range pairs {
		args := append([]string{}, base...)
		prefix := ""
		if multi {
			prefix = fmt.Sprintf("Pair %d · %s/", pi+1, pairLabel(pp.dest, drive))
		}
		// unticked items: anchored excludes relative to rsync's transfer root
		if skips := skipPatterns(pp.pj, pp.srcs, drive); len(skips) > 0 {
			var shown []string
			for _, sp := range skips {
				args = append(args, "--exclude="+sp.pattern)
				if multi {
					shown = append(shown, fmt.Sprintf("Pair %d · %s", pi+1, sp.display))
				} else {
					shown = append(shown, sp.display)
				}
			}
			r.update(func() { run.Excluded = append(run.Excluded, shown...) })
			logf("%sUnticked (excluded): %s", pairTag(pi), strings.Join(shown, ", "))
		}
		// Mirror: rsync stops ALL deletions if it can't open a source folder
		// ("IO error encountered -- skipping file deletion"). Find those folders
		// first and exclude them, so deletion works everywhere else and their
		// copies on the destination are left as they are.
		if o.Delete {
			if bad, more := unreadableDirs(pp, drive, o.SkipSystem); len(bad) > 0 {
				root := func(src string) string {
					if pp.pj.CopyMode == "contents" {
						return src
					}
					return filepath.Dir(src)
				}
				var shown []string
				for _, d := range bad {
					for _, src := range pp.srcs {
						if underOrEqual(d, src) && d != src {
							rel, _ := filepath.Rel(root(src), d)
							rel = filepath.ToSlash(rel)
							args = append(args, "--exclude=/"+rsyncLiteral(rel))
							shown = append(shown, rel)
							break
						}
					}
				}
				msg := fmt.Sprintf("%sMirror: USB Sync can't read %d source folder(s), so they were skipped and their copies on the destination were left as they are: %s", pairTag(pi), len(bad)+more, strings.Join(shown, ", "))
				if more > 0 {
					msg += fmt.Sprintf(" and %d more", more)
				}
				msg += ". Give USB Sync read access to sync them."
				logf("%s", msg)
				r.update(func() {
					run.Warn = append(run.Warn, msg)
					for _, x := range shown {
						if multi {
							x = fmt.Sprintf("Pair %d · %s", pi+1, x)
						}
						run.Excluded = append(run.Excluded, x+" (can't read)")
					}
				})
			}
		}
		if pp.pj.CopyMode == "contents" {
			logf("%sSync the contents of %s directly into %s", pairTag(pi), pp.srcs[0], pp.dest)
		} else {
			logf("%sCopy %s into %s as subfolder(s)", pairTag(pi), strings.Join(pp.srcs, ", "), pp.dest)
		}
		pps, err := planPasses(run, pp.pj, pp.srcs, pi*16, logf)
		if err != nil {
			fail("Could not prepare the date filter: " + err.Error())
			return
		}
		for k := range pps {
			pps[k].pair, pps[k].dest, pps[k].args, pps[k].prefix = pi, pp.dest, args, prefix
		}
		passes = append(passes, pps...)
	}
	r.update(func() { run.Passes = len(passes) })
	curPass := 0 // index into passes of the rsync running now

	var noSpace bool
	const maxChanged, maxUnchanged = 500000, 200000
	var storedChanged, storedUnchanged int
	worst := 0

	// Progress bookkeeping. doneBefore = bytes of files already finished in
	// this run; the current file's progress comes from rsync's progress lines
	// and/or from watching the size of rsync's temporary file at the
	// destination (some rsync builds don't report progress through a pipe).
	var passBase, lastPassBytes, doneBefore, lastB int64
	var passFilesBase, lastPassFiles, progressLines, timeErrors int
	lastT := time.Now()
	var copyStart time.Time
	recalc := func() { // call with r.mu held
		run.Bytes = doneBefore + run.CurDone
		if run.ToCopyBytes > 0 {
			run.Percent = int(min(run.Bytes*100/run.ToCopyBytes, 99))
		}
		now := time.Now()
		if dt := now.Sub(lastT).Seconds(); dt >= 0.8 {
			inst := float64(run.Bytes-lastB) / dt
			if run.Speed == 0 {
				run.Speed = inst
			} else {
				run.Speed = 0.7*run.Speed + 0.3*inst
			}
			lastT, lastB = now, run.Bytes
		}
		run.EtaSecs = -1
		if run.ToCopyBytes > 0 && run.Speed > 1 && now.Sub(copyStart) > 3*time.Second {
			run.EtaSecs = int(float64(max(run.ToCopyBytes-run.Bytes, 0)) / run.Speed)
		}
	}

	onLine := func(line string) {
		if m := progRe.FindStringSubmatch(line); m != nil {
			r.update(func() {
				progressLines++
				passBytes := atoi64(m[1])
				lastPassBytes = passBytes
				run.Rate, run.ETA = m[3], m[4]
				if m[5] != "" {
					x, _ := strconv.Atoi(m[5])
					run.FilesDone = max(run.FilesDone, passFilesBase+x)
					lastPassFiles = x
				}
				if m[6] != "" {
					run.ToCheck, _ = strconv.Atoi(m[6])
					run.Total, _ = strconv.Atoi(m[7])
				}
				if cur := passBase + passBytes - doneBefore; run.CurSize > 0 && cur > run.CurDone {
					run.CurDone = min(cur, run.CurSize)
				}
				if run.ToCopyBytes > 0 {
					recalc()
				} else {
					// no pre-scan (checksum mode): fall back to rsync's numbers
					run.Bytes = max(run.Bytes, passBase+passBytes)
					run.Percent, _ = strconv.Atoi(m[2])
				}
			})
			return
		}
		if m := itemRe.FindStringSubmatch(line); m != nil {
			status, reasons := classify(m[1])
			size, raw := atoi64(m[2]), m[3]
			// rsync reports deletions with size 0. In a preview the file is
			// still there, so look up its real size on the destination.
			if run.DryRun && m[1] == "*deleting" && !strings.HasSuffix(raw, "/") {
				if fi, err := os.Lstat(filepath.Join(passes[curPass].dest, raw)); err == nil && fi.Mode().IsRegular() {
					size = fi.Size()
				}
			}
			name := passes[curPass].prefix + raw
			store := true
			r.update(func() {
				run.Scanned++
				if run.DryRun {
					run.Current = name // show what is being checked
				}
				if status == "new" || status == "update" || status == "delete" {
					run.Recent = append(run.Recent, recentItem{status, name, size})
					if len(run.Recent) > 12 {
						run.Recent = run.Recent[len(run.Recent)-12:]
					}
				}
				switch status {
				case "new", "update":
					if status == "new" {
						run.NewFiles++
						run.NewBytes += size
					} else {
						run.Updated++
						run.UpdBytes += size
					}
					if run.CurSize > 0 {
						doneBefore += run.CurSize // previous file finished
						run.FilesDone++
					}
					run.Current, run.CurSize, run.CurDone = name, size, 0
					run.curRaw, run.curDest, run.curPass = raw, passes[curPass].dest, curPass
					if !run.DryRun {
						recalc()
					}
				case "delete":
					if !strings.HasSuffix(raw, "/") { // folders aren't counted as files
						run.Deleted++
						run.DelBytes += size
					}
				case "newdir":
					run.NewDirs++
				case "unchanged":
					run.Unchanged++
				}
				if status == "new" || status == "update" || status == "unchanged" {
					run.TotalBytes += size
				}
				if status == "unchanged" || status == "dir" {
					if storedUnchanged >= maxUnchanged {
						store, run.Truncated = false, true
					} else {
						storedUnchanged++
					}
				} else if storedChanged >= maxChanged {
					store, run.Truncated = false, true
				} else {
					storedChanged++
				}
			})
			if store {
				fmt.Fprintf(itemsW, "%s\t%s\t%d\t%s\n", status, strings.Join(reasons, ","), size, name)
			}
			if status != "unchanged" && status != "dir" {
				fmt.Fprintln(lf, line)
			}
			return
		}
		if m := statRe.FindStringSubmatch(line); m != nil {
			n := atoi64(m[2])
			r.update(func() {
				if strings.HasPrefix(m[1], "Number") {
					run.FilesXfer += n
				} else {
					run.BytesXfer += n
				}
			})
		} else if strings.Contains(line, "failed to set times on") {
			// harmless: content was copied, only the date couldn't be set
			r.update(func() { timeErrors++ })
		} else if strings.Contains(line, "skipping file deletion") {
			r.update(func() {
				w := "rsync skipped Mirror deletions because it couldn't read part of the source (see Errors). Nothing at the destination was deleted after that point."
				if len(run.Warn) == 0 || run.Warn[len(run.Warn)-1] != w {
					run.Warn = append(run.Warn, w)
				}
			})
		} else if strings.HasPrefix(line, "rsync error: some files/attrs were not transferred") {
			// summary line for code 23; judged from the individual errors instead
		} else if errRe.MatchString(line) {
			r.update(func() {
				run.ErrorCount++
				if len(run.Errors) < 200 {
					run.Errors = append(run.Errors, line)
				}
			})
		}
		if strings.Contains(line, "No space left on device") {
			noSpace = true
		}
		fmt.Fprintln(lf, line)
	}

	passArgs := func(p pass, extra ...string) []string {
		args := append(append([]string{}, p.args...), extra...)
		if p.listFile != "" {
			args = append(args, "--from0", "--files-from="+p.listFile)
		}
		args = append(args, p.srcs...)
		return append(args, strings.TrimSuffix(p.dest, "/")+"/")
	}

	// Finish files a previous run left half-copied, before anything else.
	if !run.DryRun && !run.cancelled {
		for _, pp := range pairs {
			r.resumePartials(run, rsyncBin, pp.pj, pp.srcs, pp.dest, logf)
		}
	}

	// Quick scan (a dry run) so progress can be measured against what
	// actually needs copying. Skipped for checksum mode, where it would read
	// every file twice.
	if !run.DryRun && compareMode(o) != "checksum" {
		r.update(func() { run.Phase = "scanning" })
		logf("Scanning to work out how much needs copying…")
		scanLine := func(line string) {
			if m := itemRe.FindStringSubmatch(line); m != nil {
				st, _ := classify(m[1])
				r.update(func() {
					run.Scanned++
					if st == "new" || st == "update" {
						run.ToCopyFiles++
						run.ToCopyBytes += atoi64(m[2])
					}
				})
			}
		}
		for _, p := range passes {
			if run.cancelled || (p.listFile != "" && p.count == 0) {
				continue
			}
			r.runRsync(run, rsyncBin, passArgs(p, "--dry-run"), logf, scanLine)
		}
		_ = dest
		logf("Scan: %d file(s), %s to copy", run.ToCopyFiles, humanBytes(run.ToCopyBytes))
	}
	r.update(func() { run.Phase, run.EtaSecs = "copying", -1 })
	copyStart = time.Now()
	stopWatch := make(chan struct{})
	if !run.DryRun {
		go r.watchTempFiles(run, stopWatch, recalc)
	}
	lastT = copyStart

	for i, p := range passes {
		if run.cancelled {
			break
		}
		if p.listFile != "" && p.count == 0 {
			continue
		}
		r.update(func() {
			curPass = i
			run.Pass = i + 1
			passBase += lastPassBytes
			passFilesBase += lastPassFiles
			lastPassBytes, lastPassFiles = 0, 0
		})
		code := r.runRsync(run, rsyncBin, passArgs(p), logf, onLine)
		if severity(code) > severity(worst) {
			worst = code
		}
	}
	close(stopWatch)
	if !run.DryRun && (run.cancelled || worst != 0) {
		r.mu.Lock()
		cur, cp := run.curRaw, run.curPass
		r.mu.Unlock()
		if cur != "" {
			pp := pairs[passes[cp].pair]
			recordPartial(pp.pj, pp.srcs, pp.dest, cur, logf)
		}
	}
	if !run.DryRun {
		logf("rsync sent %d live progress update(s)", progressLines)
	}

	r.update(func() {
		if timeErrors > 0 {
			run.Notes = append(run.Notes, fmt.Sprintf("%d file date(s) couldn't be set on the destination (files owned by another user). The files themselves were copied.", timeErrors))
			if worst == 23 && run.ErrorCount == 0 {
				worst = 0 // only date errors: the copy itself succeeded
			}
		}
		run.ExitCode = worst
		verb := map[bool][3]string{true: {"would be copied", "would be updated", "would be deleted"}, false: {"copied", "updated", "deleted"}}[run.DryRun]
		summary := fmt.Sprintf("%d new file(s) %s, %d %s, %d %s, %d unchanged.",
			run.NewFiles, verb[0], run.Updated, verb[1], run.Deleted, verb[2], run.Unchanged)
		switch {
		case run.cancelled:
			run.State = "cancelled"
			if run.Message == "" {
				run.Message = "Cancelled."
			}
		case worst == 0:
			run.State, run.Percent = "success", 100
			if run.ToCopyBytes > 0 {
				run.Bytes = run.ToCopyBytes
			}
			run.CurSize, run.CurDone = 0, 0
			run.Message = summary
		case worst == 24:
			run.State, run.Percent = "warning", 100
			run.Message = summary + " Some source files changed or vanished while syncing."
		case worst == 23:
			run.State = "warning"
			run.Message = summary + " Some files could not be copied — see Errors."
		default:
			run.State = "failed"
			t := exitText[worst]
			if t == "" {
				t = "unexpected error"
			}
			run.Message = fmt.Sprintf("rsync failed (code %d): %s.", worst, t)
		}
		if noSpace {
			run.Message += " The destination ran out of space."
		}
	})
	logf("Finished: %s (exit %d) — %s", run.State, worst, run.Message)
	log.Printf("run %s %s: %s", run.ID, run.State, run.Message)
}

func (r *Runner) runRsync(run *Run, bin string, args []string, logf func(string, ...any), onLine func(string)) int {
	full := append([]string{bin}, args...)
	if ionice, e := exec.LookPath("ionice"); e == nil {
		full = append([]string{ionice, "-c2", "-n7"}, full...)
	}
	if nice, e := exec.LookPath("nice"); e == nil {
		full = append([]string{nice, "-n", "10"}, full...)
	}
	logf("Command: %s", strings.Join(full, " "))

	cmd := exec.Command(full[0], full[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	pr, pw, err := os.Pipe()
	if err != nil {
		onLine("rsync: cannot create pipe: " + err.Error())
		return -1
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		onLine("rsync: could not start: " + err.Error())
		return -1
	}
	pw.Close()
	pid := cmd.Process.Pid
	r.update(func() {
		run.pgid = pid
		run.cancelFn = func() {
			syscall.Kill(-pid, syscall.SIGTERM)
			syscall.Kill(-pid, syscall.SIGCONT) // a paused rsync must wake up to exit
			go func() { time.Sleep(8 * time.Second); syscall.Kill(-pid, syscall.SIGKILL) }()
		}
		if run.cancelled {
			run.cancelFn()
		} else if run.Paused {
			syscall.Kill(-pid, syscall.SIGSTOP) // paused between passes: stay paused
		}
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		sc.Split(splitCRLF)
		for sc.Scan() {
			if line := sc.Text(); strings.TrimSpace(line) != "" {
				onLine(line)
			}
		}
		io.Copy(io.Discard, pr)
	}()
	werr := cmd.Wait()
	<-done
	pr.Close()
	if werr == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		if c := ee.ExitCode(); c >= 0 {
			return c
		}
		return 20
	}
	return -1
}

// ---------- handlers ----------

func handleRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JobID  string `json:"jobId"`
		Job    *Job   `json:"job"`
		DryRun bool   `json:"dryRun"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, 400, "bad request")
		return
	}
	var j Job
	if req.JobID != "" {
		var ok bool
		if j, ok = findJob(req.JobID); !ok {
			errJSON(w, 404, "job not found")
			return
		}
	} else if req.Job != nil {
		j = *req.Job
		keepID := j.ID
		if keepID != "" {
			if _, ok := findJob(keepID); !ok {
				keepID = ""
			}
		}
		if err := validateJob(&j); err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		j.ID = keepID
	} else {
		errJSON(w, 400, "nothing to run")
		return
	}
	run, err := runner.enqueue(&j, req.DryRun, "manual")
	if err != nil {
		errJSON(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, lite(run))
}

func handleCancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RunID string `json:"runId"`
	}
	readJSON(r, &req)
	if !runner.cancel(req.RunID) {
		errJSON(w, 404, "run not found")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(runner.statusJSON())
}

func handleRunDetail(w http.ResponseWriter, r *http.Request) {
	run := runner.find(r.PathValue("id"))
	if run == nil {
		errJSON(w, 404, "run not found")
		return
	}
	writeJSON(w, 200, run)
}

func handleLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		errJSON(w, 400, "bad id")
		return
	}
	f, err := os.Open(runFile(id, ".log"))
	if err != nil {
		errJSON(w, 404, "log not found")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	const max = 1 << 20
	if fi, err := f.Stat(); err == nil && fi.Size() > max {
		f.Seek(fi.Size()-max, io.SeekStart)
		fmt.Fprintf(w, "… showing the last 1 MB of a %.1f MB log …\n", float64(fi.Size())/(1<<20))
	}
	io.Copy(w, f)
}

func humanBytes(n int64) string {
	f, u := float64(n), []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for f >= 1024 && i < len(u)-1 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", f, u[i])
}

var (
	rsyncVerOnce  sync.Once
	rsyncMaj      int
	rsyncMin      int
	rsyncVersionR = regexp.MustCompile(`version\s+(\d+)\.(\d+)`)
)

func rsyncAtLeast(bin string, maj, mn int) bool {
	rsyncVerOnce.Do(func() {
		out, _ := exec.Command(bin, "--version").Output()
		if m := rsyncVersionR.FindSubmatch(out); m != nil {
			rsyncMaj, _ = strconv.Atoi(string(m[1]))
			rsyncMin, _ = strconv.Atoi(string(m[2]))
		}
	})
	return rsyncMaj > maj || (rsyncMaj == maj && rsyncMin >= mn)
}

// watchTempFiles checks once a second how much of the current file has been
// written, by looking at rsync's temporary file (".name.XXXXXX" next to the
// final destination). Once found, only that one file is stat'ed each second;
// the folder is listed again only when a new file starts. This keeps progress
// moving even if rsync doesn't report it through the pipe.
func (r *Runner) watchTempFiles(run *Run, stop chan struct{}, recalc func()) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var curName, tmpPath string
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		r.mu.Lock()
		shown, name, dest, size := run.Current, run.curRaw, run.curDest, run.CurSize
		r.mu.Unlock()
		if name == "" || dest == "" || size == 0 {
			continue
		}
		if dest+"/"+name != curName {
			curName, tmpPath = dest+"/"+name, ""
		}
		var written int64 = -1
		if tmpPath != "" {
			if fi, err := os.Stat(tmpPath); err == nil {
				written = fi.Size()
			} else {
				tmpPath = "" // renamed into place or gone
			}
		}
		if tmpPath == "" {
			target := filepath.Join(dest, name)
			dir, prefix := filepath.Dir(target), "."+filepath.Base(target)+"."
			if ents, err := os.ReadDir(dir); err == nil {
				for _, e := range ents {
					n := e.Name()
					if !e.IsDir() && strings.HasPrefix(n, prefix) && len(n) == len(prefix)+6 {
						if fi, err := e.Info(); err == nil && fi.Size() > written {
							written, tmpPath = fi.Size(), filepath.Join(dir, n)
						}
					}
				}
			}
		}
		if written < 0 {
			continue
		}
		r.update(func() {
			if run.Current == shown && written > run.CurDone {
				run.CurDone = min(written, run.CurSize)
				recalc()
			}
		})
	}
}

// canSetTimes checks whether this process may set modification times in dir
// (falls back to the drive root when dir doesn't exist yet). Writes and removes
// a tiny hidden probe file.
func canSetTimes(dir, root string) bool {
	if os.Getenv("USBSYNC_TEST_NOTIMES") == "1" {
		return false
	}
	if _, err := os.Stat(dir); err != nil {
		dir = root
	}
	f, err := os.CreateTemp(dir, ".usbsync-probe-")
	if err != nil {
		return true // can't tell; let rsync try
	}
	name := f.Name()
	f.Close()
	defer os.Remove(name)
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(name, old, old); err != nil {
		return false
	}
	fi, err := os.Stat(name)
	return err == nil && fi.ModTime().Sub(old).Abs() < 3*time.Second
}

// handleRerun queues a past run again with exactly the settings it used.
func handleRerun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DryRun bool `json:"dryRun"`
	}
	readJSON(r, &req)
	old := runner.find(r.PathValue("id"))
	if old == nil {
		errJSON(w, 404, "run not found")
		return
	}
	if old.Spec == nil {
		errJSON(w, 400, "This run was made by an older version and can't be re-run. Set it up again on the Sync tab.")
		return
	}
	j := *old.Spec
	if j.ID != "" {
		if _, ok := findJob(j.ID); !ok {
			j.ID = "" // saved job was deleted since; run it as a one-off
		}
	}
	keep := j.ID
	if err := validateJob(&j); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	j.ID = keep
	run, err := runner.enqueue(&j, req.DryRun, "rerun")
	if err != nil {
		errJSON(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, lite(run))
}

// setPaused freezes (SIGSTOP) or resumes (SIGCONT) a running sync's rsync
// processes. A paused run keeps its place: resuming continues mid-file.
func (r *Runner) setPaused(id string, pause bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.active {
		if a.ID != id {
			continue
		}
		if a.cancelled {
			return errors.New("This sync is being cancelled.")
		}
		if a.pgid > 0 {
			sig := syscall.SIGCONT
			if pause {
				sig = syscall.SIGSTOP
			}
			if err := syscall.Kill(-a.pgid, sig); err != nil && err != syscall.ESRCH {
				return err
			}
		}
		a.Paused = pause
		if f, err := os.OpenFile(runFile(a.ID, ".log"), os.O_APPEND|os.O_WRONLY, 0); err == nil {
			what := "Resumed"
			if pause {
				what = "Paused"
			}
			fmt.Fprintf(f, "[%s] %s by user\n", time.Now().Format("2006-01-02 15:04:05"), what)
			f.Close()
		}
		if pause {
			a.Speed, a.EtaSecs = 0, -1
		}
		return nil
	}
	return errors.New("That sync isn't running.")
}

func handlePause(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RunID string `json:"runId"`
		Pause bool   `json:"pause"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, 400, "bad request")
		return
	}
	if err := runner.setPaused(req.RunID, req.Pause); err != nil {
		errJSON(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "paused": req.Pause})
}

type skipPat struct{ pattern, display string }

// rsyncLiteral escapes a path so rsync matches it literally. Backslashes are
// only treated as escapes when the pattern contains a wildcard.
func rsyncLiteral(p string) string {
	if !strings.ContainsAny(p, "*?[") {
		return p
	}
	var b strings.Builder
	for _, c := range p {
		if strings.ContainsRune(`\*?[`, c) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// skipPatterns turns a job's unticked items into anchored rsync excludes.
// rsync's transfer root is the source's parent when the folder itself is
// copied ("into"), or the source folder itself for "contents".
func skipPatterns(j *Job, srcs []string, drive *Drive) []skipPat {
	var out []skipPat
	for _, k := range j.Skip {
		abs := k.Path
		if k.Side == "usb" {
			abs = filepath.Join(drive.Mount, k.Path)
		}
		for _, s := range srcs {
			if abs == s || !underOrEqual(abs, s) {
				continue
			}
			root := filepath.Dir(s)
			if j.CopyMode == "contents" {
				root = s
			}
			rel, err := filepath.Rel(root, abs)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			rel = filepath.ToSlash(rel)
			out = append(out, skipPat{pattern: "/" + rsyncLiteral(rel), display: rel})
			break
		}
	}
	return out
}

// unreadableDirs walks a pair's source folders and returns the sub-folders
// this app's user can't open (at most 200, plus how many more there were).
// Synology system folders and unticked items are skipped, like rsync does.
func unreadableDirs(pp pairPaths, drive *Drive, skipSystem bool) ([]string, int) {
	skip := map[string]bool{}
	for _, k := range pp.pj.Skip {
		if k.Side == "usb" {
			skip[filepath.Join(drive.Mount, k.Path)] = true
		} else {
			skip[filepath.Clean(k.Path)] = true
		}
	}
	var bad []string
	more := 0
	for _, src := range pp.srcs {
		filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if d != nil && !d.IsDir() {
				return nil
			}
			if p != src {
				if skip[p] || (skipSystem && (hiddenDir(filepath.Base(p)) || filepath.Base(p) == ".SynologyWorkingDirectory")) {
					return fs.SkipDir
				}
			}
			if err != nil || (d != nil && d.IsDir() && !canRead(p)) {
				if p != src {
					if len(bad) < 200 {
						bad = append(bad, p)
					} else {
						more++
					}
				}
				return fs.SkipDir
			}
			return nil
		})
	}
	return bad, more
}
