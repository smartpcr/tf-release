package layout

import (
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

// BuiltinEnv is merged UNDER spec environment (spec wins) — DESIGN §9.1.
func BuiltinEnv(app, version string, p Paths) map[string]string {
	return map[string]string{
		"LD_APP":         app,
		"LD_VERSION":     version,
		"LD_RELEASE_DIR": p.Current, // stable path across releases
		"LD_SHARED_DIR":  p.Shared,
	}
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
