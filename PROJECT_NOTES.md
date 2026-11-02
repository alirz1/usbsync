# USB Sync — Project Notes

Native Synology DSM 7 package (SPK) that rsyncs NAS shared folders to and from USB/eSATA drives.
Built for the owner's SA6400 (x86_64 / epyc7002, DSM 7.x, Xpenology), but it works on any x86_64 DSM 7 box.

- Current version: **1.9.7-0028**
- Web UI: port **8765**, also opened from the DSM main menu as "USB Sync"

Keep this file up to date with every release. It must be included in every source zip; `build.sh` does that automatically.

---

## 1. Features

### Sync page
- **Two panes.**
  - Left: NAS shared folders, grouped by volume.
  - Right: the selected USB/eSATA drive. A drive picker appears when more than one drive is attached.
- **Direction toggle.** NAS → USB or USB → NAS.
- **Folder tree.**
  - Tap a folder name to expand it.
  - Tick folders on the source side and pick one destination folder.
  - Unticking a sub-folder or file inside a ticked folder makes it an exclusion for that job.
  - Locked folders show a lock icon with the tooltip *"USBSync" user needs permission to this share/folder*.
- **Copy mode** (set automatically, editable).
  - "into": `dest/TV`.
  - "contents": `TV → TV`. Used when the destination already has the same name.
- **Multiple folder pairs per job.** Use "Add pair".
  - Mirror is refused when the pairs' targets overlap.
- **Rules / comparison.** These map to rsync flags:

  | Choice | rsync behaviour |
  |---|---|
  | Size + modified date (default) | quick check |
  | Name + size | `--size-only` |
  | Checksum | `--checksum` |
  | Name only, skip existing | `--ignore-existing` |
  | Always copy | `--ignore-times` |
  | Skip files newer at destination | `--update` |
  | Only update files that already exist | `--existing` |
  | Size limits | `--min-size` / `--max-size` |
  | Modified within N days / since a date | file list built in Go, passed as `--files-from` |
  | Timestamp window | `--modify-window` (auto, or a custom value) |
  | Skip Synology system folders | `@eaDir`, `#recycle`, `#snapshot`, `.SynologyWorkingDirectory` |
  | Custom exclude patterns | user patterns, one per line |

- **Mirror.** Deletes extra files at the destination with `--delete`, during the sync.
  - **ON by default** (1.9.3+).
- **"Send notification upon completion" checkbox.**
  - In the bottom action bar.
  - **Off by default** and saved with the job (`options.notify`).
- **Draft tabs.**
  - Several sync setups can be open at once. Each tab is named after its source folder.
  - "New sync" opens a fresh tab without clearing the others.
  - "Clear" resets only the current tab.
- **Eject button** on the drive pane.
  - Uses the saved auto-eject admin account if there is one; otherwise it asks for DSM admin credentials.

### Preview (dry run)
- Live counting while the preview scans.
- Runs inside its own tab, so you can switch to other jobs meanwhile.
- **GUI report.**
  - Stat cards: new, updated, to delete (with **total size of the files to delete**, 1.9.4+), unchanged.
  - Filter chips, and a folder tree with a reason per file ("newer at source", "size differs", …).
  - Search.
  - The Rules section lists the user's exclusions (not the system folders).
- A folder that will be deleted shows once, on its own row, with a "Will delete" tag.

### Running jobs
- **Parallel runs**: default 3, settable 1–8.
  - Folder conflicts between running jobs are detected; the conflicting job queues.
- **Progress.**
  - A pre-scan dry run gives the total bytes to copy.
  - Per-file progress comes from watching rsync's temp file (`.name.XXXXXX`) once a second.
  - Works with rsync 3.1.2.
- **Pause/resume.** SIGSTOP/SIGCONT on the rsync process group.
- **Cancel.**
- Jobs keep running when the browser is closed. They run in the server process.
- **Resume big files.**
  - Interrupted transfers are kept in `.usbsync-partial`.
  - The next run continues them with `--append-verify --partial`.
