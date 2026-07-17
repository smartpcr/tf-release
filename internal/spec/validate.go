package spec

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// ValidationError carries the ERR_SPEC_INVALID contract (DESIGN §12). Its
// Error() text is `[ERR_SPEC_INVALID] <msg>`, where <msg> always names the
// offending JSON path (e.g. `artifact.version: required ...`). This makes the
// stable pipeline/test contract observable in-process without a provider layer.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return "[ERR_SPEC_INVALID] " + e.Msg }

// Code exposes the stable error code (DESIGN §12) for callers matching on it.
func (e *ValidationError) Code() string { return "ERR_SPEC_INVALID" }

func vErr(format string, a ...interface{}) error {
	return &ValidationError{Msg: fmt.Sprintf(format, a...)}
}

// requireEnv enforces env-var NAME hygiene (DESIGN §11): a referenced-but-unset
// env var yields ERR_SPEC_INVALID naming the exact JSON path that referenced it,
// before any dial. Empty name means "not referenced" and is a no-op.
func requireEnv(name, jsonPath string) error {
	if name == "" {
		return nil
	}
	if _, ok := os.LookupEnv(name); !ok {
		return vErr("env var %s referenced by %s is not set", name, jsonPath)
	}
	return nil
}

var (
	reName     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reVersion  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	reChecksum = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	reSvcName  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)
)

// patternOS is the §14 support matrix.
var patternOS = map[PatternType]map[OSKind]bool{
	PatternConsoleApp:     {OSWindows: true, OSLinux: true},
	PatternWindowsService: {OSWindows: true},
	PatternNodeWebApp:     {OSWindows: true},
	PatternDotnetAPI:      {OSWindows: true},
	PatternClusterGeneric: {OSWindows: true},
	PatternDockerCont:     {OSWindows: true, OSLinux: true},
}

func validateTarget(t *Target, isCluster bool) error {
	switch t.Transport {
	case TransportSSH, TransportWinRM, TransportLocal:
	case "":
		return vErr("target.transport: required (ssh|winrm|local)")
	default:
		return vErr("target.transport: %q not one of ssh|winrm|local", t.Transport)
	}
	switch t.OS {
	case OSWindows, OSLinux:
	default:
		return vErr("target.os: required (windows|linux)")
	}
	n := len(t.Hosts)
	if isCluster {
		if n < 2 || n > 16 {
			return vErr("target.hosts: cluster_generic_service needs 2..16 hosts, got %d", n)
		}
	} else if n != 1 {
		return vErr("target.hosts: need exactly 1 host, got %d", n)
	}
	if t.Transport == TransportLocal && (n != 1 || t.Hosts[0] != "localhost") {
		return vErr(`target.hosts: transport local requires exactly ["localhost"]`)
	}
	if t.Port < 0 || t.Port > 65535 {
		return vErr("target.port: out of range")
	}
	if t.Transport != TransportLocal {
		if t.Credentials.Username == "" {
			return vErr("target.credentials.username: required for %s", t.Transport)
		}
		if t.Transport == TransportWinRM && t.Credentials.PasswordEnv == "" {
			return vErr("target.credentials.password_env: required for winrm")
		}
		if t.Transport == TransportSSH &&
			t.Credentials.PasswordEnv == "" && t.Credentials.PrivateKeyEnv == "" {
			return vErr("target.credentials: one of password_env|private_key_env required for ssh")
		}
	}
	// env var NAMES must resolve on the runner (VAL-08 fires here, pre-dial).
	// Each error names the precise JSON path that referenced the missing var.
	if err := requireEnv(t.Credentials.PasswordEnv, "target.credentials.password_env"); err != nil {
		return err
	}
	if err := requireEnv(t.Credentials.PrivateKeyEnv, "target.credentials.private_key_env"); err != nil {
		return err
	}
	return nil
}

