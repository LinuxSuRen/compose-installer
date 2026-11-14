package main

// Pack engine: builds a self-contained, installable tar.gz bundle from a
// docker compose project. Runs on a machine with network access and docker.
//
// The flow is fully config-driven (compose-installer.json, see config.go):
// product + environment select the compose file, an optional version
// manifest pins per-component image tags, and compose profiles trim the
// orchestration to the requested variant. See manifest.go for the bundle
// layout contract shared with the installer.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// pullAttempts is how many times a single image pull is attempted (including
// the first try).
const pullAttempts = 3

// packLogger is the slice of *Job the pack engine needs: progress logging
// plus the final status transition. Declared as an interface so the pack
// engine depends on nothing but this logging surface.
type packLogger interface {
	Logf(format string, args ...any)
	finish(status, errMsg string)
}

// resolvePackPlatform resolves the target platform of a pack request:
// empty/"auto" follows the packing machine (containers are always linux, so
// a darwin/arm64 machine defaults to linux/arm64); anything else must be
// linux/amd64 or linux/arm64.
func resolvePackPlatform(req string) (string, error) {
	p := strings.TrimSpace(req)
	if p == "" || p == "auto" {
		return "linux/" + runtime.GOARCH, nil
	}
	if p != "linux/amd64" && p != "linux/arm64" {
		return "", fmt.Errorf("unsupported target platform %q (expected linux/amd64 or linux/arm64; empty/auto follows the packing machine)", p)
	}
	return p, nil
}

// registryLogin runs docker login with the given account (credentials go
// through stdin, never onto the command line).
func registryLogin(ctx context.Context, j packLogger, registry, user, password string) error {
	j.Logf("logging into image registry: %s (user %s)", registry, user)
	cmd := exec.CommandContext(ctx, "docker", "login", "-u", user, "--password-stdin", registry)
	cmd.Stdin = strings.NewReader(password)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker login failed: %s", lastLines(out, 2))
	}
	j.Logf("registry login succeeded")
	return nil
}

// productName resolves the effective product name the same way
// InstallerConfig.Product does (used for the manifest and bundle naming).
func productName(c *InstallerConfig, requested string) string {
	if requested != "" {
		return requested
	}
	if c.DefaultProduct != "" {
		return c.DefaultProduct
	}
	if len(c.Products) == 1 {
		for k := range c.Products {
			return k
		}
	}
	return ""
}