- **Mirror safety.** rsync stops all deletions after any read error ("IO error encountered -- skipping file deletion").
  - USB Sync pre-excludes unreadable source folders so the rest can still be mirrored.
  - A warning explains when deletions were skipped.
- **Perishable filters** (`--filter=-p …`) for system and user excludes.
  - Excluded junk (`@eaDir` etc.) no longer blocks deleting its parent folder ("cannot delete non-empty directory").
- Drives that can't set file times (NTFS/exFAT owned by another user):
  - "failed to set times" errors are counted as notes, not failures.
  - `-r` is used instead of `-rt` when needed.

### Saved jobs
- Saved with name, folders, pairs, exclusions and options.
- Editable even when the job's drive is unplugged.
- **Schedules**: none / daily / weekly at a time.
- **Run when the drive is plugged in** (per-job tick, global on/off in Settings).
  - Sends a notification titled **"usbsync will start in N seconds"** naming the drive and job.
  - Waits the global delay (default 60 s, 0–3600), then starts. It can be cancelled from Activity during the wait.
  - Drives already attached when the app starts are ignored.
- **Drive identity** is a hardware ID from sysfs, plus the partition:
  - USB `idVendor:idProduct:serial`, or
  - model + size, or
  - wwid.
  - Legacy `mnt:` IDs are migrated only if the drive name matches. A job never fires for a different drive.

### Activity / History
- Live cards for active runs and a queue.
- History keeps the **last 50 runs**, each with a log and a per-file report.
- Re-run from History.

### Notifications (global, Settings)
- **Channels.**
  - Gmail SMTP with an app password: STARTTLS 587 or TLS 465.
  - Pushbullet with an access token.
- **Switches:** Successful syncs / Synced with warnings / Failed syncs.
- **Never sent for** previews or cancelled runs.
- **Who triggers them.**
  - Hand-started runs (Sync now, Run from Jobs, Re-run) notify **only if the job's "Send notification upon completion" box is ticked**.
  - Scheduled and plugged-in runs always notify, subject to the switches.
- **Message contents:**
  - job name and outcome;
  - copied size and file count;
  - problem count and reason (for warnings);
  - Mirror-skip warnings;
  - total size of the job;
  - auto-eject result.
  - No URL.
- Each channel has a test button.

### Auto-eject (global, Settings, 1.9.6)
- "Eject the USB drive when a sync finishes" applies to all jobs.
- Needs a **DSM administrator account saved in Settings**, because DSM lets only admins eject.
  - The account is checked on save.
  - With 2FA, a code is entered once and DSM trusts USB Sync (device token), so later ejects need no code.
- **Skipped** for previews, cancelled runs, and while another job is using the drive or about to start on it.
- The result is appended to the run message and the notification.

### Settings, other
- Require DSM sign-in: off by default.
  - Local network, VPN and Tailscale (100.64/10) clients need no login.
  - Public IPs always need an admin login.
- **Export/import of all settings and jobs** (1.9.7: password-protected).
  - Export asks for a **backup password** (8+ characters, typed twice).
  - The Gmail app password, Pushbullet token, eject account password and DSM trusted-device token are removed from the readable part and stored encrypted in `"secrets"`.
  - Jobs and other settings stay readable.
  - Import asks for the password. A wrong password is rejected and counts toward the sign-in failure lockout.
  - **"Import without passwords"** keeps the secrets already saved on this NAS. Any channel left without one (Gmail, Pushbullet, auto-eject) is switched off, and the import says so.
  - Files from older versions (plain secrets, no `"secrets"` block) still import as before.
- The locked-folders banner can be dismissed forever.

---

## 2. Structure