func validateArtifact(a *Artifact, p PatternType) error {
	switch a.Type {
	case ArtifactZip, ArtifactNupkg, ArtifactDocker:
	default:
		return vErr("artifact.type: %q not one of zip|nupkg|docker_image", a.Type)
	}
	if !reVersion.MatchString(a.Version) {
		return vErr("artifact.version: required, must match %s", reVersion.String())
	}
	// artifact × pattern (DESIGN §14)
	if a.Type == ArtifactDocker && p != PatternDockerCont {
		return vErr("artifact.type docker_image only valid with pattern docker_container")
	}
	if a.Type != ArtifactDocker && p == PatternDockerCont {
		return vErr("pattern docker_container requires artifact.type docker_image")
	}
	if a.Type != ArtifactDocker {
		if !reChecksum.MatchString(a.Checksum) {
			return vErr(`artifact.checksum: required, format "sha256:<64 hex>"`)
		}
	} else if a.Checksum != "" {
		return vErr("artifact.checksum: forbidden for docker_image (use source.digest)")
	}
	switch a.FetchMode {
	case "", "target_pull", "runner_push":
	default:
		return vErr("artifact.fetch_mode: %q not one of target_pull|runner_push", a.FetchMode)
	}
	s := &a.Source
	switch s.Type {
	case "http":
		if s.URL == "" {
			return vErr("artifact.source.url: required for http")
		}
	case "file":
		if s.Path == "" {
			return vErr("artifact.source.path: required for file")
		}
		if a.FetchMode == "target_pull" {
			return vErr("artifact.fetch_mode: target_pull invalid with file source")
		}
	case "nuget_feed":
		if s.FeedURL == "" || s.PackageID == "" {
			return vErr("artifact.source: feed_url and package_id required for nuget_feed")
		}
	case "docker_registry":
		if a.Type != ArtifactDocker {
			return vErr("artifact.source.type docker_registry requires artifact.type docker_image")
		}
		if s.Image == "" {
			return vErr("artifact.source.image: required for docker_registry")
		}
	default:
		return vErr("artifact.source.type: %q not one of http|file|nuget_feed|docker_registry", s.Type)
	}
	if err := requireEnv(s.Auth.TokenEnv, "artifact.source.auth.token_env"); err != nil {
		return err
	}
	if err := requireEnv(s.Auth.PasswordEnv, "artifact.source.auth.password_env"); err != nil {
		return err
	}
	return nil
}

