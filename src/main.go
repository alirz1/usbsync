package main

import (
	"crypto/rand"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

var (
	version    = "1.9.7"
	dataDir    string
	listenAddr string
	noAuth     bool
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	flag.StringVar(&dataDir, "data", envOr("SYNOPKG_PKGVAR", "./data"), "data directory")
	flag.StringVar(&listenAddr, "listen", envOr("USBSYNC_LISTEN", ":8765"), "listen address")
	flag.BoolVar(&noAuth, "noauth", false, "disable sign-in (testing only)")
	flag.Parse()

	if err := os.MkdirAll(filepath.Join(dataDir, "logs"), 0o755); err != nil {
		log.Fatal(err)
	}
	loadSettings()
	loadStore()
	runner.loadHistory()
	go runner.loop()
	go scheduler()
	go driveWatcher()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", handleLogin)
	mux.HandleFunc("POST /api/logout", handleLogout)
	mux.HandleFunc("GET /api/me", handleMe)
	mux.HandleFunc("GET /api/shares", auth(handleShares))
	mux.HandleFunc("GET /api/drives", auth(handleDrives))
	mux.HandleFunc("POST /api/eject", auth(handleEject))
	mux.HandleFunc("POST /api/eject/account", auth(handleEjectAccount))
	mux.HandleFunc("GET /api/ls", auth(handleLs))
	mux.HandleFunc("POST /api/mkdir", auth(handleMkdir))
	mux.HandleFunc("GET /api/jobs", auth(handleJobs))
	mux.HandleFunc("POST /api/jobs", auth(handleSaveJob))
	mux.HandleFunc("DELETE /api/jobs/{id}", auth(handleDeleteJob))
	mux.HandleFunc("POST /api/run", auth(handleRun))
	mux.HandleFunc("POST /api/cancel", auth(handleCancel))
	mux.HandleFunc("POST /api/autorun/cancel", auth(handleCancelPending))
	mux.HandleFunc("POST /api/pause", auth(handlePause))
	mux.HandleFunc("GET /api/status", auth(handleStatus))
	mux.HandleFunc("GET /api/logs/{id}", auth(handleLog))
	mux.HandleFunc("GET /api/runs/{id}", auth(handleRunDetail))
	mux.HandleFunc("POST /api/runs/{id}/rerun", auth(handleRerun))
	mux.HandleFunc("GET /api/settings", auth(handleGetSettings))
	mux.HandleFunc("POST /api/settings", auth(handleSaveSettings))
	mux.HandleFunc("POST /api/notify/test", auth(handleTestNotify))
	mux.HandleFunc("POST /api/backup/export", auth(handleExport))
	mux.HandleFunc("POST /api/backup/import", auth(handleImport))
	mux.HandleFunc("GET /api/runs/{id}/tree", auth(handleRunTree))

	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	srv := &http.Server{Addr: listenAddr, Handler: headers(mux), ReadHeaderTimeout: 10 * time.Second}

	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		<-ch
		log.Print("shutting down")
		runner.shutdown()
		runner.waitIdle(12 * time.Second)
		srv.Close()
	}()

	log.Printf("USB Sync %s listening on %s (data: %s)", version, listenAddr, dataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func headers(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		h.ServeHTTP(w, r)
	})
}

// ---------- JSON helpers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------- Sessions ----------

const cookieName = "usbsync_sid"

type session struct {
	user string
	exp  time.Time
}

var (
	sessMu   sync.Mutex
	sessions = map[string]*session{}
)

func currentUser(r *http.Request) string {
	if noAuth {
		return "dev"
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		if !getSettings().RequireLogin && isLocalRequest(r) {
			return "local network"
		}
		return ""
	}
	sessMu.Lock()
	defer sessMu.Unlock()
	s := sessions[c.Value]
	if s == nil || time.Now().After(s.exp) {
		delete(sessions, c.Value)
		if !getSettings().RequireLogin && isLocalRequest(r) {
			return "local network"
		}
		return ""
	}
	s.exp = time.Now().Add(12 * time.Hour)
	return s.user
}

