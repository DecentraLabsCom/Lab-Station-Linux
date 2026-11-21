# Lab Station operational logging contract

Windows and Linux use the same contract for the primary Station log.

## File and record format

- Active file: `labstation.log`, UTF-8, newline-delimited JSON (one record per line).
- The `.log` extension is retained on both platforms for the existing operator path.
- Daily archives use `labstation-YYYYMMDD.log.gz`; a previous-format file is kept as `labstation-legacy-YYYYMMDD.log.gz` during migration.
- Every new record has the same fields:
  - `schemaVersion`: integer, currently `1`
  - `timestamp`: UTC timestamp in RFC 3339 format, to whole seconds
  - `level`: `DEBUG`, `INFO`, `WARNING`, or `ERROR`
  - `event`: stable event identifier; the value can be platform-specific
  - `message`: short human-readable description
  - `platform`, `host`, and `profile`
  - `details`: an object for operational context; empty when there is none
- Windows maps the old `server` profile name to `dedicated` in log records. Linux retains its configured profile, including `fmu-only`.

## Detail and frequency

- INFO records describe operations, outcomes, and status changes.
- A status summary is written at startup, when readiness changes, and at most once every five minutes while readiness is unchanged.
- Successful low-level probes and per-heartbeat records are DEBUG. They are omitted by default; set `LABSTATION_LOG_LEVEL=DEBUG` in the service environment for troubleshooting.
- Warnings and errors remain visible. Repeated identical warnings are represented by the first warning and a summary no more than once every five minutes. Errors are not rate-limited.
- Status polling, heartbeat publication, lease watchdog, and FMU health-check intervals are not changed by the logging policy.

## Rotation and retention

- Rotate the active file on the first log write after the local calendar day changes.
- Compress closed files as gzip archives with the same archive naming convention on both platforms. If compression fails, keep the closed `.log` archive plain and retain it under the same 180-day rule.
- Delete archives older than 180 days. Cleanup runs during normal logging; if the Station is powered off, cleanup runs after its next log write.
- On upgrade, an active file that does not use schema version 1 is archived once as a legacy log before the new writer starts. Legacy contents retain their original format until that archive expires.
