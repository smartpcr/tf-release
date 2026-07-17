foreach($d in @('C:\deploy\sample-svc\releases','C:\deploy\sample-svc\shared','C:\deploy\sample-svc\shared\logs','C:\deploy\sample-svc\staging')){ New-Item -ItemType Directory -Force -Path $d | Out-Null }
exit 0