func runPack(ctx context.Context, j packLogger, req PackRequest) (err error) {
	defer func() {
		if err != nil {
			j.finish("failed", err.Error())
		} else {
			j.finish("success", "")
		}
	}()

	// 0. Locate the project and resolve the product config. Environment,
	//     compose file, workdir, version manifest and default profiles all
	//     come from compose-installer.json; zero-config projects fall back to
	//     a single product around the auto-detected compose file.
	projectDir, err := resolveProjectDir(req.ProjectDir)
	if err != nil {
		return err
	}
	j.Logf("project dir: %s", projectDir)

	cfg, err := loadInstallerConfig(projectDir)
	if err != nil {
		return err
	}
	p, err := cfg.Product(req.Product)
	if err != nil {
		return err
	}
	name := productName(cfg, req.Product)

	composeFile, err := p.ComposeFileFor(req.Env)
	if err != nil {
		return err
	}
	workdir := filepath.FromSlash(p.Workdir)
	fullWorkdir := filepath.Join(projectDir, workdir)
	if composeFile == "" {
		// Zero-config product: pack whatever compose file can be detected.
		if composeFile = DetectComposeFile(fullWorkdir); composeFile == "" {
			return fmt.Errorf("no compose file found in %s (expected one of: %s)",
				fullWorkdir, strings.Join(composeFileCandidates, ", "))
		}
	}
	j.Logf("product: %s / env: %s / compose: %s", name, req.Env, filepath.Join(workdir, composeFile))

	// 0.4 Log into the image registry first when credentials were supplied
	//     (otherwise the machine's existing docker login state is reused).
	//     Online bundles pull nothing on the packing machine — no login.
	if req.Online {
		j.Logf("online bundle: no images pulled on the packing machine, skipping registry login")
	} else if req.RegistryUser != "" {
		if req.Registry == "" {
			return fmt.Errorf("registry is required when registryUser is set")
		}
		if err := registryLogin(ctx, j, req.Registry, req.RegistryUser, req.RegistryPassword); err != nil {
			return err
		}
	}

	// 0.5 Target platform: empty/"auto" follows the packing machine
	//     (containers are always linux); an explicit value enables cross
	//     packing (e.g. arm64 boxes packed from an x86 machine).
	platform, perr := resolvePackPlatform(req.Platform)
	if perr != nil {
		return perr
	}
	if platform == "linux/"+runtime.GOARCH {
		j.Logf("target platform: %s (same as the packing machine)", platform)
	} else {
		j.Logf("target platform: %s (cross packing, packing machine is linux/%s)", platform, runtime.GOARCH)
	}

	// 1. Resolve component tags. Environments with a version manifest are
	//    pinned by it (major version -> component -> tag); others use the
	//    tags baked into the compose file / env template, and req.Version is
	//    a free label used only for naming and the manifest.
	components := map[string]string{}
	var version string
	if p.HasVersions(req.Env) {
		manifestFile := filepath.Join(projectDir, filepath.FromSlash(p.VersionFileFor(req.Env)))
		vm, err := loadVersionManifest(manifestFile)
		if err != nil {
			return fmt.Errorf("load version manifest: %w", err)
		}
		version = req.Version
		if version == "" {
			version = vm.Latest
		}
		ver, ok := vm.Versions[version]
		if !ok {
			return fmt.Errorf("major version %s is not in the version manifest %s", version, manifestFile)
		}
		components = ver
		j.Logf("major version: %s, components: %d", version, len(ver))
	} else {
		version = req.Version
	}

	profiles := req.Profiles
	if len(profiles) == 0 {
		profiles = p.DefaultProfiles
	}
	// Copy kept for the file name: step 5.7 clears profiles after trimming
	// (the bundle compose becomes self-contained, the installer needs no
	// --profile), but the output file name must keep the profile segment —
	// otherwise bundles of different variants become indistinguishable.
	nameProfiles := profiles

	// 2. Temp working directory
	bundleDir, err := os.MkdirTemp("", "compose-installer-bundle-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(bundleDir)
	for _, d := range []string{"images", "repo", "docker-offline"} {
		if err := os.MkdirAll(filepath.Join(bundleDir, d), 0o755); err != nil {
			return err
		}
	}

	// 3. Resolve the full image list via docker compose
	images, err := resolveComposeImages(ctx, j, fullWorkdir, composeFile, components, profiles)
	if err != nil {
		return err
	}
	j.Logf("images: %d", len(images))

	// 4. Pull + docker save (skipped for online bundles: the target machine
	//    pulls the images itself at install time)
	if req.Online {
		j.Logf("online bundle: skipping image pull and save (%d images will be pulled on the target machine)", len(images))
		if err := os.RemoveAll(filepath.Join(bundleDir, "images")); err != nil {
			return err
		}
	} else if err := pullAndSave(ctx, j, images, filepath.Join(bundleDir, "images", name+".tar"), platform); err != nil {
		return err
	}

	// 5. Copy the minimal file set actually needed at install time
	//    (whitelist): only the selected environment's compose file, the env
	//    template, the declared extra files, the version manifest, and the
	//    local (./xxx) directories referenced by the compose file. Everything
	//    else (CI configs, READMEs, docs, editor droppings) stays out.
	if err := copyRepoSubset(j, projectDir, filepath.Join(bundleDir, "repo"), p, req.Env, composeFile); err != nil {
		return fmt.Errorf("copy project files: %w", err)
	}

	// 5.5 Environment convergence: inside the bundle the compose file is
	//     always named docker-compose.yml (environment suffixes dropped) —
	//     no other environment's orchestration leaks, and nobody on the
	//     target machine has to tell file names apart.
	if err := sanitizeComposeFiles(filepath.Join(bundleDir, "repo", workdir), composeFile); err != nil {
		return fmt.Errorf("normalize compose files: %w", err)
	}
	j.Logf("compose normalized to docker-compose.yml (env: %s)", req.Env)

	// 5.7 Trim the compose to the selected profiles: service blocks not
	//     belonging to a selected profile are dropped and the profiles
	//     markers of kept services are stripped — the bundle compose is
	//     self-contained, the installer passes no --profile and other
	//     variants' orchestration is not shipped. Services without a
	//     profiles field (unconditionally enabled) are kept, matching
	//     compose semantics.
	if len(profiles) > 0 {
		composeCopy := filepath.Join(bundleDir, "repo", workdir, "docker-compose.yml")
		kept, serr := stripComposeProfiles(composeCopy, profiles)
		if serr != nil {
			return fmt.Errorf("trim compose to profiles: %w", serr)
		}
		j.Logf("compose trimmed to profiles %v: %d services kept (installer needs no --profile)", profiles, kept)
		profiles = nil
	}

	// 6. Extract the docker/compose offline install materials from the
	//    installer image. Online bundles carry none: the installer installs
	//    docker over the network (get.docker.com) when the target lacks it.
	if req.Online {
		j.Logf("online bundle: no docker offline materials (docker is installed over the network when missing)")
		if err := os.RemoveAll(filepath.Join(bundleDir, "docker-offline")); err != nil {
			return err
		}
	} else if req.InstallerImage != "" {
		if err := extractInstaller(ctx, j, req.InstallerImage, req.InstallerPath, filepath.Join(bundleDir, "docker-offline"), platform); err != nil {
			return err
		}
	} else {
		j.Logf("no installer image given: bundle carries no docker offline materials (the installer must skip docker setup)")
		if err := os.RemoveAll(filepath.Join(bundleDir, "docker-offline")); err != nil {
			return err
		}
	}

	// 7. Write manifest.json — the only contract with the installer
	//    (ComposeFile is always the normalized docker-compose.yml).
	m := BundleManifest{
		SchemaVersion:  BundleSchemaVersion,
		Created:        time.Now(),
		Product:        name,
		Env:            req.Env,
		Version:        version,
		ComposeFile:    "docker-compose.yml",
		Workdir:        workdir,
		Profiles:       profiles,
		Images:         images,
		Components:     components,
		Online:         req.Online,
		Platform:       platform,
		InstallDirName: p.InstallDirName,
		RequiredEnv:    p.RequiredEnv,
		EnvExports:     p.EnvExports,
		EnvRewrites:    p.EnvRewrites,
	}
	mraw, _ := json.MarshalIndent(&m, "", "  ")
	if err := os.WriteFile(filepath.Join(bundleDir, "manifest.json"), mraw, 0o644); err != nil {
		return err
	}

	// 8. Pack everything into a single tar.gz (online bundles are KB~MB,
	//    this is instant)
	output := req.Output
	if output == "" {
		osMkdirAll(filepath.Join(projectDir, "dist"))
		outName := name + "-" + req.Env
		if version != "" {
			outName += "-" + version
		}
		// Selected profiles go into the file name (multiple joined with
		// "+"), e.g. robot-test-1.0.2-mj+cam-…
		if len(nameProfiles) > 0 {
			outName += "-" + strings.Join(nameProfiles, "+")
		}
		if req.Online {
			outName += "-online"
		}
		// Architecture suffix (amd64/arm64): obvious at a glance, no more
		// installing a bundle of the wrong architecture.
		outName += "-" + strings.TrimPrefix(platform, "linux/")
		output = filepath.Join(projectDir, "dist", outName+"-"+time.Now().Format("20060102-150405")+".tar.gz")
	}
	if err := tarGzDir(bundleDir, output); err != nil {
		return fmt.Errorf("create bundle archive: %w", err)
	}
	st, _ := os.Stat(output)
	j.Logf("bundle complete: %s (%.1f MB)", output, float64(st.Size())/1024/1024)
	return nil
}

// resolveComposeImages uses docker compose config --images to get every
// image reference of the compose file (honoring env-file interpolation and
// profiles).
func resolveComposeImages(ctx context.Context, j packLogger, workdir, composeFile string, components map[string]string, profiles []string) ([]string, error) {
	envFile := filepath.Join(workdir, ".env")
	if _, err := os.Stat(envFile); os.IsNotExist(err) {
		envFile = filepath.Join(workdir, ".env.example")
		if _, err := os.Stat(envFile); os.IsNotExist(err) {
			// No env file at all (a valid zero-config shape): compose runs
			// without --env-file and interpolates from the exported vars only.
			envFile = ""
		}
	}
	if envFile != "" {
		j.Logf("env-file: %s", envFile)
	} else {
		j.Logf("env-file: none (interpolating from exported variables only)")
	}

	// Component tags from the version manifest are injected as
	// <COMPONENT>_TAG=tag so ${...} references in the compose resolve to the
	// pinned versions; profiles select the service set compose renders.
	exportEnv := append([]string{}, tagEnvVars(components)...)
	if len(profiles) > 0 {
		exportEnv = append(exportEnv, "COMPOSE_PROFILES="+strings.Join(profiles, ","))
	}

	args := []string{"compose", "-f", composeFile}
	if envFile != "" {
		args = append(args, "--env-file", filepath.Base(envFile))
	}
	args = append(args, "config", "--images")

	// Strict interpolations (${VAR:?err}) of variables that only matter at
	// install time (requiredEnv tokens, addresses...) would abort config on
	// the packing machine where those values do not exist yet. Their values
	// never influence the image list (image-affecting vars are the component
	// tags above), so on each "required variable X is missing a value" error
	// we inject a non-empty placeholder for X and retry. Only variables
	// compose actually demands get placeholders, and component tags keep
	// their real values.
	reRequiredVar := regexp.MustCompile(`required variable ([A-Za-z_][A-Za-z0-9_]*) is missing a value`)
	var images []string
	for attempt := 0; ; attempt++ {
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Dir = workdir
		cmd.Env = append(os.Environ(), exportEnv...)
		out, err := cmd.Output()
		if err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				return nil, err
			}
			msg := string(ee.Stderr)
			m := reRequiredVar.FindStringSubmatch(msg)
			if m == nil || attempt >= 10 {
				return nil, fmt.Errorf("docker compose config failed: %s", msg)
			}
			j.Logf("interpolation demands %s (install-time value); injecting a placeholder for image resolution", m[1])
			exportEnv = append(exportEnv, m[1]+"=pack-placeholder")
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				images = append(images, line)
			}
		}
		break
	}
	if len(images) == 0 {
		return nil, fmt.Errorf("compose resolved to no images (%s)", composeFile)
	}
	return images, nil
}

