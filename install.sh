#!/bin/sh

set -eu

repository="eyvant/spm"
requested_version="latest"
install_dir="${SPM_INSTALL_DIR:-${HOME}/.local/bin}"
completion_shell="${SPM_COMPLETION:-auto}"
uninstall=false
force=false

usage() {
  cat <<'EOF'
Install or uninstall spm.

Usage:
  install.sh [--version VERSION] [--install-dir DIRECTORY]
             [--completion auto|bash|zsh|fish|none]
  install.sh --uninstall [--install-dir DIRECTORY] [--force]

Options:
  --version VERSION       Install a release such as v0.1.0 or 0.1.0.
  --install-dir DIRECTORY Install to DIRECTORY instead of ~/.local/bin.
  --completion SHELL      Install completion for SHELL (default: auto).
  --no-completion         Do not install shell completion.
  --uninstall             Remove spm and all installed shell completions.
  --force                 Uninstall even when managed user services exist.
  -h, --help              Show this help.

Environment:
  SPM_INSTALL_DIR         Default installation directory.
  SPM_COMPLETION          Default completion mode: auto, bash, zsh, fish, or none.
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
    --completion)
      [ "$#" -ge 2 ] || fail "--completion requires a value"
      completion_shell=$2
      shift 2
      ;;
    --no-completion)
      completion_shell="none"
      shift
      ;;
    --uninstall)
      uninstall=true
      shift
      ;;
    --force)
      force=true
      shift
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

if [ "$force" = true ] && [ "$uninstall" != true ]; then
  fail "--force is only valid with --uninstall"
fi

directory_has_entries() {
  [ -d "$1" ] || return 1
  for entry in "$1"/* "$1"/.[!.]* "$1"/..?*; do
    if [ -e "$entry" ] || [ -L "$entry" ]; then
      return 0
    fi
  done
  return 1
}

managed_services_exist() {
  case "$(uname -s)" in
    Linux)
      config_home="${XDG_CONFIG_HOME:-${HOME}/.config}"
      if directory_has_entries "${config_home}/spm/services" || directory_has_entries "/etc/spm/services"; then
        return 0
      fi
      for unit in "${config_home}/systemd/user"/spm-*.service "${config_home}/systemd/user"/spm-*.timer; do
        if [ -e "$unit" ] || [ -L "$unit" ]; then
          return 0
        fi
      done
      for unit in /etc/systemd/system/spm-*.service /etc/systemd/system/spm-*.timer; do
        if [ -e "$unit" ] || [ -L "$unit" ]; then
          return 0
        fi
      done
      ;;
    Darwin)
      if directory_has_entries "${HOME}/Library/Application Support/spm/services" || directory_has_entries "/Library/Application Support/spm/services"; then
        return 0
      fi
      for plist in "${HOME}/Library/LaunchAgents"/com.eyvant.spm.*.plist; do
        if [ -e "$plist" ] || [ -L "$plist" ]; then
          return 0
        fi
      done
      for plist in /Library/LaunchDaemons/com.eyvant.spm.*.plist; do
        if [ -e "$plist" ] || [ -L "$plist" ]; then
          return 0
        fi
      done
      ;;
  esac
  return 1
}

remove_installed_file() {
  if [ -e "$1" ] || [ -L "$1" ]; then
    rm -f -- "$1"
    printf 'Removed %s\n' "$1"
    removed=true
  fi
}

if [ "$uninstall" = true ]; then
  if managed_services_exist && [ "$force" != true ]; then
    fail "managed services still exist; run 'spm delete all --yes' and 'sudo spm --system delete all --yes' as applicable, or pass --force to remove the CLI without deleting services"
  fi

  removed=false
  remove_installed_file "${install_dir}/spm"
  remove_installed_file "${XDG_DATA_HOME:-${HOME}/.local/share}/bash-completion/completions/spm.bash"
  remove_installed_file "${HOME}/.zfunc/_spm"
  remove_installed_file "${XDG_CONFIG_HOME:-${HOME}/.config}/fish/completions/spm.fish"
  rmdir "$install_dir" 2>/dev/null || true

  if [ "$removed" = true ]; then
    printf 'Uninstalled spm. Managed service data was not removed.\n'
  else
    printf 'spm is not installed in %s.\n' "$install_dir"
  fi
  exit 0
fi

case "$completion_shell" in
  auto|bash|zsh|fish|none) ;;
  *) fail "unsupported completion shell: $completion_shell; use auto, bash, zsh, fish, or none" ;;
esac

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v tar >/dev/null 2>&1 || fail "tar is required"
command -v install >/dev/null 2>&1 || fail "install is required"

case "$(uname -s)" in
  Linux) platform="linux" ;;
  Darwin) platform="macos" ;;
  *) fail "unsupported operating system: $(uname -s); Windows users should run install.ps1" ;;
esac

case "$(uname -m)" in
  x86_64|amd64)
    architecture="amd64"
    ;;
  aarch64|arm64)
    architecture="arm64"
    ;;
  armv7l|armv7)
    [ "$platform" = "linux" ] || fail "unsupported architecture on macOS: $(uname -m)"
    architecture="armv7"
    ;;
  riscv64)
    [ "$platform" = "linux" ] || fail "unsupported architecture on macOS: $(uname -m)"
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

archive="spm-${version}-${platform}-${architecture}.tar.gz"
download_base="https://github.com/${repository}/releases/download/${version}"
temporary_dir=$(mktemp -d)
trap 'rm -rf "$temporary_dir"' EXIT HUP INT TERM

printf 'Downloading spm %s for %s/%s...\n' "$version" "$platform" "$architecture"
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

if [ "$completion_shell" = "auto" ]; then
  detected_shell=${SHELL:-}
  detected_shell=${detected_shell##*/}
  case "$detected_shell" in
    bash|zsh|fish) completion_shell=$detected_shell ;;
    *)
      completion_shell="none"
      printf 'Shell completion was not installed because $SHELL is not bash, zsh, or fish.\n'
      printf 'Run the installer again with --completion bash|zsh|fish to install it explicitly.\n'
      ;;
  esac
fi

case "$completion_shell" in
  bash)
    completion_dir="${XDG_DATA_HOME:-${HOME}/.local/share}/bash-completion/completions"
    completion_path="${completion_dir}/spm.bash"
    ;;
  zsh)
    completion_dir="${HOME}/.zfunc"
    completion_path="${completion_dir}/_spm"
    ;;
  fish)
    completion_dir="${XDG_CONFIG_HOME:-${HOME}/.config}/fish/completions"
    completion_path="${completion_dir}/spm.fish"
    ;;
esac

if [ "$completion_shell" != "none" ]; then
  completion_file="${temporary_dir}/spm-completion"
  "${install_dir}/spm" completion "$completion_shell" > "$completion_file"
  mkdir -p "$completion_dir"
  install -m 0644 "$completion_file" "$completion_path"
  printf 'Installed %s completion to %s\n' "$completion_shell" "$completion_path"

  if [ "$completion_shell" = "zsh" ]; then
    printf 'Ensure %s is in fpath, then run: autoload -Uz compinit && compinit\n' "$completion_dir"
  fi
fi

case ":${PATH}:" in
  *":${install_dir}:"*) ;;
  *) printf 'Add %s to PATH to run spm without an absolute path.\n' "$install_dir" ;;
esac
