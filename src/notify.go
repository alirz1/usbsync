package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

// Notifications for finished syncs: one rule for all jobs. Sent for real
// syncs that succeeded, finished with warnings or failed — never for
// cancelled runs or previews. Syncs started by hand (Sync now, Run, Re-run)
// only notify when the job's "Send notification" box is ticked; scheduled
// and plugged-in runs always do.

func cleanNotify(n *NotifySettings) error {
	e := &n.Email
	e.Host = strings.TrimSpace(e.Host)
	if e.Host == "" {
		e.Host = "smtp.gmail.com"
	}
	if e.Port == 0 {
		e.Port = 587
	}
	if e.Port < 1 || e.Port > 65535 {
		return errors.New("Invalid mail server port.")
	}
	e.User = strings.TrimSpace(e.User)
	e.Pass = strings.ReplaceAll(e.Pass, " ", "") // Gmail shows app passwords in groups of 4
	var to []string
	for _, a := range e.To {
		for _, p := range strings.FieldsFunc(a, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' }) {
			if _, err := mail.ParseAddress(p); err != nil {
				return fmt.Errorf("“%s” isn't a valid email address.", p)
			}
			to = append(to, p)
		}
	}
	e.To = to
	if e.Enabled {
		if _, err := mail.ParseAddress(e.User); err != nil {
			return errors.New("Enter your Gmail address.")
		}
		if e.Pass == "" {
			return errors.New("Enter a Gmail app password.")
		}
		if len(e.To) == 0 {
			e.To = []string{e.User}
		}
	}
	n.Pushbullet.Token = strings.TrimSpace(n.Pushbullet.Token)
	if n.Pushbullet.Enabled && n.Pushbullet.Token == "" {
		return errors.New("Enter your Pushbullet access token.")
	}
	n.AppURL = strings.TrimSpace(n.AppURL)
	return nil
}

// message builds the notification text: the job name and the outcome.
func message(run *Run) (title, body string) {
	copied := run.NewBytes + run.UpdBytes
	files := run.NewFiles + run.Updated
	plural := func(n int) string {
		if n == 1 {
			return "1 file"
		}
		return fmt.Sprintf("%d files", n)
	}
	switch run.State {
	case "success":
		title = fmt.Sprintf("USB Sync: “%s” completed", run.JobName)
		body = fmt.Sprintf("Sync job “%s” completed successfully.", run.JobName)
		if files > 0 {
			body += fmt.Sprintf("\nCopied: %s (%s)", humanBytes(copied), plural(files))
		} else {
			body += "\nCopied: nothing — everything was already up to date"
		}
	case "warning":
		title = fmt.Sprintf("USB Sync: “%s” finished with warnings", run.JobName)
		body = fmt.Sprintf("Sync job “%s” finished with warnings.", run.JobName)
		if files > 0 {
			body += fmt.Sprintf("\nCopied: %s (%s)", humanBytes(copied), plural(files))
		}
		if run.ErrorCount > 0 {
			body += fmt.Sprintf("\nProblems: %d", run.ErrorCount)
		}
		// the run message is "<summary> unchanged. <reason>" — keep the reason
		if _, reason, found := strings.Cut(run.Message, "unchanged."); found && strings.TrimSpace(reason) != "" {
			body += "\n" + strings.TrimSpace(reason)
		}
		for _, w := range run.Warn {
			body += "\n" + w
		}
	default:
		title = fmt.Sprintf("USB Sync: “%s” failed", run.JobName)
		body = fmt.Sprintf("Sync job “%s” failed.", run.JobName)
		if files > 0 {
			body += fmt.Sprintf("\nCopied before it stopped: %s (%s)", humanBytes(copied), plural(files))
		}
	}
	if run.TotalBytes > 0 {
		body += "\nTotal size of the job: " + humanBytes(run.TotalBytes)
	}
	return
}

func notifyRun(run *Run, extra string) {
	if run.DryRun {
		return
	}
	if (run.Trigger == "manual" || run.Trigger == "rerun") && (run.job == nil || !run.job.Options.Notify) {
		return
	}
	n := getSettings().Notify
	switch run.State {
	case "success":
		if !n.OnSuccess {
			return
		}
	case "warning":
		if !n.OnWarning {
			return
		}
	case "failed":
		if !n.OnFailure {
			return
		}
	default: // cancelled
		return
	}
	if !n.Email.Enabled && !n.Pushbullet.Enabled {
		return
	}
	title, body := message(run)
	if extra != "" {
		body += "\n" + extra
	}
	notes := sendAll(title, body)
	if f, err := os.OpenFile(runFile(run.ID, ".log"), os.O_APPEND|os.O_WRONLY, 0); err == nil {
		for _, s := range notes {
			fmt.Fprintf(f, "[%s] Notification: %s\n", time.Now().Format("2006-01-02 15:04:05"), s)
		}
		f.Close()
	}
	log.Printf("run %s notifications: %s", run.ID, strings.Join(notes, "; "))
}

func outcome(what string, err error) string {
	if err != nil {
		return what + " failed — " + err.Error()
	}
	return what + " sent"
}

func retry(f func() error) error {
	var err error
	for i := 0; i < 3; i++ {
		if err = f(); err == nil {
			return nil
		}
		time.Sleep(time.Duration(5*(i+1)) * time.Second)
	}
	return err
}

