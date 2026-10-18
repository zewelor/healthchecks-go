#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."
destination="$(mktemp -d)"
trap 'rm -rf "$destination"' EXIT
docker build --file - --progress=quiet --output "type=local,dest=$destination" . <<'DOCKERFILE'
FROM scratch
COPY . /context
DOCKERFILE
find "$destination/context" -type f -printf '%P\n' | sort
