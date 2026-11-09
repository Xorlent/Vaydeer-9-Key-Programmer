#!/usr/bin/env bash
# Build the macOS (cgo/IOKit) binary, using a macOS SDK the installed linker can handle.
#
# A Command Line Tools update can ship an SDK whose .tbd stubs use arm64e.x1-*
# architectures the bundled ld cannot parse, which breaks the default go build. Each SDK
# is therefore probe-linked newest-first and the first that links is used. Set SDKROOT to
# pick an SDK directly.
set -euo pipefail
cd "$(dirname "$0")"

sdk_links() {
  local sdk="$1" dir ok=1
  dir="$(mktemp -d "${TMPDIR:-/tmp}/kmsdk.XXXXXX")" || return 1
  printf 'int main(void){return 0;}\n' > "$dir/t.c"
  if xcrun clang -isysroot "$sdk" "$dir/t.c" -o "$dir/t.out" \
    -lSystem -lresolv -framework IOKit -framework CoreFoundation >/dev/null 2>&1; then
    ok=0
  fi
  rm -rf "$dir"
  return $ok
}

if [[ -z "${SDKROOT:-}" ]]; then
  shopt -s nullglob
  candidates=(
    /Library/Developer/CommandLineTools/SDKs/MacOSX[0-9]*.sdk
    /Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX[0-9]*.sdk
  )
  if [[ ${#candidates[@]} -eq 0 ]]; then
    echo "build-mac.sh: no macOS SDK found (install Xcode Command Line Tools)" >&2
    exit 1
  fi

  # Newest-first by version; a bare "27" counts as 27.0.
  ordered=()
  while IFS=$'\t' read -r _key path; do
    ordered+=("$path")
  done < <(
    for sdk in "${candidates[@]}"; do
      name="${sdk##*/}"
      ver="${name#MacOSX}"
      ver="${ver%.sdk}"
      major="${ver%%.*}"
      minor="${ver#*.}"
      minor="${minor%%.*}"
      [[ "$minor" == "$ver" ]] && minor=0
      printf '%d\t%s\n' "$((major * 10000 + minor))" "$sdk"
    done | sort -k1,1nr
  )

  found=""
  for sdk in "${ordered[@]}"; do
    if sdk_links "$sdk"; then
      found="$sdk"
      break
    fi
  done
  if [[ -z "$found" ]]; then
    echo "build-mac.sh: no usable macOS SDK found (try updating Xcode Command Line Tools)" >&2
    exit 1
  fi
  SDKROOT="$found"
fi

echo "building with SDKROOT=$SDKROOT" >&2
export SDKROOT GOOS=darwin CGO_ENABLED=1

case "$(uname -m)" in
  arm64|aarch64) GOARCH=arm64 ;;
  x86_64)        GOARCH=amd64 ;;
  *)             GOARCH="$(go env GOARCH)" ;;
esac
export GOARCH

exec go build -trimpath -ldflags '-s -w' -o Vaydeer9-mac .