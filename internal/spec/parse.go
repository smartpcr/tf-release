package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var varToken = regexp.MustCompile(`\$(\$?)\{var:([A-Za-z_][A-Za-z0-9_]*)\}`)

// Substitute resolves ${var:NAME} tokens from vars. `$${var:X}` escapes to a
// literal `${var:X}` (DESIGN §6.6). Unknown names return ERR_SPEC_INVALID text.
func Substitute(raw string, vars map[string]string) (string, error) {
	var firstErr error
	out := varToken.ReplaceAllStringFunc(raw, func(m string) string {
		g := varToken.FindStringSubmatch(m)
		if g[1] == "$" { // escaped
			return "${var:" + g[2] + "}"
		}
		v, ok := vars[g[2]]
		if !ok {
			if firstErr == nil {
				firstErr = fmt.Errorf("unresolved variable %q", g[2])
			}
			return m
		}
		return v
	})
	return out, firstErr
}

func toJSON(raw string) ([]byte, error) {
	trimmed := strings.TrimLeft(raw, " \t\r\n")
	if strings.HasPrefix(trimmed, "{") { // already JSON
		return []byte(raw), nil
	}
	var v interface{}
	if err := yaml.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("yaml parse: %w", err)
	}
	return json.Marshal(normalizeYAML(v))
}

// yaml.v3 already yields map[string]interface{} for string keys, but nested
// non-string keys must be rejected deterministically.
func normalizeYAML(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			t[k] = normalizeYAML(val)
		}
		return t
	case []interface{}:
		for i, val := range t {
			t[i] = normalizeYAML(val)
		}
		return t
	default:
		return v
	}
}

// CanonicalHash returns sha256 over canonical JSON (sorted keys — Go's
// json.Marshal of map sorts keys). Drives spec_hash / plan diffs (DESIGN D7).
func CanonicalHash(jsonBytes []byte) (string, error) {
	var v interface{}
	if err := json.Unmarshal(jsonBytes, &v); err != nil {
		return "", err
	}
	canon, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

type kindProbe struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

// ParseDeploymentLenient runs substitute → yaml/json → struct → versionOverride →
// canonical hash. NO semantic validation — callers merging provider
// default_target validate afterwards (DESIGN §6.2 merge-then-validate).
func ParseDeploymentLenient(raw string, vars map[string]string, versionOverride string) (*Deployment, string, error) {
	sub, err := Substitute(raw, vars)
	if err != nil {
		return nil, "", err
	}
	jb, err := toJSON(sub)
	if err != nil {
		return nil, "", err
	}
	var probe kindProbe
	_ = json.Unmarshal(jb, &probe)
	if probe.Kind != "" && probe.Kind != "Deployment" {
		return nil, "", fmt.Errorf("kind: expected Deployment, got %q", probe.Kind)
	}
	var d Deployment
	dec := json.NewDecoder(strings.NewReader(string(jb)))
	dec.DisallowUnknownFields()
	// dec.Decode invokes Pattern.UnmarshalJSON, which performs type-directed
	// decoding of the pattern.type discriminated union into the matching concrete
	// member (DESIGN §6.4). See types.go Pattern.decodeUnion.
	if err := dec.Decode(&d); err != nil {
		return nil, "", fmt.Errorf("spec decode: %w", err)
	}
	if versionOverride != "" {
		d.Artifact.Version = versionOverride
	}
	canonBytes, _ := json.Marshal(&d)
	hash, err := CanonicalHash(canonBytes)
	if err != nil {
		return nil, "", err
	}
	return &d, hash, nil
}

// ParseDeployment = ParseDeploymentLenient + ValidateDeployment.
func ParseDeployment(raw string, vars map[string]string, versionOverride string) (*Deployment, string, error) {
	d, hash, err := ParseDeploymentLenient(raw, vars, versionOverride)
	if err != nil {
		return nil, "", err
	}
	if err := ValidateDeployment(d); err != nil {
		return nil, "", err
	}
	return d, hash, nil
}

// ParseTestRunLenient mirrors ParseDeploymentLenient for kind TestRun.
func ParseTestRunLenient(raw string, vars map[string]string) (*TestRun, string, error) {
	sub, err := Substitute(raw, vars)
	if err != nil {
		return nil, "", err
	}
	jb, err := toJSON(sub)
	if err != nil {
		return nil, "", err
	}
	var probe kindProbe
	_ = json.Unmarshal(jb, &probe)
	if probe.Kind != "" && probe.Kind != "TestRun" {
		return nil, "", fmt.Errorf("kind: expected TestRun, got %q", probe.Kind)
	}
	var t TestRun
	dec := json.NewDecoder(strings.NewReader(string(jb)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return nil, "", fmt.Errorf("spec decode: %w", err)
	}
	canonBytes, _ := json.Marshal(&t)
	hash, err := CanonicalHash(canonBytes)
	if err != nil {
		return nil, "", err
	}
	return &t, hash, nil
}

// ParseTestRun = ParseTestRunLenient + ValidateTestRun.
func ParseTestRun(raw string, vars map[string]string) (*TestRun, string, error) {
	t, hash, err := ParseTestRunLenient(raw, vars)
	if err != nil {
		return nil, "", err
	}
	if err := ValidateTestRun(t); err != nil {
		return nil, "", err
	}
	return t, hash, nil
}

// MergeTargetDefaults applies provider default_target field-by-field (DESIGN §6.2).
func MergeTargetDefaults(t *Target, def *Target) {
	if def == nil {
		return
	}
	if t.Transport == "" {
		t.Transport = def.Transport
	}
	if len(t.Hosts) == 0 && len(def.Hosts) > 0 {
		t.Hosts = append([]string{}, def.Hosts...)
	}
	if t.OS == "" {
		t.OS = def.OS
	}
	if t.Port == 0 {
		t.Port = def.Port
	}
	if t.Credentials.Username == "" {
		t.Credentials.Username = def.Credentials.Username
	}
	if t.Credentials.PasswordEnv == "" {
		t.Credentials.PasswordEnv = def.Credentials.PasswordEnv
	}
	if t.Credentials.PrivateKeyEnv == "" {
		t.Credentials.PrivateKeyEnv = def.Credentials.PrivateKeyEnv
	}
	if t.WinRM.UseHTTPS == nil {
		t.WinRM.UseHTTPS = def.WinRM.UseHTTPS
	}
	if !t.WinRM.InsecureSkipVerify {
		t.WinRM.InsecureSkipVerify = def.WinRM.InsecureSkipVerify
	}
}