// auth wraps API handlers: requires a session, and a custom header on
// state-changing requests (blocks cross-site form posts).
func auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Header.Get("X-USBSync") != "1" {
			errJSON(w, http.StatusForbidden, "missing request header")
			return
		}
		if currentUser(r) == "" {
			errJSON(w, http.StatusUnauthorized, "not signed in")
			return
		}
		h(w, r)
	}
}

var (
	failMu sync.Mutex
	fails  = map[string][]time.Time{}
)

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func tooManyFailures(ip string) bool {
	failMu.Lock()
	defer failMu.Unlock()
	cut := time.Now().Add(-10 * time.Minute)
	var keep []time.Time
	for _, t := range fails[ip] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	fails[ip] = keep
	return len(keep) >= 5
}

func recordFailure(ip string) {
	failMu.Lock()
	fails[ip] = append(fails[ip], time.Now())
	failMu.Unlock()
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-USBSync") != "1" {
		errJSON(w, http.StatusForbidden, "missing request header")
		return
	}
	var req struct {
		User string `json:"user"`
		Pass string `json:"pass"`
		OTP  string `json:"otp"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "bad request")
		return
	}
	req.User = strings.TrimSpace(req.User)
	ip := clientIP(r)
	if tooManyFailures(ip) {
		errJSON(w, http.StatusTooManyRequests, "Too many failed attempts. Try again in a few minutes.")
		return
	}
	if !noAuth {
		if err := dsmLogin(req.User, req.Pass, strings.TrimSpace(req.OTP)); err != nil {
			if errors.Is(err, errOTPRequired) {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Enter your two-factor authentication code.", "otp": true})
				return
			}
			recordFailure(ip)
			time.Sleep(700 * time.Millisecond)
			errJSON(w, http.StatusUnauthorized, err.Error())
			return
		}
		if !isAdmin(req.User) {
			errJSON(w, http.StatusForbidden, "Only members of the DSM administrators group can use USB Sync.")
			return
		}
	}
	tok := randHex(32)
	sessMu.Lock()
	sessions[tok] = &session{user: req.User, exp: time.Now().Add(12 * time.Hour)}
	sessMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
	})
	writeJSON(w, http.StatusOK, map[string]any{"user": req.User})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		sessMu.Lock()
		delete(sessions, c.Value)
		sessMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func handleMe(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == "" {
		errJSON(w, http.StatusUnauthorized, "not signed in")
		return
	}
	_, cerr := r.Cookie(cookieName)
	writeJSON(w, http.StatusOK, map[string]any{
		"user": u, "version": version, "signedIn": cerr == nil && u != "local network",
		"requireLogin": getSettings().RequireLogin, "autoRunOff": getSettings().AutoRunOff, "autoRunDelay": autoRunDelaySecs(), "autoEject": getSettings().AutoEject, "ejectSet": getSettings().Eject.User != "" && getSettings().Eject.Pass != "", "permBannerDismissed": getSettings().PermBannerDismissed, "nasTime": time.Now().Format("2006-01-02 15:04"),
		"pkgUser": envOr("SYNOPKG_PKGNAME", "USBSync"),
	})
}

// ---------- DSM authentication ----------

var errOTPRequired = errors.New("two-factor code required")

func dsmPorts() (string, string) {
	hp, sp := "5000", "5001"
	for _, f := range []string{"/etc/synoinfo.conf", "/etc.defaults/synoinfo.conf"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			v = strings.Trim(strings.TrimSpace(v), `"`)
			switch strings.TrimSpace(k) {
			case "admin_port":
				if v != "" {
					hp = v
				}
			case "secure_admin_port":
				if v != "" {
					sp = v
				}
			}
		}
		break
	}
	return hp, sp
}

// dsmLogin verifies credentials against DSM's own Web API on localhost.
// dsmSession is a short-lived DSM Web API session on localhost.
type dsmSession struct {
	client *http.Client
	base   string
	sid    string
	token  string
}

