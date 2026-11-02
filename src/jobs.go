package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Pair is one source → destination folder pair of a job.
type Pair struct {
	Sources  []PathRef `json:"sources"`
	Skip     []PathRef `json:"skip,omitempty"`
	Dest     PathRef   `json:"dest"`
	CopyMode string    `json:"copyMode"`
}

// pairJob returns a copy of the job that describes just pair i, so the
// single-pair logic (paths, excludes, copy mode) can be reused per pair.
func (j *Job) pairJob(i int) *Job {
	c := *j
	p := j.Pairs[i]
	c.Pairs = nil
	c.Sources, c.Skip, c.Dest, c.CopyMode = p.Sources, p.Skip, p.Dest, p.CopyMode
	return &c
}

// ensurePairs fills Pairs from the single-pair fields for jobs saved before
// pairs existed.
func (j *Job) ensurePairs() {
	if len(j.Pairs) == 0 && (len(j.Sources) > 0 || j.Dest.Path != "") {
		j.Pairs = []Pair{{Sources: j.Sources, Skip: j.Skip, Dest: j.Dest, CopyMode: j.CopyMode}}
	}
}

type PathRef struct {
	Side string `json:"side"` // "nas" (absolute path) or "usb" (path relative to the drive root)
	Path string `json:"path"`
}

type ModFilter struct {
	Mode  string `json:"mode"` // "" | any | days | since
	Days  int    `json:"days"`
	Since string `json:"since"` // YYYY-MM-DD
}

type Options struct {
	// Compare decides when a file counts as changed:
	// quick (size + modified date), size, checksum, name (skip existing), always
	Compare      string    `json:"compare"`
	Update       bool      `json:"update"`       // skip files newer at destination
	ExistingOnly bool      `json:"existingOnly"` // only update files that already exist
	MinSize      string    `json:"minSize"`      // e.g. "10M"
	MaxSize      string    `json:"maxSize"`
	Modified     ModFilter `json:"modified"`
	ModifyWindow *int      `json:"modifyWindow"` // nil = automatic
	Delete       bool      `json:"delete"`
	SkipSystem   bool      `json:"skipSystem"`
	Excludes     []string  `json:"excludes"`
	Notify       bool      `json:"notify,omitempty"`   // notify when a hand-started run finishes
	Checksum     bool      `json:"checksum,omitempty"` // legacy (1.0)
}

var sizeRe = regexp.MustCompile(`^\d+(\.\d+)?[KMG]$`)

// normalizeOptions upgrades options saved by older versions.
func normalizeOptions(o *Options) {
	if o.Compare == "" {
		o.Compare = "quick"
		if o.Checksum {
			o.Compare = "checksum"
		}
	}
	o.Checksum = false
	if o.Modified.Mode == "" {
		o.Modified.Mode = "any"
	}
}

func validateOptions(o *Options) error {
	normalizeOptions(o)
	switch o.Compare {
	case "quick", "size", "checksum", "name", "always":
	default:
		return errors.New("Invalid comparison mode.")
	}
	if o.Compare == "name" && o.ExistingOnly {
		return errors.New("“File name only” skips existing files and “update existing only” skips new ones — together nothing would be copied.")
	}
	o.MinSize, o.MaxSize = strings.ToUpper(strings.TrimSpace(o.MinSize)), strings.ToUpper(strings.TrimSpace(o.MaxSize))
	for _, v := range []string{o.MinSize, o.MaxSize} {
		if v != "" && !sizeRe.MatchString(v) {
			return errors.New("Size limits must look like 500K, 10M or 2G.")
		}
	}
	switch o.Modified.Mode {
	case "any":
	case "days":
		if o.Modified.Days < 1 || o.Modified.Days > 36500 {
			return errors.New("Enter a number of days between 1 and 36500.")
		}
	case "since":
		if _, err := time.Parse("2006-01-02", o.Modified.Since); err != nil {
			return errors.New("Enter the “modified since” date as YYYY-MM-DD.")
		}
	default:
		return errors.New("Invalid date filter.")
	}
	if o.Modified.Mode != "any" && o.Delete {
		return errors.New("Mirror (delete) can't be combined with a date filter — it would delete every older file at the destination.")
	}
	if o.ModifyWindow != nil && (*o.ModifyWindow < 0 || *o.ModifyWindow > 86400) {
		return errors.New("Time tolerance must be 0–86400 seconds.")
	}
	return nil
}

type Schedule struct {
	Mode    string `json:"mode"` // none | daily | weekly
	Time    string `json:"time"` // HH:MM
	Weekday int    `json:"weekday"`
}

