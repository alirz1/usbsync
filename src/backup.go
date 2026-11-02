package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"
)

// Export / import of all app settings: saved jobs plus Settings. The secrets
// (Gmail app password, Pushbullet token, auto-eject DSM account password and
// trusted-device token) are taken out of the readable part and stored in
// "secrets", encrypted with a password chosen at export:
// AES-256-GCM, key = PBKDF2-HMAC-SHA256(password, random salt, 600 000 rounds).
// Jobs and the other settings stay readable, so they can be restored even
// without the password.

type backupFile struct {
	App      string        `json:"app"`
	Version  string        `json:"version"`
	Exported time.Time     `json:"exported"`
	Settings Settings      `json:"settings"`
	Jobs     []Job         `json:"jobs"`
	Secrets  *sealedSecret `json:"secrets,omitempty"`
}

type sealedSecret struct {
	Alg   string `json:"alg"` // AES-256-GCM
	KDF   string `json:"kdf"` // PBKDF2-SHA256
	Iter  int    `json:"iter"`
	Salt  []byte `json:"salt"`  // base64 in JSON
	Nonce []byte `json:"nonce"` // base64 in JSON
	Data  []byte `json:"data"`  // ciphertext + tag
}

type backupSecrets struct {
	EmailPass     string `json:"emailPass,omitempty"`
	PushToken     string `json:"pushToken,omitempty"`
	EjectPass     string `json:"ejectPass,omitempty"`
	EjectDeviceID string `json:"ejectDeviceId,omitempty"`
}

const (
	backupIter   = 600000
	backupAAD    = "USBSync backup secrets v1"
	minBackupPwd = 8
)

var errBadBackupPassword = errors.New("Wrong backup password.")