func validatePattern(p *Pattern, os OSKind) error {
	sup, ok := patternOS[p.Type]
	if !ok {
		return vErr("pattern.type: %q unknown", p.Type)
	}
	if !sup[os] {
		return vErr("pattern.type %s not supported on os %s (see support matrix)", p.Type, os)
	}
	needSvc := func() error {
		if !reSvcName.MatchString(p.ServiceName) {
			return vErr("pattern.service_name: required, must match %s", reSvcName.String())
		}
		switch p.StartType {
		case "", "auto", "manual", "delayed":
		default:
			return vErr("pattern.start_type: %q not one of auto|manual|delayed", p.StartType)
		}
		if p.Account.Username != "" && p.Account.Username != "LocalSystem" &&
			p.Account.Username != `NT AUTHORITY\NetworkService` &&
			p.Account.Username != `NT AUTHORITY\LocalService` &&
			p.Account.PasswordEnv == "" {
			return vErr("pattern.account.password_env: required for account %q", p.Account.Username)
		}
		return nil
	}
	switch p.Type {
	case PatternConsoleApp:
		if p.Exe == "" {
			return vErr("pattern.exe: required for console_app")
		}
	case PatternWindowsService:
		if err := needSvc(); err != nil {
			return err
		}
		if p.Exe == "" {
			return vErr("pattern.exe: required for windows_service")
		}
		switch p.Wrapper {
		case "", "none":
		case "winsw":
			if p.WinswExe == "" {
				return vErr("pattern.winsw_exe: required when wrapper=winsw (D2: wrapper ships inside artifact)")
			}
		default:
			return vErr("pattern.wrapper: %q not one of none|winsw", p.Wrapper)
		}
	case PatternNodeWebApp:
		if err := needSvc(); err != nil {
			return err
		}
		if p.Entry == "" {
			return vErr("pattern.entry: required for node_web_app")
		}
		if p.Port <= 0 || p.Port > 65535 {
			return vErr("pattern.port: required (1..65535) for node_web_app")
		}
		if p.WinswExe == "" {
			return vErr("pattern.winsw_exe: required for node_web_app (D2)")
		}
	case PatternDotnetAPI:
		if err := needSvc(); err != nil {
			return err
		}
		switch p.Launcher {
		case "", "exe":
			if p.Exe == "" {
				return vErr("pattern.exe: required when launcher=exe")
			}
		case "dotnet_dll":
			if p.DLL == "" {
				return vErr("pattern.dll: required when launcher=dotnet_dll")
			}
		default:
			return vErr("pattern.launcher: %q not one of exe|dotnet_dll", p.Launcher)
		}
		switch p.Hosting {
		case "", "windows_service_native":
		case "winsw":
			if p.WinswExe == "" {
				return vErr("pattern.winsw_exe: required when hosting=winsw")
			}
		default:
			return vErr("pattern.hosting: %q not one of windows_service_native|winsw", p.Hosting)
		}
	case PatternClusterGeneric:
		if err := needSvc(); err != nil {
			return err
		}
		if p.RoleName == "" {
			return vErr("pattern.role_name: required for cluster_generic_service")
		}
		if p.Exe == "" {
			return vErr("pattern.exe: required for cluster_generic_service")
		}
		if p.Wrapper != "" && p.Wrapper != "none" {
			return vErr("pattern.wrapper: not supported for cluster_generic_service (D5b)")
		}
	case PatternDockerCont:
		if p.ContainerName == "" {
			return vErr("pattern.container_name: required for docker_container")
		}
	}
	return nil
}

func validateHealth(h *HealthCheck) error {
	switch h.EffectiveType() {
	case "none":
	case "http":
		if h.HTTP.URL == "" {
			return vErr("health_check.http.url: required")
		}
	case "tcp":
		if h.TCP.Port <= 0 || h.TCP.Port > 65535 {
			return vErr("health_check.tcp.port: required (1..65535)")
		}
	case "exec":
		if h.Exec.Command == "" {
			return vErr("health_check.exec.command: required")
		}
	default:
		return vErr("health_check.type: %q not one of http|tcp|exec|none", h.Type)
	}
	return nil
}

// ValidateDeployment enforces DESIGN §6 exhaustively.
func ValidateDeployment(d *Deployment) error {
	if d.APIVersion != "labdeploy/v1" {
		return vErr(`apiVersion: expected "labdeploy/v1", got %q`, d.APIVersion)
	}
	if d.Kind != "Deployment" {
		return vErr(`kind: expected "Deployment", got %q`, d.Kind)
	}
	if !reName.MatchString(d.Metadata.Name) {
		return vErr("metadata.name: required, must match %s", reName.String())
	}
	if err := validateTarget(&d.Target, d.Pattern.Type == PatternClusterGeneric); err != nil {
		return err
	}
	if err := validatePattern(&d.Pattern, d.Target.OS); err != nil {
		return err
	}
	if err := validateArtifact(&d.Artifact, d.Pattern.Type); err != nil {
		return err
	}
	if err := validateHealth(&d.HealthCheck); err != nil {
		return err
	}
	if d.Strategy.KeepReleases != nil && *d.Strategy.KeepReleases < 1 {
		return vErr("strategy.keep_releases: must be >= 1")
	}
	if d.Pattern.PreferredOwner != "" {
		found := false
		for _, h := range d.Target.Hosts {
			if equalsFold(h, d.Pattern.PreferredOwner) {
				found = true
			}
		}
		if !found {
			return vErr("pattern.preferred_owner: %q not in target.hosts", d.Pattern.PreferredOwner)
		}
	}
	for i, f := range d.Files {
		if err := validateRelPath(f.Path, i); err != nil {
			return err
		}
	}
	if err := requireEnv(d.Pattern.Account.PasswordEnv, "pattern.account.password_env"); err != nil {
		return err
	}
	return nil
}

