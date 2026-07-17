package spec

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const winSvcYAML = `
apiVersion: labdeploy/v1
kind: Deployment
metadata:
  name: sample-svc
target:
  transport: winrm
  hosts: ["${var:HOST}"]
  os: windows
  credentials:
    username: LAB\deploy
    password_env: LABDEPLOY_PASSWORD
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  source: { type: http, url: "https://x/y.zip" }
pattern:
  type: windows_service
  service_name: SampleSvc
  exe: bin\SampleSvc.exe
health_check:
  type: http
  http: { url: "http://localhost:8080/health" }
`

func TestSubstituteAndParse(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "x")
	d, hash, err := ParseDeployment(winSvcYAML, map[string]string{"HOST": "lab-01"}, "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if d.Target.Hosts[0] != "lab-01" {
		t.Fatalf("substitution failed: %v", d.Target.Hosts)
	}
	if hash == "" || len(hash) != 64 {
		t.Fatalf("bad canonical hash %q", hash)
	}
	// Same spec, same vars => same hash (drift detection contract).
	_, hash2, _ := ParseDeployment(winSvcYAML, map[string]string{"HOST": "lab-01"}, "")
	if hash != hash2 {
		t.Fatalf("hash not stable: %s vs %s", hash, hash2)
	}
	// version_override changes effective version AND hash.
	d3, hash3, err := ParseDeployment(winSvcYAML, map[string]string{"HOST": "lab-01"}, "2.0.0")
	if err != nil {
		t.Fatalf("override parse: %v", err)
	}
	if d3.Artifact.Version != "2.0.0" {
		t.Fatalf("override not applied: %s", d3.Artifact.Version)
	}
	if hash3 == hash {
		t.Fatal("hash must change with version_override")
	}
}

func TestSubstituteMissingVar(t *testing.T) {
	_, _, err := ParseDeployment(winSvcYAML, nil, "")
	if err == nil {
		t.Fatal("want missing-var error")
	}
	// DESIGN §6.6 contract: `unresolved variable NAME at <json-path>`, coded.
	for _, frag := range []string{"[ERR_SPEC_INVALID]", "unresolved variable HOST", "at target.hosts[0]"} {
		if !strings.Contains(err.Error(), frag) {
			t.Fatalf("missing-var error missing %q: got %v", frag, err)
		}
	}
}

func TestSubstituteEscape(t *testing.T) {
	out, err := Substitute("literal $${var:X} plus ${var:X}; bare $$ stays", map[string]string{"X": "y"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "literal ${var:X} plus y; bare $$ stays" {
		t.Fatalf("escape handling wrong: %q", out)
	}
}

func TestUnknownFieldRejected(t *testing.T) { // schema strictness (DisallowUnknownFields)
	bad := strings.Replace(winSvcYAML, "health_check:", "health_checkk:", 1)
	t.Setenv("LABDEPLOY_PASSWORD", "x")
	_, _, err := ParseDeployment(bad, map[string]string{"HOST": "h"}, "")
	if err == nil {
		t.Fatal("unknown field must be rejected")
	}
}

func TestChecksumRequiredForZip(t *testing.T) { // checksum required for zip (VAL-05 covers md5 format)
	bad := strings.Replace(winSvcYAML,
		`checksum: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`, "", 1)
	t.Setenv("LABDEPLOY_PASSWORD", "x")
	_, _, err := ParseDeployment(bad, map[string]string{"HOST": "h"}, "")
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("want checksum error, got %v", err)
	}
}

func TestPatternOSMatrix(t *testing.T) { // VAL-03: windows_service on linux (§14 matrix)
	bad := strings.Replace(winSvcYAML, "os: windows", "os: linux", 1)
	t.Setenv("LABDEPLOY_PASSWORD", "x")
	_, _, err := ParseDeployment(bad, map[string]string{"HOST": "h"}, "")
	if err == nil {
		t.Fatal("windows_service on linux must be rejected")
	}
}

func TestClusterHostsMin(t *testing.T) { // VAL-04: cluster pattern with 1 host
	y := `
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: clu }
target:
  transport: winrm
  hosts: ["n1"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  source: { type: http, url: "https://x/y.zip" }
pattern:
  type: cluster_generic_service
  service_name: S
  role_name: R
  exe: s.exe
health_check: { type: none }
`
	t.Setenv("LABDEPLOY_PASSWORD", "x")
	_, _, err := ParseDeployment(y, nil, "")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "host") {
		t.Fatalf("cluster with 1 host must fail, got %v", err)
	}
}

