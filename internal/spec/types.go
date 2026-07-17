// Package spec defines the Deployment / TestRun documents (DESIGN §6, §7).
package spec

type OSKind string

const (
	OSWindows OSKind = "windows"
	OSLinux   OSKind = "linux"
)

type TransportKind string

const (
	TransportSSH   TransportKind = "ssh"
	TransportWinRM TransportKind = "winrm"
	TransportLocal TransportKind = "local"
)

type PatternType string

const (
	PatternConsoleApp     PatternType = "console_app"
	PatternWindowsService PatternType = "windows_service"
	PatternNodeWebApp     PatternType = "node_web_app"
	PatternDotnetAPI      PatternType = "dotnet_api"
	PatternClusterGeneric PatternType = "cluster_generic_service"
	PatternDockerCont     PatternType = "docker_container"
)

type ArtifactType string

const (
	ArtifactZip    ArtifactType = "zip"
	ArtifactNupkg  ArtifactType = "nupkg"
	ArtifactDocker ArtifactType = "docker_image"
)

// ---------------------------------------------------------------------------

type Metadata struct {
	Name   string            `json:"name" yaml:"name"`
	Labels map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
}

type Credentials struct {
	Username      string `json:"username,omitempty" yaml:"username,omitempty"`
	PasswordEnv   string `json:"password_env,omitempty" yaml:"password_env,omitempty"`
	PrivateKeyEnv string `json:"private_key_env,omitempty" yaml:"private_key_env,omitempty"`
}

type WinRMOpts struct {
	UseHTTPS           *bool `json:"use_https,omitempty" yaml:"use_https,omitempty"`
	InsecureSkipVerify bool  `json:"insecure_skip_verify,omitempty" yaml:"insecure_skip_verify,omitempty"`
	TimeoutSeconds     int   `json:"timeout_seconds,omitempty" yaml:"timeout_seconds,omitempty"`
}

type SSHOpts struct {
	HostKey        string `json:"host_key,omitempty" yaml:"host_key,omitempty"` // ""=accept any (lab)
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" yaml:"timeout_seconds,omitempty"`
}

type Target struct {
	Transport      TransportKind `json:"transport,omitempty" yaml:"transport,omitempty"`
	Hosts          []string      `json:"hosts,omitempty" yaml:"hosts,omitempty"`
	OS             OSKind        `json:"os,omitempty" yaml:"os,omitempty"`
	Port           int           `json:"port,omitempty" yaml:"port,omitempty"`
	Credentials    Credentials   `json:"credentials,omitempty" yaml:"credentials,omitempty"`
	WinRM          WinRMOpts     `json:"winrm,omitempty" yaml:"winrm,omitempty"`
	SSH            SSHOpts       `json:"ssh,omitempty" yaml:"ssh,omitempty"`
	ConnectRetries *int          `json:"connect_retries,omitempty" yaml:"connect_retries,omitempty"`
}

type SourceAuth struct {
	Header      string `json:"header,omitempty" yaml:"header,omitempty"`
	TokenEnv    string `json:"token_env,omitempty" yaml:"token_env,omitempty"`
	Scheme      string `json:"scheme,omitempty" yaml:"scheme,omitempty"` // nuget: bearer|basic
	Username    string `json:"username,omitempty" yaml:"username,omitempty"`
	PasswordEnv string `json:"password_env,omitempty" yaml:"password_env,omitempty"`
}

type Source struct {
	Type string `json:"type" yaml:"type"` // http|file|nuget_feed|docker_registry
	// http
	URL string `json:"url,omitempty" yaml:"url,omitempty"`
	// file (path on runner)
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
	// nuget_feed
	FeedURL   string `json:"feed_url,omitempty" yaml:"feed_url,omitempty"`
	PackageID string `json:"package_id,omitempty" yaml:"package_id,omitempty"`
	// docker_registry
	Image  string `json:"image,omitempty" yaml:"image,omitempty"`
	Tag    string `json:"tag,omitempty" yaml:"tag,omitempty"`
	Digest string `json:"digest,omitempty" yaml:"digest,omitempty"`

	Auth SourceAuth `json:"auth,omitempty" yaml:"auth,omitempty"`
}

type Artifact struct {
	Type      ArtifactType `json:"type" yaml:"type"`
	Version   string       `json:"version" yaml:"version"`
	Checksum  string       `json:"checksum,omitempty" yaml:"checksum,omitempty"` // sha256:<hex>
	FetchMode string       `json:"fetch_mode,omitempty" yaml:"fetch_mode,omitempty"`
	Source    Source       `json:"source" yaml:"source"`
}

