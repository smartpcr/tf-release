package pattern

import (
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// nodeRC builds a node_web_app ReleaseCtx the same way engine.releaseCtx does:
// builtin env (with node PORT) merged UNDER the spec environment (spec wins).
func nodeRC(patternPort int, specEnv map[string]string) ReleaseCtx {
	p := layout.NewPaths(spec.OSWindows, `C:\deploy`, "web", "1.0.0")
	s := &spec.Deployment{
		Metadata: spec.Metadata{Name: "web"},
		Target:   spec.Target{OS: spec.OSWindows},
		Artifact: spec.Artifact{Version: "1.0.0"},
		Pattern: spec.Pattern{
			Type:  spec.PatternNodeWebApp,
			Entry: "server.js",
			Port:  patternPort,
		},
		Environment: specEnv,
	}
	env := layout.MergeEnv(layout.BuiltinEnv(s.Metadata.Name, s.Artifact.Version, p, s.Pattern.Port), s.Environment)
	return ReleaseCtx{App: s.Metadata.Name, Version: s.Artifact.Version, P: p, Spec: s, Env: env}
}

// Regression: DESIGN §9.1 mandates spec `environment` wins. wrapped() must NOT
// clobber a spec-provided PORT with pattern.port.
func TestNodeWrappedPortPrecedence(t *testing.T) {
	var n NodeWebApp

	// Spec environment sets PORT=9090; pattern.port=3000. Spec must win end-to-end.
	rc := nodeRC(3000, map[string]string{"PORT": "9090"})
	if got := rc.Env["PORT"]; got != "9090" {
		t.Fatalf("pre-wrap merged PORT: got %q want 9090 (spec must win)", got)
	}
	w := n.wrapped(rc)
	if got := w.Env["PORT"]; got != "9090" {
		t.Errorf("wrapped clobbered spec PORT: got %q want 9090", got)
	}

	// No spec PORT: builtin node port (pattern.port) is used.
	rc2 := nodeRC(3000, nil)
	w2 := n.wrapped(rc2)
	if got := w2.Env["PORT"]; got != "3000" {
		t.Errorf("wrapped PORT fallback: got %q want 3000", got)
	}

	// Other builtins survive the wrap.
	if w2.Env["LD_APP"] != "web" || w2.Env["LD_SHARED_DIR"] == "" {
		t.Errorf("wrapped dropped builtins: %v", w2.Env)
	}
}