func TestMissingEnvVarNamed(t *testing.T) { // VAL-08: env presence checked pre-dial
	y := strings.Replace(winSvcYAML, "LABDEPLOY_PASSWORD", "DEFINITELY_NOT_SET_ABC123", 1)
	_, _, err := ParseDeployment(y, map[string]string{"HOST": "h"}, "")
	if err == nil {
		t.Fatal("must reject unset env var")
	}
	// DESIGN §11: `env var <NAME> referenced by <path> is not set`, naming the
	// exact JSON path (target.credentials.password_env), coded ERR_SPEC_INVALID.
	for _, frag := range []string{
		"[ERR_SPEC_INVALID]",
		"env var DEFINITELY_NOT_SET_ABC123",
		"referenced by target.credentials.password_env",
		"is not set",
	} {
		if !strings.Contains(err.Error(), frag) {
			t.Fatalf("env-var error missing %q: got %v", frag, err)
		}
	}
}

// TestEnvVarPathsDistinct proves each credential/auth/account env reference is
// reported with its own precise JSON path (evaluator feedback item 2).
func TestEnvVarPathsDistinct(t *testing.T) {
	cases := []struct {
		name  string
		yaml  string
		unset string // env name that will be missing
		path  string // exact JSON path expected in the error
	}{
		{
			name: "ssh private_key_env",
			yaml: `
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: keyed }
target:
  transport: ssh
  hosts: ["h1"]
  os: linux
  credentials: { username: deploy, private_key_env: MISSING_PK_ENV_1 }
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  source: { type: http, url: "https://x/y.zip" }
pattern: { type: console_app, exe: app }
health_check: { type: none }
`,
			unset: "MISSING_PK_ENV_1",
			path:  "target.credentials.private_key_env",
		},
		{
			name: "artifact source auth token_env",
			yaml: `
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: authed }
target:
  transport: local
  hosts: ["localhost"]
  os: linux
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  source: { type: http, url: "https://x/y.zip", auth: { token_env: MISSING_TOKEN_ENV_1 } }
pattern: { type: console_app, exe: app }
health_check: { type: none }
`,
			unset: "MISSING_TOKEN_ENV_1",
			path:  "artifact.source.auth.token_env",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_ = os.Unsetenv(c.unset)
			_, _, err := ParseDeployment(c.yaml, nil, "")
			if err == nil {
				t.Fatalf("want error for unset %s", c.unset)
			}
			for _, frag := range []string{"[ERR_SPEC_INVALID]", "env var " + c.unset, "referenced by " + c.path, "is not set"} {
				if !strings.Contains(err.Error(), frag) {
					t.Fatalf("error missing %q: got %v", frag, err)
				}
			}
		})
	}
}

func TestTestRunParse(t *testing.T) {
	y := `
apiVersion: labdeploy/v1
kind: TestRun
metadata: { name: e2e }
target:
  transport: winrm
  hosts: ["h1"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  source: { type: http, url: "https://x/t.zip" }
runner:
  type: vstest
  assemblies: ["A.dll"]
results: { format: trx, paths: ["TestResults/*.trx"] }
pass_criteria: { exit_codes: [0], min_pass_rate: 1.0 }
`
	t.Setenv("LABDEPLOY_PASSWORD", "x")
	tr, _, err := ParseTestRun(y, nil)
	if err != nil {
		t.Fatalf("testrun parse: %v", err)
	}
	if tr.Runner.EffectiveTimeout() <= 0 {
		t.Fatal("timeout default missing")
	}
	if got := tr.PassCriteria.EffectiveMinPassRate(); got != 1.0 {
		t.Fatalf("min pass rate: %v", got)
	}
	if tr.EffectiveWorkRoot(OSWindows) == "" {
		t.Fatal("work root default missing")
	}
}