func pullAndSave(ctx context.Context, j packLogger, images []string, saveFile, platform string) error {
	for i, img := range images {
		if err := pullForPlatform(ctx, j, i+1, len(images), img, platform); err != nil {
			return err
		}
	}
	j.Logf("docker save -> %s", filepath.Base(saveFile))
	args := append([]string{"save", "-o", saveFile}, images...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Saving the batch does not name the failing image: export one by
		// one to locate the culprit (only on failure; stdout is discarded so
		// GB-scale tars never hit memory).
		j.Logf("docker save failed, probing images one by one…")
		var bad []string
		for _, img := range images {
			probe := exec.CommandContext(ctx, "docker", "save", img)
			probe.Stdout = io.Discard
			var errb strings.Builder
			probe.Stderr = &errb
			if perr := probe.Run(); perr != nil {
				bad = append(bad, fmt.Sprintf("%s (%s)", img, lastLines([]byte(errb.String()), 2)))
			}
		}
		if len(bad) > 0 {
			return fmt.Errorf("docker save failed, problem images: %s", strings.Join(bad, "; "))
		}
		return fmt.Errorf("docker save failed: %s", lastLines(out, 3))
	}
	return nil
}

// manifestList mirrors the multi-arch list output of docker manifest inspect.
type manifestList struct {
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		} `json:"platform"`
	} `json:"manifests"`
}

