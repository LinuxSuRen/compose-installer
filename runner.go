package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Runner abstracts "execute an action on the target machine": local (local) or
// SSH remote (remote). The install flow depends only on this interface and
// does not care whether the underlying transport is exec or ssh/sftp.
type Runner interface {
	Name() string // display only: "local" or "ssh://user@host"
	// Platform is the target platform: for local, the OS the tool itself runs
	// on (linux/darwin/windows); for SSH remotes always linux (Ubuntu). The
	// install flow adapts on this (df syntax / time calibration / docker
	// install method / default install dir, etc.).
	Platform() string
	Close() error
	// Run executes a bash script on the target (env vars and cd dir are
	// injected automatically); output is streamed to out in real time; a
	// non-nil return means the command failed.
	Run(ctx context.Context, out io.Writer, dir, cmd string, env []string) error
	// Capture runs a command and returns its full stdout+stderr, for probes
	// that judge on command output (Exists/df/date/md5 etc.). Do not replace
	// it with Run+buffer: on some environments the exec channel silently
	// drops the tail of the output at the instant the command exits (fast
	// commands show empty output with exit code 0). The SSH implementation
	// avoids this via a remote temp file + SFTP read-back.
	Capture(ctx context.Context, dir, cmd string, env []string) (string, error)
	MkdirAll(dir string) error
	// UploadFile uploads a file; log writes explanations into the job log
	// (e.g. whether scp or SFTP was chosen).
	UploadFile(src, dst string, log LogFunc, progress func(sent, total int64)) error
	WriteFile(dst string, content []byte) error
	Exists(path string) (bool, error)
	// FileMD5 returns the md5 (lowercase hex) of the file on the target
	// machine; returns "" when the file does not exist.
	FileMD5(path string) (string, error)
	// Exec invokes the executable directly with argv (no shell): locally this
	// is a direct cmd-level exec; remotely each argument is quoted verbatim
	// and executed in the target shell. env holds extra environment vars (K=V).
	Exec(ctx context.Context, out io.Writer, dir string, argv []string, env []string) error
	// ExecStdin is like Exec but writes the given content to the process
	// stdin (e.g. the password for docker login).
	ExecStdin(ctx context.Context, out io.Writer, dir string, argv []string, env []string, stdin string) error
}

// LogFunc is the job log function (same shape as *Job.Logf).
type LogFunc func(format string, args ...any)

// ---------------- local ----------------

type LocalRunner struct{}

func NewLocalRunner() *LocalRunner { return &LocalRunner{} }

func (r *LocalRunner) Name() string { return "local" }

// Platform: for a local install the target is the OS the tool runs on
// (macOS/Windows/Linux local installs are supported).
func (r *LocalRunner) Platform() string { return runtime.GOOS }

func (r *LocalRunner) Close() error { return nil }

// windowsBashPath is the fallback shell for leftover bash scripts on Windows,
// in order: WSL -> git-bash. Key operations (docker/tar etc.) already go
// through Exec as direct executable invocations (equivalent to a direct cmd
// exec) and depend on no shell; this is only the fallback for the few
// remaining bash scripts.
var windowsBashCache string

func windowsBashPath() string {
	if windowsBashCache != "" {
		return windowsBashCache
	}
	// 1) WSL bash (system32\bash.exe, or bash on PATH)
	if p, err := exec.LookPath("bash"); err == nil {
		windowsBashCache = p
		return p
	}
	// 2) common git-bash install locations
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles"), "Git", "bin", "bash.exe"),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Git", "bin", "bash.exe"),
		filepath.Join(os.Getenv("LocalAppData"), "Programs", "Git", "bin", "bash.exe"),
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			windowsBashCache = c
			return c
		}
	}
	windowsBashCache = "bash"
	return windowsBashCache
}

// bashBin returns the bash executable to use on the current platform.
func bashBin() string {
	if runtime.GOOS == "windows" {
		return windowsBashPath()
	}
	return "bash"
}

