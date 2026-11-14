package main

// The install engine: drives a bundle onto the local machine or an SSH remote
// Linux host. Everything platform/host-specific about the *product* is gone —
// the installer only trusts the bundle's manifest.json and never reads the
// packer-side product configuration.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// envTemplateName is the conventional name of the env template shipped inside
// every bundle at repo/<workdir>/.env.example (manifest schema 1 does not carry
// the original template name, so the packer normalizes it to this name).
const envTemplateName = ".env.example"

// envKeyRe validates user-provided .env key names before they are ever
// written or interpolated (defense against broken dotenv files / injection).
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// dockerDesktopURL Docker's official download page. macOS/Windows get no
// automatic docker installation (both the offline materials and get.docker.com
// are Linux-only); the user must install it manually.
const dockerDesktopURL = "https://www.docker.com/products/docker-desktop/"

func runInstall(ctx context.Context, j *Job, store *JobStore, req InstallRequest) (err error) {
	defer func() {
		if err != nil {
			j.finish("failed", err.Error())
		} else {
			j.finish("success", "")
		}
	}()

	// 1. Validate the bundle and its manifest.
	bundlePath, err := filepath.Abs(req.BundlePath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(bundlePath); err != nil {
		return fmt.Errorf("bundle not found: %s", bundlePath)
	}
	m, err := readBundleManifest(bundlePath)
	if err != nil {
		return err
	}
	j.Logf("bundle: %s", filepath.Base(bundlePath))
	j.Logf("manifest: product=%s env=%s version=%s profiles=%v images=%d", m.Product, m.Env, m.Version, m.Profiles, len(m.Images))

	// Online bundles: registry credentials on the target must come in pairs
	// (pulling private images needs both). Fail fast before the execution
	// channel is opened so parameter errors surface before we ever connect.
	if m.Online && req.RegistryUser != "" && req.RegistryPassword == "" {
		return fmt.Errorf("a registry password is required when a registry user is given (leave both empty to reuse the target's existing docker login)")
	}

	// RequiredEnv values come from the user at install time and cannot be
	// derived anywhere else. Validate them before any connection is opened:
	// parameter errors must not surface after the target has been reached
	// (the .env assembly re-validates the final file later).
	var missingEnv []string
	for _, k := range m.RequiredEnv {
		if strings.TrimSpace(req.EnvVars[k]) == "" {
			missingEnv = append(missingEnv, k)
		}
	}
	if len(missingEnv) > 0 {
		return fmt.Errorf("missing required environment variables: %s (fill them in before installing)", strings.Join(missingEnv, ", "))
	}

	// 2. Establish the execution channel.
	var runner Runner
	switch req.Mode {
	case "local", "":
		// Local installs are cross-platform (Linux/macOS/Windows): the whole
		// flow runs through bash; on Windows that means Git for Windows
		// (which also brings the ssh/scp family).
		if _, err := exec.LookPath("bash"); err != nil {
			return fmt.Errorf("local install requires bash: on Windows install Git for Windows and make sure bash is on PATH, or install over SSH to a Linux host instead")
		}
		runner = NewLocalRunner()
	case "remote":
		runner, err = NewSSHRunner(req.SSH)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported mode: %q (local|remote)", req.Mode)
	}
	defer runner.Close()
	j.Logf("execution channel: %s (%s)", runner.Name(), platformLabel(runner.Platform()))
	if sr, ok := runner.(*SSHRunner); ok && !sr.SFTPEnabled() {
		j.Logf("target sshd has no SFTP subsystem enabled; it will be enabled automatically later")
	}

	// 3. Target architecture check (before uploading the big bundle — fails
	//    fastest): offline bundle images are architecture-specific; a mismatch
	//    only shows up as "exec format error" when containers start, so it is
	//    intercepted up front. Online bundles are pulled by the target for its
	//    own architecture — a mismatch is only a warning there.
	targetArch := detectTargetArch(ctx, runner, j)
	if m.Platform != "" && targetArch != "" {
		if m.Platform != targetArch {
			if m.Online {
				j.Logf("warning: bundle was packed for %s, target is %s; online mode will pull images for the target's architecture (pulling fails later if the registry lacks that architecture)", m.Platform, targetArch)
			} else {
				return fmt.Errorf("architecture mismatch: this offline bundle carries %s images but the target is %s — they cannot run (repack with the target's architecture)", m.Platform, targetArch)
			}
		} else {
			j.Logf("architecture check: bundle %s matches the target", m.Platform)
		}
	} else if m.Platform == "" {
		j.Logf("note: bundle does not declare an architecture (legacy pack), skipping the architecture check")
	}

	// Default install directory: the directory name comes from the bundle
	// (manifest InstallDirName, falling back to the product name). It is
	// computed by defaultInstallDirFor — the single source of truth shared
	// with the default-directory API, so the UI hint and the backend fallback
	// can never disagree.
	installDir := req.InstallDir
	if installDir == "" {
		sshUser := ""
		if req.Mode == "remote" {
			sshUser = req.SSH.User
		}
		installDir = defaultInstallDirFor(m.DirName(), req.Mode, sshUser)
		j.Logf("no install directory given, using default: %s", installDir)
	}
	// Target paths always use / separators (the tool itself may run on
	// Windows; a backslash breaks the remote bash/tar).
	installDir = filepath.ToSlash(installDir)

	// Determine the elevation method up front: directory creation,
	// extraction and file writes below may all need it.
	sudoPass := req.SudoPassword
	if sudoPass == "" && req.Mode == "remote" {
		sudoPass = req.SSH.Password
	}
	sudo, sudoMode, err := sudoPrefix(ctx, runner, j, sudoPass)
	if err != nil {
		return err
	}

	// Enable the sftp subsystem automatically when the target lacks it
	// (internal-sftp + reload), so uploads take the fast path.
	if sr, ok := runner.(*SSHRunner); ok && !sr.SFTPEnabled() {
		j.Logf("target sshd has no SFTP subsystem, enabling it now (internal-sftp, reload keeps the connection)...")
		if err := sr.EnableSFTP(ctx, j, sudo); err != nil {
			return fmt.Errorf("failed to enable SFTP automatically: %w (add 'Subsystem sftp internal-sftp' to /etc/ssh/sshd_config on the target and run systemctl reload ssh, then retry)", err)
		}
		j.Logf("SFTP enabled")
	}

	// 4. SSH session timeout tuning (optional): raise the target sshd's idle
	//    tolerance to 30 minutes (ClientAliveInterval 60 × ClientAliveCountMax
	//    30) so sessions are not dropped in the middle of long uploads or
	//    extractions. Any failure only warns and never blocks the install.
	if _, ok := runner.(*SSHRunner); ok && runner.Platform() == "linux" {
		j.Logf("checking SSH session timeout configuration (target: 30 minutes, optional step)...")
		tuneSSHDSessionTimeout(ctx, runner, j, sudo)
	}

	// 5. Time calibration (before upload): target clock drift breaks TLS and
	//    token validation — offline machines with drifting clocks are the
	//    classic case, and once TLS fails everything after it fails
	//    mysteriously. Compare epoch values and, beyond the threshold, set
	//    the target clock straight to the local time via date -s (no NTP
	//    involved; epoch assignment is timezone-independent), then persist
	//    the result to the hardware clock (RTC) so it survives reboots.
	//    date -s is GNU coreutils syntax, Linux only; macOS/Windows skip it.
	if runner.Platform() == "linux" {
		if err := syncTargetTime(ctx, runner, j, sudo); err != nil {
			j.Logf("warning: %v (not blocking the install)", err)
		}
	} else {
		j.Logf("target is %s, skipping time calibration (Linux only)", platformLabel(runner.Platform()))
	}

	// 6. Disk space check (before upload): offline bundles are estimated
	//    roughly at 3x the bundle size (bundle itself + extracted image tars
	//    + docker load expansion on disk) — running out aborts the install.
	//    Online bundles have no local size anchor, so they are estimated by
	//    image count (average 1.5 GiB each) and only warn — the real
	//    requirement is whatever docker pull reports.
	if m.Online {
		need := uint64(len(m.Images)) * 3 / 2 * 1024 * 1024 * 1024
		if err := checkDiskSpace(ctx, runner, j, installDir, need, true); err != nil {
			return err
		}
	} else if st, serr := os.Stat(bundlePath); serr == nil {
		if err := checkDiskSpace(ctx, runner, j, installDir, uint64(st.Size())*3, false); err != nil {
			return err
		}
	}

	// 7. Upload the bundle (skipped when the target already has a file with
	//    a matching md5).
	remoteBundle := path.Join(installDir, "bundle.tar.gz")
	if err := ensureInstallDir(ctx, runner, j, installDir, sudo, loginuser(req)); err != nil {
		return err
	}

	j.Logf("computing local md5: %s", bundlePath)
	localMD5, err := fileMD5Local(bundlePath)
	if err != nil {
		return fmt.Errorf("failed to compute local md5: %w", err)
	}
	remoteMD5, md5err := runner.FileMD5(remoteBundle)
	if md5err != nil {
		j.Logf("failed to read target md5 (%v), uploading anyway", md5err)
	}
	if md5err == nil && remoteMD5 != "" && remoteMD5 == localMD5 {
		j.Logf("target already has an identical file (md5 matches), skipping upload: %s", remoteBundle)
	} else {
		j.Logf("uploading bundle -> %s (local md5: %s)", remoteBundle, localMD5)
		lastLog := ""
		err = runner.UploadFile(bundlePath, remoteBundle, j.Logf, func(sent, total int64) {
			pct := int(sent * 100 / max(total, 1))
			msg := fmt.Sprintf("upload progress: %d%% (%.1f/%.1f MB)", pct, float64(sent)/1e6, float64(total)/1e6)
			if msg != lastLog && (pct%10 == 0 || sent == total) {
				j.Logf("%s", msg)
				lastLog = msg
			}
		})
		if err != nil {
			return fmt.Errorf("upload failed: %w", err)
		}
		// Verify after upload: large transfers can corrupt silently.
		j.Logf("upload finished, verifying target md5 (multi-GB files take seconds to tens of seconds; no output during that is normal)...")
		postMD5, err := runner.FileMD5(remoteBundle)
		if err != nil {
			return fmt.Errorf("post-upload md5 verification failed: %w", err)
		}
		if postMD5 != localMD5 {
			return fmt.Errorf("post-upload md5 mismatch (target %s, local %s), please retry", postMD5, localMD5)
		}
		j.Logf("md5 verified, file is intact")
	}

	// 8. Extract (incremental): a successful extraction writes an md5 marker
	//    (.bundle-extracted) into the install directory; on re-runs, when the
	//    marker matches the current bundle md5 and the content directory
	//    exists, extraction is skipped — together with the md5 upload skip
	//    this makes re-runs reach the later steps within seconds.
	//    When re-extraction is needed (old repo/images cleared to avoid
	//    mixed versions; the bundle itself is kept — on failure the md5 check
	//    above skips the re-upload on the next run, it is only removed after
	//    full success), note: bind-mount directories created by
	//    dockerd/containers (root) during a previous deployment (e.g.
	//    repo/data) are owned by root, and a plain user's rm would fail, so
	//    the cleanup goes through sudo.
	extractMarker := path.Join(installDir, ".bundle-extracted")
	lr, isLocal := runner.(*LocalRunner)
	skipExtract := false
	if localMD5 != "" {
		markerMatched := false
		if isLocal {
			if b, rerr := os.ReadFile(extractMarker); rerr == nil &&
				strings.TrimSpace(string(b)) == localMD5 {
				markerMatched = true
			}
		} else {
			if mk, merr := runner.Capture(ctx, "", fmt.Sprintf(`cat %s 2>/dev/null || true`, shQuote(extractMarker)), nil); merr == nil &&
				strings.TrimSpace(mk) == localMD5 {
				markerMatched = true
			}
		}
		if markerMatched {
			if ok, oerr := runner.Exists(path.Join(installDir, "repo")); oerr == nil && ok {
				j.Logf("extracted content matches the current bundle (md5 marker hit), skipping extraction")
				skipExtract = true
			}
		}
	}
	if !skipExtract {
		j.Logf("extracting into %s (GB-scale bundles take 1-3 minutes; see the sparse progress below)", installDir)
		tw := &throttledWriter{j: j, prefix: "extract: ", every: 25}
		if isLocal {
			// Local: native Go cleanup + direct tar invocation (Win10+ ships
			// bsdtar; no shell involved).
			for _, rel := range []string{"repo", "images", "docker-offline", "manifest.json", ".bundle-extracted"} {
				if err := os.RemoveAll(filepath.Join(installDir, rel)); err != nil {
					return fmt.Errorf("failed to clean old directories: %w", err)
				}
			}
			if err := lr.Exec(ctx, tw, installDir, []string{"tar", "-xzf", remoteBundle, "-C", ".", "-v"}, nil); err != nil {
				return fmt.Errorf("extraction failed: %w", err)
			}
		} else if err := runner.Run(ctx, tw, installDir, fmt.Sprintf(
			`%s rm -rf repo images docker-offline manifest.json %s && tar -xzf %s -C . -v`,
			sudo, shQuote(extractMarker), shQuote(remoteBundle)), nil); err != nil {
			return fmt.Errorf("extraction failed: %w", err)
		}
		if err := runner.WriteFile(extractMarker, []byte(localMD5)); err != nil {
			j.Logf("warning: failed to write the extraction marker (this install is unaffected; a re-run will extract again): %v", err)
		}
		j.Logf("extraction finished")
	}

	// 9. Install docker/compose (from the bundle's installer materials).
	//    auto:   skip when the target already has working docker+compose,
	//            otherwise install (default);
	//    offline: run the install materials right away (the script itself
	//            skips a healthy docker; --force reinstalls);
	//    skip:   do not install.
	dockerPolicy := req.DockerInstall
	if dockerPolicy == "" {
		dockerPolicy = DockerInstallAuto
	}
	needInstall := false
	switch dockerPolicy {
	case DockerInstallSkip:
		j.Logf("skipping docker installation as configured")
		if req.DockerForce {
			j.Logf("note: force-reinstall is set but the docker policy is skip; honoring skip")
		}
	case DockerInstallOffline:
		needInstall = true
	default:
		ready, derr := dockerReady(ctx, runner, sudo)
		if derr != nil {
			return derr
		}
		if req.DockerForce {
			// Force install: bypass "skip when a working docker exists".
			j.Logf("force install: bypassing the auto-detection, reinstalling docker")
			needInstall = true
		} else if ready {
			// Version floor assertion: a working `docker info` does not mean
			// the version is sufficient — old daemons (< 20.10) cannot start
			// containers on cgroup v2 hosts, and "it runs so skip it" would
			// leave the old version in place on the target.
			suff, sreason, serr := dockerVersionSufficient(ctx, runner, sudo)
			if serr != nil {
				j.Logf("warning: docker version probe failed (%v), treating as usable and skipping installation", serr)
				j.Logf("auto-detection: target already has working docker and compose, skipping installation")
			} else if suff {
				j.Logf("auto-detection: target already has working docker and compose, skipping installation")
			} else {
				j.Logf("auto-detection: %s, installing from the bundle materials instead (this restarts docker and affects running containers)", sreason)
				needInstall = true
			}
		} else {
			j.Logf("auto-detection: target is missing docker/compose, will install")
			needInstall = true
		}
	}
	if needInstall {
		if runner.Platform() != "linux" {
			// macOS/Windows get no automatic docker installation (the offline
			// materials are bash+systemd based, get.docker.com is Linux-only);
			// the user installs Docker Desktop manually.
			return fmt.Errorf("automatic docker installation only supports Linux: the current target is %s. Install and start Docker Desktop manually (official download: %s ), then set the docker policy to skip and retry",
				platformLabel(runner.Platform()), dockerDesktopURL)
		}
		if m.Online {
			// Online bundle: the target installs docker straight from the
			// official get.docker.com script. The script installs docker-ce
			// and docker-compose-plugin and enables the systemd service.
			// curl missing falls back to wget.
			j.Logf("installing docker/compose online: get.docker.com (includes the compose plugin)")
			installCmd := fmt.Sprintf(
				`(curl -fsSL https://get.docker.com -o get-docker.sh || wget -qO get-docker.sh https://get.docker.com) && `+
					`%s sh get-docker.sh && rm -f get-docker.sh`, sudo)
			if err := runWithRetryRemote(ctx, runner, j, installDir, installCmd, "docker online install"); err != nil {
				return fmt.Errorf("docker online install failed: %w (the target must be able to reach get.docker.com; alternatively use an offline bundle and install from its materials)", err)
			}
			ready, derr := dockerReady(ctx, runner, sudo)
			if derr != nil {
				return fmt.Errorf("post-install docker verification failed: %w", derr)
			}
			if !ready {
				return fmt.Errorf("post-install docker verification failed: docker/compose still unavailable (if the compose plugin did not come with the script, install docker-compose-plugin on the target and retry)")
			}
			j.Logf("docker and compose are ready")
		} else {
			script := req.InstallScript
			if script == "" {
				script = "install.sh"
			}
			scriptOK, err := runner.Exists(path.Join(installDir, "docker-offline", script))
			if err != nil {
				// Execution failure and a missing file must be told apart:
				// the former is a channel/environment problem (swallowing
				// the error once got misreported as "install materials
				// missing" while the file existed), which badly misleads
				// troubleshooting.
				return fmt.Errorf("checking the docker install materials failed to execute (the file is not necessarily missing): %w", err)
			}
			if !scriptOK {
				return fmt.Errorf("install materials missing: %s (the bundle was packed without an installer image, so docker cannot be installed — choose skip or install docker on the target yourself)",
					path.Join(installDir, "docker-offline", script))
			}
			args := req.InstallScriptArgs
			if len(args) == 0 {
				args = []string{"--yes"}
			}
			if req.DockerForce {
				forced := false
				for _, a := range args {
					if a == "--force" {
						forced = true
						break
					}
				}
				if !forced {
					args = append(args, "--force")
				}
			}
			j.Logf("installing docker/compose: docker-offline/%s %s", script, strings.Join(args, " "))
			if err := runner.Run(ctx, j, installDir, fmt.Sprintf(
				`%s bash %s %s`, sudo, shQuote(path.Join("docker-offline", script)),
				strings.Join(quoteAll(args), " ")), nil); err != nil {
				return fmt.Errorf("docker installation failed (check the install materials and passwordless sudo): %w", err)
			}
		}
	}

	// 10. (Linux targets) Grant the current user direct docker access by
	//     adding it to the docker group. This branch requires "user is
	//     non-root and docker needs sudo" (sudo != "") — root and docker
	//     group users can already connect directly, nothing to do. The docker
	//     socket is typically root:docker 660; group members use docker
	//     without sudo. Membership is checked against the group database
	//     (/etc/group), and changes take effect on the **next login
	//     session**; this install keeps using the elevation method decided
	//     earlier and is unaffected. Failures only warn (docker still works
	//     through sudo).
	if runner.Platform() == "linux" && sudo != "" {
		var who strings.Builder
		if err := runner.Run(ctx, &who, "", `id -un`, nil); err == nil {
			user := strings.TrimSpace(who.String())
			if validUnixUsername(user) && user != "root" {
				if userInDockerGroupDB(ctx, runner, user) {
					j.Logf("user %s is already in the docker group (if this session still needs sudo, logging in again removes the need)", user)
				} else {
					j.Logf("adding user %s to the docker group (docker commands work without sudo after logging in again)...", user)
					var probe strings.Builder
					if err := runner.Run(ctx, &probe, "",
						fmt.Sprintf(`%s getent group docker`, sudo), nil); err != nil {
						// No docker group (docker hand-installed by an admin):
						// create the group first.
						var noop strings.Builder
						if err := runner.Run(ctx, &noop, "",
							fmt.Sprintf(`%s groupadd docker`, sudo), nil); err != nil {
							j.Logf("warning: failed to create the docker group: %v", err)
						}
					}
					if uerr := runner.Run(ctx, j, "",
						fmt.Sprintf(`%s usermod -aG docker %s`, sudo, shQuote(user)), nil); uerr != nil {
						j.Logf("warning: failed to add to the docker group (this install is unaffected; docker keeps running through sudo): %v", uerr)
					}
				}
			}
		}
	}

	// 11. Container egress (NAT) pre-check: when daemon.json contains
	//     "iptables": false, dockerd writes no MASQUERADE rule and containers
	//     cannot reach the public internet (the host itself works fine).
	//     Remedies by policy: restart = merge the config + restart docker
	//     (recommended); manual = no restart, add the rules by hand after
	//     compose up and persist them (the networks only exist then);
	//     skip = warn only.
	natPolicy := req.NATFixPolicy
	if natPolicy == "" {
		natPolicy = NATFixRestart
	}
	natNeedManual := false
	if runner.Platform() == "linux" {
		j.Logf("container egress check (daemon.json iptables switch and NAT rules)...")
		if diag, derr := diagnoseNAT(ctx, runner); derr != nil {
			j.Logf("warning: NAT diagnosis did not complete (%v), continuing the install", derr)
		} else if diag.IptablesDisabled || len(diag.MissingMASQ) > 0 {
			j.Logf("%s", diag.Summary())
			switch natPolicy {
			case NATFixSkip:
				j.Logf("warning: fix skipped as configured — containers most likely cannot reach the public internet (run compose-installer doctor for the fix commands)")
			case NATFixManual:
				natNeedManual = true
				j.Logf("policy manual: docker will not be restarted; MASQUERADE rules are added and persisted automatically after compose up")
			default:
				if err := fixDaemonJSONRestart(ctx, runner, j, sudo); err != nil {
					return fmt.Errorf("failed to fix the daemon.json iptables setting: %w (switch to the manual policy or run compose-installer doctor)", err)
				}
			}
		} else {
			j.Logf("NAT/forwarding configuration is healthy, nothing to do")
		}
	}

	// 12. Host kernel quota pre-check (Linux): inotify (file watching) and
	//     the global handle/listen quotas (file-max/somaxconn) are host-wide
	//     kernel quotas — a container cannot raise them. On low-quota
	//     machines containers exhaust them at startup or during runtime
	//     (ENOSPC / Too many open files / connection queue refusals).
	//     Values below the recommended floor are raised in place (effective
	//     immediately, no docker restart) and persisted under /etc/sysctl.d/
	//     (raise-only, idempotent); overcommit/port-range items are advisory
	//     only and never touched automatically. Failures just warn;
	//     compose-installer doctor prints the fix commands.
	if runner.Platform() == "linux" {
		j.Logf("inotify quota check (containers depend on file watching; a too-low quota fails container startup)...")
		if err := ensureInotifyLimits(ctx, runner, j, sudo); err != nil {
			j.Logf("warning: %v (not blocking the install)", err)
			j.Logf("warning: if containers fail with ENOSPC / Too many open files, run compose-installer doctor on the target for the fix commands")
		}
		j.Logf("kernel quota check (file handles/listen backlog; too low causes EMFILE or connection refusals)...")
		if err := ensureKernelQuotas(ctx, runner, j, sudo); err != nil {
			j.Logf("warning: %v (not blocking the install)", err)
			j.Logf("warning: run compose-installer doctor to see the current quotas and fix commands")
		}
	}

	// 13. Build .env (bundle template + manifest tags/exports/rewrites + user
	//     overrides). Done before image acquisition: online bundles run
	//     docker compose pull, which needs the *_TAG values from .env
	//     (compose uses strict ${VAR:?required} interpolation — a missing tag
	//     fails immediately).
	repoWorkdir := path.Join(installDir, "repo", m.Workdir)
	envContent, err := buildEnvFile(ctx, runner, repoWorkdir, installDir, m, req.EnvVars, j)
	if err != nil {
		return err
	}
	if err := runner.WriteFile(path.Join(repoWorkdir, ".env"), envContent); err != nil {
		return fmt.Errorf("failed to write .env: %w", err)
	}
	j.Logf(".env written to %s", path.Join(repoWorkdir, ".env"))

	// Profiles are passed as explicit --profile arguments: sudo's default
	// env_reset drops a COMPOSE_PROFILES environment variable, excluding the
	// profile services from the run (no service selected).
	profileArgs := ""
	for _, p := range m.Profiles {
		profileArgs += " --profile " + shQuote(p)
	}
	// Local direct invocation passes profiles through the process environment.
	var upEnv []string
	if len(m.Profiles) > 0 {
		upEnv = append(upEnv, "COMPOSE_PROFILES="+strings.Join(m.Profiles, ","))
	}

	// 14. Acquire images: offline bundles docker load the bundled images;
	//     online bundles docker login + docker compose pull on the target.
	if m.Online {
		if req.RegistryUser != "" {
			// An empty registry logs into Docker Hub (docker's own default);
			// private registries must be named explicitly.
			registry := req.Registry
			registryLabel := registry
			if registryLabel == "" {
				registryLabel = "docker.io (default)"
			}
			j.Logf("logging into the image registry (on the target): %s (user %s)", registryLabel, req.RegistryUser)
			if isLocal {
				// Local: the password goes to docker login via stdin (no
				// shell, never a command-line argument).
				argv := []string{"docker", "login", "--password-stdin", "-u", req.RegistryUser}
				if registry != "" {
					argv = append(argv, registry)
				}
				if err := lr.ExecStdin(ctx, j, repoWorkdir, argv, nil, req.RegistryPassword+"\n"); err != nil {
					return fmt.Errorf("docker login failed (check the registry address and credentials): %w", err)
				}
			} else {
				// With sudo -S the first stdin line is consumed by sudo: the
				// sudo password and the registry password are fed as two
				// consecutive lines, docker login --password-stdin reads what
				// remains; root / sudo -n shapes pipe just the registry
				// password. Credentials never land in command-line arguments.
				regPart := ""
				if registry != "" {
					regPart = " " + shQuote(registry)
				}
				var loginCmd string
				if sudoMode == "pass" {
					loginCmd = fmt.Sprintf(
						`printf '%%s\n%%s\n' %s %s | sudo -S -p '' docker login --password-stdin -u %s%s`,
						shQuote(sudoPass), shQuote(req.RegistryPassword), shQuote(req.RegistryUser), regPart)
				} else {
					loginCmd = fmt.Sprintf(
						`printf '%%s\n' %s | %s docker login --password-stdin -u %s%s`,
						shQuote(req.RegistryPassword), sudo, shQuote(req.RegistryUser), regPart)
				}
				if err := runner.Run(ctx, j, repoWorkdir, loginCmd, nil); err != nil {
					return fmt.Errorf("docker login on the target failed (check the registry address and credentials): %w", err)
				}
			}
		} else {
			j.Logf("no target registry account given, reusing the target's existing docker login")
		}
		j.Logf("pulling images: docker compose pull (%d total; automatic retries on weak networks; already-pulled layers resume)", len(m.Images))
		if isLocal {
			if err := retryExec(ctx, lr, j, repoWorkdir, upEnv,
				[]string{"docker", "compose", "-f", m.ComposeFile, "--env-file", ".env", "pull"}, "docker compose pull"); err != nil {
				return fmt.Errorf("image pull failed: %w (make sure this machine can reach the image registry)", err)
			}
		} else {
			pullCmd := fmt.Sprintf(`%s docker compose -f %s%s --env-file .env pull`,
				sudo, shQuote(m.ComposeFile), profileArgs)
			if err := runWithRetryRemote(ctx, runner, j, repoWorkdir, pullCmd, "docker compose pull"); err != nil {
				return fmt.Errorf("image pull failed: %w (make sure the target machine can reach the image registry)", err)
			}
		}
	} else {
		imgTar := path.Join(installDir, "images", m.Product+".tar")
		if ok, err := runner.Exists(imgTar); err == nil && !ok {
			// Extraction just finished; dodge occasional session/filesystem
			// lag: retry once.
			time.Sleep(2 * time.Second)
			if ok2, err2 := runner.Exists(imgTar); err2 == nil && ok2 {
				j.Logf("image file missed on first check, retry confirmed it exists: %s", imgTar)
			} else {
				// Still missing: dump the directory listing into the log for
				// diagnosis (permissions, incomplete extraction, ...).
				var diag strings.Builder
				if isLocal {
					diag.WriteString(localDirListing(installDir, "images"))
				} else if out, derr := runner.Capture(ctx, "", fmt.Sprintf(
					`ls -la %s 2>&1; echo ---; ls -la %s 2>&1`,
					shQuote(path.Join(installDir, "images")), shQuote(installDir)), nil); derr == nil {
					diag.WriteString(out)
				}
				j.Logf("images directory contents:\n%s", diag.String())
				return fmt.Errorf("offline bundle is missing its image file: %s (rebuild a complete offline bundle with the packing tool, or use an online bundle; the directory listing above is for troubleshooting)", imgTar)
			}
		}
		// Non-root needs sudo here too; large images take 1-2 minutes to load.
		j.Logf("importing images (docker load) (no output for a while is normal for large images)")
		if isLocal {
			// Local: Go-side globbing + one direct docker load per tar (no
			// shell).
			tars, gerr := filepath.Glob(filepath.Join(installDir, "images", "*.tar"))
			if gerr != nil || len(tars) == 0 {
				return fmt.Errorf("no image files found to import (%s)", filepath.Join(installDir, "images"))
			}
			for _, f := range tars {
				j.Logf("load %s", filepath.Base(f))
				if err := lr.Exec(ctx, j, "", []string{"docker", "load", "-i", f}, nil); err != nil {
					return fmt.Errorf("docker load failed (%s): %w", f, err)
				}
			}
		} else if err := runner.Run(ctx, j, installDir,
			fmt.Sprintf(`for f in images/*.tar; do echo "load $f"; %s docker load -i "$f"; done`, sudo), nil); err != nil {
			return fmt.Errorf("docker load failed: %w", err)
		}
	}

	// 15. Windows local bind-mount sharing authorization probe: Docker
	//     Desktop pops a sharing confirmation dialog the first time a Windows
	//     directory is mounted. Letting that happen during compose up would
	//     block or fail the startup. A throwaway container (using an already
	//     loaded image) moves the dialog into a step that explicitly waits
	//     for the user; authorizing once persists, and the later compose up
	//     never sees the dialog again.
	if isLocal && runner.Platform() == "windows" {
		probeImg := "hello-world"
		if len(m.Images) > 0 {
			probeImg = m.Images[0]
		}
		j.Logf("probing Docker directory sharing authorization (Windows pops a confirmation dialog on first mount; waiting up to 5 minutes)")
		j.Logf(">>> if the Docker Desktop file sharing confirmation dialog appears, click Allow / Share it <<<")
		pctx, pcancel := context.WithTimeout(ctx, 5*time.Minute)
		perr := lr.Exec(pctx, j, "",
			[]string{"docker", "run", "--rm", "--entrypoint", "true", "-v", installDir + ":/probe", probeImg}, nil)
		pcancel()
		if perr != nil {
			return fmt.Errorf(
				"directory sharing authorization incomplete: %w (add %s under Docker Desktop → Settings → Resources → File Sharing and retry, or reinstall and click Allow in the dialog)",
				perr, installDir)
		}
		j.Logf("directory sharing authorization works (later mounts will not pop the dialog)")
	}

	// 16. docker compose up
	j.Logf("starting services: docker compose up -d")
	if isLocal {
		// Local: direct docker compose invocation (profiles via the process
		// environment; no shell, no sudo).
		base := []string{"docker", "compose", "-f", m.ComposeFile, "--env-file", ".env"}
		if err := lr.Exec(ctx, j, repoWorkdir, append(append([]string{}, base...), "up", "-d"), upEnv); err != nil {
			return fmt.Errorf("docker compose up failed: %w", err)
		}
		if err := lr.Exec(ctx, j, repoWorkdir, append(append([]string{}, base...), "ps"), upEnv); err != nil {
			j.Logf("warning: failed to query container status, check manually")
		}
	} else {
		profileArgs := ""
		for _, p := range m.Profiles {
			profileArgs += " --profile " + shQuote(p)
		}
		upCmd := fmt.Sprintf(`%s docker compose -f %s%s --env-file .env up -d`,
			sudo, shQuote(m.ComposeFile), profileArgs)
		if err := runner.Run(ctx, j, repoWorkdir, upCmd, nil); err != nil {
			return fmt.Errorf("docker compose up failed: %w", err)
		}
		// Status wrap-up.
		if err := runner.Run(ctx, j, repoWorkdir,
			fmt.Sprintf(`%s docker compose -f %s%s --env-file .env ps`, sudo, shQuote(m.ComposeFile), profileArgs), nil); err != nil {
			j.Logf("warning: failed to query container status, check manually")
		}
	}

	// 17. NAT plan B (manual policy with a detected problem): the compose
	//     networks exist at this point — add MASQUERADE rules for every
	//     bridge subnet and persist them (recomputed at boot).
	if natNeedManual {
		if err := ensureNATRulesManual(ctx, runner, j, sudo); err != nil {
			return fmt.Errorf("failed to add the NAT (MASQUERADE) rules: %w", err)
		}
	}

	// 18. Container connectivity self-check: a throwaway container joins the
	//     compose network and probes gateway -> public IP -> DNS; failures
	//     come back as classified, actionable conclusions instead of one
	//     generic error. Runs only for Linux SSH remote installs (the local
	//     Docker Desktop shape does not apply).
	if !isLocal && runner.Platform() == "linux" {
		j.Logf("container connectivity self-check (gateway/public/DNS)...")
		// The compose project name is the directory holding the compose file
		// (repo/<workdir> in the install layout).
		if net, nerr := composeNetworkName(ctx, runner, path.Base(repoWorkdir)); nerr != nil {
			j.Logf("warning: %v, skipping the connectivity self-check", nerr)
		} else if img := pickProbeImage(m.Images); img == "" {
			j.Logf("warning: no usable probe image in the bundle, skipping the connectivity self-check")
		} else {
			// endpoint probing is product-specific and stays with the
			// products; the generic check covers gateway/public/DNS.
			if res, perr := probeContainerEgress(ctx, runner, j, net, img, ""); perr != nil {
				j.Logf("warning: connectivity probe failed to run (%v); run compose-installer doctor to investigate", perr)
			} else {
				summary := classifyProbe(res)
				// Healthy = gateway+public+DNS all reachable. Branch on the
				// structured result instead of matching the (localized)
				// summary text produced by classifyProbe.
				healthy := res.GatewayOK && res.PublicOK && (res.DNSOK == nil || *res.DNSOK)
				if healthy {
					j.Logf("%s", summary)
				} else {
					j.Logf("warning: %s", summary)
				}
			}
		}
	}

	// 19. Give the directory ownership back: bind-mount directories created
	//     by dockerd/containers as root (repo/data etc.) end up root-owned;
	//     chown them back to the login user so the next reinstall's cleanup
	//     and extraction are not blocked.
	if sudo != "" && loginuser(req) != "" {
		j.Logf("returning ownership of the install directory to %s ...", loginuser(req))
		if err := runner.Run(ctx, j, "", fmt.Sprintf(
			`%s chown -R %s %s`, sudo, shQuote(loginuser(req)), shQuote(installDir)), nil); err != nil {
			j.Logf("warning: chown failed (this install is unaffected); the next reinstall will clean the old directory via sudo")
		}
	}

	// 20. Remove the bundle only after full success (kept on failure, so the
	//     md5 comparison skips the re-upload on the next run).
	j.Logf("install succeeded, cleaning up bundle: %s", remoteBundle)
	if isLocal {
		if err := os.Remove(remoteBundle); err != nil && !os.IsNotExist(err) {
			j.Logf("warning: bundle cleanup failed (the install result is unaffected); %s can be removed manually", remoteBundle)
		}
	} else if err := runner.Run(ctx, j, "", fmt.Sprintf(`rm -f %s`, shQuote(remoteBundle)), nil); err != nil {
		j.Logf("warning: bundle cleanup failed (the install result is unaffected); %s can be removed manually", remoteBundle)
	}
	j.Logf("install complete ✔ directory: %s", installDir)
	return nil
}

// loginuser returns the remote login user name (used to chown the install
// directory back after sudo created parts of it).
func loginuser(req InstallRequest) string {
	if req.Mode == "remote" && req.SSH.User != "" {
		return req.SSH.User
	}
	return ""
}

// defaultInstallDirFor is the single source of truth for the default install
// directory (shared by the UI default-directory API and the install fallback):
//
//	dirName: from the bundle manifest (InstallDirName, falling back to the
//	         product name)
//	remote:  non-root user -> /home/<user>/<dirName>; root/empty -> /opt/<dirName>
//	local:   Linux -> /opt/<dirName>; macOS/Windows -> ~/<dirName>
func defaultInstallDirFor(dirName, mode, sshUser string) string {
	if dirName == "" {
		dirName = "compose-installer" // defensive: malformed manifests only
	}
	if mode == "remote" {
		if sshUser != "" && sshUser != "root" {
			return "/home/" + sshUser + "/" + dirName
		}
		return "/opt/" + dirName
	}
	if runtime.GOOS != "linux" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.ToSlash(filepath.Join(home, dirName))
		}
	}
	return "/opt/" + dirName
}