func (d *dsmSession) call(form url.Values) ([]byte, error) {
	form.Set("_sid", d.sid)
	req, err := http.NewRequest("POST", d.base+"/webapi/entry.cgi", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if d.token != "" {
		req.Header.Set("X-SYNO-TOKEN", d.token)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func (d *dsmSession) logout() {
	d.call(url.Values{"api": {"SYNO.API.Auth"}, "version": {"6"}, "method": {"logout"}, "session": {"USBSync"}})
}

// dsmOpen signs in to DSM's own Web API on localhost.
func dsmOpen(user, pass, otp string) (*dsmSession, error) {
	d, _, err := dsmOpenDevice(user, pass, otp, "")
	return d, err
}

// dsmOpenDevice signs in like dsmOpen. With a two-factor code it also asks
// DSM to trust this app as a device and returns that device ID; passing the
// ID later skips the two-factor code (DSM "trusted device").
func dsmOpenDevice(user, pass, otp, deviceID string) (*dsmSession, string, error) {
	if user == "" || pass == "" {
		return nil, "", errors.New("Enter your DSM username and password.")
	}
	hp, sp := dsmPorts()
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy:           nil,
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // localhost only
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var lastErr error
	for _, base := range []string{"http://127.0.0.1:" + hp, "https://127.0.0.1:" + sp} {
		form := url.Values{
			"api": {"SYNO.API.Auth"}, "version": {"6"}, "method": {"login"},
			"account": {user}, "passwd": {pass}, "session": {"USBSync"}, "format": {"sid"},
			"enable_syno_token": {"yes"},
		}
		if otp != "" {
			form.Set("otp_code", otp)
			form.Set("enable_device_token", "yes")
			form.Set("device_name", "USB Sync")
		}
		if deviceID != "" {
			form.Set("device_id", deviceID)
			form.Set("device_name", "USB Sync")
		}
		resp, err := client.PostForm(base+"/webapi/entry.cgi", form)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("HTTP %d from %s", resp.StatusCode, base)
			continue
		}
		var out struct {
			Success bool `json:"success"`
			Data    struct {
				Sid   string `json:"sid"`
				Token string `json:"synotoken"`
				DID   string `json:"did"`
				DID2  string `json:"device_id"`
			} `json:"data"`
			Error struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			lastErr = err
			continue
		}
		if out.Success {
			did := out.Data.DID
			if did == "" {
				did = out.Data.DID2
			}
			return &dsmSession{client: client, base: base, sid: out.Data.Sid, token: out.Data.Token}, did, nil
		}
		switch out.Error.Code {
		case 400:
			return nil, "", errors.New("Incorrect username or password.")
		case 401:
			return nil, "", errors.New("This DSM account is disabled.")
		case 402:
			return nil, "", errors.New("This DSM account is not allowed to sign in.")
		case 403, 406:
			return nil, "", errOTPRequired
		case 404:
			return nil, "", errors.New("Incorrect two-factor code.")
		case 407:
			return nil, "", errors.New("Sign-in blocked by DSM auto-block.")
		default:
			return nil, "", fmt.Errorf("DSM sign-in failed (error %d).", out.Error.Code)
		}
	}
	return nil, "", fmt.Errorf("Could not reach DSM: %v", lastErr)
}

// dsmLogin verifies credentials against DSM and signs straight out again.
func dsmLogin(user, pass, otp string) error {
	d, err := dsmOpen(user, pass, otp)
	if err != nil {
		return err
	}
	d.logout()
	return nil
}

func isAdmin(user string) bool {
	b, err := os.ReadFile("/etc/group")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		p := strings.Split(line, ":")
		if len(p) >= 4 && p[0] == "administrators" {
			for _, m := range strings.Split(p[3], ",") {
				if strings.EqualFold(strings.TrimSpace(m), user) {
					return true
				}
			}
		}
	}
	return false
}