```
usbsync/
├── PROJECT_NOTES.md        this file
├── README.md               short install/usage notes
├── build.sh                builds the SPK (and the source zip)
├── src/                    Go module (single static binary, web UI embedded)
│   ├── main.go             HTTP routes, auth/sessions, DSM Web API sign-in (dsmOpen/dsmOpenDevice), /api/me, version
│   ├── runner.go           Run/Runner: queue, parallel runs, conflicts, rsync execution, progress, pause, history
│   ├── jobs.go             Job/Pair/Options types, validation, saved jobs, drive-ID migration, scheduler
│   ├── fsinfo.go           shares/drives listing (hardware IDs via sysfs), folder listing, permission checks
│   ├── settings.go         Settings (notify, auto-run, auto-eject + eject account), secret hiding, LAN detection
│   ├── notify.go           Gmail SMTP + Pushbullet, message text, notifyRun rules
│   ├── autorun.go          drive watcher (5 s poll), plugged-in countdown/cancel
│   ├── eject.go            DSM eject API, saved eject account, afterRun (auto-eject → notify)
│   ├── resume.go           partial-file tracking and --append-verify resume
│   ├── report.go           per-run .items index → folder tree for the report UI
│   ├── backup.go           settings+jobs export/import, secrets encrypted with a backup password
│   └── web/index.html      entire UI (vanilla JS/CSS, no build step)
├── spk/
│   ├── conf/privilege      run-as: package (unprivileged "USBSync" user)
│   ├── conf/resource       port-config → app/usbsync.sc (firewall entry for 8765/tcp)
│   ├── ui/config           DSM main-menu URL app entry (port 8765)
│   ├── usbsync.sc          firewall service definition
│   └── scripts/            start-stop-status (+ pre/post inst/upgrade/uninst hooks)
└── tools/
    ├── icon.py             generates package/UI icons
    └── fakebin/rsync       test shim
```

### Runtime data (on the NAS)
These files live in `/var/packages/USBSync/var/`:

| File | Contents |
|---|---|
| `settings.json` | chmod 600; holds the secrets |
| `jobs.json` | saved jobs |
| `history.json` | run history |
| `resume.json` | partial-file tracking |
| `logs/<run>.log`, `logs/<run>.items` | per-run log and per-file results |
| `usbsync.log` | service log |
| `usbsync.env` | optional, e.g. `PORT=9000` |

### Main API routes
All of these are under `/api/`.

| Area | Routes |
|---|---|
| Sign-in | `login`, `logout`, `me` |
| Browsing | `shares`, `drives`, `ls`, `mkdir` |
| Jobs and runs | `jobs`, `run`, `cancel`, `pause`, `status`, `logs/{id}`, `runs/{id}`, `runs/{id}/tree`, `runs/{id}/rerun` |
| Settings | `settings`, `notify/test`, `backup/export`, `backup/import` |
| Eject and auto-run | `eject`, `eject/account`, `autorun/cancel` |

State-changing requests need the header `X-USBSync: 1` (CSRF guard).

---

## 3. Key decisions (and why)

- **Native SPK with a Go static binary and an embedded single-file UI.** It needs no DSM dependencies and no Node/PHP, it's easy to build, and it's x86_64 only.
- **Runs as the unprivileged package user `USBSync`** (DSM 7 requirement).
  - Shares need Read/Write for "System internal user → USBSync".
  - Ownership and ACLs aren't copied.
  - Root can't be used, so eject goes through the DSM Web API with an admin account.
- **No login on the LAN by default.** The owner asked for this. Internet access still needs a DSM admin login.
- **rsync, not a custom copier.**
  - Base args: `-rt -ii --out-format="%i %l %n" --info=progress2 --stats --outbuf`.
  - `--partial-dir=.usbsync-partial` on real runs.
  - `--ignore-errors` is **not** used, so Mirror stays safe on read errors.
