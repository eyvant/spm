#!/bin/sh

set -eu

repository="eyvant/spm"
requested_version="latest"
install_dir="${SPM_INSTALL_DIR:-${HOME}/.local/bin}"

usage() {
  cat <<'EOF'
Install spm from a GitHub Release.

Usage:
  install.sh [--version VERSION] [--install-dir DIRECTORY]

Options:
  --version VERSION       Install a release such as v0.1.0 or 0.1.0.
  --install-dir DIRECTORY Install to DIRECTORY instead of ~/.local/bin.
  -h, --help              Show this help.

Environment:
  SPM_INSTALL_DIR         Default installation directory.
EOF
}

fail() {
  printf 'spm installer: %s\n' "$*" >&2
  exit 1
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      [ "$#" -ge 2 ] || fail "--version requires a value"
      requested_version=$2
      shift 2
      ;;
    --install-dir)
      [ "$#" -ge 2 ] || fail "--install-dir requires a value"
      install_dir=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown option: $1"
      ;;
  esac
done

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v tar >/dev/null 2>&1 || fail "tar is required"
command -v install >/dev/null 2>&1 || fail "install is required"

[ "$(uname -s)" = "Linux" ] || fail "spm only supports Linux"

case "$(uname -m)" in
  x86_64|amd64)
    architecture="amd64"
    ;;
  aarch64|arm64)
    architecture="arm64"
    ;;
  armv7l|armv7)
    architecture="armv7"
    ;;
  riscv64)
    architecture="riscv64"
    ;;
  *)
    fail "unsupported architecture: $(uname -m)"
    ;;
esac

if [ "$requested_version" = "latest" ]; then
  release_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' \
    "https://github.com/${repository}/releases/latest") || fail "cannot resolve the latest release"
  version=${release_url##*/}
else
  case "$requested_version" in
    v*) version=$requested_version ;;
    *) version="v${requested_version}" ;;
  esac
fi

case "$version" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) fail "invalid release version: $version" ;;
esac

archive="spm-${version}-linux-${architecture}.tar.gz"
download_base="https://github.com/${repository}/releases/download/${version}"
temporary_dir=$(mktemp -d)
trap 'rm -rf "$temporary_dir"' EXIT HUP INT TERM

printf 'Downloading spm %s for linux/%s...\n' "$version" "$architecture"
curl -fL --retry 3 --retry-delay 1 \
  -o "${temporary_dir}/${archive}" "${download_base}/${archive}"
curl -fL --retry 3 --retry-delay 1 \
  -o "${temporary_dir}/checksums.txt" "${download_base}/checksums.txt"

expected_checksum=$(awk -v filename="$archive" '$2 == filename { print $1; exit }' \
  "${temporary_dir}/checksums.txt")
[ -n "$expected_checksum" ] || fail "checksums.txt does not contain $archive"

if command -v sha256sum >/dev/null 2>&1; then
  actual_checksum=$(sha256sum "${temporary_dir}/${archive}" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
  actual_checksum=$(shasum -a 256 "${temporary_dir}/${archive}" | awk '{ print $1 }')
else
  fail "sha256sum or shasum is required"
fi

[ "$actual_checksum" = "$expected_checksum" ] || fail "checksum verification failed for $archive"

tar -xzf "${temporary_dir}/${archive}" -C "$temporary_dir"
[ -f "${temporary_dir}/spm" ] || fail "release archive does not contain spm"

mkdir -p "$install_dir"
install -m 0755 "${temporary_dir}/spm" "${install_dir}/spm"

printf 'Installed spm %s to %s/spm\n' "$version" "$install_dir"
"${install_dir}/spm" version

case ":${PATH}:" in
  *":${install_dir}:"*) ;;
  *) printf 'Add %s to PATH to run spm without an absolute path.\n' "$install_dir" ;;
esac
