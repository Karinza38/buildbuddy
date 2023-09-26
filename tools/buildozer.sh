#!/usr/bin/env bash
set -e

# TODO: provision buildozer with bazel
if ! command -v buildozer >/dev/null; then
  echo >&2 "Missing 'buildozer' in \$PATH. Install with:"
  echo >&2 "    go install github.com/bazelbuild/buildtools/buildozer@latest"
  exit 1
fi
exec buildozer "$@"
