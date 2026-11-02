package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Resuming big files: rsync runs with --partial-dir, so a file that was being
// copied when a sync was interrupted (Cancel, package stop, NAS restart) is
// kept as <dest dir>/.usbsync-partial/<name>. The next sync to that
// destination moves it into place and lets rsync append the rest
// (--append-verify), then verifies the whole file; on a mismatch rsync copies
// the file again in full.

const partialDirName = ".usbsync-partial"

type partialEntry struct {
	Src     string    `json:"src"`
	Dest    string    `json:"dest"`
	Partial string    `json:"partial"`
	When    time.Time `json:"when"`
}

var resumeMu sync.Mutex

func resumeFile() string { return filepath.Join(dataDir, "resume.json") }

func loadPartials() map[string]partialEntry {
	m := map[string]partialEntry{}
	if b, err := os.ReadFile(resumeFile()); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func savePartials(m map[string]partialEntry) {
	b, _ := json.MarshalIndent(m, "", "  ")
	writeFileAtomic(resumeFile(), b)
}

// srcForName maps a path as rsync reports it (relative to the transfer root)
// back to the source file.
func srcForName(j *Job, srcs []string, name string) string {
	if j.CopyMode == "contents" && len(srcs) == 1 {
		return filepath.Join(srcs[0], name)
	}
	first := strings.SplitN(name, "/", 2)[0]
	for _, s := range srcs {
		if filepath.Base(s) == first {
			return filepath.Join(filepath.Dir(s), name)
		}
	}
	return ""
}

// recordPartial notes the file an interrupted sync was copying, if rsync
// kept its partial data.
func recordPartial(j *Job, srcs []string, dest, name string, logf func(string, ...any)) {
	src := srcForName(j, srcs, name)
	if src == "" {
		return
	}
	destFile := filepath.Join(dest, name)
	cands := []string{filepath.Join(filepath.Dir(destFile), partialDirName, filepath.Base(destFile)), destFile}
	sfi, err := os.Stat(src)
	if err != nil {
		return
	}
	for i, p := range cands {
		fi, err := os.Stat(p)
		if err != nil || fi.Size() == 0 || fi.Size() >= sfi.Size() {
			continue
		}
		if i == 1 && fi.ModTime().Equal(sfi.ModTime()) {
			continue // a finished older copy, not a partial
		}
		resumeMu.Lock()
		m := loadPartials()
		m[destFile] = partialEntry{Src: src, Dest: destFile, Partial: p, When: time.Now()}
		savePartials(m)
		resumeMu.Unlock()
		logf("Kept %s of %s (%s) to resume next time", humanBytes(fi.Size()), name, humanBytes(sfi.Size()))
		return
	}
}

func (r *Runner) resumePartials(run *Run, rsyncBin string, pj *Job, srcs []string, dest string, logf func(string, ...any)) {
	resumeMu.Lock()
	m := loadPartials()
	var todo []partialEntry
	for k, e := range m {
		if !underOrEqual(e.Dest, dest) {
			continue
		}
		inSrc := false
		for _, s := range srcs {
			if underOrEqual(e.Src, s) {
				inSrc = true
			}
		}
		if !inSrc {
			continue
		}
		todo = append(todo, e)
		delete(m, k)
	}
	if len(todo) > 0 {
		savePartials(m)
	}
	resumeMu.Unlock()

	for _, e := range todo {
		if run.cancelled {
			return
		}
		sfi, err1 := os.Stat(e.Src)
		pfi, err2 := os.Stat(e.Partial)
		if err1 != nil || err2 != nil || pfi.Size() >= sfi.Size() {
			continue
		}
		if e.Partial != e.Dest {
			os.MkdirAll(filepath.Dir(e.Dest), 0o777)
			if err := os.Rename(e.Partial, e.Dest); err != nil {
				logf("Couldn't resume %s: %v", e.Dest, err)
				continue
			}
			os.Remove(filepath.Dir(e.Partial)) // only succeeds if now empty
		}
		name, _ := filepath.Rel(dest, e.Dest)
		done := pfi.Size()
		logf("Resuming %s: %s of %s already copied", name, humanBytes(done), humanBytes(sfi.Size()))
		r.update(func() {
			run.Phase, run.Current, run.CurSize, run.CurDone = "resuming", filepath.ToSlash(name), sfi.Size(), done
			run.Percent = int(done * 100 / sfi.Size())
		})
		stop := make(chan struct{})
		go func() { // progress: watch the destination file grow
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					if fi, err := os.Stat(e.Dest); err == nil {
						r.update(func() {
							run.CurDone = min(fi.Size(), sfi.Size())
							run.Percent = int(run.CurDone * 100 / sfi.Size())
						})
					}
				}
			}
		}()
		args := []string{"-t", "--append-verify", "--partial", "--info=progress2"} // --partial-dir is not allowed with --append
		if rsyncAtLeast(rsyncBin, 3, 1) {
			args = append(args, "--outbuf=N")
		}
		args = append(args, e.Src, filepath.Dir(e.Dest)+"/")
		code := r.runRsync(run, rsyncBin, args, logf, func(line string) {
			if !progRe.MatchString(line) {
				logf("%s", line)
			}
		})
		close(stop)
		if code == 0 {
			logf("Resumed %s — saved re-copying %s", name, humanBytes(done))
			r.update(func() { run.Notes = append(run.Notes, fmt.Sprintf("Resumed %s from %s.", name, humanBytes(done))) })
		} else if !run.cancelled {
			logf("Resuming %s failed (rsync code %d); it will be copied in full", name, code)
		} else {
			recordPartial(pj, srcs, dest, filepath.ToSlash(name), logf)
		}
	}
	r.update(func() { run.Current, run.CurSize, run.CurDone, run.Percent = "", 0, 0, 0 })
}
