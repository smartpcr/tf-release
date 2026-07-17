// Package layout resolves per-OS filesystem paths and environment for releases (DESIGN §7.1).
package layout

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// Paths is the on-target layout (DESIGN §9.1), pre-joined per OS.
type Paths struct {
	OS         spec.OSKind
	Root       string // <install_root>/<app>
	Releases   string
	Release    string // <releases>/<version>
	Current    string
	Shared     string
	SharedLogs string
	Staging    string
	Manifest   string
	Lock       string
	StagePkg   string // <staging>/pkg.zip
}

func sep(os spec.OSKind) string {
	if os == spec.OSLinux {
		return "/"
	}
	return `\`
}

func Join(os spec.OSKind, parts ...string) string {
	s := sep(os)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.Trim(p, `\/`))
	}
	joined := strings.Join(out, s)
	if os == spec.OSLinux {
		return "/" + joined
	}
	return joined
}

// NewPaths computes layout for app+version. On linux install_root is absolute
// already; Join re-roots. On windows we keep drive-letter roots verbatim.
func NewPaths(osKind spec.OSKind, installRoot, app, version string) Paths {
	s := sep(osKind)
	root := strings.TrimRight(installRoot, `\/`) + s + app
	p := Paths{
		OS:       osKind,
		Root:     root,
		Releases: root + s + "releases",
		Current:  root + s + "current",
		Shared:   root + s + "shared",
		Staging:  root + s + "staging",
	}
	p.SharedLogs = p.Shared + s + "logs"
	p.Manifest = root + s + "manifest.json"
	p.Lock = root + s + ".lock"
	p.StagePkg = p.Staging + s + "pkg.zip"
	if version != "" {
		p.Release = p.Releases + s + version
	}
	return p
}

// Dirs returns the release-tree directories that must exist before any op,
// in creation order (DESIGN §9.1). `current` is a junction/symlink and is NOT
// created here; `manifest.json`/`.lock` are files, not dirs.
func (p Paths) Dirs() []string {
	return []string{p.Releases, p.Shared, p.SharedLogs, p.Staging}
}

// psQuote single-quotes a string for PowerShell (doubling embedded quotes).
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// DirScript renders the idempotent directory-creation script for the target OS
// (DESIGN §9.1): `New-Item -ItemType Directory -Force` on windows, `mkdir -p`
// on linux. Output is deterministic (fixed dir order) so it can be golden-tested.
func DirScript(p Paths) string {
	dirs := p.Dirs()
	if p.OS == spec.OSWindows {
		items := make([]string, len(dirs))
		for i, d := range dirs {
			items[i] = psQuote(d)
		}
		return fmt.Sprintf("foreach($d in @(%s)){ New-Item -ItemType Directory -Force -Path $d | Out-Null }\nexit 0\n",
			strings.Join(items, ","))
	}
	items := make([]string, len(dirs))
	for i, d := range dirs {
		items[i] = shQuote(d)
	}
	return "mkdir -p " + strings.Join(items, " ") + "\n"
}

// shQuote single-quotes a string for POSIX sh (closing/escaping embedded quotes).
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// BuiltinEnv is merged UNDER spec environment (spec wins) — DESIGN §9.1.
// nodePort > 0 injects PORT (node_web_app only, per DESIGN §9.1/§9.4); callers
// pass 0 for every other pattern so PORT is absent.
func BuiltinEnv(app, version string, p Paths, nodePort int) map[string]string {
	env := map[string]string{
		"LD_APP":         app,
		"LD_VERSION":     version,
		"LD_RELEASE_DIR": p.Current, // stable path across releases
		"LD_SHARED_DIR":  p.Shared,
	}
	if nodePort > 0 {
		env["PORT"] = strconv.Itoa(nodePort)
	}
	return env
}

func MergeEnv(builtin, user map[string]string) map[string]string {
	out := make(map[string]string, len(builtin)+len(user))
	for k, v := range builtin {
		out[k] = v
	}
	for k, v := range user {
		out[k] = v
	}
	return out
}