type Job struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Direction string `json:"direction"` // to_usb | to_nas
	// CopyMode: "into" copies each selected folder as a subfolder of the
	// destination (dest/TV); "contents" syncs the folder's contents straight
	// into the destination (TV -> TV).
	CopyMode   string `json:"copyMode"`
	DriveID    string `json:"driveId"`
	DriveName  string `json:"driveName"`
	DriveMount string `json:"driveMount"`
	// Pairs: the folder pairs this job syncs, in order. Sources/Skip/Dest/
	// CopyMode below always mirror pair 1 (older versions read those).
	Pairs   []Pair    `json:"pairs,omitempty"`
	Sources []PathRef `json:"sources"`
	// Skip: items unticked inside a selected source folder (same side as
	// the sources). They are excluded from the sync.
	Skip     []PathRef `json:"skip,omitempty"`
	Dest     PathRef   `json:"dest"`
	Options  Options   `json:"options"`
	Schedule Schedule  `json:"schedule"`
	// AutoRun: start this job when its drive is plugged in (if enabled in Settings)
	AutoRun bool      `json:"autoRun,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

var (
	storeMu sync.Mutex
	jobs    = []*Job{}
	timeRe  = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)
	idRe    = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
)

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadStore() {
	b, err := os.ReadFile(filepath.Join(dataDir, "jobs.json"))
	if err != nil {
		return
	}
	if err := json.Unmarshal(b, &jobs); err != nil {
		log.Printf("jobs.json unreadable: %v", err)
		jobs = []*Job{}
	}
	for _, j := range jobs {
		normalizeOptions(&j.Options)
		if j.CopyMode == "" {
			j.CopyMode = "into"
		}
		j.ensurePairs()
	}
}

func saveStoreLocked() error {
	b, _ := json.MarshalIndent(jobs, "", "  ")
	return writeFileAtomic(filepath.Join(dataDir, "jobs.json"), b)
}

func snapshotJobs() []Job {
	storeMu.Lock()
	defer storeMu.Unlock()
	out := make([]Job, len(jobs))
	for i, j := range jobs {
		out[i] = *j
	}
	return out
}

func findJob(id string) (Job, bool) {
	storeMu.Lock()
	defer storeMu.Unlock()
	for _, j := range jobs {
		if j.ID == id {
			return *j, true
		}
	}
	return Job{}, false
}

func normUSB(p string) string {
	return filepath.Clean("/" + strings.TrimSpace(p))
}

// validatePair checks and normalizes one pair (given as a single-pair job).
func validatePair(j *Job) error {
	srcSide, dstSide := "nas", "usb"
	if j.Direction == "to_nas" {
		srcSide, dstSide = "usb", "nas"
	}
	if len(j.Sources) == 0 {
		return errors.New("Select at least one source folder.")
	}
	if len(j.Sources) > 100 {
		return errors.New("Too many source folders (max 100).")
	}
	seen := map[string]bool{}
	clean := []PathRef{}
	for _, s := range j.Sources {
		if s.Side != srcSide || strings.TrimSpace(s.Path) == "" {
			return errors.New("Source folders don't match the sync direction.")
		}
		if s.Side == "usb" {
			s.Path = normUSB(s.Path)
		} else {
			if !filepath.IsAbs(s.Path) {
				return errors.New("Invalid NAS path.")
			}
			s.Path = filepath.Clean(s.Path)
		}
		if !seen[s.Path] {
			seen[s.Path] = true
			clean = append(clean, s)
		}
	}
	j.Sources = clean
	if j.CopyMode == "" {
		j.CopyMode = "into"
	}
	if j.CopyMode != "into" && j.CopyMode != "contents" {
		return errors.New("Invalid copy mode.")
	}
	if err := validateSkip(j, srcSide); err != nil {
		return err
	}
	if j.CopyMode == "contents" && len(j.Sources) != 1 {
		return errors.New("“Sync contents” works with exactly one source folder.")
	}
	if j.Dest.Side != dstSide || strings.TrimSpace(j.Dest.Path) == "" {
		return errors.New("Select a destination folder.")
	}
	if j.Dest.Side == "usb" {
		j.Dest.Path = normUSB(j.Dest.Path)
	} else {
		if !filepath.IsAbs(j.Dest.Path) {
			return errors.New("Invalid NAS path.")
		}
		j.Dest.Path = filepath.Clean(j.Dest.Path)
	}
	return nil
}

func validateJob(j *Job) error {
	j.Name = strings.TrimSpace(j.Name)
	if len(j.Name) > 100 {
		j.Name = j.Name[:100]
	}
	switch j.Direction {
	case "to_usb", "to_nas":
	default:
		return errors.New("Choose a sync direction.")
	}
	if j.DriveID == "" {
		return errors.New("Choose an external drive.")
	}
	j.ensurePairs()
	if len(j.Pairs) == 0 {
		return errors.New("Select at least one source folder.")
	}
	if len(j.Pairs) > 50 {
		return errors.New("Too many folder pairs (max 50).")
	}
	for i := range j.Pairs {
		pj := j.pairJob(i)
		if err := validatePair(pj); err != nil {
			if len(j.Pairs) > 1 {
				return fmt.Errorf("Pair %d: %s", i+1, err.Error())
			}
			return err
		}
		j.Pairs[i] = Pair{Sources: pj.Sources, Skip: pj.Skip, Dest: pj.Dest, CopyMode: pj.CopyMode}
	}
	p0 := j.Pairs[0]
	j.Sources, j.Skip, j.Dest, j.CopyMode = p0.Sources, p0.Skip, p0.Dest, p0.CopyMode
	if j.Schedule.Mode == "" {
		j.Schedule.Mode = "none"
	}
	switch j.Schedule.Mode {
	case "none":
	case "daily", "weekly":
		if !timeRe.MatchString(j.Schedule.Time) {
			return errors.New("Enter a schedule time as HH:MM.")
		}
		if j.Schedule.Weekday < 0 || j.Schedule.Weekday > 6 {
			return errors.New("Invalid weekday.")
		}
	default:
		return errors.New("Invalid schedule.")
	}
	if err := validateOptions(&j.Options); err != nil {
		return err
	}
	var ex []string
	for _, e := range j.Options.Excludes {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if strings.ContainsAny(e, "\n\r\x00") || len(e) > 255 {
			return errors.New("Invalid exclude pattern.")
		}
		ex = append(ex, e)
	}
	if len(ex) > 50 {
		return errors.New("Too many exclude patterns (max 50).")
	}
	j.Options.Excludes = ex
	if j.Name == "" {
		base := func(r PathRef) string {
			b := filepath.Base(r.Path)
			if b == "/" {
				return j.DriveName
			}
			return b
		}
		j.Name = base(j.Sources[0])
		if len(j.Sources) > 1 {
			j.Name += fmt.Sprintf(" +%d", len(j.Sources)-1)
		}
		j.Name += " → " + base(j.Dest)
		if len(j.Pairs) > 1 {
			j.Name += fmt.Sprintf(" (+%d more)", len(j.Pairs)-1)
		}
	}
	return checkPairTargets(j)
}

// checkPairTargets refuses jobs where Mirror would let one pair delete what
// another pair copies (their target folders are the same or nested).
func checkPairTargets(j *Job) error {
	if !j.Options.Delete || len(j.Pairs) < 2 {
		return nil
	}
	type tgt struct {
		pair int
		path string
	}
	var ts []tgt
	for i, p := range j.Pairs {
		d := p.Dest.Path
		if p.CopyMode == "contents" {
			ts = append(ts, tgt{i, d})
			continue
		}
		for _, s := range p.Sources {
			ts = append(ts, tgt{i, filepath.Join(d, filepath.Base(s.Path))})
		}
	}
	for a := range ts {
		for b := range ts {
			if ts[a].pair < ts[b].pair && (underOrEqual(ts[a].path, ts[b].path) || underOrEqual(ts[b].path, ts[a].path)) {
				return fmt.Errorf("With Mirror on, pair %d and pair %d would write into the same folder (%s), so one could delete what the other copies. Use different destination folders or turn Mirror off.", ts[a].pair+1, ts[b].pair+1, ts[b].path)
			}
		}
	}
	return nil
}

// resolveJob turns a job into absolute source and destination paths,
// locating the external drive by UUID (mount points can change).
func resolveJob(j *Job) ([]string, string, *Drive, error) {
	var drive *Drive
	drives := listDrives()
	migrateDriveIDs(drives)
	if fresh, ok := findJob(j.ID); ok && j.ID != "" && strings.HasPrefix(j.DriveID, "mnt:") {
		j.DriveID = fresh.DriveID
	}
	for i := range drives {
		if driveIsJobs(drives[i], j) {
			drive = &drives[i]
			break
		}
	}
	if drive == nil {
		name := j.DriveName
		if name == "" {
			name = j.DriveMount
		}
		return nil, "", nil, fmt.Errorf("External drive %q is not connected.", name)
	}
	abs := func(r PathRef) string {
		if r.Side == "usb" {
			return filepath.Join(drive.Mount, r.Path)
		}
		return filepath.Clean(r.Path)
	}
	var srcs []string
	for _, s := range j.Sources {
		srcs = append(srcs, abs(s))
	}
	return srcs, abs(j.Dest), drive, nil
}

// ---------- handlers ----------

type jobView struct {
	Job
	DriveConnected bool `json:"driveConnected"`
}

func handleJobs(w http.ResponseWriter, r *http.Request) {
	drives := listDrives()
	migrateDriveIDs(drives)
	out := []jobView{}
	for _, j := range snapshotJobs() {
		connected := false
		for _, d := range drives {
			if driveIsJobs(d, &j) {
				connected = true
			}
		}
		out = append(out, jobView{j, connected})
	}
	writeJSON(w, 200, out)
}

func handleSaveJob(w http.ResponseWriter, r *http.Request) {
	var j Job
	if err := readJSON(r, &j); err != nil {
		errJSON(w, 400, "bad request")
		return
	}
	if err := validateJob(&j); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	now := time.Now()
	j.Updated = now
	if j.ID != "" {
		for i, old := range jobs {
			if old.ID == j.ID {
				j.Created = old.Created
				jobs[i] = &j
				if err := saveStoreLocked(); err != nil {
					errJSON(w, 500, err.Error())
					return
				}
				writeJSON(w, 200, j)
				return
			}
		}
	}
	j.ID = randHex(6)
	j.Created = now
	jobs = append(jobs, &j)
	if err := saveStoreLocked(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, j)
}

func handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	storeMu.Lock()
	defer storeMu.Unlock()
	for i, j := range jobs {
		if j.ID == id {
			jobs = append(jobs[:i], jobs[i+1:]...)
			saveStoreLocked()
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
	}
	errJSON(w, 404, "job not found")
}

// ---------- scheduler ----------

func scheduler() {
	fired := map[string]string{}
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		key := now.Format("2006-01-02 15:04")
		hm := now.Format("15:04")
		for _, j := range snapshotJobs() {
			s := j.Schedule
			if (s.Mode != "daily" && s.Mode != "weekly") || s.Time != hm {
				continue
			}
			if s.Mode == "weekly" && int(now.Weekday()) != s.Weekday {
				continue
			}
			if fired[j.ID] == key {
				continue
			}
			fired[j.ID] = key
			jc := j
			if _, err := runner.enqueue(&jc, false, "schedule"); err != nil {
				log.Printf("scheduled job %s not queued: %v", j.Name, err)
			}
		}
	}
}

// validateSkip keeps only unticked items that lie strictly inside one of the
// selected source folders.
func validateSkip(j *Job, side string) error {
	if len(j.Skip) > 2000 {
		return errors.New("Too many unticked items (max 2000).")
	}
	norm := func(p string) string {
		if side == "usb" {
			return normUSB(p)
		}
		return filepath.Clean(p)
	}
	seen := map[string]bool{}
	var out []PathRef
	for _, k := range j.Skip {
		if k.Side != side || strings.TrimSpace(k.Path) == "" || (side == "nas" && !filepath.IsAbs(k.Path)) {
			continue
		}
		k.Path = norm(k.Path)
		inside := false
		for _, s := range j.Sources {
			if k.Path != s.Path && underOrEqual(k.Path, s.Path) {
				inside = true
				break
			}
		}
		if inside && !seen[k.Path] {
			seen[k.Path] = true
			out = append(out, k)
		}
	}
	j.Skip = out
	return nil
}

// migrateDriveIDs upgrades jobs saved with an old slot-based drive ID
// ("mnt:/volumeUSB1/usbshare") to the drive's hardware ID — but only when the
// drive now in that slot has the same name as the one the job was saved
// with. A different drive in the same slot is never matched.
func migrateDriveIDs(drives []Drive) {
	storeMu.Lock()
	defer storeMu.Unlock()
	changed := false
	for _, j := range jobs {
		if !strings.HasPrefix(j.DriveID, "mnt:") {
			continue
		}
		for _, d := range drives {
			if strings.HasPrefix(d.ID, "mnt:") || "mnt:"+d.Mount != j.DriveID {
				continue
			}
			if j.DriveName != "" && j.DriveName == d.Name {
				log.Printf("job %q: drive ID %s -> %s", j.Name, j.DriveID, d.ID)
				j.DriveID, j.DriveMount = d.ID, d.Mount
				changed = true
			}
		}
	}
	if changed {
		saveStoreLocked()
	}
}

// driveIsJobs reports whether drive d is the drive job j was saved for. With
// the last-resort slot ID ("mnt:…") the drive's name must match as well.
func driveIsJobs(d Drive, j *Job) bool {
	if d.ID != j.DriveID {
		return false
	}
	if strings.HasPrefix(d.ID, "mnt:") && j.DriveName != "" && j.DriveName != d.Name {
		return false
	}
	return true
}