// platformDigestFromList finds the manifest digest of the target
// architecture (e.g. arm64) in the manifest-list JSON. isList=false means
// the output is not a multi-arch list (single-manifest image, no need to
// pull by digest); a list without the target architecture is an error (the
// registry never pushed that platform).
func platformDigestFromList(raw []byte, arch string) (digest string, isList bool, err error) {
	var ml manifestList
	if err = json.Unmarshal(raw, &ml); err != nil {
		return "", false, err
	}
	if len(ml.Manifests) == 0 {
		return "", false, nil
	}
	for _, m := range ml.Manifests {
		if m.Platform.OS == "linux" && m.Platform.Architecture == arch {
			return m.Digest, true, nil
		}
	}
	return "", true, fmt.Errorf("no linux/%s manifest pushed for this image", arch)
}

// imageRepoOf strips the :tag from an image reference (keeping registry
// ports); @digest references are returned unchanged.
func imageRepoOf(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref
	}
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i:], "/") {
		return ref[:i]
	}
	return ref
}

// pullForPlatform pulls an image and makes sure the local tag points at the
// **single-platform manifest** of the target platform.
// Background: under the containerd image store docker pull --platform leaves
// the tag pointing at the multi-arch index, and docker save then requires
// the manifests of ALL platforms of that index to be present locally — with
// only one platform pulled, save fails with "content digest … not found".
// Multi-arch images therefore go: manifest inspect resolves the target
// platform digest -> pull by digest (single manifest) -> tag back to the
// original reference. Single-manifest images, or when inspect is
// unavailable, fall back to a --platform pull.
func pullForPlatform(ctx context.Context, j packLogger, idx, total int, img, platform string) error {
	arch := strings.TrimPrefix(platform, "linux/")
	out, ierr := exec.CommandContext(ctx, "docker", "manifest", "inspect", img).Output()
	if ierr == nil {
		digest, isList, perr := platformDigestFromList(out, arch)
		if perr != nil {
			return fmt.Errorf("image %s: %w", img, perr)
		}
		if isList {
			ref := imageRepoOf(img) + "@" + digest
			j.Logf("[%d/%d] docker pull %s (%s)", idx, total, ref, platform)
			if out2, e := exec.CommandContext(ctx, "docker", "pull", ref).CombinedOutput(); e != nil {
				return fmt.Errorf("pull %s failed: %s", ref, lastLines(out2, 3))
			}
			if out3, e := exec.CommandContext(ctx, "docker", "tag", ref, img).CombinedOutput(); e != nil {
				return fmt.Errorf("docker tag %s <- %s failed: %s", img, ref, lastLines(out3, 3))
			}
			return nil
		}
	} else {
		j.Logf("[%d/%d] docker manifest inspect %s unavailable (%v), falling back to --platform pull", idx, total, img, ierr)
	}
	return pullWithRetry(ctx, j, idx, total, img, platform)
}

