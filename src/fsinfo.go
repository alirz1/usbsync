package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type Share struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Volume   string `json:"volume"`
	Readable bool   `json:"readable"`
	Writable bool   `json:"writable"`
}

type Drive struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Mount     string `json:"mount"`
	ShareName string `json:"shareName"`
	Bus       string `json:"bus"`
	Device    string `json:"device"`
	FSType    string `json:"fstype"`
	UUID      string `json:"uuid"`
	Label     string `json:"label"`
	Model     string `json:"model"`
	Total     uint64 `json:"total"`
	Free      uint64 `json:"free"`
	Readable  bool   `json:"readable"`
	Writable  bool   `json:"writable"`
}

type Entry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Readable bool   `json:"readable"`
	IsFile   bool   `json:"isFile,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

const maxListFiles = 2000

var (
	volRe      = regexp.MustCompile(`^/volume\d+$`)
	extMountRe = regexp.MustCompile(`^/volume(USB|SATA)\d+/[^/]+$`)
	octalRe    = regexp.MustCompile(`\\([0-7]{3})`)
	hexRe      = regexp.MustCompile(`\\x([0-9a-fA-F]{2})`)
)

func canRead(p string) bool  { return syscall.Access(p, 5) == nil } // R_OK|X_OK
func canWrite(p string) bool { return syscall.Access(p, 3) == nil } // W_OK|X_OK

func testList(env string) []string {
	v := os.Getenv(env)
	if v == "" {
		return nil
	}
	return strings.Split(v, ":")
}

func volNum(p string) int {
	n, _ := strconv.Atoi(strings.TrimLeft(filepath.Base(p), "volumeUSBSATA"))
	return n
}

func skipShareName(n string) bool {
	return n == "" || n == "lost+found" || strings.HasPrefix(n, "@") ||
		strings.HasPrefix(n, ".") || strings.HasPrefix(n, "#") || strings.HasPrefix(n, "aquota")
}

func listShares() []Share {
	vols := testList("USBSYNC_TEST_VOLUMES")
	if vols == nil {
		m, _ := filepath.Glob("/volume*")
		for _, v := range m {
			if volRe.MatchString(v) {
				vols = append(vols, v)
			}
		}
	}
	sort.Slice(vols, func(i, j int) bool { return volNum(vols[i]) < volNum(vols[j]) })
	out := []Share{}
	for _, v := range vols {
		ents, err := os.ReadDir(v)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if skipShareName(e.Name()) {
				continue
			}
			p := filepath.Join(v, e.Name())
			fi, err := os.Stat(p)
			if err != nil || !fi.IsDir() {
				continue
			}
			out = append(out, Share{Name: e.Name(), Path: p, Volume: filepath.Base(v), Readable: canRead(p), Writable: canWrite(p)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Volume != out[j].Volume {
			return volNum(out[i].Volume) < volNum(out[j].Volume)
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func unescapeMount(s string) string {
	return octalRe.ReplaceAllStringFunc(s, func(m string) string {
		n, _ := strconv.ParseUint(m[1:], 8, 8)
		return string(rune(n))
	})
}

func linkMap(dir string) map[string]string {
	m := map[string]string{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return m
	}
	for _, e := range ents {
		t, err := filepath.EvalSymlinks(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		m[filepath.Base(t)] = hexRe.ReplaceAllStringFunc(e.Name(), func(h string) string {
			n, _ := strconv.ParseUint(h[2:], 16, 8)
			return string(rune(n))
		})
	}
	return m
}

func readTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

var sysRoot = envOr("USBSYNC_TEST_SYSFS", "/sys")

var idSafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// hardwareID identifies a partition by the physical drive it is on: the USB
// device's vendor/product/serial (or model + capacity when there is no
// serial) plus the partition number. Unlike the mount slot (usbshare1…), it
// stays the same whichever port or order the drive is plugged in, and two
// different drives never share it.
func hardwareID(dev string) string {
	if dev == "" {
		return ""
	}
	real, err := filepath.EvalSymlinks(filepath.Join(sysRoot, "class/block", dev))
	if err != nil {
		return ""
	}
	part := readTrim(filepath.Join(real, "partition"))
	disk := real
	if part != "" {
		disk = filepath.Dir(real)
	} else {
		part = "0"
	}
	var key string
	for d := disk; d != "/" && d != "." && len(d) > len(sysRoot); d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "idVendor")); err == nil {
			serial := readTrim(filepath.Join(d, "serial"))
			if serial != "" {
				key = readTrim(filepath.Join(d, "idVendor")) + ":" + readTrim(filepath.Join(d, "idProduct")) + ":" + serial
			}
			break
		}
	}
	if key == "" { // no USB serial (or eSATA): model + exact capacity
		model := strings.Join(strings.Fields(readTrim(filepath.Join(disk, "device/vendor"))+" "+readTrim(filepath.Join(disk, "device/model"))), " ")
		size := readTrim(filepath.Join(disk, "size"))
		if wwid := readTrim(filepath.Join(disk, "device/wwid")); wwid != "" {
			key = wwid
		} else if model != "" && size != "" {
			key = model + ":" + size
		} else {
			return ""
		}
	}
	return "hw:" + idSafe.ReplaceAllString(key, "_") + ":p" + part
}

func blockModel(dev string) string {
	if dev == "" {
		return ""
	}
	real, err := filepath.EvalSymlinks(filepath.Join(sysRoot, "class/block", dev))
	if err != nil {
		return ""
	}
	if _, err := os.Stat(filepath.Join(real, "partition")); err == nil {
		real = filepath.Dir(real)
	}
	return strings.Join(strings.Fields(readTrim(filepath.Join(real, "device/vendor"))+" "+readTrim(filepath.Join(real, "device/model"))), " ")
}

func listDrives() []Drive {
	type mnt struct{ dev, mount, fstype string }
	var mounts []mnt
	if t := testList("USBSYNC_TEST_DRIVES"); t != nil {
		for _, m := range t {
			dev := ""
			if i := strings.Index(m, "@"); i > 0 { // test mode: mount@device
				m, dev = m[:i], "/dev/"+m[i+1:]
			}
			if _, err := os.Stat(m); err != nil {
				continue // test mode: a missing folder = drive unplugged
			}
			mounts = append(mounts, mnt{dev, m, envOr("USBSYNC_TEST_FSTYPE", "ext4")})
		}
	} else {
		b, _ := os.ReadFile("/proc/mounts")
		seen := map[string]bool{}
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 3 {
				continue
			}
			mp := unescapeMount(f[1])
			if extMountRe.MatchString(mp) && !seen[mp] {
				seen[mp] = true
				mounts = append(mounts, mnt{f[0], mp, f[2]})
			}
		}
	}
	uuids := linkMap("/dev/disk/by-uuid")
	labels := linkMap("/dev/disk/by-label")
	out := []Drive{}
	for _, m := range mounts {
		d := Drive{Mount: m.mount, Device: m.dev, FSType: m.fstype, ShareName: filepath.Base(m.mount), Bus: "USB"}
		if strings.Contains(m.mount, "/volumeSATA") {
			d.Bus = "eSATA"
		}
		// DSM mounts /volumeUSB<N>/usbshare and names the shared folder usbshare<N>
		n := volNum(filepath.Dir(m.mount))
		if (d.ShareName == "usbshare" || d.ShareName == "satashare") && n > 0 {
			d.ShareName += strconv.Itoa(n)
		}
		devName := ""
		if m.dev != "" {
			real, err := filepath.EvalSymlinks(m.dev)
			if err != nil {
				real = m.dev
			}
			devName = filepath.Base(real)
			if strings.HasPrefix(devName, "fuse") || !strings.HasPrefix(m.dev, "/dev/") {
				devName = "" // not a block device (e.g. some FUSE mounts)
			}
		}
		d.UUID = uuids[devName]
		d.Label = labels[devName]
		d.Model = blockModel(devName)
		var st syscall.Statfs_t
		if syscall.Statfs(m.mount, &st) == nil {
			d.Total = st.Blocks * uint64(st.Bsize)
			d.Free = st.Bavail * uint64(st.Bsize)
		}
		d.Readable, d.Writable = canRead(m.mount), canWrite(m.mount)
		switch hw := hardwareID(devName); {
		case d.UUID != "":
			d.ID = "uuid:" + d.UUID
		case hw != "":
			d.ID = hw
		default:
			d.ID = "mnt:" + m.mount // last resort: the mount slot
		}
		switch {
		case d.Label != "":
			d.Name = d.Label
		case d.Model != "":
			d.Name = d.Model
		default:
			d.Name = fmt.Sprintf("%s drive %d", d.Bus, n)
		}
		out = append(out, d)
	}
	return out
}

func isPosixFS(t string) bool {
	switch strings.ToLower(t) {
	case "ext2", "ext3", "ext4", "btrfs", "xfs", "f2fs":
		return true
	}
	return false
}

func underOrEqual(p, root string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}

// checkAllowed ensures a path lives inside a NAS share or an external drive.
func checkAllowed(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", errors.New("path must be absolute")
	}
	p = filepath.Clean(p)
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	var roots []string
	for _, s := range listShares() {
		roots = append(roots, s.Path)
	}
	for _, d := range listDrives() {
		roots = append(roots, d.Mount)
	}
	for _, r := range roots {
		rr, err := filepath.EvalSymlinks(r)
		if err != nil {
			rr = r
		}
		if underOrEqual(real, rr) {
			return p, nil
		}
	}
	return "", errors.New("path is outside the NAS shares and external drives")
}

func hiddenDir(n string) bool {
	return strings.HasPrefix(n, "@") || n == "#recycle" || n == "#snapshot" || n == ".rsync-partial" || n == partialDirName
}

// listDir returns sub-folders, and (withFiles) up to maxListFiles files.
func listDir(p string, withFiles bool) (dirs []Entry, files []Entry, moreFiles int, err error) {
	ents, err := os.ReadDir(p)
	if err != nil {
		return nil, nil, 0, err
	}
	dirs, files = []Entry{}, []Entry{}
	for _, e := range ents {
		cp := filepath.Join(p, e.Name())
		if e.IsDir() {
			if !hiddenDir(e.Name()) {
				dirs = append(dirs, Entry{Name: e.Name(), Path: cp, Readable: canRead(cp)})
			}
			continue
		}
		if !withFiles || !e.Type().IsRegular() {
			continue
		}
		if len(files) >= maxListFiles {
			moreFiles++
			continue
		}
		var size int64
		if fi, err := e.Info(); err == nil {
			size = fi.Size()
		}
		files = append(files, Entry{Name: e.Name(), Path: cp, Readable: true, IsFile: true, Size: size})
	}
	byName := func(l []Entry) {
		sort.Slice(l, func(i, j int) bool { return strings.ToLower(l[i].Name) < strings.ToLower(l[j].Name) })
	}
	byName(dirs)
	byName(files)
	return dirs, files, moreFiles, nil
}

// ---------- handlers ----------

func handleShares(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, listShares()) }
func handleDrives(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, listDrives()) }

func handleLs(w http.ResponseWriter, r *http.Request) {
	p, err := checkAllowed(r.URL.Query().Get("path"))
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	dirs, files, more, err := listDir(p, r.URL.Query().Get("files") == "1")
	if err != nil {
		if os.IsPermission(err) {
			errJSON(w, 403, "No permission to read this folder.")
			return
		}
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"path": p, "entries": dirs, "files": files, "moreFiles": more})
}

func handleMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Parent string `json:"parent"`
		Name   string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, 400, "bad request")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || name == "." || name == ".." || len(name) > 255 || strings.ContainsAny(name, "/\x00") {
		errJSON(w, 400, "Invalid folder name.")
		return
	}
	parent, err := checkAllowed(req.Parent)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	np := filepath.Join(parent, name)
	if err := os.Mkdir(np, 0o777); err != nil {
		if os.IsPermission(err) {
			errJSON(w, 403, "No permission to create a folder here.")
			return
		}
		errJSON(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"path": np})
}
