package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// VersionManifest mirrors a versions JSON file (the single source of truth
// mapping major versions to per-component image tags). Which file belongs to
// which environment is declared per product in compose-installer.json:
//
//	{"latest": "1.0", "versions": {"1.0": {"backend": "0.0.23"}}}
type VersionManifest struct {
	Latest   string                       `json:"latest"`
	Versions map[string]map[string]string `json:"versions"` // major version -> component -> tag
}

func loadVersionManifest(file string) (*VersionManifest, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var m VersionManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	return &m, nil
}

// discoverProfiles scans the compose file selected for env (workdir from the
// product config; zero-config products auto-detect the compose file) for all
// profile names — the items of profiles: blocks — so the UI can offer them
// for selection. Returns nil when the compose file cannot be found.
func discoverProfiles(projectDir string, p *ProductConfig, env string) []string {
	composeFile, err := p.ComposeFileFor(env)
	if err != nil {
		return nil
	}
	if composeFile == "" {
		composeFile = DetectComposeFile(filepath.Join(projectDir, filepath.FromSlash(p.Workdir)))
		if composeFile == "" {
			return nil
		}
	}
	file := filepath.Join(projectDir, filepath.FromSlash(p.Workdir), composeFile)
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	inProfiles := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimRight(line, " \t\r")
		if regexpProfilesHeader.MatchString(trimmed) {
			inProfiles = true
			continue
		}
		if !inProfiles {
			continue
		}
		m := regexpProfilesItem.FindStringSubmatch(trimmed)
		if m == nil {
			inProfiles = false // the list ended (non-list line)
			continue
		}
		name := strings.Trim(m[1], `"'`)
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

var (
	regexpProfilesHeader = regexp.MustCompile(`^\s{2,8}profiles:\s*$`)
	regexpProfilesItem   = regexp.MustCompile(`^\s*-\s*(\S+)`)
)

// tagEnvVars turns a component->tag map into a <COMPONENT>_TAG=tag env var
// list.
func tagEnvVars(components map[string]string) []string {
	var out []string
	for comp, tag := range components {
		v := strings.ToUpper(strings.ReplaceAll(comp, "-", "_")) + "_TAG"
		out = append(out, v+"="+tag)
	}
	sort.Strings(out)
	return out
}
