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

// specErr wraps a message in the ERR_SPEC_INVALID contract (DESIGN §12) so that
// parse/substitution failures are observable as ERR_SPEC_INVALID in-process,
// identically to schema-validation failures.
func specErr(format string, a ...interface{}) error {
	return &ValidationError{Msg: fmt.Sprintf(format, a...)}
}

// Substitute resolves ${var:NAME} tokens from vars in a single string.
// `$${var:X}` escapes to a literal `${var:X}` (DESIGN §6.6). Unknown names
// yield an ERR_SPEC_INVALID error. Prefer document-level substitution
// (via ParseDeployment*) so unresolved tokens can name the offending JSON path.
func Substitute(raw string, vars map[string]string) (string, error) {
	return substituteString(raw, vars, "")
}

// substituteString applies ${var:NAME} substitution to one string value. path
// is the JSON path of the value ("" for a bare string); it is included in the
// unresolved-variable error to satisfy the DESIGN §6.6 contract
// (`unresolved variable NAME at <json-path>`).
func substituteString(raw string, vars map[string]string, path string) (string, error) {
	var firstErr error
	out := varToken.ReplaceAllStringFunc(raw, func(m string) string {
		g := varToken.FindStringSubmatch(m)
		if g[1] == "$" { // escaped: $${var:X} -> literal ${var:X}
			return "${var:" + g[2] + "}"
		}
		v, ok := vars[g[2]]
		if !ok {
			if firstErr == nil {
				if path == "" {
					firstErr = specErr("unresolved variable %s", g[2])
				} else {
					firstErr = specErr("unresolved variable %s at %s", g[2], path)
				}
			}
			return m
		}
		return v
	})
	return out, firstErr
}

// substituteDoc walks a decoded YAML/JSON document (map/slice/scalar tree) and
// substitutes ${var:NAME} tokens in every string VALUE, tracking the JSON path
// so unresolved tokens report `unresolved variable NAME at <json-path>`.
// Substituting AFTER parse (not on raw text) guarantees variable contents can
// never alter document structure (DESIGN §6.6).
func substituteDoc(v interface{}, vars map[string]string, path string) (interface{}, error) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			nv, err := substituteDoc(val, vars, joinPath(path, k))
			if err != nil {
				return nil, err
			}
			t[k] = nv
		}
		return t, nil
	case []interface{}:
		for i, val := range t {
			nv, err := substituteDoc(val, vars, fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
			t[i] = nv
		}
		return t, nil
	case string:
		return substituteString(t, vars, path)
	default:
		return v, nil
	}
}

func joinPath(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
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

// ParseDeploymentLenient runs parse → substitute(document) → struct →
// versionOverride → canonical hash. Substitution happens on the DECODED
// document (not raw text) so ${var:*} contents cannot alter structure and
// unresolved tokens name a JSON path (DESIGN §6.6). NO semantic validation —
// callers merging provider default_target validate afterwards (DESIGN §6.2
// merge-then-validate).
func ParseDeploymentLenient(raw string, vars map[string]string, versionOverride string) (*Deployment, string, error) {
	jb, err := toJSON(raw)
	if err != nil {
		return nil, "", specErr("%v", err)
	}
	var doc interface{}
	if err := json.Unmarshal(jb, &doc); err != nil {
		return nil, "", specErr("spec parse: %v", err)
	}
	doc, err = substituteDoc(doc, vars, "")
	if err != nil {
		return nil, "", err
	}
	subBytes, err := json.Marshal(doc)
	if err != nil {
		return nil, "", specErr("spec marshal: %v", err)
	}
	var probe kindProbe
	_ = json.Unmarshal(subBytes, &probe)
	if probe.Kind != "" && probe.Kind != "Deployment" {
		return nil, "", specErr("kind: expected Deployment, got %q", probe.Kind)
	}
	var d Deployment
	dec := json.NewDecoder(strings.NewReader(string(subBytes)))
	dec.DisallowUnknownFields()
	// dec.Decode invokes Pattern.UnmarshalJSON, which performs type-directed
	// decoding of the pattern.type discriminated union into the matching concrete
	// member (DESIGN §6.4). See types.go Pattern.decodeUnion.
	if err := dec.Decode(&d); err != nil {
		return nil, "", specErr("spec decode: %v", err)
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
	jb, err := toJSON(raw)
	if err != nil {
		return nil, "", specErr("%v", err)
	}
	var doc interface{}
	if err := json.Unmarshal(jb, &doc); err != nil {
		return nil, "", specErr("spec parse: %v", err)
	}
	doc, err = substituteDoc(doc, vars, "")
	if err != nil {
		return nil, "", err
	}
	subBytes, err := json.Marshal(doc)
	if err != nil {
		return nil, "", specErr("spec marshal: %v", err)
	}
	var probe kindProbe
	_ = json.Unmarshal(subBytes, &probe)
	if probe.Kind != "" && probe.Kind != "TestRun" {
		return nil, "", specErr("kind: expected TestRun, got %q", probe.Kind)
	}
	var t TestRun
	dec := json.NewDecoder(strings.NewReader(string(subBytes)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return nil, "", specErr("spec decode: %v", err)
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
