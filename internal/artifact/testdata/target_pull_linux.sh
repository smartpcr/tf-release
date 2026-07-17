set -e
curl -fsSL -H "Authorization: $LD_AUTH_VALUE" -o '/opt/staging/pkg.zip' 'https://pkgs.example.com/v3/flatcontainer/mycompany.app/1.2.3/mycompany.app.1.2.3.nupkg' || exit 40
got=$(sha256sum '/opt/staging/pkg.zip' | cut -d' ' -f1)
[ "$got" = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" ] || { echo "checksum mismatch: $got" >&2; exit 41; }
exit 0