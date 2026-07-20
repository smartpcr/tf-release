<#
.SYNOPSIS
  Stage 9.2 pipeline-acceptance bootstrap: lay the labdeploy provider into the
  Terraform CLI filesystem mirror on a lab runner (W1/L4) so `terraform init`
  resolves it offline (DESIGN §16.1), then verify W1 reachability.

.DESCRIPTION
  Referenced by docs/stories/release-RELEASE-PROVIDER/e2e-scenarios.md Phase 8
  ("Pre-test bootstrap"). Builds terraform-provider-labdeploy_v<Version> (or reuses
  a supplied binary) and copies it into
  %APPDATA%\terraform.d\plugins\registry.local\smartpcr\labdeploy\<Version>\<os_arch>\,
  then writes a filesystem_mirror block into ~\.terraformrc. Idempotent.

.PARAMETER Version
  Provider version to install into the mirror (default 0.1.0, matching Makefile).

.PARAMETER Binary
  Optional path to a prebuilt provider binary. When omitted, the script runs
  `go build` (CGO_ENABLED=0) from the repository root.
#>
[CmdletBinding()]
param(
    [string]$Version = $(if ($env:LD_PROVIDER_VERSION) { $env:LD_PROVIDER_VERSION } else { '0.1.0' }),
    [string]$Binary  = ''
)

$ErrorActionPreference = 'Stop'

$Hostname  = 'registry.local'
$Namespace = 'smartpcr'
$Name      = 'labdeploy'

# Repo root is three levels up from tests\e2e\pipeline.
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path

$goos   = (& go env GOOS).Trim()
$goarch = (& go env GOARCH).Trim()
$osArch = "${goos}_${goarch}"

$binName = "terraform-provider-${Name}_v${Version}"
if ($goos -eq 'windows') { $binName += '.exe' }

if ([string]::IsNullOrWhiteSpace($Binary)) {
    Write-Host "==> Building $binName (CGO_ENABLED=0) from $repoRoot"
    Push-Location $repoRoot
    try {
        $env:CGO_ENABLED = '0'
        $outDir = Join-Path $repoRoot 'bin'
        New-Item -ItemType Directory -Force -Path $outDir | Out-Null
        $Binary = Join-Path $outDir $binName
        & go build -ldflags "-X main.version=$Version" -o $Binary .
        if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }
    } finally {
        Pop-Location
    }
}

if (-not (Test-Path $Binary)) { throw "provider binary not found: $Binary" }

# %APPDATA%\terraform.d on Windows; ~/.terraform.d elsewhere.
if ($goos -eq 'windows') {
    $pluginBase = Join-Path $env:APPDATA 'terraform.d\plugins'
} else {
    $pluginBase = Join-Path $HOME '.terraform.d/plugins'
}
$mirrorDir = Join-Path $pluginBase "$Hostname\$Namespace\$Name\$Version\$osArch"
New-Item -ItemType Directory -Force -Path $mirrorDir | Out-Null
Copy-Item -Force -Path $Binary -Destination (Join-Path $mirrorDir (Split-Path $Binary -Leaf))
Write-Host "==> Installed provider into mirror: $mirrorDir"

# Point the CLI at the filesystem mirror so no network registry is consulted.
$mirrorForRc = $pluginBase -replace '\\', '/'
$rcPath = Join-Path $HOME '.terraformrc'
$rcBlock = @"
provider_installation {
  filesystem_mirror {
    path    = "$mirrorForRc"
    include = ["$Hostname/*/*"]
  }
  direct {
    exclude = ["$Hostname/*/*"]
  }
}
"@
Set-Content -Path $rcPath -Value $rcBlock -Encoding utf8
Write-Host "==> Wrote filesystem_mirror block to $rcPath"

# Verify W1 reachability when the lab target is configured (reuses verify-w1.ps1
# if present; otherwise a TCP probe of the WinRM/SSH port).
$w1 = $env:LD_W1_HOST
if ($w1) {
    $verify = Join-Path $PSScriptRoot 'verify-w1.ps1'
    if (Test-Path $verify) {
        Write-Host "==> Verifying W1 via verify-w1.ps1"
        & $verify -Host $w1
    } else {
        $port = if ($env:LD_W1_PORT) { [int]$env:LD_W1_PORT } else { 5985 }
        Write-Host "==> Probing W1 ${w1}:${port}"
        $ok = Test-NetConnection -ComputerName $w1 -Port $port -InformationLevel Quiet
        if (-not $ok) { throw "W1 host $w1 not reachable on port $port" }
        Write-Host "==> W1 reachable"
    }
} else {
    Write-Host "==> LD_W1_HOST not set; skipping W1 verification (gate-tier bootstrap)"
}

Write-Host "==> Bootstrap complete."