// ValidateTestRun enforces DESIGN §7.
func ValidateTestRun(t *TestRun) error {
	if t.APIVersion != "labdeploy/v1" {
		return vErr(`apiVersion: expected "labdeploy/v1", got %q`, t.APIVersion)
	}
	if t.Kind != "TestRun" {
		return vErr(`kind: expected "TestRun", got %q`, t.Kind)
	}
	if !reName.MatchString(t.Metadata.Name) {
		return vErr("metadata.name: required, must match %s", reName.String())
	}
	if err := validateTarget(&t.Target, false); err != nil {
		return err
	}
	if t.Artifact.Type == ArtifactDocker {
		return vErr("artifact.type: docker_image not allowed in TestRun (v1)")
	}
	// TestRun artifact reuses deployment rules minus pattern coupling.
	if err := validateArtifact(&t.Artifact, PatternConsoleApp); err != nil {
		return err
	}
	switch t.Runner.Type {
	case "exec":
		if t.Runner.Command == "" {
			return vErr("runner.command: required for runner.type exec")
		}
	case "vstest", "dotnet_test", "npm":
	default:
		return vErr("runner.type: %q not one of exec|vstest|dotnet_test|npm", t.Runner.Type)
	}
	switch t.Results.Format {
	case "", "none":
	case "trx", "junit":
		if len(t.Results.Paths) == 0 {
			return vErr("results.paths: required when results.format=%s", t.Results.Format)
		}
	default:
		return vErr("results.format: %q not one of trx|junit|none", t.Results.Format)
	}
	if t.PassCriteria.MinPassRate != nil {
		if r := *t.PassCriteria.MinPassRate; r < 0 || r > 1 {
			return vErr("pass_criteria.min_pass_rate: must be within [0,1]")
		}
	}
	return nil
}

// validateRelPath enforces DESIGN §6.5: files[].path is "relative to release
// dir". It rejects empty paths, absolute paths (both POSIX "/x" and Windows
// "C:\x" / "\\host\share"), and any ".." traversal component so a spec can never
// write outside the release dir. Detection is OS-independent because a spec may
// be authored for a Windows target while validated on a Linux runner.
func validateRelPath(p string, i int) error {
	if p == "" {
		return vErr("files[%d].path: required", i)
	}
	if isAbsPath(p) {
		return vErr("files[%d].path: must be relative to the release dir, got absolute path %q", i, p)
	}
	// Normalise separators, then inspect components for "..".
	norm := strings.ReplaceAll(p, `\`, "/")
	for _, seg := range strings.Split(norm, "/") {
		if seg == ".." {
			return vErr("files[%d].path: must not contain \"..\" traversal, got %q", i, p)
		}
	}
	return nil
}

// isAbsPath reports whether p is absolute or volume-qualified under either
// POSIX or Windows rules, independent of the host OS running validation. Any
// Windows drive qualifier ("C:\x", "C:/x", "C:x", "C:..\x") is treated as
// non-relative: a drive-relative path still escapes the release dir's volume,
// so it must be rejected.
func isAbsPath(p string) bool {
	if p == "" {
		return false
	}
	// POSIX absolute or Windows root-relative / UNC ("/x", "\x", "\\host\share").
	if p[0] == '/' || p[0] == '\\' {
		return true
	}
	// Windows drive/volume qualifier: "C:" followed by anything ("C:\x", "C:/x",
	// "C:x", "C:..\x"). All are volume-anchored, not release-dir-relative.
	if len(p) >= 2 && isDriveLetter(p[0]) && p[1] == ':' {
		return true
	}
	return false
}

func isDriveLetter(c byte) bool {
	return ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z')
}

func equalsFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 32
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}