// pullWithRetry pulls a single image (--platform locks the target
// architecture); transient network errors (unexpected EOF / short read…) are
// retried with 3s/6s backoff.
func pullWithRetry(ctx context.Context, j packLogger, idx, total int, img, platform string) error {
	var lastErr error
	for attempt := 1; attempt <= pullAttempts; attempt++ {
		j.Logf("[%d/%d] docker pull --platform %s %s", idx, total, platform, img)
		cmd := exec.CommandContext(ctx, "docker", "pull", "--platform", platform, img)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("pull %s failed: %s", img, lastLines(out, 3))
		if attempt < pullAttempts {
			wait := time.Duration(attempt*3) * time.Second
			j.Logf("pull attempt %d failed (%s), retrying %d/%d in %v", attempt,
				lastLines(out, 1), attempt+1, pullAttempts, wait)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
	}
	return lastErr
}

// extractInstaller pulls the offline install materials out of the installer
// image (convention: install.sh and its dependencies under srcPath).
func extractInstaller(ctx context.Context, j packLogger, image, srcPath, dstDir, platform string) error {
	if err := extractImageDir(ctx, j, image, srcPath, dstDir, platform); err != nil {
		return err
	}
	script := filepath.Join(dstDir, "install.sh")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("install materials miss install.sh (%s)", dstDir)
	}
	return nil
}

