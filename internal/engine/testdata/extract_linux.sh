set -e
rm -rf '/opt/deploy/svc/releases/1.2.3'
mkdir -p '/opt/deploy/svc/releases/1.2.3'
unzip -o -q '/opt/deploy/svc/staging/pkg.zip' -d '/opt/deploy/svc/releases/1.2.3'
