package spec

import (
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
	if err == nil || !strings.Contains(err.Error(), "HOST") {
		t.Fatalf("want missing-var error naming HOST, got %v", err)
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

func TestUnknownFieldRejected(t *testing.T) { // VAL-02
	bad := strings.Replace(winSvcYAML, "health_check:", "health_checkk:", 1)
	t.Setenv("LABDEPLOY_PASSWORD", "x")
	_, _, err := ParseDeployment(bad, map[string]string{"HOST": "h"}, "")
	if err == nil {
		t.Fatal("unknown field must be rejected")
	}
}

func TestChecksumRequiredForZip(t *testing.T) { // VAL-04
	bad := strings.Replace(winSvcYAML,
		`checksum: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`, "", 1)
	t.Setenv("LABDEPLOY_PASSWORD", "x")
	_, _, err := ParseDeployment(bad, map[string]string{"HOST": "h"}, "")
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("want checksum error, got %v", err)
	}
}

func TestPatternOSMatrix(t *testing.T) { // VAL-06
	bad := strings.Replace(winSvcYAML, "os: windows", "os: linux", 1)
	t.Setenv("LABDEPLOY_PASSWORD", "x")
	_, _, err := ParseDeployment(bad, map[string]string{"HOST": "h"}, "")
	if err == nil {
		t.Fatal("windows_service on linux must be rejected")
	}
}

func TestClusterHostsMin(t *testing.T) { // VAL-07
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
	if err == nil || !strings.Contains(err.Error(), "DEFINITELY_NOT_SET_ABC123") {
		t.Fatalf("must name the missing env var, got %v", err)
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
