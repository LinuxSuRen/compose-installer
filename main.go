// compose-installer: a generic, docker compose based product packager and
// offline installer (Go backend + embedded web UI).
//
// Modes:
//  1. pack: on a machine with network access, pull every image and deployment
//     file of a compose project into a single tar.gz bundle;
//  2. install: on the local machine or a remote SSH Ubuntu host, install
//     docker/compose from the bundle, import the images, write .env and start
//     docker compose.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

//go:embed all:web/dist
var webDist embed.FS

// version is injected at build time: -ldflags "-X main.version=<git short hash or tag>"
var version = "dev"

func main() {
	addr := flag.String("addr", defaultAddr, "HTTP listen address")
	openBrowser := flag.Bool("open-browser", true, "open the browser after startup (disable with -open-browser=false)")
	// Container deliverables hide the "Pack" tab by default: setting ENV
	// HIDE_PACK_TAB=1 in the image is enough. Pass -hide-pack-tab=false to
	// restore the tab inside a container. The frontend reveal method is
	// documented in App.vue (a console hint plus typing "openthebox" on an
	// empty area of the page).
	hidePackTab := flag.Bool("hide-pack-tab", os.Getenv("HIDE_PACK_TAB") != "",
		"hide the \"Pack\" tab of the UI (for container deliverables; equivalent to env HIDE_PACK_TAB=1)")
	showVer := flag.Bool("v", false, "print the version and exit")
	flag.Parse()
	if *showVer {
		fmt.Println("compose-installer", version)
		return
	}

	// doctor subcommand: standalone host-level deployment diagnostics —
	// container egress (NAT), kernel quotas (inotify / file-max / somaxconn /
	// overcommit / ephemeral port range), docker version and cgroup mode
	// (read-only; prints root causes and fix commands).
	// Usage: compose-installer doctor (must run on the docker host).
	if flag.NArg() > 0 && flag.Arg(0) == "doctor" {
		os.Exit(runDoctor(context.Background(), NewLocalRunner(), os.Stdout))
	}

	store := NewJobStore()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{
			"status":   "ok",
			"version":  version,
			"hidePack": strconv.FormatBool(*hidePackTab),
		})
	})

	// Directory browser (frontend path picker): GET /api/fs?path=...
	mux.HandleFunc("GET /api/fs", handleFs)

	// Default install directory (frontend cross-field default, resolved by the
	// same helper the installer falls back to):
	// GET /api/default-install-dir?name=myapp&mode=remote&user=ubuntu
	mux.HandleFunc("GET /api/default-install-dir", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		dir := defaultInstallDirFor(q.Get("name"), q.Get("mode"), q.Get("user"))
		writeJSON(w, map[string]string{"dir": dir})
	})

	// Host machine info (install page detects the local OS): GET /api/host-info
	mux.HandleFunc("GET /api/host-info", func(w http.ResponseWriter, r *http.Request) {
		_, bashErr := exec.LookPath("bash")
		resp := map[string]any{
			"os":      runtime.GOOS,
			"osLabel": platformLabel(runtime.GOOS),
			// Geteuid is always -1 on Windows, which lands in non-root naturally.
			"isRoot": os.Geteuid() == 0,
			"bash":   bashErr == nil,
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			resp["home"] = home
		}
		writeJSON(w, resp)
	})

	// Project overview (pack page): GET /api/project?projectDir=...
	// Describes every product of the project: environments, per-environment
	// version lists / latest tags / compose profiles. An empty projectDir
	// auto-detects the project root. Zero-config projects (no
	// compose-installer.json) report a single "prod" environment with null
	// versions and auto-discovered profiles. Every array is non-nil so JSON
	// consumers always see [] instead of null.
	mux.HandleFunc("GET /api/project", func(w http.ResponseWriter, r *http.Request) {
		projectDir := r.URL.Query().Get("projectDir")
		var err error
		if projectDir == "" {
			if projectDir, err = resolveProjectDir(""); err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
		}
		cfg, err := loadInstallerConfig(projectDir)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		products := make(map[string]any, len(cfg.Products))
		for name, p := range cfg.Products {
			envs := p.Envs()
			versions := make(map[string]any, len(envs))
			latest := make(map[string]any, len(envs))
			profiles := make(map[string]any, len(envs))
			prodErr := ""
			for _, env := range envs {
				profs := discoverProfiles(projectDir, p, env)
				if profs == nil {
					profs = []string{}
				}
				profiles[env] = profs
				if !p.HasVersions(env) {
					// Unversioned environment: tags are baked into the compose
					// file / env template, so there is no version list.
					versions[env] = nil
					latest[env] = nil
					continue
				}
				vm, verr := loadVersionManifest(filepath.Join(projectDir, p.VersionFileFor(env)))
				if verr != nil {
					// A broken version manifest must not hide the other
					// environments: record the first error on the product and
					// keep going.
					if prodErr == "" {
						prodErr = verr.Error()
					}
					versions[env] = nil
					latest[env] = nil
					continue
				}
				vers := make([]string, 0, len(vm.Versions))
				for v := range vm.Versions {
					vers = append(vers, v)
				}
				sort.Strings(vers)
				versions[env] = vers
				latest[env] = vm.Latest
			}
			var errVal any // nil -> JSON null
			if prodErr != "" {
				errVal = prodErr
			}
			products[name] = map[string]any{
				"installDirName": p.InstallDirName,
				"envs":           envs,
				"versions":       versions,
				"latest":         latest,
				"profiles":       profiles,
				"error":          errVal,
			}
		}
		writeJSON(w, map[string]any{
			"projectDir":     projectDir,
			"defaultProduct": cfg.DefaultProduct,
			"products":       products,
		})
	})

	// Bundle manifest reader (install page shows bundle info):
	// GET /api/bundle-manifest?path=...
	mux.HandleFunc("GET /api/bundle-manifest", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			writeErr(w, http.StatusBadRequest, errors.New("path is required"))
			return
		}
		m, err := readBundleManifest(path)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, m)
	})

	mux.HandleFunc("POST /api/pack", func(w http.ResponseWriter, r *http.Request) {
		var req PackRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		job := store.Create("pack")
		go func() {
			if err := runPack(context.Background(), job, req); err != nil {
				log.Printf("pack job %s failed: %v", job.ID, err)
			}
		}()
		writeJSON(w, map[string]string{"jobId": job.ID})
	})

	mux.HandleFunc("POST /api/install", func(w http.ResponseWriter, r *http.Request) {
		var req InstallRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if req.BundlePath == "" {
			writeErr(w, http.StatusBadRequest, errors.New("bundlePath must not be empty"))
			return
		}
		job := store.Create("install")
		go func() {
			if err := runInstall(context.Background(), job, store, req); err != nil {
				log.Printf("install job %s failed: %v", job.ID, err)
			}
		}()
		writeJSON(w, map[string]string{"jobId": job.ID})
	})

	// Job log polling: GET /api/jobs/{id}?offset=N
	mux.HandleFunc("GET /api/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		job, ok := store.Get(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, errors.New("job not found"))
			return
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		status, lines, total := job.Snapshot(offset)
		start, end := job.Times()
		resp := map[string]any{
			"id": job.ID, "kind": job.Kind, "status": status,
			"err": job.errString(), "lines": lines, "total": total,
			"startedAtMs": start.UnixMilli(),
		}
		if !end.IsZero() {
			resp["finishedAtMs"] = end.UnixMilli()
			resp["durationMs"] = end.Sub(start).Milliseconds()
		}
		writeJSON(w, resp)
	})

	// Embedded frontend (SPA fallback to index.html)
	dist, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		log.Fatal(err)
	}
	fileServer := http.FileServer(http.FS(dist))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" {
			if _, err := fs.Stat(dist, p); err == nil {
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		index, _ := fs.ReadFile(dist, "index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(index)
	})

	// Port policy: with the default address (no explicit -addr), when the
	// default port is taken, keep trying the next ports (up to +20); with an
	// explicit -addr, use it strictly and exit on failure.
	addrSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "addr" {
			addrSet = true
		}
	})

	host, portStr, err := net.SplitHostPort(*addr)
	if err != nil {
		log.Fatalf("invalid listen address: %v", err)
	}
	basePort, _ := strconv.Atoi(portStr)

	var ln net.Listener
	for off := 0; off <= 20; off++ {
		p := basePort + off
		l, lerr := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p)))
		if lerr == nil {
			ln = l
			portStr = strconv.Itoa(p)
			break
		}
		if off == 0 {
			if addrSet {
				log.Fatalf("listen on %s failed: %v", *addr, lerr)
			}
			log.Printf("default port %d unavailable (%v), trying subsequent ports...", p, lerr)
		}
	}
	if ln == nil {
		log.Fatalf("cannot listen on any of the 20 ports from %s; specify another port with -addr", net.JoinHostPort(host, portStr))
	}

	// Browser URL: wildcard addresses such as 0.0.0.0 are shown/opened as 127.0.0.1
	urlHost := host
	if urlHost == "" || urlHost == "0.0.0.0" || urlHost == "::" {
		urlHost = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(urlHost, portStr)
	log.Printf("compose-installer %s started: %s (working directory %s)", version, url, mustWd())

	if *openBrowser {
		openBrowserPage(url)
	}

	if err := http.Serve(ln, mux); err != nil {
		log.Fatal(err)
	}
}

// openBrowserPage opens the default browser cross-platform; in headless
// environments (a server over SSH) it only logs.
func openBrowserPage(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("failed to open the browser automatically (visit %s manually): %v", url, err)
	}
}

func mustWd() string {
	wd, _ := os.Getwd()
	return wd
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
