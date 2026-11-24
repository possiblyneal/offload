package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const exportRoot = "/run/offload-export"

// exportView is the set of dev paths visible through the synthetic NFS root.
// A bind is usable only if it still shows the caller's filesystem, not a
// different mount with the same path in the installer or caller namespace.
type exportView struct {
	home, root string
	shared     []string
	caches     []string
}

func scopedExport(root, home, configDir string) exportView {
	v := exportView{root: root, home: home}
	if root == "" {
		return v
	}
	if _, err := os.Stat(filepath.Join(root, ".offload-scoped")); err != nil {
		return v
	}
	for _, p := range []string{"/etc", "/usr", "/tmp", "/var/tmp", filepath.Join(home, "code"), filepath.Join(home, "orca/workspaces")} {
		a, e1 := os.Stat(p)
		b, e2 := os.Stat(root + p)
		if e1 == nil && e2 == nil && os.SameFile(a, b) {
			v.shared = append(v.shared, p)
		}
	}
	for _, p := range remoteLocalPaths(configDir, home) {
		if st, err := os.Stat(root + p.path); err == nil && st.IsDir() {
			v.caches = append(v.caches, p.path)
		}
	}
	return v
}

// missing returns the unexported mount that covers p, or p itself when the
// scoped view omits it. An explicit bind covers its source's enclosing mount,
// but never covers a nested mount (the export view uses nonrecursive binds).
func (v exportView) missing(mounts []string, p string) string {
	if v.root == "" {
		return within(mounts, p)
	}
	if p == "" || !filepath.IsAbs(p) {
		return ""
	}
	if v.shared == nil {
		return p
	}
	if within([]string{"/proc", "/sys", "/dev", "/run"}, p) != "" {
		return within(mounts, p)
	}
	shared := within(v.shared, p)
	if shared == "" && within([]string{"/bin", "/sbin", "/lib", "/lib64"}, p) != "" {
		real := realPath(p)
		if real != p {
			return v.missing(mounts, real)
		}
	}
	if shared == "" {
		return p
	}
	for _, m := range mounts {
		if m != shared && under(m, []string{shared}) && under(p, []string{m}) {
			return m
		}
	}
	return ""
}

func (v exportView) environment(env, mounts []string) []string {
	if v.root == "" {
		return env
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		switch {
		case name == "_" || strings.HasPrefix(name, "ORCA_") || strings.HasPrefix(name, "CLAUDE_CODE_") ||
			strings.HasPrefix(name, "ANTHROPIC_") || name == "CLAUDERIPPLE_CONFIG" || name == "PI_CODING_AGENT_DIR":
			// Host agent state and sockets are not part of a build environment.
			continue
		case name == "NODE_EXTRA_CA_CERTS" && value == filepath.Join(v.home, ".clauderipple/ca.pem") && slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, "CLAUDERIPPLE_CONFIG=") }):
			continue // ClaudeRipple’s host-local CA, not a project’s CA.
		case name == "PATH":
			dirs := strings.Split(value, ":")
			dirs = slices.DeleteFunc(dirs, func(p string) bool {
				return filepath.IsAbs(p) && (v.missing(mounts, p) != "" || v.missing(mounts, realPath(p)) != "")
			})
			entry = "PATH=" + strings.Join(dirs, ":")
		}
		out = append(out, entry)
	}
	return out
}
