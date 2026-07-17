$ErrorActionPreference='Stop'
if(Test-Path 'C:\deploy\sample-svc\releases\1.2.3'){ Remove-Item -Recurse -Force 'C:\deploy\sample-svc\releases\1.2.3' }
New-Item -ItemType Directory -Force -Path 'C:\deploy\sample-svc\releases\1.2.3' | Out-Null
try { Expand-Archive -Path 'C:\deploy\sample-svc\staging\pkg.zip' -DestinationPath 'C:\deploy\sample-svc\releases\1.2.3' -Force }
catch { Write-Error $_.Exception.Message; exit 1 }
exit 0
