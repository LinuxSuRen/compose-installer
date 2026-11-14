package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// tplRunner is a test-local Runner that only supports what buildEnvFile uses:
// Exists (os.Stat) and the exact "cat <name>" Capture it issues. It avoids any
// shell dependency so the test runs on every dev machine.
type tplRunner struct{ Runner }

func (tplRunner) Exists(p string) (bool, error) {
	_, err := os.Stat(p)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (tplRunner) Capture(ctx context.Context, dir, cmd string, env []string) (string, error) {
	name, ok := strings.CutPrefix(cmd, "cat ")
	if !ok {
		return "", fmt.Errorf("tplRunner: unsupported command %q", cmd)
	}
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// archFromUname: mapping of uname -m outputs to linux/<arch>.
func TestArchFromUname(t *testing.T) {
	cases := map[string]string{
		"x86_64":  "linux/amd64",
		"amd64":   "linux/amd64",
		"aarch64": "linux/arm64",
		"arm64":   "linux/arm64",
		" ARM64 ": "linux/arm64",
		"i386":    "",
		"":        "",
	}
	for in, want := range cases {
		if got := archFromUname(in); got != want {
			t.Errorf("archFromUname(%q) = %q, want %q", in, got, want)
		}
	}
}

// validUnixUsername: user-name validation before splicing into elevated commands.
func TestValidUnixUsername(t *testing.T) {
	for _, ok := range []string{"robot", "rick", "a_b", "user-01", "dev.user"} {
		if !validUnixUsername(ok) {
			t.Errorf("validUnixUsername(%q) should pass", ok)
		}
	}
	for _, bad := range []string{"", "0user", "u s e r", "x;y", "user`id`", "USER", "way-too-long-username-over-32-chars"} {
		// note: root itself is a valid user name; the root exclusion lives at
		// the call site (user != "root")
		if validUnixUsername(bad) {
			t.Errorf("validUnixUsername(%q) should be rejected", bad)
		}
	}
}

// quoteAll: every argument gets shell-quoted (single quotes, embedded quotes
// escaped).
func TestQuoteAll(t *testing.T) {
	got := quoteAll([]string{"install.sh", "--yes", "it's", "a b"})
	want := []string{"'install.sh'", "'--yes'", `'it'\''s'`, "'a b'"}
	if len(got) != len(want) {
		t.Fatalf("quoteAll = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("quoteAll[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// envKeyRe: .env key names accepted before writing user-provided variables.
func TestEnvKeyRe(t *testing.T) {
	for _, ok := range []string{"A", "a_b", "_X9", "KEY_1", "DEVICE_ID"} {
		if !envKeyRe.MatchString(ok) {
			t.Errorf("envKeyRe should accept %q", ok)
		}
	}
	for _, bad := range []string{"", "1A", "A-B", "a b", "KEY=1", "_X ", "中文"} {
		if envKeyRe.MatchString(bad) {
			t.Errorf("envKeyRe should reject %q", bad)
		}
	}
}

// dockerVersionAtLeast: version floor parsing (distro suffixes like ~3 and
// -1, short/invalid versions).
func TestDockerVersionAtLeast(t *testing.T) {
	cases := []struct {
		ver  string
		maj  int
		min  int
		want bool
	}{
		{"20.10.24~3", 20, 10, true},
		{"20.10", 20, 10, true},
		{"20.9.7", 20, 10, false},
		{"19.03.12", 20, 10, false},
		{"27.5.1", 20, 10, true},
		{"24.0.7-1", 24, 0, true},
		{"24.0.7-1", 24, 1, false},
		{"21", 20, 10, false}, // no minor segment
		{"x.y", 20, 10, false},
		{"", 20, 10, false},
	}
	for _, c := range cases {
		if got := dockerVersionAtLeast(c.ver, c.maj, c.min); got != c.want {
			t.Errorf("dockerVersionAtLeast(%q, %d, %d) = %v, want %v", c.ver, c.maj, c.min, got, c.want)
		}
	}
}

// defaultInstallDirFor: root/non-root remote and cross-platform local shapes.
func TestDefaultInstallDirFor(t *testing.T) {
	const name = "myapp"
	if got := defaultInstallDirFor(name, "remote", ""); got != "/opt/"+name {
		t.Errorf("remote/empty user = %q, want /opt/%s", got, name)
	}
	if got := defaultInstallDirFor(name, "remote", "root"); got != "/opt/"+name {
		t.Errorf("remote/root = %q, want /opt/%s", got, name)
	}
	if got := defaultInstallDirFor(name, "remote", "ops"); got != "/home/ops/"+name {
		t.Errorf("remote/ops = %q, want /home/ops/%s", got, name)
	}
	if runtime.GOOS == "linux" {
		if got := defaultInstallDirFor(name, "local", ""); got != "/opt/"+name {
			t.Errorf("local/linux = %q, want /opt/%s", got, name)
		}
	} else if home, err := os.UserHomeDir(); err == nil {
		if got := defaultInstallDirFor(name, "local", ""); got != filepath.ToSlash(filepath.Join(home, name)) {
			t.Errorf("local/%s = %q, want %s", runtime.GOOS, got, filepath.ToSlash(filepath.Join(home, name)))
		}
	}
	// Defensive fallback for malformed manifests (no product, no dir name).
	if got := defaultInstallDirFor("", "remote", ""); got != "/opt/compose-installer" {
		t.Errorf("empty dirName = %q, want /opt/compose-installer", got)
	}
}

// buildEnvFile: layering (template → component tags → envExports →
// envRewrites → user overrides), placeholder substitution, RequiredEnv
// validation and user-key checks.
func TestBuildEnvFile(t *testing.T) {
	dir := t.TempDir()
	workdir := filepath.ToSlash(filepath.Join(dir, "repo"))
	if err := os.MkdirAll(filepath.FromSlash(workdir), 0o755); err != nil {
		t.Fatal(err)
	}
	tpl := "# template comment\nAPP_ENV=\nOLD_TAG=1.0\nEMPTY_VAR=\n"
	if err := os.WriteFile(filepath.Join(dir, "repo", ".env.example"), []byte(tpl), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	lr := tplRunner{}
	j := &Job{ID: "test"}

	m := &BundleManifest{
		Version:    "1.2",
		Components: map[string]string{"backend": "0.3"},
		EnvExports: map[string]string{"APP_VERSION": "{version}"},
		EnvRewrites: map[string]string{
			"UPLOADS_HOST_PATH": "{installDir}/data/uploads",
			"MODE_OPT":          "default",
		},
		RequiredEnv: []string{"APP_ENV"},
	}
	user := map[string]string{"APP_ENV": "prod", "MODE_OPT": "custom"}
	content, err := buildEnvFile(ctx, lr, workdir, "/opt/myapp", m, user, j)
	if err != nil {
		t.Fatalf("buildEnvFile: %v", err)
	}
	env := string(content)
	for _, want := range []string{
		"# template comment",                        // template lines preserved verbatim
		"BACKEND_TAG=0.3",                           // tagEnvVars(m.Components)
		"APP_VERSION=1.2",                           // EnvExports, {version} substituted
		"UPLOADS_HOST_PATH=/opt/myapp/data/uploads", // EnvRewrites, {installDir} substituted
		"APP_ENV=prod",                              // user override (highest precedence)
		"MODE_OPT=custom",                           // user value replaces the rewrite default
	} {
		if !strings.Contains(env, want) {
			t.Errorf("generated .env missing %q:\n%s", want, env)
		}
	}
	if strings.Contains(env, "MODE_OPT=default") {
		t.Errorf("user-provided key must suppress the envRewrites default:\n%s", env)
	}

	// RequiredEnv violation: JWT_SECRET resolves nowhere; APP_ENV is filled
	// by the user, so only JWT_SECRET may be reported.
	m2 := *m
	m2.RequiredEnv = []string{"APP_ENV", "JWT_SECRET"}
	_, err = buildEnvFile(ctx, lr, workdir, "/opt/myapp", &m2, user, j)
	if err == nil || !strings.Contains(err.Error(), "JWT_SECRET") || strings.Contains(err.Error(), "APP_ENV") {
		t.Fatalf("required-env error should list only JWT_SECRET, got %v", err)
	}

	// Empty value does not satisfy RequiredEnv either.
	m3 := *m
	m3.RequiredEnv = []string{"EMPTY_VAR"}
	_, err = buildEnvFile(ctx, lr, workdir, "/opt/myapp", &m3, map[string]string{}, j)
	if err == nil || !strings.Contains(err.Error(), "EMPTY_VAR") {
		t.Fatalf("empty template value should fail RequiredEnv, got %v", err)
	}

	// Invalid user key names and line breaks are rejected.
	if _, err = buildEnvFile(ctx, lr, workdir, "/opt/myapp", m, map[string]string{"bad-key": "x"}, j); err == nil || !strings.Contains(err.Error(), "invalid environment variable name") {
		t.Fatalf("invalid key should be rejected, got %v", err)
	}
	if _, err = buildEnvFile(ctx, lr, workdir, "/opt/myapp", m, map[string]string{"A": "x\ny"}, j); err == nil || !strings.Contains(err.Error(), "line breaks") {
		t.Fatalf("newline value should be rejected, got %v", err)
	}
}

// readBundleManifest: reads only the root manifest.json; missing manifest is
// an explicit error.
func TestReadBundleManifest(t *testing.T) {
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty.tar.gz")
	writeTarGz(t, empty, map[string]string{"repo/x.txt": "hi"})
	if _, err := readBundleManifest(empty); err == nil || !strings.Contains(err.Error(), "manifest.json") {
		t.Fatalf("expected a manifest.json-missing error, got %v", err)
	}

	want := &BundleManifest{
		Product:        "myapp",
		Env:            "prod",
		Version:        "1.0",
		Workdir:        ".",
		ComposeFile:    "docker-compose.yml",
		InstallDirName: "myapp-deploy",
		RequiredEnv:    []string{"A", "B"},
		EnvRewrites:    map[string]string{"UPLOADS_HOST_PATH": "{installDir}/data/uploads"},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "good.tar.gz")
	writeTarGz(t, good, map[string]string{
		"./manifest.json":         string(raw), // "./" prefix must be tolerated
		"repo/docker-compose.yml": "x: y",
		"images/myapp.tar":        "", // payload presence is not the reader's business
	})
	got, err := readBundleManifest(good)
	if err != nil {
		t.Fatalf("readBundleManifest: %v", err)
	}
	if got.Product != want.Product || got.Env != want.Env || got.Version != want.Version ||
		got.ComposeFile != want.ComposeFile || got.Workdir != want.Workdir {
		t.Errorf("manifest round-trip mismatch: %+v", got)
	}
	if got.DirName() != "myapp-deploy" {
		t.Errorf("DirName() = %q, want myapp-deploy", got.DirName())
	}
	if len(got.RequiredEnv) != 2 || got.RequiredEnv[0] != "A" || got.RequiredEnv[1] != "B" {
		t.Errorf("RequiredEnv round-trip mismatch: %v", got.RequiredEnv)
	}
	if got.EnvRewrites["UPLOADS_HOST_PATH"] != "{installDir}/data/uploads" {
		t.Errorf("EnvRewrites round-trip mismatch: %v", got.EnvRewrites)
	}
}

// writeTarGz builds a small tar.gz for reader tests.
func writeTarGz(t *testing.T, file string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
