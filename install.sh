#!/usr/bin/env bash
set -euo pipefail

repo="jasalt/chatgpt-openai-api-adapter"
name="chatgpt-openai-api-adapter"

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "Unsupported operating system: $(uname -s)" >&2; exit 1 ;;
esac

case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

if [[ "$os" == darwin && "$arch" != arm64 ]]; then
  echo "No macOS/$arch release is available" >&2
  exit 1
fi

install_dir="${1:-${INSTALL_DIR:-$HOME/.local/bin}}"
mkdir -p "$install_dir"
if [[ ! -w "$install_dir" ]]; then
  echo "$install_dir is not writable; set INSTALL_DIR to a writable PATH directory" >&2
  exit 1
fi

artifact="$name-$os-$arch"
url="https://github.com/$repo/releases/latest/download/$artifact"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

printf 'Downloading %s...\n' "$url"
curl -fL --retry 3 --retry-delay 1 -o "$tmp_dir/$name" "$url"
install -m 0755 "$tmp_dir/$name" "$install_dir/$name"
printf 'Installed %s\n' "$install_dir/$name"

case ":${PATH:-}:" in
  *":$install_dir:"*) ;;
  *) printf 'Add %s to PATH to run %s.\n' "$install_dir" "$name" ;;
esac