type ServiceAccount struct {
	Username    string `json:"username,omitempty" yaml:"username,omitempty"`
	PasswordEnv string `json:"password_env,omitempty" yaml:"password_env,omitempty"`
}

type Recovery struct {
	RestartOnFailure *bool `json:"restart_on_failure,omitempty" yaml:"restart_on_failure,omitempty"`
}

// Pattern is a discriminated union on Type; validate.go enforces which fields
// are legal per type (DESIGN §6.4).
type Pattern struct {
	Type        PatternType `json:"type" yaml:"type"`
	InstallRoot string      `json:"install_root,omitempty" yaml:"install_root,omitempty"`
	PostInstall string      `json:"post_install,omitempty" yaml:"post_install,omitempty"`

	// console_app
	Exe           string   `json:"exe,omitempty" yaml:"exe,omitempty"`
	Args          []string `json:"args,omitempty" yaml:"args,omitempty"`
	VerifyCommand string   `json:"verify_command,omitempty" yaml:"verify_command,omitempty"`

	// windows_service / dotnet_api / cluster
	ServiceName        string         `json:"service_name,omitempty" yaml:"service_name,omitempty"`
	DisplayName        string         `json:"display_name,omitempty" yaml:"display_name,omitempty"`
	Description        string         `json:"description,omitempty" yaml:"description,omitempty"`
	Wrapper            string         `json:"wrapper,omitempty" yaml:"wrapper,omitempty"` // none|winsw
	WinswExe           string         `json:"winsw_exe,omitempty" yaml:"winsw_exe,omitempty"`
	StartType          string         `json:"start_type,omitempty" yaml:"start_type,omitempty"` // auto|manual|delayed
	Account            ServiceAccount `json:"account,omitempty" yaml:"account,omitempty"`
	Recovery           Recovery       `json:"recovery,omitempty" yaml:"recovery,omitempty"`
	StopTimeoutSeconds int            `json:"stop_timeout_seconds,omitempty" yaml:"stop_timeout_seconds,omitempty"`

	// node_web_app
	Entry       string `json:"entry,omitempty" yaml:"entry,omitempty"`
	NodeExe     string `json:"node_exe,omitempty" yaml:"node_exe,omitempty"`
	Port        int    `json:"port,omitempty" yaml:"port,omitempty"`
	InstallDeps bool   `json:"install_deps,omitempty" yaml:"install_deps,omitempty"`

	// dotnet_api
	Launcher  string `json:"launcher,omitempty" yaml:"launcher,omitempty"` // exe|dotnet_dll
	DLL       string `json:"dll,omitempty" yaml:"dll,omitempty"`
	DotnetExe string `json:"dotnet_exe,omitempty" yaml:"dotnet_exe,omitempty"`
	URLs      string `json:"urls,omitempty" yaml:"urls,omitempty"`
	Hosting   string `json:"hosting,omitempty" yaml:"hosting,omitempty"` // windows_service_native|winsw

	// cluster_generic_service
	RoleName       string `json:"role_name,omitempty" yaml:"role_name,omitempty"`
	StaticAddress  string `json:"static_address,omitempty" yaml:"static_address,omitempty"`
	PreferredOwner string `json:"preferred_owner,omitempty" yaml:"preferred_owner,omitempty"`

	// docker_container
	ContainerName string   `json:"container_name,omitempty" yaml:"container_name,omitempty"`
	Ports         []string `json:"ports,omitempty" yaml:"ports,omitempty"`
	Volumes       []string `json:"volumes,omitempty" yaml:"volumes,omitempty"`
	RestartPolicy string   `json:"restart_policy,omitempty" yaml:"restart_policy,omitempty"`
	RunArgs       []string `json:"run_args,omitempty" yaml:"run_args,omitempty"`
}

type RenderedFile struct {
	Path    string `json:"path" yaml:"path"` // relative to release dir
	Content string `json:"content" yaml:"content"`
}

type HTTPCheck struct {
	URL             string `json:"url" yaml:"url"`
	ExpectStatus    int    `json:"expect_status,omitempty" yaml:"expect_status,omitempty"`
	ExpectBodyRegex string `json:"expect_body_regex,omitempty" yaml:"expect_body_regex,omitempty"`
}

type TCPCheck struct {
	Port int `json:"port" yaml:"port"`
}

type ExecCheck struct {
	Command string `json:"command" yaml:"command"`
}

