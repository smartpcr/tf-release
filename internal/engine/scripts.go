package engine

import (
	"fmt"
	"strings"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// shq single-quotes s for POSIX sh, escaping any embedded single quote so an
// UNVALIDATED install_root-derived path cannot break out of the quoted argument
// (`install_root` is not constrained against quotes at the spec layer). This is
// the linux analogue of psq for PowerShell. See TestScriptHostilePathQuoting.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// extractScript renders the staging→release extraction script for p.OS
// (DESIGN §9.1). Windows uses `Expand-Archive -Force`; linux uses `unzip -o`.
// The staged package is always named `pkg.zip` (a nupkg is renamed to .zip at
// fetch time, DESIGN D3), so extraction is a plain unzip-in-place that wipes
// any pre-existing release dir first. The staging directory itself is wiped as
// a whole by wipeStaging before/after every op, so this script does not touch
// pkg.zip. All interpolated paths are shell-quoted. Deterministic for golden
// testing.
func extractScript(p layout.Paths) string {
	if p.OS == spec.OSWindows {
		return fmt.Sprintf(`$ErrorActionPreference='Stop'
if(Test-Path %s){ Remove-Item -Recurse -Force %s }
New-Item -ItemType Directory -Force -Path %s | Out-Null
try { Expand-Archive -Path %s -DestinationPath %s -Force }
catch { Write-Error $_.Exception.Message; exit 1 }
exit 0
`, psq(p.Release), psq(p.Release), psq(p.Release), psq(p.StagePkg), psq(p.Release))
	}
	return fmt.Sprintf(`set -e
rm -rf %s
mkdir -p %s
unzip -o -q %s -d %s
`, shq(p.Release), shq(p.Release), shq(p.StagePkg), shq(p.Release))
}

// switchScript renders the `current` junction/symlink repoint for p.OS
// (DESIGN §9.1, S3). Windows removes the existing junction with `rmdir` (which
// deletes only the link, never the target) then recreates it with `mklink /J`;
// any failure of either cmd.exe step exits 42. Linux uses an atomic
// `ln -sfn` replace; failure exits 42. Exit 42 ⇒ ERR_SWITCH. Deterministic for
// golden testing.
func switchScript(p layout.Paths) string {
	if p.OS == spec.OSWindows {
		return fmt.Sprintf(`if(Test-Path %s){ & cmd /c rmdir %s; if($LASTEXITCODE -ne 0){ Write-Error 'rmdir current failed'; exit 42 } }
& cmd /c mklink /J %s %s | Out-Null
if($LASTEXITCODE -ne 0){ Write-Error 'mklink failed'; exit 42 }
exit 0
`, psq(p.Current), quoteCmd(p.Current), quoteCmd(p.Current), quoteCmd(p.Release))
	}
	return fmt.Sprintf("ln -sfn %s %s || exit 42\n", shq(p.Release), shq(p.Current))
}
