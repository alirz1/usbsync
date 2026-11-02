package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Ejecting needs root, which DSM 7 doesn't give third-party packages. So the
// eject is done by DSM itself, through its Web API, signed in with an
// administrator's credentials (used once, never stored) — the same request
// Control Panel › External Devices › Eject makes.

// driveInUse names a running or queued sync that uses the drive, if any.
func (r *Runner) driveInUse(driveID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range [][]*Run{r.active, r.queue} {
		for _, x := range l {
			if x.job != nil && x.job.DriveID == driveID {
				return x.JobName
			}
		}
	}
	return ""
}

// collectStrings gathers every string value inside a decoded JSON value.
func collectStrings(v any, out *[]string) {
	switch t := v.(type) {
	case string:
		*out = append(*out, t)
	case []any:
		for _, x := range t {
			collectStrings(x, out)
		}
	case map[string]any:
		for _, x := range t {
			collectStrings(x, out)
		}
	}
}

func dsmError(body []byte) (bool, int) {
	var r struct {
		Success bool `json:"success"`
		Error   struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &r) != nil {
		return false, -1
	}
	return r.Success, r.Error.Code
}

// findDevID asks DSM for its external-device list and returns the dev_id of
// the device that holds the drive's share (e.g. usbshare1).
func findDevID(d *dsmSession, api string, drv *Drive) (string, error) {
	body, err := d.call(url.Values{"api": {api}, "version": {"1"}, "method": {"list"}, "additional": {`["all"]`}})
	if err != nil {
		return "", err
	}
	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Devices []map[string]any `json:"devices"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &resp) != nil || !resp.Success {
		return "", fmt.Errorf("DSM didn't return its device list")
	}
	mount := drv.Mount
	for _, dev := range resp.Data.Devices {
		id, _ := dev["dev_id"].(string)
		if id == "" {
			continue
		}
		var vals []string
		collectStrings(dev["partitions"], &vals)
		for _, v := range vals {
			v = strings.TrimSuffix(v, "/")
			if v == drv.ShareName || v == mount || v == filepath.Dir(mount) {
				return id, nil
			}
		}
	}
	return "", fmt.Errorf("DSM didn't list a device holding %s", drv.ShareName)
}

func ejectDrive(drv *Drive, user, pass, otp string) error {
	d, err := dsmOpen(user, pass, otp)
	if err != nil {
		return err
	}
	defer d.logout()
	return ejectWith(d, drv)
}

// ejectSaved ejects using the auto-eject account saved in Settings.
func ejectSaved(drv *Drive) error {
	acc := getSettings().Eject
	if acc.User == "" || acc.Pass == "" {
		return errors.New("no auto-eject account is saved in Settings")
	}
	d, did, err := dsmOpenDevice(acc.User, acc.Pass, "", acc.DeviceID)
	if err != nil {
		if errors.Is(err, errOTPRequired) {
			return errors.New("DSM asked for a two-factor code — open Settings › Auto-eject and save the account again with a code")
		}
		return err
	}
	defer d.logout()
	if did != "" && did != acc.DeviceID {
		saveEjectAccount(EjectAccount{User: acc.User, Pass: acc.Pass, DeviceID: did})
	}
	return ejectWith(d, drv)
}

func ejectWith(d *dsmSession, drv *Drive) error {
	api := "SYNO.Core.ExternalDevice.Storage.USB"
	prefix := "usb"
	if drv.Bus == "eSATA" {
		api, prefix = "SYNO.Core.ExternalDevice.Storage.eSATA", "esata"
	}
	devID, ferr := findDevID(d, api, drv)
	if devID == "" {
		// fall back to DSM's usual naming: /volumeUSB2 -> usb2
		devID = prefix + strconv.Itoa(volNum(filepath.Dir(drv.Mount)))
		log.Printf("eject: %v; trying dev_id %s", ferr, devID)
	}
	var lastCode int
	for _, idParam := range []string{strconv.Quote(devID), devID} {
		body, err := d.call(url.Values{"api": {api}, "version": {"1"}, "method": {"eject"}, "dev_id": {idParam}})
		if err != nil {
			return err
		}
		ok, code := dsmError(body)
		if ok {
			return nil
		}
		lastCode = code
		if code == 105 {
			return errors.New("This DSM account isn't allowed to eject drives (administrators only).")
		}
	}
	return fmt.Errorf("DSM refused to eject the drive (error %d). Something may still be using it — try Control Panel › External Devices › Eject", lastCode)
}

func handleEject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DriveID string `json:"driveId"`
		User    string `json:"user"`
		Pass    string `json:"pass"`
		OTP     string `json:"otp"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, 400, "bad request")
		return
	}
	var drv *Drive
	for _, d := range listDrives() {
		if d.ID == req.DriveID {
			dd := d
			drv = &dd
		}
	}
	if drv == nil {
		errJSON(w, 404, "That drive is no longer connected.")
		return
	}
	if name := runner.driveInUse(drv.ID); name != "" {
		errJSON(w, 409, fmt.Sprintf("“%s” is using this drive. Wait for it to finish or cancel it first.", name))
		return
	}
	ip := clientIP(r)
	if tooManyFailures(ip) {
		errJSON(w, 429, "Too many failed attempts. Try again in a few minutes.")
		return
	}
	var err error
	if req.Pass == "" && getSettings().Eject.Pass != "" {
		err = ejectSaved(drv) // use the saved auto-eject account
	} else {
		err = ejectDrive(drv, strings.TrimSpace(req.User), req.Pass, strings.TrimSpace(req.OTP))
	}
	if err != nil {
		if errors.Is(err, errOTPRequired) {
			writeJSON(w, 401, map[string]any{"error": "Enter your two-factor authentication code.", "otp": true})
			return
		}
		if strings.HasPrefix(err.Error(), "Incorrect") {
			recordFailure(ip)
		}
		errJSON(w, 400, err.Error())
		return
	}
	// wait for DSM to finish unmounting
	for i := 0; i < 20; i++ {
		if driveGone(drv.ID) {
			log.Printf("ejected %s (%s)", drv.Name, drv.Mount)
			writeJSON(w, 200, map[string]any{"ok": true, "message": fmt.Sprintf("%s was ejected. You can unplug it now.", drv.Name)})
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": fmt.Sprintf("DSM accepted the eject for %s, but it still shows as mounted. Check Control Panel › External Devices before unplugging.", drv.Name), "pending": true})
}

func driveGone(id string) bool {
	for _, d := range listDrives() {
		if d.ID == id {
			return false
		}
	}
	return true
}

func saveEjectAccount(a EjectAccount) error {
	setMu.Lock()
	defer setMu.Unlock()
	settings.Eject = a
	b, _ := json.MarshalIndent(settings, "", "  ")
	err := writeFileAtomic(filepath.Join(dataDir, "settings.json"), b)
	os.Chmod(filepath.Join(dataDir, "settings.json"), 0o600)
	return err
}

// handleEjectAccount saves (and checks) the DSM admin account used for
// automatic ejects. A two-factor code, if given, makes DSM trust this app so
// later ejects need no code.
func handleEjectAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User   string `json:"user"`
		Pass   string `json:"pass"`
		OTP    string `json:"otp"`
		Remove bool   `json:"remove"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, 400, "bad request")
		return
	}
	if req.Remove {
		saveEjectAccount(EjectAccount{})
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	user := strings.TrimSpace(req.User)
	pass := req.Pass
	old := getSettings().Eject
	if pass == "" && user == old.User {
		pass = old.Pass
	}
	ip := clientIP(r)
	if tooManyFailures(ip) {
		errJSON(w, 429, "Too many failed attempts. Try again in a few minutes.")
		return
	}
	devID := ""
	if user == old.User {
		devID = old.DeviceID
	}
	d, did, err := dsmOpenDevice(user, pass, strings.TrimSpace(req.OTP), devID)
	if err != nil {
		if errors.Is(err, errOTPRequired) {
			writeJSON(w, 401, map[string]any{"error": "This account uses two-factor sign-in. Enter a current code once so DSM can trust USB Sync.", "otp": true})
			return
		}
		if strings.HasPrefix(err.Error(), "Incorrect") {
			recordFailure(ip)
		}
		errJSON(w, 400, err.Error())
		return
	}
	d.logout()
	if !noAuth && !isAdmin(user) {
		errJSON(w, 400, "Only DSM administrators can eject drives. Use an account in the administrators group.")
		return
	}
	if did == "" {
		did = devID
	}
	if err := saveEjectAccount(EjectAccount{User: user, Pass: pass, DeviceID: did}); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "trusted": did != ""})
}

// afterRun: optional automatic eject, then the finished-sync notification.
func afterRun(run *Run) {
	note := ""
	if !run.DryRun && run.State != "cancelled" && run.job != nil && getSettings().AutoEject {
		note = autoEject(run)
	}
	notifyRun(run, note)
}

func autoEject(run *Run) string {
	var drv *Drive
	for _, d := range listDrives() {
		if driveIsJobs(d, run.job) {
			dd := d
			drv = &dd
		}
	}
	logNote := func(msg string) string {
		if f, err := os.OpenFile(runFile(run.ID, ".log"), os.O_APPEND|os.O_WRONLY, 0); err == nil {
			fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
			f.Close()
		}
		runner.mu.Lock()
		for _, h := range runner.history {
			if h.ID == run.ID {
				h.Message = strings.TrimSpace(h.Message + " " + msg)
			}
		}
		runner.saveHistoryLocked()
		runner.mu.Unlock()
		log.Printf("run %s: %s", run.ID, msg)
		return msg
	}
	if drv == nil {
		return logNote("Drive already disconnected — nothing to eject.")
	}
	if name := runner.driveInUse(drv.ID); name != "" {
		return logNote(fmt.Sprintf("Drive not ejected: “%s” is still using it.", name))
	}
	for _, p := range pendingList() {
		if p.DriveID == drv.ID {
			return logNote(fmt.Sprintf("Drive not ejected: “%s” is about to start on it.", p.JobName))
		}
	}
	if err := ejectSaved(drv); err != nil {
		return logNote(fmt.Sprintf("Drive not ejected: %v.", strings.TrimSuffix(err.Error(), ".")))
	}
	for i := 0; i < 20 && !driveGone(drv.ID); i++ {
		time.Sleep(500 * time.Millisecond)
	}
	if !driveGone(drv.ID) {
		return logNote(fmt.Sprintf("DSM accepted the eject for “%s” but it still shows as mounted — check Control Panel › External Devices before unplugging.", drv.Name))
	}
	return logNote(fmt.Sprintf("Drive “%s” was ejected — safe to unplug.", drv.Name))
}