// ---------- email (SMTP, e.g. Gmail with an app password) ----------

func sendEmail(n NotifySettings, subject, body string) error {
	e := n.Email
	from := mail.Address{Name: "USB Sync", Address: e.User}
	var msg bytes.Buffer
	host, _ := os.Hostname()
	fmt.Fprintf(&msg, "From: %s\r\n", from.String())
	fmt.Fprintf(&msg, "To: %s\r\n", strings.Join(e.To, ", "))
	fmt.Fprintf(&msg, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&msg, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&msg, "Message-ID: <%s@%s>\r\n", randHex(12), strings.ReplaceAll(host+".usbsync", " ", ""))
	msg.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	msg.WriteString(strings.ReplaceAll(body, "\n", "\r\n") + "\r\n")

	addr := net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
	var c *smtp.Client
	var err error
	if e.Port == 465 { // implicit TLS
		conn, derr := tls.DialWithDialer(&net.Dialer{Timeout: 20 * time.Second}, "tcp", addr, &tls.Config{ServerName: e.Host})
		if derr != nil {
			return friendlyMailErr(derr)
		}
		c, err = smtp.NewClient(conn, e.Host)
	} else {
		conn, derr := net.DialTimeout("tcp", addr, 20*time.Second)
		if derr != nil {
			return friendlyMailErr(derr)
		}
		c, err = smtp.NewClient(conn, e.Host)
		if err == nil {
			if ok, _ := c.Extension("STARTTLS"); ok {
				err = c.StartTLS(&tls.Config{ServerName: e.Host})
			} else if !isLocalHost(e.Host) {
				err = errors.New("the mail server doesn't offer encryption (STARTTLS)")
			}
		}
	}
	if err != nil {
		return friendlyMailErr(err)
	}
	defer c.Close()
	if err := c.Auth(smtp.PlainAuth("", e.User, e.Pass, e.Host)); err != nil {
		return friendlyMailErr(err)
	}
	if err := c.Mail(e.User); err != nil {
		return friendlyMailErr(err)
	}
	for _, t := range e.To {
		if err := c.Rcpt(t); err != nil {
			return friendlyMailErr(err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return friendlyMailErr(err)
	}
	if _, err := w.Write(msg.Bytes()); err != nil {
		return friendlyMailErr(err)
	}
	if err := w.Close(); err != nil {
		return friendlyMailErr(err)
	}
	return c.Quit()
}

func isLocalHost(h string) bool { return h == "localhost" || h == "127.0.0.1" || h == "::1" }

func friendlyMailErr(err error) error {
	s := err.Error()
	switch {
	case strings.Contains(s, "535"), strings.Contains(s, "534"), strings.Contains(strings.ToLower(s), "username and password not accepted"):
		return errors.New("Gmail rejected the sign-in. Use a Gmail app password (not your normal password) and check the address")
	case strings.Contains(s, "no such host"), strings.Contains(s, "i/o timeout"), strings.Contains(s, "connection refused"):
		return fmt.Errorf("couldn't reach the mail server (%v)", err)
	}
	return err
}

// ---------- Pushbullet ----------

var pushbulletURL = envOr("USBSYNC_PUSHBULLET_URL", "https://api.pushbullet.com/v2/pushes")

func sendPushbullet(token, title, body string) error {
	payload, _ := json.Marshal(map[string]string{"type": "note", "title": title, "body": body})
	req, _ := http.NewRequest("POST", pushbulletURL, bytes.NewReader(payload))
	req.Header.Set("Access-Token", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach Pushbullet (%v)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return errors.New("Pushbullet rejected the access token")
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("Pushbullet error %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// ---------- test button ----------

func handleTestNotify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string         `json:"channel"` // email | pushbullet
		Notify  NotifySettings `json:"notify"`  // unsaved values from the form
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, 400, "bad request")
		return
	}
	n := req.Notify
	saved := getSettings().Notify
	if n.Email.Pass == "" {
		n.Email.Pass = saved.Email.Pass
	}
	if n.Pushbullet.Token == "" {
		n.Pushbullet.Token = saved.Pushbullet.Token
	}
	switch req.Channel {
	case "email":
		n.Email.Enabled = true
	case "pushbullet":
		n.Pushbullet.Enabled = true
	default:
		errJSON(w, 400, "bad channel")
		return
	}
	if err := cleanNotify(&n); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	title, body := "USB Sync: test notification", "This is a test from USB Sync. Notifications are working."
	var err error
	if req.Channel == "email" {
		err = sendEmail(n, title, body)
	} else {
		err = sendPushbullet(n.Pushbullet.Token, title, body)
	}
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// sendAll delivers a message through every enabled channel and reports how
// each one went. Used for finished-sync and drive-plugged-in messages.
func sendAll(title, body string) []string {
	n := getSettings().Notify
	var notes []string
	if n.Email.Enabled {
		err := retry(func() error { return sendEmail(n, title, body) })
		notes = append(notes, outcome("Email", err))
	}
	if n.Pushbullet.Enabled {
		err := retry(func() error { return sendPushbullet(n.Pushbullet.Token, title, body) })
		notes = append(notes, outcome("Pushbullet", err))
	}
	return notes
}
