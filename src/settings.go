package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Settings are stored in settings.json in the data directory.
type Settings struct {
	// RequireLogin: when false (the default), devices on the local network,
	// VPN/Tailscale ranges and localhost use the app without signing in.
	// Requests from public addresses always need a DSM sign-in.
	RequireLogin bool `json:"requireLogin"`
	// PermBannerDismissed hides the "some folders are locked" note for good.
	PermBannerDismissed bool `json:"permBannerDismissed"`
	// MaxParallel: how many syncs may run at the same time (default 3).
	MaxParallel int            `json:"maxParallel"`
	Notify      NotifySettings `json:"notify"`
	// AutoRunOff disables "run when the drive is plugged in" for all jobs.
	AutoRunOff bool `json:"autoRunOff"`
	// AutoRunDelay: seconds between the plugged-in notification and the start
	// (nil = 60). Applies to all jobs.
	AutoRunDelay *int `json:"autoRunDelay,omitempty"`
	// AutoEject: eject the drive when a sync finishes (all jobs). Needs the
	// DSM admin account below, since only administrators can eject.
	AutoEject bool         `json:"autoEject"`
	Eject     EjectAccount `json:"eject"`
}

type EjectAccount struct {
	User     string `json:"user"`
	Pass     string `json:"pass,omitempty"`
	DeviceID string `json:"deviceId,omitempty"` // DSM trusted-device token (skips 2FA)
}

func autoRunDelaySecs() int {
	if d := getSettings().AutoRunDelay; d != nil {
		return *d
	}
	return 60
}

type NotifySettings struct {
	OnSuccess bool   `json:"onSuccess"`
	OnFailure bool   `json:"onFailure"`
	OnWarning bool   `json:"onWarning"`
	AppURL    string `json:"appUrl"` // link back to USB Sync in messages
	Email     struct {
		Enabled bool     `json:"enabled"`
		Host    string   `json:"host"`
		Port    int      `json:"port"`
		User    string   `json:"user"`
		Pass    string   `json:"pass,omitempty"`
		To      []string `json:"to"`
	} `json:"email"`
	Pushbullet struct {
		Enabled bool   `json:"enabled"`
		Token   string `json:"token,omitempty"`
	} `json:"pushbullet"`
}

var (
	setMu    sync.Mutex
	settings Settings
)

func loadSettings() {
	settings.Notify.OnSuccess, settings.Notify.OnFailure, settings.Notify.OnWarning = true, true, true
	settings.Notify.Email.Host, settings.Notify.Email.Port = "smtp.gmail.com", 587
	b, err := os.ReadFile(filepath.Join(dataDir, "settings.json"))
	if err == nil {
		json.Unmarshal(b, &settings)
	}
}

// publicSettings hides stored secrets; the UI only learns whether they're set.
func publicSettings() map[string]any {
	s := getSettings()
	passSet, tokSet := s.Notify.Email.Pass != "", s.Notify.Pushbullet.Token != ""
	ejectSet, ejectTrusted := s.Eject.User != "" && s.Eject.Pass != "", s.Eject.DeviceID != ""
	s.Notify.Email.Pass, s.Notify.Pushbullet.Token = "", ""
	s.Eject.Pass, s.Eject.DeviceID = "", ""
	b, _ := json.Marshal(s)
	var m map[string]any
	json.Unmarshal(b, &m)
	m["emailPassSet"], m["pushTokenSet"] = passSet, tokSet
	m["ejectSet"], m["ejectTrusted"] = ejectSet, ejectTrusted
	return m
}

func getSettings() Settings {
	setMu.Lock()
	defer setMu.Unlock()
	return settings
}

var privateNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{
		"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16",
		"100.64.0.0/10", // CGNAT range used by Tailscale
		"::1/128", "fc00::/7", "fe80::/10",
	} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

func isPrivateIP(s string) bool {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range privateNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// isLocalRequest decides whether a request comes from the local network.
// A direct client is judged by its own address. If the direct client is
// this machine (e.g. DSM's reverse proxy), the proxy's X-Forwarded-For entry
// for the real client is used instead.
func isLocalRequest(r *http.Request) bool {
	ip := clientIP(r)
	if !isPrivateIP(ip) {
		return false
	}
	if p := net.ParseIP(ip); p != nil && p.IsLoopback() {
		for _, h := range []string{"X-Forwarded-For", "X-Real-IP"} {
			if v := r.Header.Get(h); v != "" {
				parts := strings.Split(v, ",")
				return isPrivateIP(parts[len(parts)-1])
			}
		}
	}
	return true
}

func handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, publicSettings())
}

func handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	s := getSettings() // fields not sent keep their current value
	oldPass, oldTok := s.Notify.Email.Pass, s.Notify.Pushbullet.Token
	oldEject := s.Eject // the eject account is only changed via /api/eject/account
	if err := readJSON(r, &s); err != nil {
		errJSON(w, 400, "bad request")
		return
	}
	// blank secret = keep the stored one
	if s.Notify.Email.Pass == "" {
		s.Notify.Email.Pass = oldPass
	}
	if s.Notify.Pushbullet.Token == "" {
		s.Notify.Pushbullet.Token = oldTok
	}
	if err := cleanNotify(&s.Notify); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	s.Eject = oldEject
	if s.AutoEject && (s.Eject.User == "" || s.Eject.Pass == "") {
		errJSON(w, 400, "Auto-eject needs a DSM administrator account — save one under Auto-eject first.")
		return
	}
	if s.MaxParallel < 0 || s.MaxParallel > 8 {
		errJSON(w, 400, "Choose between 1 and 8 syncs at once.")
		return
	}
	if s.AutoRunDelay != nil && (*s.AutoRunDelay < 0 || *s.AutoRunDelay > 3600) {
		errJSON(w, 400, "The start delay must be between 0 and 3600 seconds.")
		return
	}
	setMu.Lock()
	settings = s
	b, _ := json.MarshalIndent(settings, "", "  ")
	err := writeFileAtomic(filepath.Join(dataDir, "settings.json"), b)
	os.Chmod(filepath.Join(dataDir, "settings.json"), 0o600) // holds mail password / token
	setMu.Unlock()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	runner.poke() // a higher limit may let queued runs start
	writeJSON(w, 200, publicSettings())
}