// extractImageDir extracts a directory from an image into dstDir (docker cp
// adds an extra basename layer; when the destination was empty — this is the
// only entry — that single subdirectory is flattened away). extractInstaller
// additionally verifies install.sh exists. The image is pulled by digest for
// the target platform — under the containerd store only then does docker
// create select the target platform variant, keeping the extracted docker /
// compose binaries consistent with the target machine.
func extractImageDir(ctx context.Context, j packLogger, image, srcPath, dstDir, platform string) error {
	if srcPath == "" {
		srcPath = "/offline"
	}
	// Pull for the target platform (by digest: under the containerd image
	// store a plain docker create would otherwise select the packing
	// machine's variant), so the extracted files match the target — an arm64
	// target would otherwise be stopped by the materials' architecture
	// guard.
	if platform == "" {
		platform = "linux/" + runtime.GOARCH
	}
	j.Logf("pulling image %s (%s)", image, platform)
	if err := pullForPlatform(ctx, j, 1, 1, image, platform); err != nil {
		return fmt.Errorf("pull image: %w (the image must publish a %s variant)", err, platform)
	}
	// Container names only allow [a-zA-Z0-9][a-zA-Z0-9_.-] — no slashes from
	// module paths. The trailing "true" is a fallback command: plain
	// file-carrier images (FROM scratch, no ENTRYPOINT/CMD) make the daemon
	// reject a bare docker create ("no command specified"); the command is
	// never actually executed (docker cp only reads the filesystem).
	ctr := fmt.Sprintf("compose-installer-extract-%d", time.Now().UnixNano())
	if out, err := exec.CommandContext(ctx, "docker", "create", "--name", ctr, image, "true").CombinedOutput(); err != nil {
		return fmt.Errorf("docker create failed: %s", lastLines(out, 3))
	}
	defer exec.CommandContext(context.Background(), "docker", "rm", "-f", ctr).Run()

	src := ctr + ":" + srcPath
	j.Logf("extracting %s:%s", image, srcPath)
	if out, err := exec.CommandContext(ctx, "docker", "cp", src, dstDir).CombinedOutput(); err != nil {
		// On failure attach the image facts to tell what was actually
		// pulled: architecture (is it really the requested variant), layer
		// count (0 layers = empty scratch carrier, the copied materials are
		// lost) and created time (is this the latest CI push).
		var probe strings.Builder
		if pout, perr := exec.CommandContext(ctx, "docker", "image", "inspect", image,
			"--format", "{{.Architecture}}/{{.Os}} layers={{len .RootFS.Layers}} created={{.Created}}").CombinedOutput(); perr == nil {
			probe.WriteString(" image facts: " + strings.TrimSpace(string(pout)))
		}
		return fmt.Errorf("docker cp failed (verify %s exists in the image): %s%s", srcPath, lastLines(out, 3), probe.String())
	}
	// docker cp <ctr>:/path dst puts everything under dst/<basename>; when
	// dst was empty (only this one subdirectory) flatten one level so
	// dst/install.sh is directly usable. A non-empty dst (several extractions
	// into docker-offline/) keeps the subdirectory layer.
	base := strings.Trim(strings.ReplaceAll(srcPath, "\\", "/"), "/")
	parts := strings.Split(base, "/")
	inner := filepath.Join(dstDir, parts[len(parts)-1])
	if _, err := os.Stat(inner); err == nil {
		entries, _ := os.ReadDir(dstDir)
		if len(entries) == 1 {
			tmp := dstDir + "-flat"
			if err := os.Rename(inner, tmp); err != nil {
				return err
			}
			os.RemoveAll(dstDir)
			if err := os.Rename(tmp, dstDir); err != nil {
				return err
			}
		}
	}
	return nil
}

// repoSkipList names the directories/files skipped while copying bundled
// trees (huge, machine-generated or credential-bearing).
func repoSkipList() map[string]bool {
	return map[string]bool{
		".git": true, ".env": true, ".env.local": true,
		"data": true, "certs": true, "dist": true,
		"node_modules": true, "__pycache__": true, ".DS_Store": true,
	}
}