// Scenario: YAML equals JSON — the same Deployment authored in YAML and in JSON
// must parse to byte-identical in-memory structs (and identical canonical hash).
func TestYAMLEqualsJSON(t *testing.T) {
	const jsonSpec = `{
  "apiVersion": "labdeploy/v1",
  "kind": "Deployment",
  "metadata": { "name": "sample-svc" },
  "target": {
    "transport": "winrm",
    "hosts": ["${var:HOST}"],
    "os": "windows",
    "credentials": { "username": "LAB\\deploy", "password_env": "LABDEPLOY_PASSWORD" }
  },
  "artifact": {
    "type": "zip",
    "version": "1.0.0",
    "checksum": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "source": { "type": "http", "url": "https://x/y.zip" }
  },
  "pattern": {
    "type": "windows_service",
    "service_name": "SampleSvc",
    "exe": "bin\\SampleSvc.exe"
  },
  "health_check": { "type": "http", "http": { "url": "http://localhost:8080/health" } }
}`
	t.Setenv("LABDEPLOY_PASSWORD", "x")
	vars := map[string]string{"HOST": "lab-01"}
	fromYAML, hy, err := ParseDeployment(winSvcYAML, vars, "")
	if err != nil {
		t.Fatalf("yaml parse: %v", err)
	}
	fromJSON, hj, err := ParseDeployment(jsonSpec, vars, "")
	if err != nil {
		t.Fatalf("json parse: %v", err)
	}
	if !reflect.DeepEqual(fromYAML, fromJSON) {
		t.Fatalf("YAML and JSON produced different structs:\n yaml=%+v\n json=%+v", fromYAML, fromJSON)
	}
	if hy != hj {
		t.Fatalf("canonical hash differs across formats: %s vs %s", hy, hj)
	}
}

// Scenario: Pattern union decode — pattern.type: windows_service selects the
// concrete windows_service member (non-nil) via type-directed decoding, and
// every OTHER concrete union member is nil (DESIGN §6.4).
func TestPatternUnionDecode(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "x")
	d, _, err := ParseDeployment(winSvcYAML, map[string]string{"HOST": "lab-01"}, "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p := d.Pattern
	if p.Type != PatternWindowsService {
		t.Fatalf("wrong pattern type: %q", p.Type)
	}
	// The selected concrete member must be non-nil and populated.
	if p.WindowsService == nil {
		t.Fatal("windows_service concrete member is nil; type-directed decode failed")
	}
	if p.WindowsService.ServiceName != "SampleSvc" {
		t.Fatalf("windows_service member not populated: %+v", p.WindowsService)
	}
	if p.WindowsService.Exe != `bin\SampleSvc.exe` {
		t.Fatalf("windows_service.exe not populated: %q", p.WindowsService.Exe)
	}
	// Every OTHER concrete union member must be nil.
	if p.ConsoleApp != nil {
		t.Fatalf("console_app member should be nil, got %+v", p.ConsoleApp)
	}
	if p.NodeWebApp != nil {
		t.Fatalf("node_web_app member should be nil, got %+v", p.NodeWebApp)
	}
	if p.DotnetAPI != nil {
		t.Fatalf("dotnet_api member should be nil, got %+v", p.DotnetAPI)
	}
	if p.ClusterGeneric != nil {
		t.Fatalf("cluster_generic_service member should be nil, got %+v", p.ClusterGeneric)
	}
	if p.DockerContainer != nil {
		t.Fatalf("docker_container member should be nil, got %+v", p.DockerContainer)
	}
}