// tuneSSHDSessionTimeout optional optimization: make sure the target sshd's
// idle session tolerance is >= 30 minutes. Idempotently rewrites
// ClientAliveInterval=60 / ClientAliveCountMax=30 in /etc/ssh/sshd_config
// (60s probes × 30 missed replies = 30 minutes), validates with sshd -t and
// reloads (no disconnect). Failures only warn and never block the install.
func tuneSSHDSessionTimeout(ctx context.Context, runner Runner, j *Job, sudo string) {
	script := `
set -e
ensure() {
  if grep -qE "^[[:space:]]*$1[[:space:]]" /etc/ssh/sshd_config; then
    sed -i -E "s|^[[:space:]]*$1[[:space:]].*|$1 $2|" /etc/ssh/sshd_config
  else
    printf '\n%s %s\n' "$1" "$2" >> /etc/ssh/sshd_config
  fi
}
ensure ClientAliveInterval 60
ensure ClientAliveCountMax 30
/usr/sbin/sshd -t 2>/dev/null || sshd -t
systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || killall -HUP sshd 2>/dev/null || true
`
	if err := runner.Run(ctx, j, "", fmt.Sprintf(`%s bash -c %s`, sudo, shQuote(script)), nil); err != nil {
		j.Logf("warning: SSH session timeout tuning failed (%v); the install continues", err)
		return
	}
	j.Logf("SSH session timeout confirmed at 30 minutes (ClientAliveInterval 60 × CountMax 30)")
}

