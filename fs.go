package main

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// fsEntry is one directory listing entry, used by the frontend path picker.
type fsEntry struct {
	Name    string `json:"name"`
	IsDir   bool   `json:"isDir"`
	Size    int64  `json:"size,omitempty"`
	ModTime string `json:"modTime,omitempty"`
}

// handleFs GET /api/fs?path=...: lists the directory contents on the machine
// running the tool. Local-only (127.0.0.1) usage scenario, so no permission
// tightening is applied; directories first, sorted by name.
func handleFs(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path = home
		} else {
			wd, _ := os.Getwd()
			path = wd
		}
	}
	path = filepath.Clean(path)

	st, err := os.Stat(path)
	if err != nil {
		// The user typed an incomplete path: try to complete the last
		// segment as a prefix inside the parent directory.
		parent, prefix := filepath.Split(path)
		if pst, perr := os.Stat(filepath.Clean(parent)); perr == nil && pst.IsDir() {
			entries, _ := listDir(filepath.Clean(parent))
			for _, e := range entries {
				if strings.HasPrefix(strings.ToLower(e.Name), strings.ToLower(prefix)) {
					cand := filepath.Join(filepath.Clean(parent), e.Name)
					if cst, cerr := os.Stat(cand); cerr == nil {
						path, st = cand, cst
						err = nil
						break
					}
				}
			}
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}

	if !st.IsDir() {
		// The path is a file: dynamically list its parent directory (focus
		// carries the file name), so the frontend preselects that file
		// instead of erroring with "not a directory" and dead-ending the
		// browser.
		parentDir := filepath.Dir(path)
		entries, lerr := listDir(parentDir)
		if lerr != nil {
			writeErr(w, http.StatusBadRequest, lerr)
			return
		}
		parent := filepath.Dir(parentDir)
		if parent == parentDir {
			parent = ""
		}
		writeJSON(w, map[string]any{
			"path": parentDir, "isDir": true, "parent": parent,
			"entries": entries, "roots": fsRoots(),
			"focus": filepath.Base(path),
		})
		return
	}

	entries, err := listDir(path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	parent := filepath.Dir(path)
	if parent == path {
		parent = "" // the root has no parent
	}
	writeJSON(w, map[string]any{
		"path": path, "isDir": true, "parent": parent,
		"entries": entries, "roots": fsRoots(),
	})
}

// fsRoots returns the switchable roots: on Windows the existing drive letters
// (C:\, D:\, ...), "/" on other systems. Used by the frontend path picker for
// cross-drive switching — on Windows, going "up" stops at the drive root; a
// non-technical user cannot reach other drives without a drive-letter bar.
func fsRoots() []string {
	if runtime.GOOS != "windows" {
		return []string{"/"}
	}
	var roots []string
	for c := 'A'; c <= 'Z'; c++ {
		p := string(c) + `:\`
		if _, err := os.Stat(p); err == nil {
			roots = append(roots, p)
		}
	}
	return roots
}

func listDir(path string) ([]fsEntry, error) {
	dirents, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var dirs, files []fsEntry
	for _, d := range dirents {
		info, err := d.Info()
		if err != nil {
			continue // info missing due to permissions or concurrent deletion; skip
		}
		e := fsEntry{Name: d.Name(), IsDir: d.IsDir()}
		if !e.IsDir {
			e.Size = info.Size()
		}
		e.ModTime = info.ModTime().Format("2006-01-02 15:04")
		if e.IsDir {
			dirs = append(dirs, e)
		} else {
			files = append(files, e)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name) })
	sort.Slice(files, func(i, j int) bool { return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name) })
	return append(dirs, files...), nil
}