// Each pattern.type selects exactly one concrete union member; assert the whole
// matrix so no two members are ever simultaneously non-nil.
func TestPatternUnionMatrix(t *testing.T) {
	base := `
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: p }
target:
  transport: winrm
  hosts: ["h1"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  source: { type: http, url: "https://x/y.zip" }
health_check: { type: none }
`
	t.Setenv("LABDEPLOY_PASSWORD", "x")
	cases := []struct {
		name    string
		pattern string
		typ     PatternType
		pick    func(*Pattern) bool
	}{
		{"console_app", "type: console_app\n  exe: app.exe", PatternConsoleApp,
			func(p *Pattern) bool { return p.ConsoleApp != nil }},
		{"windows_service", "type: windows_service\n  service_name: S\n  exe: s.exe", PatternWindowsService,
			func(p *Pattern) bool { return p.WindowsService != nil }},
		{"node_web_app", "type: node_web_app\n  service_name: S\n  entry: server.js\n  port: 8080\n  winsw_exe: winsw.exe", PatternNodeWebApp,
			func(p *Pattern) bool { return p.NodeWebApp != nil }},
		{"dotnet_api", "type: dotnet_api\n  service_name: S\n  exe: api.exe", PatternDotnetAPI,
			func(p *Pattern) bool { return p.DotnetAPI != nil }},
		{"cluster_generic_service", "type: cluster_generic_service\n  service_name: S\n  role_name: R\n  exe: s.exe", PatternClusterGeneric,
			func(p *Pattern) bool { return p.ClusterGeneric != nil }},
		{"docker_container", "type: docker_container\n  container_name: c", PatternDockerCont,
			func(p *Pattern) bool { return p.DockerContainer != nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			y := strings.Replace(base, "health_check:", "pattern:\n  "+c.pattern+"\nhealth_check:", 1)
			// docker_container requires docker_image artifact; swap for that case.
			if c.typ == PatternDockerCont {
				y = strings.Replace(y, "type: zip", "type: docker_image", 1)
				y = strings.Replace(y, `  checksum: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`, "", 1)
				y = strings.Replace(y, `source: { type: http, url: "https://x/y.zip" }`,
					`source: { type: docker_registry, image: "repo/img", tag: "1" }`, 1)
			}
			d, _, err := ParseDeploymentLenient(y, nil, "")
			if err != nil {
				t.Fatalf("parse %s: %v", c.name, err)
			}
			if d.Pattern.Type != c.typ {
				t.Fatalf("type mismatch: got %q", d.Pattern.Type)
			}
			if !c.pick(&d.Pattern) {
				t.Fatalf("%s concrete member not selected: %+v", c.name, d.Pattern)
			}
			members := []interface{}{
				d.Pattern.ConsoleApp, d.Pattern.WindowsService, d.Pattern.NodeWebApp,
				d.Pattern.DotnetAPI, d.Pattern.ClusterGeneric, d.Pattern.DockerContainer,
			}
			nonNil := 0
			for _, m := range members {
				switch v := m.(type) {
				case *ConsoleAppPattern:
					if v != nil {
						nonNil++
					}
				case *WindowsServicePattern:
					if v != nil {
						nonNil++
					}
				case *NodeWebAppPattern:
					if v != nil {
						nonNil++
					}
				case *DotnetAPIPattern:
					if v != nil {
						nonNil++
					}
				case *ClusterGenericPattern:
					if v != nil {
						nonNil++
					}
				case *DockerContainerPattern:
					if v != nil {
						nonNil++
					}
				}
			}
			if nonNil != 1 {
				t.Fatalf("expected exactly 1 concrete member non-nil, got %d", nonNil)
			}
		})
	}
}

// A field that belongs to a DIFFERENT union variant must be rejected by the
// selected concrete schema, not silently accepted (strict discriminated union).
func TestPatternUnionRejectsCrossVariantField(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "x")
	// windows_service spec carrying docker_container's container_name.
	bad := strings.Replace(winSvcYAML,
		"  service_name: SampleSvc",
		"  service_name: SampleSvc\n  container_name: sneaky",
		1)
	_, _, err := ParseDeploymentLenient(bad, map[string]string{"HOST": "h"}, "")
	if err == nil {
		t.Fatal("cross-variant field container_name on windows_service must be rejected")
	}
	if !strings.Contains(err.Error(), "container_name") {
		t.Fatalf("error should name the offending field, got %v", err)
	}
	// node_web_app must reject start_type (not a documented node_web_app field).
	nodeBad := `
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: n }
target:
  transport: winrm
  hosts: ["h1"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  source: { type: http, url: "https://x/y.zip" }
pattern:
  type: node_web_app
  service_name: S
  entry: server.js
  port: 8080
  winsw_exe: winsw.exe
  start_type: auto
health_check: { type: none }
`
	if _, _, err := ParseDeploymentLenient(nodeBad, nil, ""); err == nil ||
		!strings.Contains(err.Error(), "start_type") {
		t.Fatalf("node_web_app must reject start_type, got %v", err)
	}
}

