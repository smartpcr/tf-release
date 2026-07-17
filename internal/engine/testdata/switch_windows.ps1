if(Test-Path 'C:\deploy\sample-svc\current'){ & cmd /c rmdir "C:\deploy\sample-svc\current"; if($LASTEXITCODE -ne 0){ Write-Error 'rmdir current failed'; exit 42 } }
& cmd /c mklink /J "C:\deploy\sample-svc\current" "C:\deploy\sample-svc\releases\1.2.3" | Out-Null
if($LASTEXITCODE -ne 0){ Write-Error 'mklink failed'; exit 42 }
exit 0
