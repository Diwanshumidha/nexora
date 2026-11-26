#!/bin/sh
# Sets the app version everywhere it is recorded: the Go constant the
# update check compares against, and the packaging metadata of each OS.
#   scripts/set-version.sh 0.2.0      (a leading "v" is dropped)
set -eu
v="${1#v}"
case "$v" in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "usage: $0 <major.minor.patch>" >&2; exit 2 ;;
esac
cd "$(dirname "$0")/.."

sed -i.bak "s/^const Version = \".*\"/const Version = \"$v\"/" internal/version/version.go
sed -i.bak "s/^\(  version: \)\"[^\"]*\"/\1\"$v\"/" build/config.yml
sed -i.bak "s/^version: \".*\"/version: \"$v\"/" build/linux/nfpm/nfpm.yaml
# Plain BRE without \| so it works with BSD sed (macOS) too.
sed -i.bak -e "s/\"file_version\": \"[^\"]*\"/\"file_version\": \"$v\"/" \
  -e "s/\"product_version\": \"[^\"]*\"/\"product_version\": \"$v\"/" build/windows/info.json
sed -i.bak "s/^!define INFO_PRODUCTVERSION \".*\"/!define INFO_PRODUCTVERSION \"$v\"/" build/windows/nsis/project.nsi
# Only Nexora's own identity: the manifest also names a system DLL version.
sed -i.bak "s/\(assemblyIdentity type=\"win32\" name=\"io\.codedecoders\.nexora\" version=\"\)[^\"]*\"/\1$v.0\"/" build/windows/wails.exe.manifest
for plist in build/darwin/Info.plist build/darwin/Info.dev.plist; do
  # The <string> right after each version key.
  sed -i.bak "/<key>CFBundle\(Short\)\{0,1\}Version\(String\)\{0,1\}<\/key>/{n;s/<string>[^<]*<\/string>/<string>$v<\/string>/;}" "$plist"
done
find internal/version build -name '*.bak' -delete

grep -q "\"$v\"" internal/version/version.go || { echo "failed to set the version" >&2; exit 1; }
echo "version set to $v"
