# shellcheck shell=bash
# Sourced by the cross-runtime benches (bench-build, bench-external, bench-perf, bench-run).

# docker_cli prints the path of Docker's own CLI, the one that takes --context, and fails
# when the PATH has none. Never `command -v docker`: hermetic's bin comes first on the PATH
# and links `docker` to ossein's docker-shaped front, which answers to the name, knows no
# context, and would have every Docker daemon reported unreachable.
docker_cli() {
  local candidate
  while IFS= read -r candidate; do
    if "$candidate" --version 2>/dev/null | grep -q '^Docker version '; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done < <(type -ap docker)
  return 1
}