// checkDiskSpace verifies the filesystem holding the target install directory
// has enough free space; insufficient space aborts the install (before a huge
// bundle is uploaded or half-extracted). Unrunnable df only warns and skips.
// warnOnly=true (online bundles, where the estimate has no precise anchor)
// downgrades insufficiency to a warning.
// Platform notes: Linux/SSH and Windows (git-bash GNU df) use
// -B1 --output=avail (bytes); macOS has BSD df, so df -k reads the KB column
// and converts.
func checkDiskSpace(ctx context.Context, runner Runner, j *Job, dir string, need uint64, warnOnly bool) error {
	// The directory may not exist yet (first install): walk up to the nearest
	// existing ancestor so df does not fail on a nonexistent path and skip
	// the check entirely.
	checkDir := filepath.ToSlash(dir)
	for i := 0; i < 8; i++ {
		if ok, err := runner.Exists(checkDir); err == nil && ok {
			break
		}
		parent := path.Dir(checkDir)
		if parent == checkDir {
			break
		}
		checkDir = parent
	}
	var avail uint64
	if _, isLocal := runner.(*LocalRunner); isLocal {
		// Local: system API (Windows GetDiskFreeSpaceEx / unix statfs), no
		// shell involved.
		v, derr := diskAvailable(checkDir)
		if derr != nil {
			j.Logf("warning: cannot check local disk space (%v), skipping the check", derr)
			return nil
		}
		avail = v
	} else {
		cmd := fmt.Sprintf(`df -B1 --output=avail %s | tail -1`, shQuote(checkDir))
		unit := uint64(1)
		if runner.Platform() == "darwin" {
			cmd = fmt.Sprintf(`df -k %s | tail -1 | awk '{print $4}'`, shQuote(checkDir))
			unit = 1024
		}
		out, err := runner.Capture(ctx, "", cmd, nil)
		if err != nil {
			j.Logf("warning: cannot check target disk space (%v), skipping the check", err)
			return nil
		}
		v, perr := strconv.ParseUint(strings.TrimSpace(out), 10, 64)
		if perr != nil {
			j.Logf("warning: unparseable disk space output (%q), skipping the check", out)
			return nil
		}
		avail = v * unit
	}
	if checkDir != filepath.ToSlash(dir) {
		j.Logf("disk check: %s does not exist yet, checking the nearest ancestor %s", filepath.ToSlash(dir), checkDir)
	}
	j.Logf("disk check: %s has %.1f GB available, roughly %.1f GB needed (%s)",
		checkDir, gib(avail), gib(need), map[bool]string{
			true:  "online bundle: rough estimate by image count, for reference only",
			false: "estimated at 3x the bundle size: bundle + extraction + image expansion",
		}[warnOnly])
	if avail < need {
		msg := fmt.Sprintf("insufficient disk space on the target: %.1f GB available < %.1f GB needed; free up space (see df -h) and retry", gib(avail), gib(need))
		if warnOnly {
			j.Logf("warning: %s (online bundles carry a rough estimate only; the install continues and docker pull decides)", msg)
			return nil
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// dockerReady probes whether docker and compose both work on the target
// (shared by the auto detection and the post-install verification). A non-nil
// error means the probe itself failed to execute (channel problem) — strictly
// distinguished from "docker not ready" (false, nil).
func dockerReady(ctx context.Context, runner Runner, sudo string) (bool, error) {
	if lr, ok := runner.(*LocalRunner); ok {
		// Local: invoke docker directly (no shell; on Windows this is immune
		// to the WSL bash PATH).
		var sink strings.Builder
		if err := lr.Exec(ctx, &sink, "", []string{"docker", "info"}, nil); err != nil {
			return false, nil
		}
		sink.Reset()
		return lr.Exec(ctx, &sink, "", []string{"docker", "compose", "version"}, nil) == nil, nil
	}
	probe, err := runner.Capture(ctx, "", fmt.Sprintf(
		`%s docker info >/dev/null 2>&1 && %s docker compose version >/dev/null 2>&1 && echo __OK__ || echo __MISSING__`,
		sudo, sudo), nil)
	if err != nil {
		return false, fmt.Errorf("docker probe command failed to execute: %w", err)
	}
	return strings.TrimSpace(probe) == "__OK__", nil
}

// dockerMinMajor/dockerMinMinor minimum daemon version: 20.10 is where cgroup
// v2 support landed (Ubuntu 22.04+/Debian 11+/RHEL 9 default to v2; an old
// daemon cannot start containers on such hosts) and where the compose v2
// plugin ecosystem is mature. Anything older counts as "unusable" and the auto
// policy replaces it with the bundled materials.
const (
	dockerMinMajor = 20
	dockerMinMinor = 10
)

// dockerVersionAtLeast reports whether the daemon version is >= maj.min
// ("27.5.1", "20.10.24~3" — only the first two numeric segments count;
// unparseable versions count as insufficient and the caller decides).
func dockerVersionAtLeast(ver string, maj, min int) bool {
	ver = strings.TrimSpace(strings.SplitN(ver, "~", 2)[0])
	parts := strings.Split(ver, ".")
	if len(parts) < 2 {
		return false
	}
	// Strip possible suffixes (e.g. the prerelease/distro tail of "24.0.7-1").
	for i := range parts[:2] {
		if j := strings.IndexAny(parts[i], "-+"); j >= 0 {
			parts[i] = parts[i][:j]
		}
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return major > maj || (major == maj && minor >= min)
}

// dockerVersionSufficient probes the installed docker's version and cgroup
// shape and decides whether it meets the minimum requirement (the version
// floor also guarantees cgroup v2 compatibility). ok=false comes with a reason;
// probe failures are strictly distinguished from "insufficient" (err means the
// probe itself failed).
func dockerVersionSufficient(ctx context.Context, runner Runner, sudo string) (ok bool, reason string, err error) {
	ver, verr := runner.Capture(ctx, "", fmt.Sprintf(`%s docker version --format '{{.Server.Version}}' 2>/dev/null`, sudo), nil)
	if verr != nil {
		return false, "", fmt.Errorf("failed to read the docker version: %w", verr)
	}
	ver = strings.TrimSpace(ver)
	if ver == "" {
		return false, "", fmt.Errorf("failed to read the docker version: empty output (is the daemon running?)")
	}
	if !dockerVersionAtLeast(ver, dockerMinMajor, dockerMinMinor) {
		return false, fmt.Sprintf("docker %s is below the minimum required %d.%d (it cannot start containers on cgroup v2 hosts)", ver, dockerMinMajor, dockerMinMinor), nil
	}
	return true, "", nil
}

// retryExec retries direct local command invocations (same 3-attempt backoff
// the packing side uses for pulls).
func retryExec(ctx context.Context, lr *LocalRunner, j *Job, dir string, env []string, argv []string, label string) error {
	var lastErr error
	for attempt := 1; attempt <= pullAttempts; attempt++ {
		j.Logf("[%d/%d] %s", attempt, pullAttempts, label)
		if err := lr.Exec(ctx, j, dir, argv, env); err == nil {
			if attempt > 1 {
				j.Logf("%s succeeded (attempt %d)", label, attempt)
			}
			return nil
		} else {
			lastErr = err
		}
		if attempt < pullAttempts {
			wait := time.Duration(attempt*3) * time.Second
			j.Logf("attempt %d failed, retrying in %v", attempt, wait)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
	}
	return lastErr
}

// localDirListing renders a diagnostic listing of a local directory's content
// (the ls -la equivalent).
func localDirListing(base string, sub string) string {
	var b strings.Builder
	for _, rel := range []string{filepath.Join(base, sub), base} {
		fmt.Fprintf(&b, "== %s ==\n", rel)
		entries, err := os.ReadDir(rel)
		if err != nil {
			fmt.Fprintf(&b, "read failed: %v\n", err)
			continue
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			fmt.Fprintf(&b, "%s %10d %s\n", info.ModTime().Format("2006-01-02 15:04"), info.Size(), e.Name())
		}
		b.WriteString("--\n")
	}
	return b.String()
}

// runWithRetryRemote runs a command on the target with automatic retries —
// the same 3-attempt backoff (3s/6s) the packing side uses for pulls, suited
// to weak networks; label is for the log output.
func runWithRetryRemote(ctx context.Context, runner Runner, j *Job, dir, cmd, label string) error {
	var lastErr error
	for attempt := 1; attempt <= pullAttempts; attempt++ {
		j.Logf("[%d/%d] %s", attempt, pullAttempts, label)
		err := runner.Run(ctx, j, dir, cmd, nil)
		if err == nil {
			if attempt > 1 {
				j.Logf("%s succeeded (attempt %d)", label, attempt)
			}
			return nil
		}
		lastErr = err
		if attempt < pullAttempts {
			wait := time.Duration(attempt*3) * time.Second
			j.Logf("attempt %d failed, retrying in %v", attempt, wait)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
	}
	return lastErr
}

// gib converts bytes to GiB (1 decimal place, display only).
func gib(v uint64) float64 {
	return float64(v) / 1024 / 1024 / 1024
}

// syncTargetTime calibrates the target clock: compare epoch seconds and, when
// the drift exceeds the threshold (2 seconds), set the target clock straight
// to the local time with `date -s @epoch` (no NTP per design; epoch assignment
// is timezone-independent), then `hwclock --systohc` persists the result to
// the hardware clock — otherwise the system clock falls back to the old RTC
// value after reboot. A failed set only warns and never blocks the install.
func syncTargetTime(ctx context.Context, runner Runner, j *Job, sudo string) error {
	remote, err := remoteEpoch(ctx, runner)
	if err != nil {
		return err
	}
	diff := time.Now().Unix() - remote
	if diff < 0 {
		diff = -diff
	}
	if diff <= 2 {
		j.Logf("time check: target matches this machine (drift %d s)", diff)
		return nil
	}
	j.Logf("time check: target drifts %d s, calibrating straight to this machine's time", diff)
	setCmd := fmt.Sprintf(`%s date -s @%d`, sudo, time.Now().Unix())
	if err := runner.Run(ctx, j, "", setCmd, nil); err != nil {
		return fmt.Errorf("failed to set the target time: %w", err)
	}
	// Persist the calibration to the hardware clock (RTC): date -s only
	// changes the system clock, which falls back to the stale RTC value after
	// a reboot (the classic trap on edge devices with inaccurate/dead RTC
	// batteries). Machines without an RTC (VMs etc.) only warn here.
	if err := runner.Run(ctx, j, "", fmt.Sprintf(`%s hwclock --systohc`, sudo), nil); err != nil {
		j.Logf("warning: failed to write the hardware clock (%v) — the time may fall back after reboot; normal for machines without an RTC (e.g. VMs)", err)
	} else {
		j.Logf("system time written to the hardware clock (survives reboot)")
	}
	if after, aerr := remoteEpoch(ctx, runner); aerr == nil {
		d := time.Now().Unix() - after
		if d < 0 {
			d = -d
		}
		j.Logf("time calibration done, current drift %d s", d)
	}
	return nil
}

// remoteEpoch reads the target's current epoch seconds.
func remoteEpoch(ctx context.Context, runner Runner) (int64, error) {
	out, err := runner.Capture(ctx, "", `date +%s`, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to read the target time: %w", err)
	}
	v, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unexpected target time output: %q", out)
	}
	return v, nil
}

// ensureInstallDir creates the install directory: first as the current user;
// on failure (e.g. no permission under /opt) with sudo available, create it
// via sudo and hand ownership to the login user so the later upload /
// extraction / .env writes need no further elevation.
func ensureInstallDir(ctx context.Context, runner Runner, j *Job, installDir, sudo, user string) error {
	if err := runner.MkdirAll(installDir); err == nil {
		return nil
	}
	if sudo != "" {
		j.Logf("creating %s as the current user failed; creating it with sudo and handing over ownership", installDir)
		chown := ""
		if user != "" {
			chown = fmt.Sprintf(` && %s chown -R %s %s`, sudo, shQuote(user), shQuote(installDir))
		}
		if err := runner.Run(ctx, j, "", fmt.Sprintf(
			`%s mkdir -p %s%s`, sudo, shQuote(installDir), chown), nil); err != nil {
			return fmt.Errorf("sudo failed to create the install directory: %w", err)
		}
		return nil
	}
	return fmt.Errorf("failed to create the install directory (permission denied): %s; non-root users should pick a home directory (e.g. /home/<user>/%s) or provide a sudo password", installDir, path.Base(installDir))
}

// detectTargetArch probes the target's CPU architecture (uname -m →
// linux/amd64|linux/arm64); a failed probe returns an empty string (the check
// is skipped, the install is not blocked).
func detectTargetArch(ctx context.Context, runner Runner, j *Job) string {
	var buf strings.Builder
	if err := runner.Run(ctx, &buf, "", `uname -m`, nil); err != nil {
		j.Logf("warning: cannot probe the target architecture (%v), skipping the architecture check", err)
		return ""
	}
	arch := archFromUname(strings.TrimSpace(buf.String()))
	if arch == "" {
		j.Logf("warning: unrecognized target architecture %q, skipping the architecture check", strings.TrimSpace(buf.String()))
		return ""
	}
	j.Logf("target architecture: %s", arch)
	return arch
}

// archFromUname maps a uname -m output to linux/<arch>; unknown returns "".
func archFromUname(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "x86_64", "amd64":
		return "linux/amd64"
	case "aarch64", "arm64":
		return "linux/arm64"
	default:
		return ""
	}
}

// validUnixUsername checks that a user name contains only safe characters
// (defense before splicing it into an elevated command).
func validUnixUsername(u string) bool {
	return regexp.MustCompile(`^[a-z_][a-z0-9._-]{0,31}$`).MatchString(u)
}

// userInDockerGroupDB checks the group database (/etc/group, via
// id -nG <user>) for docker group membership — independent of the session's
// effective groups (a newly added member needs to log in again).
func userInDockerGroupDB(ctx context.Context, runner Runner, user string) bool {
	var buf strings.Builder
	if err := runner.Run(ctx, &buf, "", fmt.Sprintf(`id -nG %s`, shQuote(user)), nil); err != nil {
		return false
	}
	for _, g := range strings.Fields(buf.String()) {
		if g == "docker" {
			return true
		}
	}
	return false
}

// platformLabel maps platform codes to display names.
func platformLabel(p string) string {
	switch p {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	default:
		return "Linux"
	}
}

// sudoPrefix decides how commands are elevated on the target:
//
//	macOS/Windows: docker comes from Docker Desktop and the current user can
//	               reach it; there is no sudo concept. When docker is not
//	               usable, point at the manual install (the install script is
//	               Linux-only);
//	Linux:         root        -> no sudo needed;
//	              sudo -n     -> passwordless sudo works;
//	              sudo -S     -> password fed via stdin (never a sudo
//	                           argument);
//	              direct docker -> no sudo but the user is in the docker
//	                           group (docker works directly); no elevation
//	                           needed (the install directory must be
//	                           user-writable; chown is skipped).
//
// The returned prefix is followed by a space and the privileged command;
// mode is ""|"nopass"|"pass" so commands that also consume stdin (like
// docker login --password-stdin) can be built per shape.
func sudoPrefix(ctx context.Context, runner Runner, j *Job, pass string) (string, string, error) {
	platform := runner.Platform()
	if platform == "darwin" || platform == "windows" {
		if ok, _ := dockerReady(ctx, runner, ""); ok {
			j.Logf("the current user can use docker directly (%s Docker Desktop); no elevation needed", platformLabel(platform))
			return "", "", nil
		}
		return "", "", fmt.Errorf("docker is unavailable locally: %s has no automatic docker install; install and start Docker Desktop manually (official download: %s , the docker compose plugin included) and retry, or switch to an SSH install onto a Linux host",
			platformLabel(platform), dockerDesktopURL)
	}

	idu, err := runner.Capture(ctx, "", `id -u`, nil)
	if err != nil {
		return "", "", fmt.Errorf("user probe failed: %w", err)
	}
	if strings.TrimSpace(idu) == "0" {
		j.Logf("target user is root; no sudo needed")
		return "", "", nil
	}

	// 1) passwordless sudo
	var probe strings.Builder
	if err := runner.Run(ctx, &probe, "", `sudo -n true 2>/dev/null`, nil); err == nil {
		j.Logf("target user is not root; passwordless sudo available (sudo -n)")
		return "sudo -n", "nopass", nil
	}

	// 2) password sudo (via stdin; -S reads the password from stdin)
	if pass != "" {
		var pw strings.Builder
		if err := runner.Run(ctx, &pw, "", fmt.Sprintf(
			`printf '%%s\n' %s | sudo -S -p '' true 2>/dev/null`, shQuote(pass)), nil); err == nil {
			j.Logf("target user is not root; using sudo -S (password fed via stdin)")
			return fmt.Sprintf(`printf '%%s\n' %s | sudo -S -p ''`, shQuote(pass)), "pass", nil
		}
		return "", "", fmt.Errorf("sudo password verification failed: make sure the password is correct (the SSH login password is used by default; a separate sudo password can be provided)")
	}

	// 3) no sudo but docker reachable directly (local Linux docker-group user)
	if ok, _ := dockerReady(ctx, runner, ""); ok {
		j.Logf("no sudo for the current user, but docker works directly (docker group user): no elevation needed; use a user-writable install directory such as the home directory")
		return "", "", nil
	}

	return "", "", fmt.Errorf("passwordless sudo is unavailable and no sudo password was given: configure NOPASSWD for the user, log in as root, add the user to the docker group, or provide a sudo password on the install page")
}

// buildEnvFile renders the target .env in fixed layers, later entries
// overriding earlier ones for the same key (docker compose resolves duplicate
// dotenv keys last-write-wins, so the file order is the precedence):
//
//  1. the bundle's env template (repo/<workdir>/.env.example) — template
//     lines, comments and blanks preserved verbatim;
//  2. tagEnvVars(m.Components) — fresh component tag lines (*_TAG);
//  3. m.EnvExports — manifest-provided extra entries, {version} in values
//     replaced by the bundle's version;
//  4. m.EnvRewrites — install-time defaults, {installDir} in values
//     replaced by the actual install directory; keys the user provided
//     explicitly in EnvVars are skipped entirely (the user value wins, the
//     default is not even written);
//  5. req.EnvVars (the extra map) — user overrides, highest precedence.
//
// At the end every m.RequiredEnv key must resolve to a present, non-empty
// value in the final file; missing keys fail with their names listed, so the
// caller can collect exactly what is missing instead of letting compose die
// later with an obscure ${VAR:?required} interpolation error.
func buildEnvFile(ctx context.Context, runner Runner, repoWorkdir, installDir string, m *BundleManifest, extra map[string]string, j *Job) ([]byte, error) {
	var base []byte
	exists, err := runner.Exists(path.Join(repoWorkdir, envTemplateName))
	if err == nil && exists {
		if out, cerr := runner.Capture(ctx, repoWorkdir, "cat "+envTemplateName, nil); cerr == nil {
			base = []byte(out)
		}
	}
	var out []string
	for _, line := range strings.Split(string(base), "\n") {
		out = append(out, line)
	}
	for _, kv := range tagEnvVars(m.Components) {
		out = append(out, kv)
	}
	for _, k := range sortedEnvKeys(m.EnvExports) {
		out = append(out, k+"="+strings.ReplaceAll(m.EnvExports[k], "{version}", m.Version))
	}
	for _, k := range sortedEnvKeys(m.EnvRewrites) {
		if _, userProvided := extra[k]; userProvided {
			continue // explicit user value wins; do not write the default
		}
		v := strings.ReplaceAll(m.EnvRewrites[k], "{installDir}", installDir)
		out = append(out, k+"="+v)
		j.Logf("env default applied: %s=%s (manifest envRewrites; override it via envVars)", k, v)
	}
	for _, k := range sortedEnvKeys(extra) {
		v := extra[k]
		if !envKeyRe.MatchString(k) {
			return nil, fmt.Errorf("invalid environment variable name: %q", k)
		}
		if strings.ContainsAny(v, "\n\r") {
			return nil, fmt.Errorf("environment variable %s must not contain line breaks", k)
		}
		out = append(out, k+"="+v)
	}
	// RequiredEnv validation against the final file (duplicate keys resolve
	// last-write-wins, matching how docker compose reads it).
	values := map[string]string{}
	for _, line := range out {
		idx := strings.Index(line, "=")
		if idx <= 0 {
			continue
		}
		values[line[:idx]] = line[idx+1:]
	}
	var missing []string
	for _, k := range m.RequiredEnv {
		if strings.TrimSpace(values[k]) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("missing required environment variables: %s (provide them via envVars and retry)", strings.Join(missing, ", "))
	}
	return []byte(strings.Join(out, "\n")), nil
}

// sortedEnvKeys returns the keys of m sorted, so generated .env files are
// deterministic (Go map iteration order is random).
func sortedEnvKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func quoteAll(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		out = append(out, shQuote(a))
	}
	return out
}

// readBundleManifest reads only the manifest.json at the bundle root (no full
// extraction).
func readBundleManifest(bundlePath string) (*BundleManifest, error) {
	f, err := os.Open(bundlePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("failed to read the bundle: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if strings.TrimPrefix(filepath.ToSlash(hdr.Name), "./") == "manifest.json" {
			raw, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			var m BundleManifest
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, fmt.Errorf("failed to parse manifest.json: %w", err)
			}
			return &m, nil
		}
	}
	return nil, fmt.Errorf("manifest.json is missing from the bundle (it may not be a bundle produced by this tool)")
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// throttledWriter passes command output through sparsely: one line every
// `every` lines — progress stays visible without flooding the log.
type throttledWriter struct {
	j      *Job
	prefix string
	every  int
	n      int
}

func (w *throttledWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line == "" {
			continue
		}
		w.n++
		if w.n%w.every == 1 {
			w.j.Logf("%s%s", w.prefix, line)
		}
	}
	return len(p), nil
}

// fileMD5Local computes a local file's md5 (streaming, large-file capable).
func fileMD5Local(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