func (r *LocalRunner) Run(ctx context.Context, out io.Writer, dir, cmd string, env []string) error {
	full := renderScript(dir, cmd, env)
	c := exec.CommandContext(ctx, bashBin(), "-c", full)
	c.Stdout = out
	c.Stderr = out
	c.Env = append(os.Environ(), "COMPOSE_INSTALLER=1")
	return c.Run()
}

// Capture (local): run via bash and return the combined output (a local pipe
// has no channel race; capture directly).
func (r *LocalRunner) Capture(ctx context.Context, dir, cmd string, env []string) (string, error) {
	full := renderScript(dir, cmd, env)
	c := exec.CommandContext(ctx, bashBin(), "-c", full)
	c.Env = append(os.Environ(), "COMPOSE_INSTALLER=1")
	out, err := c.CombinedOutput()
	return string(out), err
}

// Exec (local): invoke the executable directly (no shell, CreateProcess/fork+exec).
func (r *LocalRunner) Exec(ctx context.Context, out io.Writer, dir string, argv []string, env []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("Exec: empty argv")
	}
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Dir = dir
	c.Stdout = out
	c.Stderr = out
	c.Env = append(os.Environ(), env...)
	return c.Run()
}

// ExecStdin (local): invoke directly and write to the process stdin.
func (r *LocalRunner) ExecStdin(ctx context.Context, out io.Writer, dir string, argv []string, env []string, stdin string) error {
	if len(argv) == 0 {
		return fmt.Errorf("ExecStdin: empty argv")
	}
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Dir = dir
	c.Stdout = out
	c.Stderr = out
	c.Stdin = strings.NewReader(stdin)
	c.Env = append(os.Environ(), env...)
	return c.Run()
}

func (r *LocalRunner) MkdirAll(dir string) error { return os.MkdirAll(dir, 0o755) }