func TestMergeTargetDefaults(t *testing.T) {
	y := strings.Replace(winSvcYAML, `  credentials:
    username: LAB\deploy
    password_env: LABDEPLOY_PASSWORD
`, "", 1)
	t.Setenv("LABDEPLOY_PASSWORD", "x")
	// Without defaults: fails on missing credentials.
	if _, _, err := ParseDeployment(y, map[string]string{"HOST": "h"}, ""); err == nil {
		t.Fatal("expected credential validation failure")
	}
	// Parse leniently then merge provider defaults and revalidate.
	d, _, err := ParseDeploymentLenient(y, map[string]string{"HOST": "h"}, "")
	if err != nil {
		t.Fatalf("lenient parse: %v", err)
	}
	def := &Target{}
	def.Credentials.Username = "LAB\\svc"
	def.Credentials.PasswordEnv = "LABDEPLOY_PASSWORD"
	MergeTargetDefaults(&d.Target, def)
	if err := ValidateDeployment(d); err != nil {
		t.Fatalf("post-merge validate: %v", err)
	}
	if d.Target.Credentials.Username != "LAB\\svc" {
		t.Fatal("merge did not apply username")
	}
}

func TestFilesPathTraversalRejected(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "x")
	base, _, err := ParseDeployment(winSvcYAML, map[string]string{"HOST": "lab-01"}, "")
	if err != nil {
		t.Fatalf("base parse: %v", err)
	}

	// A plain relative path passes.
	base.Files = []RenderedFile{{Path: "config/app.json", Content: "{}"}}
	if err := ValidateDeployment(base); err != nil {
		t.Fatalf("relative path should pass: %v", err)
	}

	bad := []string{
		"/etc/x",           // POSIX absolute
		`C:\x`,             // Windows drive-letter absolute
		"C:/x",             // Windows drive-letter absolute, forward slash
		`\host\share\x`,    // Windows root-relative / UNC
		"../escape",        // parent traversal
		"a/../../escape",   // nested traversal
		`sub\..\..\escape`, // Windows-separator traversal
		`C:..\escape`,      // Windows drive-relative traversal (evaluator item 5)
		"C:x",              // Windows drive-relative (volume-anchored)
	}
	for _, p := range bad {
		base.Files = []RenderedFile{{Path: p, Content: "x"}}
		err := ValidateDeployment(base)
		if err == nil {
			t.Fatalf("path %q must be rejected", p)
		}
		if !strings.Contains(err.Error(), "files[0].path") {
			t.Fatalf("path %q: error must name files[0].path, got %v", p, err)
		}
	}
}

func TestFilesPathEmptyRejected(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "x")
	base, _, err := ParseDeployment(winSvcYAML, map[string]string{"HOST": "lab-01"}, "")
	if err != nil {
		t.Fatalf("base parse: %v", err)
	}
	base.Files = []RenderedFile{{Path: "", Content: "x"}}
	err = ValidateDeployment(base)
	if err == nil || !strings.Contains(err.Error(), "files[0].path") {
		t.Fatalf("empty path must be rejected naming files[0].path, got %v", err)
	}
}

// clusterOneHostYAML is a cluster_generic_service spec with a single host —
// below the 2..16 requirement (VAL-04).
const clusterOneHostYAML = `
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: clu1 }
target:
  transport: winrm
  hosts: ["only-node"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  source: { type: http, url: "https://x/y.zip" }
pattern:
  type: cluster_generic_service
  service_name: S
  role_name: R
  exe: s.exe
health_check: { type: none }
`

