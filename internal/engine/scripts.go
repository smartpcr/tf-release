package engine

import (
	"fmt"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// extractScript renders the staging→release extraction script for p.OS
// (DESIGN §9.1). Windows uses `Expand-Archive -Force`; linux uses `unzip -o`.
// The staged package is always named `pkg.zip` (a nupkg is renamed to .zip at
// fetch time, DESIGN D3), so extraction is a plain unzip-in-place that wipes
// any pre-existing release dir first. Output is deterministic so it can be
// golden-tested.
func extractScript(p layout.Paths) string {
	if p.OS == spec.OSWindows {
		return fmt.Sprintf(`$ErrorActionPreference='Stop'
if(Test-Path %s){ Remove-Item -Recurse -Force %s }
New-Item -ItemType Directory -Force -Path %s | Out-Null
try { Expand-Archive -Path %s -DestinationPath %s -Force }
catch { Write-Error $_.Exception.Message; exit 1 }
Remove-Item -Force -ErrorAction SilentlyContinue %s
exit 0
`, psq(p.Release), psq(p.Release), psq(p.Release), psq(p.StagePkg), psq(p.Release), psq(p.StagePkg))
	}
	return fmt.Sprintf(`set -e
rm -rf '%s'
mkdir -p '%s'
unzip -o -q '%s' -d '%s'
rm -f '%s'
`, p.Release, p.Release, p.StagePkg, p.Release, p.StagePkg)
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
	return fmt.Sprintf("ln -sfn '%s' '%s' || exit 42\n", p.Release, p.Current)
}