func (r *LocalRunner) UploadFile(src, dst string, log LogFunc, progress func(sent, total int64)) error {
	if log != nil {
		log("local mode: copying directly %s -> %s", src, dst)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	var sent int64
	buf := make([]byte, 1024*1024)
	for {
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
			sent += int64(n)
			if progress != nil {
				progress(sent, st.Size())
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return nil
}

func (r *LocalRunner) WriteFile(dst string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, content, 0o644)
}

func (r *LocalRunner) Exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (r *LocalRunner) FileMD5(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---------------- ssh ----------------

type SSHConfig struct {
	Host          string `json:"host"`
	Port          int    `json:"port"`
	User          string `json:"user"`
	Password      string `json:"password"`
	KeyFile       string `json:"keyFile"`
	KeyPassphrase string `json:"keyPassphrase"`
}

type SSHRunner struct {
	cfg    SSHConfig
	client *ssh.Client
	sftp   *sftp.Client // may be nil: when the target sshd has no sftp subsystem, file operations fall back to shell
}

// SFTPEnabled reports whether the target sshd provides the sftp subsystem
// (for upstream logging).
func (r *SSHRunner) SFTPEnabled() bool { return r.sftp != nil }

func NewSSHRunner(cfg SSHConfig) (*SSHRunner, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("SSH host must not be empty")
	}
	if cfg.Port == 0 {
		cfg.Port = 22
	}
	if cfg.User == "" {
		cfg.User = "root"
	}

	// An explicitly configured private key path must be readable; fail
	// immediately instead of silently degrading.
	var explicitKey []byte
	if cfg.KeyFile != "" {
		var err error
		if explicitKey, err = os.ReadFile(cfg.KeyFile); err != nil {
			return nil, fmt.Errorf("failed to read private key: %w", err)
		}
	}

	dial := func(auth []ssh.AuthMethod) (*ssh.Client, error) {
		cc := &ssh.ClientConfig{
			User:            cfg.User,
			Auth:            auth,
			HostKeyCallback: ssh.InsecureIgnoreHostKey(), // intranet delivery scenario; host fingerprints are not verified
			Timeout:         15 * time.Second,
		}
		return ssh.Dial("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)), cc)
	}

	authAll := sshAuth(cfg, explicitKey, true)
	if authAll == nil {
		return nil, fmt.Errorf("provide an SSH password or private key (~/.ssh default keys and the SSH agent are also tried automatically)")
	}

	// Auth strategy (mimics manual ssh):
	//   1st attempt: private keys first (explicit keyFile > ~/.ssh default
	//   keys > SSH agent), password last;
	//   2nd attempt (only if the 1st failed AND a password was given):
	//   password only (password + keyboard-interactive), because some sshds
	//   drop the connection right after a publickey failure so the password
	//   method would never be tried.
	client, err := dial(authAll)
	if err != nil && cfg.Password != "" {
		client, err = dial(sshAuth(cfg, explicitKey, false))
	}
	if err != nil {
		return nil, fmt.Errorf(
			"SSH connect/auth failed: %w (automatically tried: explicit private key, ~/.ssh default keys, SSH agent, password/keyboard-interactive; please check: port reachable, username correct, password or key valid, key passphrase provided)",
			err)
	}
	sc, serr := sftp.NewClient(client,
		sftp.UseConcurrentWrites(true), // concurrent writes improve large-file throughput
	)
	if serr != nil {
		// The target sshd has no sftp subsystem (subsystem request failed):
		// degrade to nil first; the install flow calls EnableSFTP to turn it
		// on automatically and reconnect.
		sc = nil
	}
	return &SSHRunner{cfg: cfg, client: client, sftp: sc}, nil
}

// EnableSFTP turns on the sftp subsystem on the target: appends
// `Subsystem sftp internal-sftp` to /etc/ssh/sshd_config (built into sshd, no
// external binary needed), validates the config, reloads sshd (SIGHUP keeps
// existing connections alive), then rebuilds the SSH connection so the
// subsystem takes effect immediately. Requires privilege (root or a sudo
// prefix).
func (r *SSHRunner) EnableSFTP(ctx context.Context, out io.Writer, sudo string) error {
	script := `
set -e
if ! grep -qE '^[[:space:]]*Subsystem[[:space:]]+sftp' /etc/ssh/sshd_config; then
  printf '\nSubsystem sftp internal-sftp\n' >> /etc/ssh/sshd_config
fi
/usr/sbin/sshd -t 2>/dev/null || sshd -t
systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || killall -HUP sshd 2>/dev/null || true
`
	cmd := fmt.Sprintf(`%s bash -c %s`, sudo, shQuote(script))
	if err := r.Run(ctx, out, "", cmd, nil); err != nil {
		return fmt.Errorf("failed to modify sshd config: %w", err)
	}

	// sshd reads its config when each connection is established, so a reload
	// only affects new connections: rebuild the connection, then retry sftp.
	// NOTE: the original source kept time.Sleep(time.Second) inert inside
	// this comment (same line); kept commented out to preserve behavior.
	// time.Sleep(time.Second)
	old := r.client
	client, err := r.redial()
	if err != nil {
		r.client = old // restore the old connection as a fallback, avoiding a half-broken state
		return err
	}
	old.Close()
	r.client = client
	sc, err := sftp.NewClient(client, sftp.UseConcurrentWrites(true))
	if err != nil {
		return fmt.Errorf("sftp still unavailable after reconnect: %w", err)
	}
	r.sftp = sc
	return nil
}

// redial re-establishes the SSH connection with the same config.
func (r *SSHRunner) redial() (*ssh.Client, error) {
	var explicitKey []byte
	if r.cfg.KeyFile != "" {
		raw, err := os.ReadFile(r.cfg.KeyFile)
		if err != nil {
			return nil, err
		}
		explicitKey = raw
	}
	auth := sshAuth(r.cfg, explicitKey, true)
	if auth == nil {
		auth = sshAuth(r.cfg, explicitKey, false)
	}
	if auth == nil {
		return nil, fmt.Errorf("no usable SSH auth method")
	}
	cc := &ssh.ClientConfig{
		User:            r.cfg.User,
		Auth:            auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	return ssh.Dial("tcp", net.JoinHostPort(r.cfg.Host, strconv.Itoa(r.cfg.Port)), cc)
}

// passwordAuthMethods registers both password and keyboard-interactive for
// password auth — Ubuntu sshds are commonly configured to accept only
// keyboard-interactive validation.
func passwordAuthMethods(password string) []ssh.AuthMethod {
	return []ssh.AuthMethod{
		ssh.Password(password),
		ssh.KeyboardInteractive(
			func(name, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, 0, len(questions))
				for range questions {
					answers = append(answers, password)
				}
				return answers, nil
			}),
	}
}

// sshAuth assembles the auth methods. withKeys=false means password only
// (used for the second dial after a failure). Returns nil when no auth
// method is available at all.
func sshAuth(cfg SSHConfig, explicitKey []byte, withKeys bool) []ssh.AuthMethod {
	var methods []ssh.AuthMethod

	if withKeys {
		var signers []ssh.Signer
		addKey := func(raw []byte) {
			var s ssh.Signer
			var err error
			if cfg.KeyPassphrase != "" {
				s, err = ssh.ParsePrivateKeyWithPassphrase(raw, []byte(cfg.KeyPassphrase))
			} else {
				s, err = ssh.ParsePrivateKey(raw)
			}
			if err == nil {
				signers = append(signers, s)
			}
		}
		// 1) the explicitly specified private key
		if len(explicitKey) > 0 {
			addKey(explicitKey)
		}
		// 2) default private keys under ~/.ssh (same behavior as manual ssh)
		if home, err := os.UserHomeDir(); err == nil {
			for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
				if cfg.KeyFile != "" && filepath.Join(home, ".ssh", name) == cfg.KeyFile {
					continue
				}
				if raw, err := os.ReadFile(filepath.Join(home, ".ssh", name)); err == nil {
					addKey(raw)
				}
			}
		}
		// 3) SSH agent (common with git-bash / linux; SSH_AUTH_SOCK points
		// at the agent socket)
		if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
			if c, err := net.Dial("unix", sock); err == nil {
				ag := agent.NewClient(c)
				if ss, err := ag.Signers(); err == nil {
					signers = append(signers, ss...)
				}
			}
		}
		if len(signers) > 0 {
			methods = append(methods, ssh.PublicKeys(signers...))
		}
	}

	if cfg.Password != "" {
		methods = append(methods, passwordAuthMethods(cfg.Password)...)
	}

	return methods
}

func (r *SSHRunner) Name() string {
	return fmt.Sprintf("ssh://%s@%s:%d", r.cfg.User, r.cfg.Host, r.cfg.Port)
}

// Platform: SSH remote install targets are always Linux (Ubuntu).
func (r *SSHRunner) Platform() string { return "linux" }

func (r *SSHRunner) Close() error {
	if r.sftp != nil {
		r.sftp.Close()
	}
	return r.client.Close()
}

// sshd does not accept client env by default, so env vars are uniformly
// injected through an export prefix.
func (r *SSHRunner) Run(ctx context.Context, out io.Writer, dir, cmd string, env []string) error {
	full := renderScript(dir, cmd, env)
	sess, err := r.client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdout = out
	sess.Stderr = out
	done := make(chan error, 1)
	go func() { done <- sess.Run(full) }()
	select {
	case <-ctx.Done():
		sess.Signal(ssh.SIGKILL)
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// Capture (remote): the command output is redirected into a remote temp
// file, then read back via SFTP. The exec channel output is NOT captured
// directly: in the wild, on certain environments (kernel/sshd/virtualization
// combinations) the tail of the output is silently lost as the channel
// closes at the instant the command exits — fast commands (test -e, df,
// date, etc.) return empty output with exit code 0, which corrupts probes
// (e.g. a file that really exists gets judged as missing). The SFTP data
// channel is unaffected by this problem, so probe-type output uniformly goes
// through a file. Degrades to direct capture when sftp is unavailable (old
// behavior preserved; the EnableSFTP flow recovers it).
func (r *SSHRunner) Capture(ctx context.Context, dir, cmd string, env []string) (string, error) {
	if r.sftp == nil {
		var b bytes.Buffer
		if err := r.Run(ctx, &b, dir, cmd, env); err != nil {
			return "", err
		}
		return b.String(), nil
	}
	// The directory part of the temp file path must already exist; do not
	// fold module paths (containing slashes) into it.
	tmp := fmt.Sprintf("/tmp/.compose-installer-capture-%d", time.Now().UnixNano())
	wrapped := fmt.Sprintf("( %s ) > %s 2>&1", strings.TrimRight(cmd, "\n"), shQuote(tmp))
	if err := r.Run(ctx, io.Discard, dir, wrapped, env); err != nil {
		return "", err
	}
	defer func() { _ = r.sftp.Remove(tmp) }()
	f, err := r.sftp.Open(tmp)
	if err != nil {
		return "", fmt.Errorf("failed to read command output %s: %w", tmp, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("failed to read command output %s: %w", tmp, err)
	}
	return string(data), nil
}

// Exec (remote): every argv element is safely quoted, then joined into one
// command (semantics identical to a direct invocation).
func (r *SSHRunner) Exec(ctx context.Context, out io.Writer, dir string, argv []string, env []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("Exec: empty argv")
	}
	quoted := make([]string, 0, len(argv))
	for _, a := range argv {
		quoted = append(quoted, shQuote(a))
	}
	return r.Run(ctx, out, dir, strings.Join(quoted, " "), env)
}

// ExecStdin (remote): like Exec, and writes the given content to the remote
// process stdin.
func (r *SSHRunner) ExecStdin(ctx context.Context, out io.Writer, dir string, argv []string, env []string, stdin string) error {
	if len(argv) == 0 {
		return fmt.Errorf("ExecStdin: empty argv")
	}
	quoted := make([]string, 0, len(argv))
	for _, a := range argv {
		quoted = append(quoted, shQuote(a))
	}
	full := renderScript(dir, strings.Join(quoted, " "), env)
	sess, err := r.client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdout = out
	sess.Stderr = out
	sess.Stdin = strings.NewReader(stdin)
	return sess.Run(full)
}

func (r *SSHRunner) MkdirAll(dir string) error {
	if r.sftp != nil {
		return r.sftp.MkdirAll(filepath.ToSlash(dir))
	}
	return r.Run(context.Background(), io.Discard, "",
		"mkdir -p "+shQuote(filepath.ToSlash(dir)), nil)
}

// UploadFile uploads a file to the remote, trying in priority order:
//  1. the local scp command (good throughput; BatchMode makes password
//     scenarios fail fast instead of hanging, returning false on failure)
//  2. the built-in SFTP (requires the sftp subsystem on the target sshd;
//     EnableSFTP can turn it on automatically when it is missing)
//
// When both are unavailable an error is returned (no slow fallback, to avoid
// GB-size transfers becoming far too slow).
func (r *SSHRunner) UploadFile(src, dst string, log LogFunc, progress func(sent, total int64)) error {
	if r.tryScp(src, dst, log, progress) {
		return nil
	}
	if r.sftp != nil {
		return r.sftpUpload(src, dst, progress)
	}
	return fmt.Errorf("no usable upload channel: scp did not succeed and SFTP is not enabled on the target (the install flow usually enables SFTP automatically; you can also add 'Subsystem sftp internal-sftp' to /etc/ssh/sshd_config manually and restart sshd)")
}

// tryScp tries uploading via the local scp; BatchMode=yes makes password-auth
// scenarios fail immediately instead of hanging while waiting for input.
func (r *SSHRunner) tryScp(src, dst string, log LogFunc, progress func(sent, total int64)) bool {
	scpPath, err := exec.LookPath("scp")
	if err != nil {
		return false
	}
	args := []string{
		"-P", strconv.Itoa(r.cfg.Port),
		"-o", "StrictHostKeyChecking=no",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=15",
	}
	if r.cfg.KeyFile != "" {
		args = append(args, "-i", r.cfg.KeyFile)
	}
	args = append(args, src, fmt.Sprintf("%s@%s:%s", r.cfg.User, r.cfg.Host, filepath.ToSlash(dst)))

	if log != nil {
		log("local scp detected (%s): uploading via scp first", scpPath)
	}
	cmd := exec.Command(scpPath, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		if log != nil {
			log("scp upload did not succeed (%s): falling back to built-in SFTP upload", lastRunes(string(out), 160))
		}
		return false
	}
	if progress != nil {
		if st, serr := os.Stat(src); serr == nil {
			progress(st.Size(), st.Size())
		}
	}
	return true
}

// lastRunes returns the tail of a string (for truncating error messages).
func lastRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func (r *SSHRunner) sftpUpload(src, dst string, progress func(sent, total int64)) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if err := r.MkdirAll(filepath.ToSlash(filepath.Dir(dst))); err != nil {
		return err
	}
	remote, err := r.sftp.Create(filepath.ToSlash(dst))
	if err != nil {
		return err
	}
	defer remote.Close()
	var sent int64
	buf := make([]byte, 1024*256)
	for {
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := remote.Write(buf[:n]); werr != nil {
				return werr
			}
			sent += int64(n)
			if progress != nil {
				progress(sent, st.Size())
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return nil
}

func (r *SSHRunner) WriteFile(dst string, content []byte) error {
	p := filepath.ToSlash(dst)
	if r.sftp != nil {
		if err := r.sftp.MkdirAll(filepath.ToSlash(filepath.Dir(dst))); err != nil {
			return err
		}
		f, err := r.sftp.Create(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Write(content)
		return err
	}
	// shell fallback: the content is base64-encoded and restored through a
	// pipe, avoiding quoting/newline escaping issues
	b64 := base64.StdEncoding.EncodeToString(content)
	return r.Run(context.Background(), io.Discard, "", fmt.Sprintf(
		`mkdir -p %s && printf '%%s' %s | base64 -d > %s`,
		shQuote(filepath.ToSlash(filepath.Dir(dst))), shQuote(b64), shQuote(p)), nil)
}

func (r *SSHRunner) Exists(path string) (bool, error) {
	out, err := r.Capture(context.Background(), "",
		fmt.Sprintf("test -e %s && echo yes || echo no", shQuote(filepath.ToSlash(path))), nil)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "yes", nil
}

// FileMD5 runs md5sum on the remote (Ubuntu ships coreutils).
func (r *SSHRunner) FileMD5(file string) (string, error) {
	p := shQuote(filepath.ToSlash(file))
	out, err := r.Capture(context.Background(), "",
		fmt.Sprintf(`if [ -f %s ]; then md5sum < %s; else echo __MISSING__; fi`, p, p), nil)
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" || out == "__MISSING__" {
		return "", nil
	}
	fields := strings.Fields(out)
	return strings.ToLower(fields[0]), nil
}

// ---------------- shared ----------------

// renderScript assembles the final script handed to bash: export env vars +
// cd + command.
func renderScript(dir, cmd string, env []string) string {
	var b bytes.Buffer
	for _, kv := range env {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		fmt.Fprintf(&b, "export %s=%s\n", parts[0], shQuote(parts[1]))
	}
	if dir != "" {
		fmt.Fprintf(&b, "cd %s\n", shQuote(dir))
	}
	b.WriteString("set -e\n")
	b.WriteString(cmd)
	b.WriteString("\n")
	return b.String()
}

// shQuote wraps in single quotes and escapes inner single quotes, preventing
// injection.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