func backupKey(password string, salt []byte, iter int) (cipher.AEAD, error) {
	key, err := pbkdf2.Key(sha256.New, password, salt, iter, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealSecrets(sec backupSecrets, password string) (*sealedSecret, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	gcm, err := backupKey(password, salt, backupIter)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	plain, _ := json.Marshal(sec)
	return &sealedSecret{Alg: "AES-256-GCM", KDF: "PBKDF2-SHA256", Iter: backupIter, Salt: salt, Nonce: nonce,
		Data: gcm.Seal(nil, nonce, plain, []byte(backupAAD))}, nil
}

func openSecrets(s *sealedSecret, password string) (backupSecrets, error) {
	var sec backupSecrets
	if s.Alg != "AES-256-GCM" || s.KDF != "PBKDF2-SHA256" || s.Iter < 1000 || s.Iter > 10000000 || len(s.Salt) < 8 {
		return sec, errors.New("The file's encrypted section is in an unknown format.")
	}
	gcm, err := backupKey(password, s.Salt, s.Iter)
	if err != nil {
		return sec, err
	}
	if len(s.Nonce) != gcm.NonceSize() {
		return sec, errors.New("The file's encrypted section is damaged.")
	}
	plain, err := gcm.Open(nil, s.Nonce, s.Data, []byte(backupAAD))
	if err != nil {
		return sec, errBadBackupPassword // wrong password (or the file was altered)
	}
	if err := json.Unmarshal(plain, &sec); err != nil {
		return sec, errors.New("The file's encrypted section is damaged.")
	}
	return sec, nil
}

func handleExport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, 400, "bad request")
		return
	}
	if utf8.RuneCountInString(req.Password) < minBackupPwd {
		errJSON(w, 400, fmt.Sprintf("Choose a backup password of at least %d characters.", minBackupPwd))
		return
	}
	s := getSettings()
	sec := backupSecrets{EmailPass: s.Notify.Email.Pass, PushToken: s.Notify.Pushbullet.Token, EjectPass: s.Eject.Pass, EjectDeviceID: s.Eject.DeviceID}
	sealed, err := sealSecrets(sec, req.Password)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	s.Notify.Email.Pass, s.Notify.Pushbullet.Token, s.Eject.Pass, s.Eject.DeviceID = "", "", "", ""
	s.PermBannerDismissed = false
	b := backupFile{App: "USBSync", Version: version, Exported: time.Now(), Settings: s, Jobs: snapshotJobs(), Secrets: sealed}
	data, _ := json.MarshalIndent(b, "", "  ")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="usbsync-settings-%s.json"`, time.Now().Format("2006-01-02")))
	w.Write(data)
}

func handleImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		File     backupFile `json:"file"`
		Password string     `json:"password"`
		// SkipSecrets: import without the encrypted passwords/tokens. The ones
		// already saved on this NAS are kept; channels left without one are
		// switched off.
		SkipSecrets bool `json:"skipSecrets"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil || req.File.App != "USBSync" {
		errJSON(w, 400, "That isn't a USB Sync settings file.")
		return
	}
	b := req.File
	s := b.Settings
	var notes []string
	if b.Secrets != nil {
		if req.SkipSecrets {
			cur := getSettings()
			s.Notify.Email.Pass, s.Notify.Pushbullet.Token = cur.Notify.Email.Pass, cur.Notify.Pushbullet.Token
			s.Eject = cur.Eject
		} else {
			if tooManyFailures(clientIP(r)) {
				errJSON(w, 429, "Too many wrong passwords. Try again in a few minutes.")
				return
			}
			sec, err := openSecrets(b.Secrets, req.Password)
			if err != nil {
				if errors.Is(err, errBadBackupPassword) {
					recordFailure(clientIP(r))
					writeJSON(w, 400, map[string]any{"error": err.Error(), "badPassword": true})
					return
				}
				errJSON(w, 400, err.Error())
				return
			}
			s.Notify.Email.Pass, s.Notify.Pushbullet.Token = sec.EmailPass, sec.PushToken
			s.Eject.Pass, s.Eject.DeviceID = sec.EjectPass, sec.EjectDeviceID
		}
		// anything still missing a secret is switched off rather than failing
		if s.Notify.Email.Enabled && s.Notify.Email.Pass == "" {
			s.Notify.Email.Enabled = false
			notes = append(notes, "Gmail notifications were turned off (no app password)")
		}
		if s.Notify.Pushbullet.Enabled && s.Notify.Pushbullet.Token == "" {
			s.Notify.Pushbullet.Enabled = false
			notes = append(notes, "Pushbullet was turned off (no access token)")
		}
		if s.AutoEject && (s.Eject.User == "" || s.Eject.Pass == "") {
			s.AutoEject = false
			notes = append(notes, "Auto-eject was turned off (no DSM admin account)")
		}
	}
	// settings
	if err := cleanNotify(&s.Notify); err != nil {
		errJSON(w, 400, "Notification settings in the file: "+err.Error())
		return
	}
	if s.MaxParallel < 0 || s.MaxParallel > 8 {
		s.MaxParallel = 3
	}
	// jobs: add new ones, replace ones with the same id, keep the rest
	var bad []string
	var good []*Job
	for i := range b.Jobs {
		j := b.Jobs[i]
		id := j.ID
		if err := validateJob(&j); err != nil {
			bad = append(bad, fmt.Sprintf("%s (%s)", j.Name, err.Error()))
			continue
		}
		if id == "" || !idRe.MatchString(id) {
			id = randHex(6)
		}
		j.ID = id
		if j.Created.IsZero() {
			j.Created = time.Now()
		}
		j.Updated = time.Now()
		good = append(good, &j)
	}

	setMu.Lock()
	s.PermBannerDismissed = settings.PermBannerDismissed
	settings = s
	data, _ := json.MarshalIndent(settings, "", "  ")
	err := writeFileAtomic(filepath.Join(dataDir, "settings.json"), data)
	os.Chmod(filepath.Join(dataDir, "settings.json"), 0o600)
	setMu.Unlock()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}

	storeMu.Lock()
	added, replaced := 0, 0
	for _, nj := range good {
		found := false
		for i, old := range jobs {
			if old.ID == nj.ID {
				nj.Created = old.Created
				jobs[i] = nj
				found = true
				replaced++
				break
			}
		}
		if !found {
			jobs = append(jobs, nj)
			added++
		}
	}
	err = saveStoreLocked()
	storeMu.Unlock()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	runner.poke()
	writeJSON(w, 200, map[string]any{"added": added, "replaced": replaced, "skipped": bad, "notes": notes})
}