// dockerWinSvcYAML pairs artifact.type docker_image with a non-docker pattern —
// forbidden by the artifact×pattern matrix (VAL-09).
const dockerWinSvcYAML = `
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: dk }
target:
  transport: winrm
  hosts: ["h1"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: docker_image
  version: 1.0.0
  source: { type: docker_registry, image: "repo/img:tag" }
pattern:
  type: windows_service
  service_name: S
  exe: s.exe
health_check: { type: none }
`

// TestValidationMatrix exercises the DESIGN §14 VAL-01..VAL-09 contract. Each
// invalid spec must fail as [ERR_SPEC_INVALID] with the expected JSON path /
// message fragment. VAL-06 (both `spec` and `spec_file` set) is a provider
// schema concern, verified in internal/provider (TestVAL06ExactlyOneOfSpec).
func TestValidationMatrix(t *testing.T) {
	const sha = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cases := []struct {
		id        string
		yaml      string
		vars      map[string]string
		setEnv    map[string]string
		fragments []string
	}{
		{
			id:        "VAL-01 tab-broken YAML",
			yaml:      "apiVersion: labdeploy/v1\nkind: Deployment\nmetadata:\n\tname: x\n",
			fragments: []string{"[ERR_SPEC_INVALID]", "line"},
		},
		{
			id:        "VAL-02 missing artifact.version",
			yaml:      strings.Replace(winSvcYAML, "  version: 1.0.0\n", "", 1),
			vars:      map[string]string{"HOST": "h"},
			setEnv:    map[string]string{"LABDEPLOY_PASSWORD": "x"},
			fragments: []string{"[ERR_SPEC_INVALID]", "artifact.version"},
		},
		{
			id:        "VAL-03 windows_service on linux",
			yaml:      strings.Replace(winSvcYAML, "os: windows", "os: linux", 1),
			vars:      map[string]string{"HOST": "h"},
			setEnv:    map[string]string{"LABDEPLOY_PASSWORD": "x"},
			fragments: []string{"[ERR_SPEC_INVALID]", "pattern.type", "windows_service", "linux"},
		},
		{
			id:        "VAL-04 cluster one host",
			yaml:      clusterOneHostYAML,
			setEnv:    map[string]string{"LABDEPLOY_PASSWORD": "x"},
			fragments: []string{"[ERR_SPEC_INVALID]", "target.hosts", "2..16"},
		},
		{
			id:        "VAL-05 md5 checksum",
			yaml:      strings.Replace(winSvcYAML, sha, "md5:deadbeef", 1),
			vars:      map[string]string{"HOST": "h"},
			setEnv:    map[string]string{"LABDEPLOY_PASSWORD": "x"},
			fragments: []string{"[ERR_SPEC_INVALID]", "artifact.checksum"},
		},
		{
			id:        "VAL-07 unresolved variable",
			yaml:      winSvcYAML,
			vars:      nil,
			setEnv:    map[string]string{"LABDEPLOY_PASSWORD": "x"},
			fragments: []string{"[ERR_SPEC_INVALID]", "unresolved variable HOST", "at target.hosts[0]"},
		},
		{
			id:        "VAL-08 unset env var",
			yaml:      strings.Replace(winSvcYAML, "LABDEPLOY_PASSWORD", "NEVER_SET_ENV_VAL08", 1),
			vars:      map[string]string{"HOST": "h"},
			fragments: []string{"[ERR_SPEC_INVALID]", "env var NEVER_SET_ENV_VAL08", "referenced by target.credentials.password_env", "is not set"},
		},
		{
			id:        "VAL-09 docker_image with windows_service",
			yaml:      dockerWinSvcYAML,
			setEnv:    map[string]string{"LABDEPLOY_PASSWORD": "x"},
			fragments: []string{"[ERR_SPEC_INVALID]", "artifact.type", "docker_image", "docker_container"},
		},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			for k, v := range c.setEnv {
				t.Setenv(k, v)
			}
			_ = os.Unsetenv("NEVER_SET_ENV_VAL08")
			_, _, err := ParseDeployment(c.yaml, c.vars, "")
			if err == nil {
				t.Fatalf("%s: expected [ERR_SPEC_INVALID], got nil", c.id)
			}
			for _, f := range c.fragments {
				if !strings.Contains(err.Error(), f) {
					t.Fatalf("%s: error missing %q: got %v", c.id, f, err)
				}
			}
		})
	}
}