type HealthCheck struct {
	Type                string    `json:"type,omitempty" yaml:"type,omitempty"` // http|tcp|exec|none
	HTTP                HTTPCheck `json:"http,omitempty" yaml:"http,omitempty"`
	TCP                 TCPCheck  `json:"tcp,omitempty" yaml:"tcp,omitempty"`
	Exec                ExecCheck `json:"exec,omitempty" yaml:"exec,omitempty"`
	InitialDelaySeconds int       `json:"initial_delay_seconds,omitempty" yaml:"initial_delay_seconds,omitempty"`
	IntervalSeconds     int       `json:"interval_seconds,omitempty" yaml:"interval_seconds,omitempty"`
	TimeoutSeconds      int       `json:"timeout_seconds,omitempty" yaml:"timeout_seconds,omitempty"`
}

type ClusterStrategy struct {
	DrainTimeoutSeconds int `json:"drain_timeout_seconds,omitempty" yaml:"drain_timeout_seconds,omitempty"`
	HealthSettleSeconds int `json:"health_settle_seconds,omitempty" yaml:"health_settle_seconds,omitempty"`
}

type Strategy struct {
	KeepReleases       *int            `json:"keep_releases,omitempty" yaml:"keep_releases,omitempty"`
	RollbackOnFailure  *bool           `json:"rollback_on_failure,omitempty" yaml:"rollback_on_failure,omitempty"`
	Cluster            ClusterStrategy `json:"cluster,omitempty" yaml:"cluster,omitempty"`
	LockTimeoutSeconds int             `json:"lock_timeout_seconds,omitempty" yaml:"lock_timeout_seconds,omitempty"`
}

type EventLogSpec struct {
	Log      string `json:"log" yaml:"log"`
	Provider string `json:"provider,omitempty" yaml:"provider,omitempty"`
	MaxLevel int    `json:"max_level,omitempty" yaml:"max_level,omitempty"` // 1=Critical..5=Verbose; default 3
}

type LogsSpec struct {
	Paths            []string       `json:"paths,omitempty" yaml:"paths,omitempty"`
	WindowsEventLogs []EventLogSpec `json:"windows_event_logs,omitempty" yaml:"windows_event_logs,omitempty"`
}

// Deployment is the parsed kind: Deployment document.
type Deployment struct {
	APIVersion  string            `json:"apiVersion" yaml:"apiVersion"`
	Kind        string            `json:"kind" yaml:"kind"`
	Metadata    Metadata          `json:"metadata" yaml:"metadata"`
	Target      Target            `json:"target,omitempty" yaml:"target,omitempty"`
	Artifact    Artifact          `json:"artifact" yaml:"artifact"`
	Pattern     Pattern           `json:"pattern" yaml:"pattern"`
	Environment map[string]string `json:"environment,omitempty" yaml:"environment,omitempty"`
	Files       []RenderedFile    `json:"files,omitempty" yaml:"files,omitempty"`
	HealthCheck HealthCheck       `json:"health_check,omitempty" yaml:"health_check,omitempty"`
	Strategy    Strategy          `json:"strategy,omitempty" yaml:"strategy,omitempty"`
	Logs        LogsSpec          `json:"logs,omitempty" yaml:"logs,omitempty"`
}

// ------------------------------- TestRun -----------------------------------

type Runner struct {
	Type           string            `json:"type" yaml:"type"` // exec|vstest|dotnet_test|npm
	Command        string            `json:"command,omitempty" yaml:"command,omitempty"`
	Args           []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Assemblies     []string          `json:"assemblies,omitempty" yaml:"assemblies,omitempty"` // vstest
	Project        string            `json:"project,omitempty" yaml:"project,omitempty"`       // dotnet_test
	Script         string            `json:"script,omitempty" yaml:"script,omitempty"`         // npm (default: test)
	ExtraArgs      []string          `json:"extra_args,omitempty" yaml:"extra_args,omitempty"`
	WorkingDir     string            `json:"working_dir,omitempty" yaml:"working_dir,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" yaml:"timeout_seconds,omitempty"`
	Env            map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
}

type Results struct {
	Format string   `json:"format,omitempty" yaml:"format,omitempty"` // trx|junit|none
	Paths  []string `json:"paths,omitempty" yaml:"paths,omitempty"`
}

type PassCriteria struct {
	ExitCodes   []int    `json:"exit_codes,omitempty" yaml:"exit_codes,omitempty"`
	MinPassRate *float64 `json:"min_pass_rate,omitempty" yaml:"min_pass_rate,omitempty"`
}

type Collect struct {
	Logs             []string       `json:"logs,omitempty" yaml:"logs,omitempty"`
	AppSharedOf      string         `json:"app_shared_of,omitempty" yaml:"app_shared_of,omitempty"`
	WindowsEventLogs []EventLogSpec `json:"windows_event_logs,omitempty" yaml:"windows_event_logs,omitempty"`
	DestinationDir   string         `json:"destination_dir,omitempty" yaml:"destination_dir,omitempty"`
}

