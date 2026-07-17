$ProgressPreference='SilentlyContinue'
$ErrorActionPreference='Stop'
$h=@{}; if($env:LD_AUTH_VALUE){$h['Authorization']=$env:LD_AUTH_VALUE}
try { Invoke-WebRequest -UseBasicParsing -Uri 'https://pkgs.example.com/v3/flatcontainer/mycompany.app/1.2.3/mycompany.app.1.2.3.nupkg' -Headers $h -OutFile 'C:\staging\pkg.zip' }
catch { Write-Error $_.Exception.Message; exit 40 }
$sha=(Get-FileHash 'C:\staging\pkg.zip' -Algorithm SHA256).Hash.ToLower()
if($sha -ne 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'){ Write-Error "checksum mismatch: $sha"; exit 41 }
exit 0