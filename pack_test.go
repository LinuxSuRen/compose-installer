package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testLogger is a silent packLogger for tests.
type testLogger struct{}

func (testLogger) Logf(format string, args ...any) {}
func (testLogger) finish(status, errMsg string)    {}

// resolvePackPlatform: target platform resolution (empty/auto follows the
// packing machine).
func TestResolvePackPlatform(t *testing.T) {
	auto := "linux/" + runtime.GOARCH
	for _, in := range []string{"", "  ", "auto"} {
		got, err := resolvePackPlatform(in)
		if err != nil || got != auto {
			t.Errorf("resolvePackPlatform(%q) = %q, %v; want %q", in, got, err, auto)
		}
	}
	for _, ok := range []string{"linux/amd64", "linux/arm64"} {
		if got, err := resolvePackPlatform(ok); err != nil || got != ok {
			t.Errorf("resolvePackPlatform(%q) = %q, %v; want the input back", ok, got, err)
		}
	}
	for _, bad := range []string{"amd64", "linux/386", "darwin/arm64", "arm"} {
		if _, err := resolvePackPlatform(bad); err == nil {
			t.Errorf("resolvePackPlatform(%q) must fail", bad)
		}
	}
}

// platformDigestFromList: pick the digest per architecture from a
// multi-arch manifest list.
func TestPlatformDigestFromList(t *testing.T) {
	list := []byte(`{"schemaVersion":2,"manifests":[
	  {"digest":"sha256:aaa","platform":{"os":"linux","architecture":"amd64"}},
	  {"digest":"sha256:bbb","platform":{"os":"linux","architecture":"arm64"}},
	  {"digest":"sha256:ccc","platform":{"os":"unknown","architecture":"unknown"}}]}`)
	if d, isList, err := platformDigestFromList(list, "arm64"); err != nil || !isList || d != "sha256:bbb" {
		t.Errorf("arm64 = %q,%v,%v; want sha256:bbb", d, isList, err)
	}
	if d, isList, err := platformDigestFromList(list, "amd64"); err != nil || !isList || d != "sha256:aaa" {
		t.Errorf("amd64 = %q,%v,%v; want sha256:aaa", d, isList, err)
	}
	if _, _, err := platformDigestFromList(list, "386"); err == nil {
		t.Error("missing architecture must fail")
	}
	// Single-manifest image (no manifests array).
	single := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`)
	if _, isList, err := platformDigestFromList(single, "arm64"); err != nil || isList {
		t.Errorf("single manifest = %v,%v; want not-a-list and no error", isList, err)
	}
}

// imageRepoOf: strip :tag (keep registry ports), @digest untouched.
func TestImageRepoOf(t *testing.T) {
	cases := map[string]string{
		"reg.io:5000/ns/app:dev":      "reg.io:5000/ns/app",
		"reg.io/ns/app:latest":        "reg.io/ns/app",
		"reg.io/ns/app":               "reg.io/ns/app",
		"reg.io/ns/app@sha256:abc":    "reg.io/ns/app@sha256:abc",
		"reg.io:5000/ns/app@sha256:x": "reg.io:5000/ns/app@sha256:x",
	}
	for in, want := range cases {
		if got := imageRepoOf(in); got != want {
			t.Errorf("imageRepoOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// extractLocalMountDirs: local mount references of a compose file (runtime
// directories excluded).
func TestExtractLocalMountDirs(t *testing.T) {
	compose := `
services:
  mosquitto:
    volumes:
      - ./mosquitto/mosquitto.conf:/mosquitto/config/mosquitto.conf:ro
      - ./mosquitto/entrypoint.sh:/entrypoint.sh:ro
      - certs:/mosquitto/certs:ro
  backend:
    volumes:
      - ./data/uploads:/app/uploads
  nginx:
    volumes:
      - ./nginx/dev.conf:/etc/nginx/conf.d/default.conf:ro
`
	got := extractLocalMountDirs(compose)
	want := []string{"mosquitto", "nginx"}
	if len(got) != len(want) {
		t.Fatalf("extractLocalMountDirs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("extractLocalMountDirs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// tagEnvVars: component->tag map becomes <COMPONENT>_TAG=tag env vars.
func TestTagEnvVars(t *testing.T) {
	got := tagEnvVars(map[string]string{"backend": "0.0.23", "face-detect": "1.2"})
	want := []string{"BACKEND_TAG=0.0.23", "FACE_DETECT_TAG=1.2"}
	if len(got) != len(want) {
		t.Fatalf("tagEnvVars = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tagEnvVars[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// stripComposeProfiles: trim a compose to the selected profiles and strip
// the profiles markers.
func TestStripComposeProfiles(t *testing.T) {
	const compose = `services:
  robot-controller:
    profiles:
      - mj
    image: ctrl:1
    volumes:
      - ./data:/data
      - ./certs:/certs

  device-robot-rzt:
    profiles:
      - rzt
    image: rzt:1

  mock-data:
    image: mock:1

volumes:
  shared: {}
`
	dir := t.TempDir()
	file := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(file, []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	kept, err := stripComposeProfiles(file, []string{"rzt"})
	if err != nil {
		t.Fatalf("strip: %v", err)
	}
	if kept != 2 { // device-robot-rzt + the profile-less mock-data
		t.Fatalf("kept services = %d, want 2", kept)
	}
	out, _ := os.ReadFile(file)
	s := string(out)
	for _, bad := range []string{"robot-controller", "profiles:", "mj", "ctrl:1"} {
		if strings.Contains(s, bad) {
			t.Errorf("trimmed compose still contains %q:\n%s", bad, s)
		}
	}
	for _, want := range []string{"services:", "device-robot-rzt:", "mock-data:", "volumes:", "rzt:1"} {
		if !strings.Contains(s, want) {
			t.Errorf("trimmed compose misses %q:\n%s", want, s)
		}
	}

	// A selected profile matching no service must fail the pack early.
	if err := os.WriteFile(file, []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := stripComposeProfiles(file, []string{"nonexistent"}); err == nil {
		t.Error("unknown profile must be rejected")
	}
}

// sanitizeComposeFiles: the selected compose file becomes
// docker-compose.yml, sibling environment files are removed.
func TestSanitizeComposeFiles(t *testing.T) {
	root := t.TempDir()
	mk := func(name, content string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Exactly one compose file must remain: docker-compose.yml with the
	// selected file's content.
	assertOnlyCompose := func(dir, wantContent string) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
		if err != nil {
			t.Fatalf("docker-compose.yml missing: %v", err)
		}
		if string(raw) != wantContent {
			t.Fatalf("docker-compose.yml content = %q, want %q", raw, wantContent)
		}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			n := e.Name()
			if (strings.HasPrefix(n, "docker-compose") || strings.HasPrefix(n, "compose.")) && n != "docker-compose.yml" {
				t.Fatalf("unexpected leftover compose file: %s", n)
			}
		}
	}

	// Scenario 1: suffixed environment -> the normalized name is overwritten
	// by the rename (never deleted first).
	mk("docker-compose.yml", "prod")
	mk("docker-compose.dev.yml", "dev")
	mk("docker-compose.test.yml", "test")
	if err := sanitizeComposeFiles(root, "docker-compose.test.yml"); err != nil {
		t.Fatalf("test env: %v", err)
	}
	assertOnlyCompose(root, "test")

	// Scenario 2: prod environment (no suffix already).
	mk("docker-compose.yml", "prod")
	mk("docker-compose.dev.yml", "dev")
	if err := sanitizeComposeFiles(root, "docker-compose.yml"); err != nil {
		t.Fatalf("prod env: %v", err)
	}
	assertOnlyCompose(root, "prod")

	// Scenario 3: a non-canonical selected name (compose.yaml) is renamed
	// too, and .yaml siblings are removed as well.
	mk("compose.yaml", "canonical")
	mk("docker-compose.dev.yaml", "dev-yaml")
	if err := sanitizeComposeFiles(root, "compose.yaml"); err != nil {
		t.Fatalf("compose.yaml: %v", err)
	}
	assertOnlyCompose(root, "canonical")

	// Scenario 4: a missing directory is skipped, not an error.
	if err := sanitizeComposeFiles(filepath.Join(root, "not-exist"), "docker-compose.yml"); err != nil {
		t.Fatalf("missing dir should be skipped, got %v", err)
	}
}

// copyRepoSubset: config-driven whitelist — selected compose, env template,
// extra files, version manifest and compose-referenced mount dirs; nothing
// else ships.
func TestCopyRepoSubset(t *testing.T) {
	root := t.TempDir()
	mk := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Project layout: whitelist entries plus junk that must stay out.
	mk("docker-compose.test.yml",
		"services:\n  backend:\n    volumes:\n      - ./nginx/dev.conf:/x\n      - ./prometheus/prometheus.yml:/y\n")
	mk("docker-compose.yml", "services: {}\n") // other env: must not ship
	mk(".env.example", "A=1\n")
	mk("nginx/dev.conf", "server;\n")
	mk("prometheus/prometheus.yml", "scrape;\n")
	mk("scripts/restart.sh", "#!/bin/sh\n")
	mk("scripts/node_modules/pkg.js", "junk") // skipped inside a collected dir
	mk("versions/test.json", `{"latest":"1.0"}`)
	mk("Makefile", "all:\n")
	mk("README.md", "readme")
	mk("cicd/pipeline.yaml", "jobs: []\n")

	log := testLogger{}
	exists := func(base, rel string) bool {
		_, err := os.Stat(filepath.Join(base, filepath.FromSlash(rel)))
		return err == nil
	}

	p := &ProductConfig{
		Workdir:      ".",
		ComposeFiles: map[string]string{"test": "docker-compose.test.yml", "prod": "docker-compose.yml"},
		EnvTemplate:  ".env.example",
		ExtraFiles:   []string{"scripts", "docs"}, // docs does not exist: skipped
		Versions:     map[string]string{"test": "versions/test.json"},
	}
	p.applyDefaults("platform")

	// test: mount dirs referenced by the compose ship automatically.
	dst := t.TempDir()
	if err := copyRepoSubset(log, root, dst, p, "test", "docker-compose.test.yml"); err != nil {
		t.Fatalf("test: %v", err)
	}
	for _, want := range []string{
		"docker-compose.test.yml", ".env.example", "nginx/dev.conf",
		"prometheus/prometheus.yml", "scripts/restart.sh", "versions/test.json",
	} {
		if !exists(dst, want) {
			t.Errorf("test: missing %s", want)
		}
	}
	for _, unwanted := range []string{"docker-compose.yml", "Makefile", "README.md", "cicd", "docs", "scripts/node_modules"} {
		if exists(dst, unwanted) {
			t.Errorf("test: unexpected %s", unwanted)
		}
	}

	// prod: the compose references no ./ dir and prod has no version
	// manifest — declared extra files still ship.
	mk("docker-compose.yml", "services:\n  db:\n    image: db:1\n")
	dst2 := t.TempDir()
	if err := copyRepoSubset(log, root, dst2, p, "prod", "docker-compose.yml"); err != nil {
		t.Fatalf("prod: %v", err)
	}
	for _, want := range []string{"docker-compose.yml", ".env.example", "scripts/restart.sh"} {
		if !exists(dst2, want) {
			t.Errorf("prod: missing %s", want)
		}
	}
	for _, unwanted := range []string{"nginx", "prometheus", "versions", "Makefile"} {
		if exists(dst2, unwanted) {
			t.Errorf("prod: unexpected %s", unwanted)
		}
	}

	// Product in a subdirectory workdir: everything resolves under robot/.
	p2 := &ProductConfig{Workdir: "robot", ComposeFiles: map[string]string{"prod": "docker-compose.yml"}}
	p2.applyDefaults("robot")
	mk("robot/docker-compose.yml",
		"services:\n  broker:\n    volumes:\n      - ./mosquitto/mosquitto.conf:/m:ro\n")
	mk("robot/.env.example", "B=2\n")
	mk("robot/mosquitto/mosquitto.conf", "conf\n")
	dst3 := t.TempDir()
	if err := copyRepoSubset(log, root, dst3, p2, "prod", "docker-compose.yml"); err != nil {
		t.Fatalf("workdir: %v", err)
	}
	for _, want := range []string{
		"robot/docker-compose.yml", "robot/.env.example", "robot/mosquitto/mosquitto.conf",
	} {
		if !exists(dst3, want) {
			t.Errorf("workdir: missing %s", want)
		}
	}
	for _, unwanted := range []string{"docker-compose.yml", ".env.example", "nginx", "versions"} {
		if exists(dst3, unwanted) {
			t.Errorf("workdir: unexpected %s (must stay under robot/)", unwanted)
		}
	}
}

// discoverProfiles: profile names of the selected compose file; nil when
// the file is missing or the env is undefined.
func TestDiscoverProfiles(t *testing.T) {
	dir := t.TempDir()
	compose := `services:
  ctrl:
    profiles:
      - mj
    image: ctrl:1
  rzt:
    profiles:
      - "rzt"
    image: rzt:1
  base:
    image: base:1
`
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &ProductConfig{ComposeFiles: map[string]string{"prod": "docker-compose.yml"}}
	p.applyDefaults("robot")
	got := discoverProfiles(dir, p, "prod")
	want := []string{"mj", "rzt"}
	if len(got) != len(want) {
		t.Fatalf("discoverProfiles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("discoverProfiles[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Zero-config: the empty mapping auto-detects the compose file.
	z := zeroConfig().Products["default"]
	if got := discoverProfiles(dir, z, "prod"); len(got) != 2 {
		t.Fatalf("zero-config discoverProfiles = %v, want 2 profiles", got)
	}

	// Undefined env or missing compose file -> nil.
	if got := discoverProfiles(dir, p, "nope"); got != nil {
		t.Errorf("undefined env must return nil, got %v", got)
	}
	empty := t.TempDir()
	if got := discoverProfiles(empty, p, "prod"); got != nil {
		t.Errorf("missing compose file must return nil, got %v", got)
	}
}

// loadInstallerConfig without compose-installer.json: a zero-config product
// around the auto-detected compose file.
func TestLoadInstallerConfigZeroConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadInstallerConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultProduct != "default" || len(cfg.Products) != 1 {
		t.Fatalf("zero config = %+v", cfg)
	}
	p, err := cfg.Product("")
	if err != nil {
		t.Fatal(err)
	}
	if p.Workdir != "." || p.EnvTemplate != ".env.example" || p.InstallDirName != "default" {
		t.Fatalf("zero config defaults: %+v", p)
	}
	if f, err := p.ComposeFileFor("prod"); err != nil || f != "" {
		t.Fatalf("ComposeFileFor(prod) = %q, %v; want empty (auto-detect)", f, err)
	}
	if p.HasVersions("prod") {
		t.Fatal("zero config must not have versions")
	}
	if DetectComposeFile(dir) != "docker-compose.yml" {
		t.Fatalf("DetectComposeFile = %q, want docker-compose.yml", DetectComposeFile(dir))
	}
}

// loadInstallerConfig with compose-installer.json: products resolve with
// defaults, unknown names list the available ones.
func TestLoadInstallerConfigFile(t *testing.T) {
	dir := t.TempDir()
	raw := `{
	  "defaultProduct": "robot",
	  "products": {
	    "robot": {
	      "workdir": "robot",
	      "composeFiles": {"prod": "docker-compose.yml", "test": "docker-compose.test.yml"},
	      "versions": {"prod": "robot/versions/prod.json"},
	      "defaultProfiles": ["mj"],
	      "installDirName": "my-robot"
	    },
	    "lite": {"composeFiles": {"prod": "compose.yaml"}}
	  }
	}`
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadInstallerConfig(dir)
	if err != nil {
		t.Fatal(err)
	}

	p, err := cfg.Product("") // default product
	if err != nil {
		t.Fatal(err)
	}
	if p.Workdir != "robot" || p.InstallDirName != "my-robot" {
		t.Fatalf("robot product = %+v", p)
	}
	if len(p.DefaultProfiles) != 1 || p.DefaultProfiles[0] != "mj" {
		t.Fatalf("defaultProfiles = %v", p.DefaultProfiles)
	}
	if !p.HasVersions("prod") || p.VersionFileFor("prod") != "robot/versions/prod.json" {
		t.Fatalf("versions of prod = %q", p.VersionFileFor("prod"))
	}
	if f, err := p.ComposeFileFor("test"); err != nil || f != "docker-compose.test.yml" {
		t.Fatalf("ComposeFileFor(test) = %q, %v", f, err)
	}
	if _, err := p.ComposeFileFor("dev"); err == nil {
		t.Error("undefined env must fail")
	}

	if _, err := cfg.Product("nope"); err == nil {
		t.Error("unknown product must fail")
	}
	lite, err := cfg.Product("lite")
	if err != nil {
		t.Fatal(err)
	}
	if lite.Workdir != "." || lite.EnvTemplate != ".env.example" || lite.InstallDirName != "lite" {
		t.Fatalf("lite defaults = %+v", lite)
	}
}
