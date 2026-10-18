#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
image="${HC_COMPAT_IMAGE:-healthchecks/healthchecks:v4.4@sha256:770e49ba5482153eb3f90391d86c6a6c64f4b58ac82ff88ac1b54d5b4f23f438}"
suffix="$$"
container="hc-compat-upstream-${suffix}"
candidate_args=()

if [[ -n "${CANDIDATE_URL:-}" ]]; then
  candidate_args+=(--candidate-url "$CANDIDATE_URL")
fi
if [[ -n "${COMPAT_REPORT:-}" ]]; then
  candidate_args+=(--report "$COMPAT_REPORT")
fi

cleanup() {
  docker rm --force "$container" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

docker image inspect "$image" >/dev/null 2>&1 || docker pull "$image"
docker run --rm \
  --network none \
  --volume "$script_dir:/compat:ro" \
  --entrypoint python \
  "$image" \
  /compat/check_cron_vectors.py
port="$(python3 - <<'PY'
import socket

with socket.socket() as sock:
    sock.bind(("127.0.0.1", 0))
    print(sock.getsockname()[1])
PY
)"
docker run --detach \
  --name "$container" \
  --publish "127.0.0.1:$port:8000/tcp" \
  --env DEBUG=False \
  --env SECRET_KEY=compatibility-oracle-only \
  --env ALLOWED_HOSTS=127.0.0.1,localhost \
  --env DB_NAME=/tmp/healthchecks-compat.sqlite \
  --entrypoint sh \
  "$image" \
  -c './manage.py migrate && exec ./manage.py runserver 0.0.0.0:8000' >/dev/null
if ! docker port "$container" 8000/tcp >/dev/null; then
  docker inspect --format 'port-bindings={{json .HostConfig.PortBindings}} published={{json .NetworkSettings.Ports}}' "$container" >&2
  docker logs "$container" >&2
  exit 1
fi

python3 - "$port" <<'PY'
import time
import sys
import urllib.error
import urllib.request

port = int(sys.argv[1])
deadline = time.monotonic() + 90
while time.monotonic() < deadline:
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/", timeout=1):
            break
    except urllib.error.HTTPError as error:
        if error.code < 500:
            break
    except OSError:
        time.sleep(0.25)
else:
    raise SystemExit("upstream Healthchecks v4.4 did not start before timeout")
PY

if ! docker exec "$container" ./manage.py shell -c '
import uuid
from django.contrib.auth.models import User
from hc.accounts.models import Project
from hc.api.models import Check

user, _ = User.objects.get_or_create(username="compatibility-runner")
project, _ = Project.objects.get_or_create(
    owner=user,
    name="Compatibility oracle",
    defaults={"badge_key": uuid.uuid4()},
)
for code, name in [
    ("11111111-1111-4111-8111-111111111111", "API matrix"),
    ("22222222-2222-4222-8222-222222222222", "State semantics"),
]:
    Check.objects.get_or_create(
        code=code,
        defaults={"project": project, "name": name},
    )
print("Seeded two disposable checks")
' >/dev/null; then
  docker logs "$container" >&2
  exit 1
fi

printf 'Healthchecks image: %s\n' "$image"
printf 'Oracle URL: http://127.0.0.1:%s\n' "$port"
if [[ -n "${CANDIDATE_URL:-}" ]]; then
  printf 'Candidate URL: %s\n' "$CANDIDATE_URL"
fi

python3 "$script_dir/compare.py" \
  --base-url "http://127.0.0.1:$port" \
  --container "$container" \
  "${candidate_args[@]}"
