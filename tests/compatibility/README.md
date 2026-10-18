# Upstream compatibility oracle

This opt-in harness compares the UUID ping routes with the official Healthchecks
v4.4 image. It sends only local requests. The upstream listens on a randomly
selected port published only on `127.0.0.1` and stores its SQLite database
inside a disposable container. The harness runs the web process without
Healthchecks' notification daemons and configures no notification channels,
SMTP credentials, or webhook. The container is removed when the command exits.
The candidate URL, when supplied, must also be an HTTP loopback URL.

Run only the upstream oracle:

```sh
bash tests/compatibility/run-upstream.sh
```

With the candidate already listening locally on the check UUID in
`api_cases.json`:

```sh
CANDIDATE_URL=http://127.0.0.1:8000 bash tests/compatibility/run-upstream.sh
```

Set `COMPAT_REPORT=/tmp/healthchecks-compatibility.json` to save a JSON artifact
with the HTTP observations and state assertion result. Its parent directory
must already exist. Cron vector results are printed separately before the HTTP probes.

The harness checks GET, HEAD, and POST for success, start, fail, log, and exit
statuses 0, 1, 255, and 256. It also covers valid, invalid, and mismatched run
IDs; trailing-slash forms; malformed paths; and an
unknown UUID. Response bodies are checked where they form part of the stable
ping response. The upstream database is queried directly only inside its
disposable container to check start, success, failure, log, and run-ID state
transitions. The candidate is contacted only through its ping HTTP routes; no
candidate admin API is assumed. The required method subset is GET, HEAD, and
POST. Upstream v4.4's ping view is more permissive about HTTP methods, while
the candidate intentionally returns 405 for methods outside this subset.

The command also runs shared CronSim vectors in a separate container with networking disabled.
These cover fixed schedules expecting only the first occurrence of London's repeated autumn hour,
minute wildcards through its spring gap and repeated autumn hour, plus Sunday `7`.
The candidate normalizes Sunday `7` to `0` before using robfig/cron v3.

DST compatibility is deliberately limited. For fixed minute/hour schedules, the candidate filters
out the second occurrence of a repeated local time, matching CronSim's first-occurrence policy.
If the first occurrence is already past, it advances to the next scheduled occurrence instead
of returning a past deadline. The engine tests cover a single success followed by no second ping,
including a success received during the repeated hour. Wildcards retain robfig/cron's behavior.

All other calendar calculations use robfig/cron directly. Unlike CronSim's fixed-time behavior,
nonexistent spring-forward times are skipped rather than moved to the end of the gap.
Half-hour offset transitions can skip valid occurrences. These accepted library behaviors are
covered separately by `TestCronLibraryDSTBehavior`, not by the shared upstream vectors.

The candidate intentionally supports only `*` and a single number in each field,
plus `*/N` in minute/hour fields. It rejects lists, ranges, names, extensions,
simultaneous day-of-month/day-of-week restrictions, and schedules without an
occurrence in every calendar year. CronSim features outside this whitelist are
not compatibility targets. The candidate's config tests cover these rejections.

The harness image is pinned to the official amd64 v4.4 image manifest
`healthchecks/healthchecks:v4.4@sha256:770e49ba5482153eb3f90391d86c6a6c64f4b58ac82ff88ac1b54d5b4f23f438`.
The upstream `v4.4` source tag resolves to commit
`49924faadf0f24a846abc3acf3014b396fdc1f29`; its
[`requirements.txt`](https://github.com/healthchecks/healthchecks/blob/v4.4/requirements.txt)
pins `cronsim==2.7`. The installed version is checked inside the image before
validating [cron_vectors.json](cron_vectors.json). The relevant upstream code is
in [`hc/api/urls.py`](https://github.com/healthchecks/healthchecks/blob/v4.4/hc/api/urls.py),
[`hc/api/views.py`](https://github.com/healthchecks/healthchecks/blob/v4.4/hc/api/views.py),
and [`hc/api/models.py`](https://github.com/healthchecks/healthchecks/blob/v4.4/hc/api/models.py).

To run only the cron vectors:

```sh
docker run --rm --network none \
  --volume "$PWD/tests/compatibility:/compat:ro" \
  --entrypoint python \
  healthchecks/healthchecks:v4.4@sha256:770e49ba5482153eb3f90391d86c6a6c64f4b58ac82ff88ac1b54d5b4f23f438 \
  /compat/check_cron_vectors.py
```

The shared vectors record CronSim 2.7 behavior in `Europe/London` and `UTC`:
fixed schedules expect only the first autumn occurrence;
minute wildcards skip the spring gap and traverse the repeated autumn hour;
weekday `7` denotes Sunday.

Known intentional scope differences: this new service starts configured checks
as `awaiting`, while upstream v4.4 reports a never-pinged check as `new`;
paused state is declarative here, whereas upstream also has user-controlled
pause and manual-resume behavior. This harness checks ping transport and the
upstream's event state, not Healthchecks account or management APIs.