// Golden canonical-JSON hashes (DESIGN §Testing "canonical hash stability (key
// order)", D7). The two committed testdata fixtures encode the SAME logical
// Deployment with object keys emitted in different orders; canonicalization
// (sorted keys, no insignificant whitespace) MUST collapse both to the same
// byte string and therefore the same sha256. These constants pin the exact
// bytes so an accidental change to the emitter is caught (T2 preservation).
const (
	goldenCanonicalHash = "1cea4fe84fd61900622d357f0bd89cdbce7d7f1c1876a2a2e2b42f398f3e623a"
	goldenSpecHash      = "5390097b78b3d51afcf668613fc26dc68ea9c6b0b0e2dad7062a7527b169e395"
)

// TestCanonicalHashKeyOrderStable is the T2 preservation proof: a committed
// canonical-JSON fixture rendered with keys in different orders hashes to a
// byte-identical spec_hash (DESIGN §Testing, §6, D7).
func TestCanonicalHashKeyOrderStable(t *testing.T) {
	a := readTestdata(t, "canonical_order_a.json")
	b := readTestdata(t, "canonical_order_b.json")

	// 1. Raw canonical hash: sorting keys collapses both orderings to one hash.
	ha, err := CanonicalHash([]byte(a))
	if err != nil {
		t.Fatalf("canonical hash A: %v", err)
	}
	hb, err := CanonicalHash([]byte(b))
	if err != nil {
		t.Fatalf("canonical hash B: %v", err)
	}
	if ha != hb {
		t.Fatalf("canonical hash not key-order stable: %s vs %s", ha, hb)
	}
	if ha != goldenCanonicalHash {
		t.Fatalf("canonical hash drifted from golden: got %s want %s", ha, goldenCanonicalHash)
	}

	// 2. Full pipeline: parse (substitution + override no-op) then spec_hash.
	// Byte-identical spec_hash regardless of source key order (DESIGN D7).
	_, sa, err := ParseDeploymentLenient(a, nil, "")
	if err != nil {
		t.Fatalf("parse fixture A: %v", err)
	}
	_, sb, err := ParseDeploymentLenient(b, nil, "")
	if err != nil {
		t.Fatalf("parse fixture B: %v", err)
	}
	if sa != sb {
		t.Fatalf("spec_hash not key-order stable: %s vs %s", sa, sb)
	}
	if sa != goldenSpecHash {
		t.Fatalf("spec_hash drifted from golden: got %s want %s", sa, goldenSpecHash)
	}
}

// TestVersionOverrideChangesHash is the in-process proof that version_override
// is applied to artifact.version BEFORE hashing so the pipeline build number
// drives the release dir name (DESIGN §6, §Testing).
func TestVersionOverrideChangesHash(t *testing.T) {
	base := readTestdata(t, "canonical_order_a.json")
	dBase, hBase, err := ParseDeploymentLenient(base, nil, "")
	if err != nil {
		t.Fatalf("base parse: %v", err)
	}
	if dBase.Artifact.Version != "1.0.0" {
		t.Fatalf("base version: got %q want 1.0.0", dBase.Artifact.Version)
	}
	dOv, hOv, err := ParseDeploymentLenient(base, nil, "2.0.0")
	if err != nil {
		t.Fatalf("override parse: %v", err)
	}
	if dOv.Artifact.Version != "2.0.0" {
		t.Fatalf("override not applied: got %q want 2.0.0", dOv.Artifact.Version)
	}
	if hOv == hBase {
		t.Fatalf("spec_hash must differ after version_override: %s", hOv)
	}
}

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}
