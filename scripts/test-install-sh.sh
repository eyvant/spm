#!/bin/sh

set -eu

script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd)
repository_dir=$(dirname "$script_dir")
installer="${repository_dir}/install.sh"
temporary_base=${TMPDIR:-/tmp}
temporary_dir=$(mktemp -d "${temporary_base%/}/spm-installer-test.XXXXXX")

cleanup() {
  case "$temporary_dir" in
    "${temporary_base%/}"/spm-installer-test.*)
      rm -rf -- "$temporary_dir"
      ;;
    *)
      printf 'Refusing to remove unexpected test path: %s\n' "$temporary_dir" >&2
      return 1
      ;;
  esac
}
trap cleanup EXIT HUP INT TERM

fail() {
  printf 'installer test: %s\n' "$*" >&2
  exit 1
}

test_home="${temporary_dir}/home"
install_dir="${temporary_dir}/bin"
config_home="${temporary_dir}/config"
data_home="${temporary_dir}/data"
fixture_dir="${temporary_dir}/release"
fake_bin="${temporary_dir}/fake-bin"
release_version="v9.8.7"
export HOME=$test_home
export SPM_INSTALL_DIR=$install_dir
export XDG_CONFIG_HOME=$config_home
export XDG_DATA_HOME=$data_home

case "$(uname -s)" in
  Linux) platform=linux ;;
  Darwin) platform=macos ;;
  *) fail "unsupported test platform: $(uname -s)" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) architecture=amd64 ;;
  aarch64|arm64) architecture=arm64 ;;
  armv7l|armv7) architecture=armv7 ;;
  riscv64) architecture=riscv64 ;;
  *) fail "unsupported test architecture: $(uname -m)" ;;
esac

archive="spm-${release_version}-${platform}-${architecture}.tar.gz"
mkdir -p "$fixture_dir/payload" "$fake_bin"
cat > "${fixture_dir}/payload/spm" <<'EOF'
#!/bin/sh
case "${1:-}" in
  version) printf 'spm test-version\n' ;;
  completion) printf '# test completion for %s\n' "${2:-unknown}" ;;
  *) exit 2 ;;
esac
EOF
chmod 0755 "${fixture_dir}/payload/spm"
tar -czf "${fixture_dir}/${archive}" -C "${fixture_dir}/payload" spm

if command -v sha256sum >/dev/null 2>&1; then
  archive_checksum=$(sha256sum "${fixture_dir}/${archive}" | awk '{ print $1 }')
else
  archive_checksum=$(shasum -a 256 "${fixture_dir}/${archive}" | awk '{ print $1 }')
fi
printf '%s  %s\n' "$archive_checksum" "$archive" > "${fixture_dir}/checksums.txt"

cat > "${fake_bin}/curl" <<'EOF'
#!/bin/sh
set -eu
output=
write_format=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o)
      output=$2
      shift 2
      ;;
    -w)
      write_format=$2
      shift 2
      ;;
    *)
      url=$1
      shift
      ;;
  esac
done
if [ "$output" = /dev/null ] && [ -n "$write_format" ]; then
  printf 'https://github.test/eyvant/spm/releases/tag/%s' "$SPM_TEST_RELEASE_VERSION"
  exit 0
fi
cp "${SPM_TEST_FIXTURE_DIR}/${url##*/}" "$output"
EOF
chmod 0755 "${fake_bin}/curl"
export SPM_TEST_FIXTURE_DIR=$fixture_dir
export SPM_TEST_RELEASE_VERSION=$release_version
PATH="${fake_bin}:$PATH"
export PATH

sh "$installer" --completion bash
[ -x "${install_dir}/spm" ] || fail "installer did not create an executable"
[ "$("${install_dir}/spm" version)" = "spm test-version" ] || fail "installed executable did not run"
grep -q "test completion for bash" "${data_home}/bash-completion/completions/spm.bash" || fail "bash completion was not installed"

sh "$installer" --version 9.8.7 --completion zsh
grep -q "test completion for zsh" "${test_home}/.zfunc/_spm" || fail "zsh completion was not installed"

sh "$installer" --version "$release_version" --completion fish
grep -q "test completion for fish" "${config_home}/fish/completions/spm.fish" || fail "fish completion was not installed"

no_completion_dir="${temporary_dir}/no-completion-bin"
sh "$installer" --version 9.8.7 --install-dir "$no_completion_dir" --no-completion
[ -x "${no_completion_dir}/spm" ] || fail "--install-dir did not install the executable"

bad_install_dir="${temporary_dir}/bad-checksum-bin"
printf '%064d  %s\n' 0 "$archive" > "${fixture_dir}/checksums.txt"
if output=$(sh "$installer" --version 9.8.7 --install-dir "$bad_install_dir" --no-completion 2>&1); then
  fail "installation succeeded with an invalid checksum"
fi
case "$output" in
  *"checksum verification failed"*) ;;
  *) fail "unexpected checksum error: $output" ;;
esac
[ ! -e "${bad_install_dir}/spm" ] || fail "checksum failure installed an executable"

mkdir -p "$install_dir" \
  "${data_home}/bash-completion/completions" \
  "${test_home}/.zfunc" \
  "${config_home}/fish/completions"
: > "${install_dir}/spm"
: > "${data_home}/bash-completion/completions/spm.bash"
: > "${test_home}/.zfunc/_spm"
: > "${config_home}/fish/completions/spm.fish"

sh "$installer" --uninstall

for path in \
  "${install_dir}/spm" \
  "${data_home}/bash-completion/completions/spm.bash" \
  "${test_home}/.zfunc/_spm" \
  "${config_home}/fish/completions/spm.fish"; do
  [ ! -e "$path" ] || fail "uninstall left $path behind"
done

case "$platform" in
  linux) managed_service_dir="${config_home}/spm/services/api" ;;
  macos) managed_service_dir="${test_home}/Library/Application Support/spm/services/api" ;;
esac
mkdir -p "$install_dir" "$managed_service_dir"
: > "${install_dir}/spm"
if output=$(sh "$installer" --uninstall 2>&1); then
  fail "uninstall succeeded while a managed service existed"
fi
case "$output" in
  *"managed services still exist"*) ;;
  *) fail "unexpected managed-service error: $output" ;;
esac
[ -e "${install_dir}/spm" ] || fail "blocked uninstall removed the executable"

sh "$installer" --uninstall --force
[ ! -e "${install_dir}/spm" ] || fail "forced uninstall left the executable behind"

if output=$(sh "$installer" --force 2>&1); then
  fail "--force succeeded without --uninstall"
fi
case "$output" in
  *"--force is only valid with --uninstall"*) ;;
  *) fail "unexpected --force validation error: $output" ;;
esac

printf 'install.sh install and uninstall tests passed\n'
