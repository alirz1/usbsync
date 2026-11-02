package main

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Run jobs when their drive is plugged in. A drive that is already attached
// when the app starts (e.g. after a NAS reboot) does not count as plugged in.

type pendingStart struct {
	ID        string    `json:"id"`
	JobID     string    `json:"jobId"`
	JobName   string    `json:"jobName"`
	DriveID   string    `json:"driveId"`
	DriveName string    `json:"driveName"`
	StartAt   time.Time `json:"startAt"`
	cancel    chan struct{}
}

var (
	autoMu  sync.Mutex
	pending = map[string]*pendingStart{}
)

func pendingList() []*pendingStart {
	autoMu.Lock()
	defer autoMu.Unlock()
	out := []*pendingStart{}
	for _, p := range pending {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartAt.Before(out[j].StartAt) })
	return out
}

func cancelPending(id string) bool {
	autoMu.Lock()
	defer autoMu.Unlock()
	if p := pending[id]; p != nil {
		close(p.cancel)
		delete(pending, id)
		log.Printf("auto-start of %q cancelled", p.JobName)
		return true
	}
	return false
}

func cancelPendingForDrive(driveID string) {
	autoMu.Lock()
	defer autoMu.Unlock()
	for id, p := range pending {
		if p.DriveID == driveID {
			close(p.cancel)
			delete(pending, id)
			log.Printf("auto-start of %q cancelled: drive removed", p.JobName)
		}
	}
}

func driveWatcher() {
	seen := map[string]bool{}
	for _, d := range listDrives() {
		seen[d.ID] = true // already attached at startup: not "plugged in"
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		now := map[string]Drive{}
		list := listDrives()
		migrateDriveIDs(list)
		for _, d := range list {
			now[d.ID] = d
		}
		for id := range seen {
			if _, ok := now[id]; !ok {
				delete(seen, id)
				cancelPendingForDrive(id)
			}
		}
		for id, d := range now {
			if seen[id] {
				continue
			}
			seen[id] = true
			log.Printf("drive plugged in: %s (%s)", d.Name, d.Mount)
			onDrivePlugged(d)
		}
	}
}

func onDrivePlugged(d Drive) {
	if getSettings().AutoRunOff {
		return
	}
	var due []Job
	for _, j := range snapshotJobs() {
		if j.AutoRun && driveIsJobs(d, &j) {
			due = append(due, j)
		}
	}
	if len(due) == 0 {
		return
	}
	delay := autoRunDelaySecs()
	startAt := time.Now().Add(time.Duration(delay) * time.Second)
	var names []string
	for _, j := range due {
		p := &pendingStart{ID: "auto-" + randHex(4), JobID: j.ID, JobName: j.Name, DriveID: d.ID, DriveName: d.Name, StartAt: startAt, cancel: make(chan struct{})}
		autoMu.Lock()
		pending[p.ID] = p
		autoMu.Unlock()
		names = append(names, "“"+j.Name+"”")
		go waitAndStart(p)
	}
	when := fmt.Sprintf("in %d seconds", delay)
	if delay == 1 {
		when = "in 1 second"
	}
	title := "usbsync will start " + when
	if delay == 0 {
		when, title = "now", "usbsync is starting now"
	}
	var body string
	if len(names) == 1 {
		body = fmt.Sprintf("Drive “%s” was plugged in. Sync job %s will start %s.", d.Name, names[0], when)
	} else {
		body = fmt.Sprintf("Drive “%s” was plugged in. Sync jobs %s will start %s.", d.Name, strings.Join(names, ", "), when)
	}
	go func() {
		if notes := sendAll(title, body); len(notes) > 0 {
			log.Printf("drive plugged in notification: %s", strings.Join(notes, "; "))
		}
	}()
}

func waitAndStart(p *pendingStart) {
	select {
	case <-p.cancel:
		return
	case <-time.After(time.Until(p.StartAt)):
	}
	autoMu.Lock()
	if pending[p.ID] != p {
		autoMu.Unlock()
		return
	}
	delete(pending, p.ID)
	autoMu.Unlock()
	j, ok := findJob(p.JobID)
	if !ok || !j.AutoRun {
		return
	}
	present := false
	for _, d := range listDrives() {
		if driveIsJobs(d, &j) {
			present = true
		}
	}
	if !present {
		return
	}
	if _, err := runner.enqueue(&j, false, "drive"); err != nil {
		log.Printf("auto-start of %q skipped: %v", j.Name, err)
	}
}

func handleCancelPending(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	readJSON(r, &req)
	if !cancelPending(req.ID) {
		errJSON(w, 404, "Nothing waiting with that id.")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