- **Mirror deletes during the transfer** (rsync's default delete-during).
- **Preview delete sizes** are read from the destination with `lstat`, because rsync reports deleted files as 0 bytes. A real run's "Files deleted" box shows the count only.
- **Folders aren't counted as files** in the delete count.
- **Hardware drive IDs** stop a job from firing for the wrong drive. This came from a bug where a WD My Book triggered a SanDisk job.
- **Notifications are global**, with one rule set for all jobs. The per-job box only gates hand-started runs.
- **History is capped at 50.**
- **Secrets on the NAS are stored in plain text** in a 600-permission file. Encrypting them there adds little, because the key would sit on the same NAS.
- **Exports encrypt only the secrets, with a user-chosen backup password** (1.9.7).
  - Method: AES-256-GCM, with the key derived by PBKDF2-HMAC-SHA256 (600,000 rounds, random 16-byte salt, fixed associated data "USBSync backup secrets v1"). Go 1.24 standard library only.
  - The owner chose this over leaving secrets out, or tying the key to the DSM password.
  - Only the secrets are encrypted, so a forgotten password still lets jobs and settings be restored.
- **Auto-eject is a global switch, not per job.** The owner asked for this in 1.9.6. An earlier per-job prototype was dropped.
- Screenshots can't be shown in Package Center for manual installs; that needs a package source server (`snapshot` field). The info file has `description` and `changelog`, and could add `maintainer_url`, `support_url` or `helpurl`.

---

## 4. Build setup

- **Requirements:** Go 1.22+ (built with Go 1.24), Python 3 (icons), `tar`, `gzip`, `md5sum`, `zip`.
- **Build:** run `./build.sh`.
  1. Generates icons.
  2. Builds `src` with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=…"`.
  3. Packs `package.tgz` (bin, ui, app).
  4. Writes `INFO`.
  5. Outputs `dist/USBSync-<VERSION>-x86_64.spk`.
  6. Writes the source zip `dist/USBSync-<version>-source.zip`, which includes `PROJECT_NOTES.md`.
- **INFO highlights:** `arch="x86_64 epyc7002"`, `os_min_ver="7.0-40000"`, `adminport="8765"`, `silent_upgrade="yes"`, `thirdparty="yes"`.
- **Versioning:** `VERSION="X.Y.Z-NNNN"` in `build.sh`, and keep `version = "X.Y.Z"` in `src/main.go` in step. Bump the build number each release.
- **Install:**
  1. Package Center › Manual Install › choose the .spk. Allow "Any publisher" once.
  2. Grant shared-folder permissions to `USBSync`.
- **Local testing** (no NAS):

  ```
  USBSYNC_TEST_VOLUMES=<dir> USBSYNC_TEST_DRIVES=<dir>[,…] ./usbsync -listen 127.0.0.1:8765 -data ./data [-noauth]
  ```

  - Other test hooks: `USBSYNC_TEST_SYSFS`, `USBSYNC_TEST_FSTYPE`, `USBSYNC_TEST_NOTIMES`, `USBSYNC_PUSHBULLET_URL`.

---

## 5. Version history (recent)

| Version | Changes |
|---|---|
| 1.9.7 | Password-protected settings export: secrets encrypted with AES-256-GCM using a backup password. Import asks for the password, or imports without passwords. |
| 1.9.6 | Global auto-eject with a saved DSM admin account. Per-job "Send notification upon completion" box. Lock-icon tooltip. Fixed stray vertical scroll arrows (drive bar and tab bar). Richer description and changelog in INFO. |
| 1.9.5 | Notifications for syncs that finish with warnings ("Synced with warnings" switch). |
| 1.9.4 | Preview shows the total size of the files to delete. Deleted folders show once. |
| 1.9.3 | Mirror on by default. |
| 1.9.2 | Perishable exclude filters (fixes "cannot delete non-empty directory"). |
| 1.9.1 | Mirror keeps working past unreadable source folders, with a warning when rsync skips deletions. |
| 1.9.0 | Multiple folder pairs per job. |
| 1.0–1.8 | Hardware drive IDs, editing jobs while unplugged, global plug-in start delay, export/import, sizes in notifications, plug-in auto-run, resuming big files, Gmail/Pushbullet, eject, unticked sub-folders as exclusions, touch-friendly UI, pause, parallel jobs/tabs, live preview, re-run, progress, folder-to-folder copy mode, comparison rules, GUI report, saved jobs. |