// TestRun is the parsed kind: TestRun document.
type TestRun struct {
	APIVersion   string       `json:"apiVersion" yaml:"apiVersion"`
	Kind         string       `json:"kind" yaml:"kind"`
	Metadata     Metadata     `json:"metadata" yaml:"metadata"`
	Target       Target       `json:"target,omitempty" yaml:"target,omitempty"`
	Artifact     Artifact     `json:"artifact" yaml:"artifact"`
	Runner       Runner       `json:"runner" yaml:"runner"`
	Results      Results      `json:"results,omitempty" yaml:"results,omitempty"`
	PassCriteria PassCriteria `json:"pass_criteria,omitempty" yaml:"pass_criteria,omitempty"`
	Collect      Collect      `json:"collect,omitempty" yaml:"collect,omitempty"`
	InstallRoot  string       `json:"install_root,omitempty" yaml:"install_root,omitempty"`
}

// ----------------------------- defaults helpers -----------------------------

func (t *Target) EffectivePort() int {
	if t.Port != 0 {
		return t.Port
	}
	switch t.Transport {
	case TransportSSH:
		return 22
	case TransportWinRM:
		if t.WinRM.UseHTTPS == nil || *t.WinRM.UseHTTPS {
			return 5986
		}
		return 5985
	default:
		return 0
	}
}

func (t *Target) EffectiveConnectRetries() int {
	if t.ConnectRetries != nil {
		return *t.ConnectRetries
	}
	return 3
}

func (s *Strategy) EffectiveKeepReleases() int {
	if s.KeepReleases != nil && *s.KeepReleases >= 1 {
		return *s.KeepReleases
	}
	return 3
}

func (s *Strategy) EffectiveRollback() bool {
	if s.RollbackOnFailure != nil {
		return *s.RollbackOnFailure
	}
	return true
}

func (s *Strategy) EffectiveLockTimeout() int {
	if s.LockTimeoutSeconds > 0 {
		return s.LockTimeoutSeconds
	}
	return 900
}

func (c *ClusterStrategy) EffectiveDrain() int {
	if c.DrainTimeoutSeconds > 0 {
		return c.DrainTimeoutSeconds
	}
	return 300
}

func (c *ClusterStrategy) EffectiveSettle() int {
	if c.HealthSettleSeconds > 0 {
		return c.HealthSettleSeconds
	}
	return 10
}

func (h *HealthCheck) EffectiveType() string {
	if h.Type == "" {
		return "none"
	}
	return h.Type
}

func (h *HealthCheck) Budget() (initial, interval, timeout int) {
	initial, interval, timeout = h.InitialDelaySeconds, h.IntervalSeconds, h.TimeoutSeconds
	if initial <= 0 {
		initial = 5
	}
	if interval <= 0 {
		interval = 5
	}
	if timeout <= 0 {
		timeout = 60
	}
	return
}

func (p *Pattern) EffectiveStopTimeout() int {
	if p.StopTimeoutSeconds > 0 {
		return p.StopTimeoutSeconds
	}
	return 30
}

func (p *Pattern) EffectiveInstallRoot(os OSKind) string {
	if p.InstallRoot != "" {
		return p.InstallRoot
	}
	if os == OSLinux {
		return "/opt/deploy"
	}
	return `C:\deploy`
}

func (r *Runner) EffectiveTimeout() int {
	if r.TimeoutSeconds > 0 {
		return r.TimeoutSeconds
	}
	return 1800
}

func (pc *PassCriteria) EffectiveExitCodes() []int {
	if len(pc.ExitCodes) > 0 {
		return pc.ExitCodes
	}
	return []int{0}
}

func (pc *PassCriteria) EffectiveMinPassRate() float64 {
	if pc.MinPassRate != nil {
		return *pc.MinPassRate
	}
	return 1.0
}

// EffectiveWorkRoot returns where test packages land (DESIGN §7.1).
func (t *TestRun) EffectiveWorkRoot(os OSKind) string {
	if t.InstallRoot != "" {
		return t.InstallRoot
	}
	if os == OSLinux {
		return "/opt/deploy"
	}
	return `C:\deploy`
}

// EffectiveDestinationDir on the RUNNER (DESIGN §7.4); default ./labdeploy-results.
func (c *Collect) EffectiveDestinationDir() string {
	if c.DestinationDir != "" {
		return c.DestinationDir
	}
	return "labdeploy-results"
}
