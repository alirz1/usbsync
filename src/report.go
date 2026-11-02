package main

import (
	"bufio"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// A run's per-file results (logs/<id>.items, one "status\treasons\tsize\tpath"
// line per item) are indexed into a directory tree on demand for the report UI.

type counts struct {
	New       int   `json:"new"`
	Update    int   `json:"update"`
	Delete    int   `json:"delete"`
	Unchanged int   `json:"unchanged"`
	Bytes     int64 `json:"bytes"` // bytes to copy (new + updated)
}

type fileItem struct {
	Name    string   `json:"name"`
	Path    string   `json:"path"`
	Status  string   `json:"status"`
	Reasons []string `json:"reasons,omitempty"`
	Size    int64    `json:"size"`
	IsDir   bool     `json:"isDir,omitempty"`
}

type dirNode struct {
	New   bool
	Gone  bool // the folder itself is deleted (Mirror)
	Files []fileItem
	Dirs  map[string]bool
	C     counts
}

type runIndex struct {
	dirs map[string]*dirNode
}

var (
	idxMu    sync.Mutex
	idxCache = map[string]*runIndex{}
	idxOrder []string
)

func (ix *runIndex) ensure(p string) *dirNode {
	if n := ix.dirs[p]; n != nil {
		return n
	}
	n := &dirNode{Dirs: map[string]bool{}}
	ix.dirs[p] = n
	if p != "" {
		parent := path.Dir(p)
		if parent == "." {
			parent = ""
		}
		ix.ensure(parent).Dirs[p] = true
	}
	return n
}

func (c *counts) add(status string, size int64) {
	switch status {
	case "new":
		c.New++
		c.Bytes += size
	case "update":
		c.Update++
		c.Bytes += size
	case "delete":
		c.Delete++
	case "unchanged":
		c.Unchanged++
	}
}

func buildIndex(id string) (*runIndex, error) {
	f, err := os.Open(runFile(id, ".items"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ix := &runIndex{dirs: map[string]*dirNode{}}
	ix.ensure("")
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "\t", 4)
		if len(parts) < 4 {
			continue
		}
		status, size, p := parts[0], parts[2], parts[3]
		isDir := strings.HasSuffix(p, "/")
		p = strings.TrimSuffix(p, "/")
		if p == "" || p == "." {
			continue
		}
		if status == "newdir" || status == "dir" {
			n := ix.ensure(p)
			n.New = status == "newdir"
			continue
		}
		parent := path.Dir(p)
		if parent == "." {
			parent = ""
		}
		sz, _ := strconv.ParseInt(size, 10, 64)
		var reasons []string
		if parts[1] != "" {
			reasons = strings.Split(parts[1], ",")
		}
		if isDir {
			if status == "delete" {
				ix.ensure(p).Gone = true // shown on the folder's own row
			}
			continue
		}
		ix.ensure(parent).Files = append(ix.ensure(parent).Files, fileItem{Name: path.Base(p), Path: p, Status: status, Reasons: reasons, Size: sz, IsDir: isDir})
		for d := parent; ; {
			ix.dirs[d].C.add(status, sz)
			if d == "" {
				break
			}
			if d = path.Dir(d); d == "." {
				d = ""
			}
		}
	}
	return ix, sc.Err()
}

func getIndex(id string) (*runIndex, error) {
	idxMu.Lock()
	defer idxMu.Unlock()
	if ix := idxCache[id]; ix != nil {
		return ix, nil
	}
	ix, err := buildIndex(id)
	if err != nil {
		return nil, err
	}
	idxCache[id] = ix
	idxOrder = append(idxOrder, id)
	for len(idxOrder) > 3 {
		delete(idxCache, idxOrder[0])
		idxOrder = idxOrder[1:]
	}
	return ix, nil
}

func matchFilter(filter, status string) bool {
	switch filter {
	case "all":
		return true
	case "changes":
		return status == "new" || status == "update" || status == "delete"
	default:
		return status == filter
	}
}

func countFor(filter string, c counts) int {
	switch filter {
	case "all":
		return c.New + c.Update + c.Delete + c.Unchanged
	case "changes":
		return c.New + c.Update + c.Delete
	case "new":
		return c.New
	case "update":
		return c.Update
	case "delete":
		return c.Delete
	case "unchanged":
		return c.Unchanged
	}
	return 0
}

type dirView struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	New    bool   `json:"new"`
	Gone   bool   `json:"gone"`
	Counts counts `json:"counts"`
}

func handleRunTree(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		errJSON(w, 400, "bad id")
		return
	}
	if run := runner.find(id); run == nil || run.End == nil {
		errJSON(w, 409, "The report is available once the run has finished.")
		return
	}
	ix, err := getIndex(id)
	if err != nil {
		errJSON(w, 404, "No file details were recorded for this run.")
		return
	}
	q := r.URL.Query()
	filter := q.Get("filter")
	if filter == "" {
		filter = "changes"
	}
	limit := 1000
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && n <= 5000 {
		limit = n
	}

	// search: flat list across the whole tree
	if term := strings.ToLower(strings.TrimSpace(q.Get("q"))); term != "" {
		var out []fileItem
		total := 0
		for _, n := range ix.dirs {
			for _, f := range n.Files {
				if matchFilter(filter, f.Status) && strings.Contains(strings.ToLower(f.Path), term) {
					total++
					if len(out) < limit {
						out = append(out, f)
					}
				}
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
		writeJSON(w, 200, map[string]any{"files": out, "dirs": []dirView{}, "more": total - len(out), "search": true})
		return
	}

	dir := strings.Trim(q.Get("dir"), "/")
	n := ix.dirs[dir]
	if n == nil {
		writeJSON(w, 200, map[string]any{"files": []fileItem{}, "dirs": []dirView{}, "more": 0})
		return
	}
	dirs := []dirView{}
	for d := range n.Dirs {
		c := ix.dirs[d]
		if filter != "all" && countFor(filter, c.C) == 0 && !(filter == "changes" && c.New) && !(c.Gone && (filter == "changes" || filter == "delete")) {
			continue
		}
		dirs = append(dirs, dirView{Name: path.Base(d), Path: d, New: c.New, Gone: c.Gone, Counts: c.C})
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name) })
	files := []fileItem{}
	total := 0
	for _, f := range n.Files {
		if matchFilter(filter, f.Status) {
			total++
			files = append(files, f)
		}
	}
	sort.Slice(files, func(i, j int) bool { return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name) })
	more := 0
	if len(files) > limit {
		more = len(files) - limit
		files = files[:limit]
	}
	writeJSON(w, 200, map[string]any{"dir": dir, "dirs": dirs, "files": files, "more": more, "counts": n.C})
}
