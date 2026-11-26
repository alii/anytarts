#!/bin/sh
# Installs the latest anytarts into ~/.local/bin.
set -eu
case "$(uname -s)-$(uname -m)" in
Darwin-arm64) file=anytarts-darwin-arm64 ;;
Darwin-x86_64) file=anytarts-darwin-amd64 ;;
*) echo "anytarts only runs on macOS" >&2; exit 1 ;;
esac
url=${ANYTARTS_URL:-https://github.com/alii/anytarts/releases/latest/download}
bin=$HOME/.local/bin
mkdir -p "$bin"
tmp=$(mktemp -d)
curl -fsSL -o "$tmp/$file" "$url/$file"
curl -fsSL -o "$tmp/checksums.txt" "$url/checksums.txt"
(cd "$tmp" && grep " $file\$" checksums.txt | shasum -a 256 -c - >/dev/null)
chmod 755 "$tmp/$file"
mv "$tmp/$file" "$bin/anytarts"
echo "Installed $bin/anytarts"
case ":$PATH:" in
*":$bin:"*) ;;
*) echo "Run this so your shell can find it, then open a new window:"
   echo "  echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.zshrc" ;;
esac