func copyTree(src, dst string, skip map[string]bool) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := d.Name()
		if _, bad := skip[name]; bad {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz") {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

// packRuntimeDirs are the ./ directories referenced by compose volumes that
// are generated at runtime on the target machine (dockerd creates ./data on
// up; certs may hold the packing machine's own credentials and must never
// ship) — they are excluded from the auto-collected mount directories.
var packRuntimeDirs = map[string]bool{
	"data": true, "certs": true,
}

var reLocalMount = regexp.MustCompile(`(?m)^\s*-\s+\./([A-Za-z0-9._-]+)`)

// extractLocalMountDirs collects the local directory names referenced by
// compose volumes (the first segment of ./xxx). The pack whitelist includes
// them automatically — adding a mount directory to the orchestration needs
// no packer change, eliminating the silent failure where dockerd turns a
// missing mount source into an empty directory. Only the short volumes
// syntax is recognized; the long form (type: bind) is a known blind spot.
func extractLocalMountDirs(composeText string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range reLocalMount.FindAllStringSubmatch(composeText, -1) {
		name := m[1]
		if packRuntimeDirs[name] || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// copyRepoSubset copies the minimal install-time whitelist into the bundle:
// the selected environment's compose file, the env template, the declared
// extra files, the version manifest, plus every local (./xxx) directory the
// selected compose file references — whether e.g. nginx/ ships is decided
// by parsing the selected compose, not by a hardcoded list, so the packer
// follows automatically when the orchestration gains a mount directory.
// Missing entries are skipped with a log line; runtime directories never
// ship (see packRuntimeDirs). The source project is never modified.
func copyRepoSubset(j packLogger, projectDir, dstRepo string, p *ProductConfig, env, composeFile string) error {
	workdir := filepath.FromSlash(p.Workdir)
	srcWorkdir := filepath.Join(projectDir, workdir)
	dstWorkdir := filepath.Join(dstRepo, workdir)

	copyOne := func(srcBase, dstBase, rel string) error {
		src := filepath.Join(srcBase, rel)
		st, err := os.Stat(src)
		if err != nil {
			j.Logf("not found in project, skipped: %s", rel)
			return nil
		}
		dst := filepath.Join(dstBase, rel)
		if !st.IsDir() {
			// Single file: copyTree only handles directories (a root
			// rel=="." is skipped there), copy directly here.
			in, err := os.Open(src)
			if err != nil {
				return err
			}
			defer in.Close()
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			out, err := os.Create(dst)
			if err != nil {
				return err
			}
			defer out.Close()
			if _, err := io.Copy(out, in); err != nil {
				return err
			}
		} else if err := copyTree(src, dst, repoSkipList()); err != nil {
			return err
		}
		j.Logf("packed file: %s", rel)
		return nil
	}

	// Entries relative to the compose working directory.
	items := append([]string{composeFile, p.EnvTemplate}, p.ExtraFiles...)
	// Generic collection: the local (./xxx) directories referenced by the
	// selected compose file ship automatically (entries missing from the
	// project are skipped by copyOne; runtime directories are excluded).
	if raw, err := os.ReadFile(filepath.Join(srcWorkdir, composeFile)); err == nil {
		items = append(items, extractLocalMountDirs(string(raw))...)
	}
	for _, rel := range items {
		if err := copyOne(srcWorkdir, dstWorkdir, rel); err != nil {
			return err
		}
	}

	// The version manifest is resolved against the project root so the
	// target machine reads the same component tags (single source of
	// truth).
	if vf := p.VersionFileFor(env); vf != "" {
		if err := copyOne(projectDir, dstRepo, filepath.FromSlash(vf)); err != nil {
			return err
		}
	}
	return nil
}

// sanitizeComposeFiles normalizes the compose files of the copied workdir:
// the selected environment's compose file is renamed to docker-compose.yml
// and every other docker-compose*.yml/.yaml in the same directory is
// removed, so the target machine never deals with environment suffixes and
// no other environment's orchestration leaks. The source project stays
// untouched.
func sanitizeComposeFiles(workdirCopy, selected string) error {
	dir := filepath.Dir(filepath.Join(workdirCopy, selected))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	isEnvCompose := func(name string) bool {
		return strings.HasPrefix(name, "docker-compose") &&
			(strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml"))
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, name)
		// The selected file is renamed no matter how it is named (also
		// non-canonical names such as compose.yaml).
		if name == selected {
			if name != "docker-compose.yml" {
				if err := os.Rename(p, filepath.Join(dir, "docker-compose.yml")); err != nil {
					return err
				}
			}
			continue
		}
		if !isEnvCompose(name) {
			continue
		}
		// The normalized target name docker-compose.yml is never deleted:
		// for suffixed environments it gets overwritten by the rename above;
		// deleting before renaming would lose the file on some platforms.
		if name == "docker-compose.yml" {
			continue
		}
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	return nil
}

// tarGzDir packs a directory's contents (not the outer directory itself)
// into a single tar.gz.
func tarGzDir(srcDir, output string) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	f, err := os.Create(output)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(tw, in)
		return err
	})
}

func osMkdirAll(dir string) {
	_ = os.MkdirAll(dir, 0o755)
}

// lastLines returns the last n lines of a command output, joined with " | ".
func lastLines(out []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

var (
	reComposeTopKey   = regexp.MustCompile(`^[A-Za-z][\w.-]*:`)
	reComposeService  = regexp.MustCompile(`^  ([A-Za-z0-9][\w.-]*):\s*(#.*)?$`)
	reComposeProfHead = regexp.MustCompile(`^    profiles:\s*(#.*)?$`)
	reComposeProfItem = regexp.MustCompile(`^      -\s+(\S+)`)
)

// stripComposeProfiles trims a compose file to the selected profiles:
// service blocks not belonging to a selected profile are dropped and the
// profiles markers of kept services are stripped, making the bundle compose
// self-contained — the installer passes no --profile and other variants'
// orchestration is not shipped. Services without a profiles field
// (unconditionally enabled) are kept, matching compose --profile semantics.
// The repo compose uses a tidy 2/4/6-space indentation without YAML
// anchors, so a line-level state machine handles it reliably.
func stripComposeProfiles(file string, selected []string) (int, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return 0, err
	}
	sel := map[string]bool{}
	for _, p := range selected {
		sel[p] = true
	}
	var out []string
	var block []string // buffered lines of the current service block (header + body)
	var blockProfiles []string
	inServices, inProf := false, false
	total, kept := 0, 0
	allProfiles := map[string]bool{}

	flush := func() {
		if len(block) == 0 {
			return
		}
		total++
		keep := len(blockProfiles) == 0
		for _, p := range blockProfiles {
			if sel[p] {
				keep = true
				break
			}
		}
		if keep {
			// Emit the block, skipping the profiles: header and its list
			// items.
			for i := 0; i < len(block); i++ {
				if reComposeProfHead.MatchString(block[i]) {
					i++
					for i < len(block) && reComposeProfItem.MatchString(block[i]) {
						i++
					}
					i-- // compensate the outer loop's increment
					continue
				}
				out = append(out, block[i])
			}
			kept++
		}
		block, blockProfiles, inProf = nil, nil, false
	}

	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		switch {
		case inServices && reComposeService.MatchString(line):
			flush()
			block = append(block, line)
		case reComposeTopKey.MatchString(line):
			flush()
			inServices = strings.HasPrefix(line, "services:")
			out = append(out, line)
		case len(block) > 0:
			block = append(block, line)
			if reComposeProfHead.MatchString(line) {
				inProf = true
			} else if inProf {
				if m := reComposeProfItem.FindStringSubmatch(line); m != nil {
					name := strings.Trim(m[1], `"'`)
					blockProfiles = append(blockProfiles, name)
					allProfiles[name] = true
				} else {
					inProf = false
				}
			}
		default:
			out = append(out, line)
		}
	}
	flush()

	if total == 0 {
		return 0, fmt.Errorf("no service definitions found in compose: %s", file)
	}
	// Selected profiles must exist in the compose (fail early on typos
	// instead of shipping a bundle with only the unconditional services).
	// Profile-less services are unaffected.
	var unknown []string
	for _, p := range selected {
		if !allProfiles[p] {
			unknown = append(unknown, p)
		}
	}
	if len(unknown) > 0 {
		var avail []string
		for p := range allProfiles {
			avail = append(avail, p)
		}
		sort.Strings(avail)
		return 0, fmt.Errorf("profiles not found in compose: %v (available: %v)", unknown, avail)
	}
	if kept == 0 {
		return 0, fmt.Errorf("selected profiles %v match no services in compose", selected)
	}
	// Collapse the consecutive blank lines left behind by dropped blocks
	// (keep a single blank line between services).
	var compact []string
	for _, l := range out {
		if strings.TrimSpace(l) == "" && len(compact) > 0 && strings.TrimSpace(compact[len(compact)-1]) == "" {
			continue
		}
		compact = append(compact, l)
	}
	return kept, os.WriteFile(file, []byte(strings.Join(compact, "\n")), 0o644)
}
