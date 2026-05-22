# seekfs prototype notes

Binary:
`seekfs.exe`

Source:
`cmd/seekfs/main.go`

## Current Commands

Build an index:

```powershell
.\seekfs.exe index -db .\seekfs_docs_downloads.gsi -root $env:USERPROFILE\Documents -root $env:USERPROFILE\Downloads
```

Search names:

```powershell
.\seekfs.exe search -db .\seekfs_docs_downloads.gsi -n 100 query
```

Search full paths:

```powershell
.\seekfs.exe search -db .\seekfs_docs_downloads.gsi -path -n 100 query
```

Count name matches:

```powershell
.\seekfs.exe count -db .\seekfs_docs_downloads.gsi query
```

Run a resident query server:

```powershell
.\seekfs.exe serve -db .\seekfs_docs_downloads.gsi -addr 127.0.0.1:47832
```

Query through the resident server:

```powershell
.\seekfs.exe search -addr 127.0.0.1:47832 -n 100 query
.\seekfs.exe count -addr 127.0.0.1:47832 query
```

Build an NTFS USN/MFT index:

```powershell
.\seekfs.exe index-usn -volume C: -db .\seekfs_c_usn.gsi
```

Install and use the privileged service path from an elevated PowerShell:

```powershell
.\seekfs.exe install-service -pipe \\.\pipe\seekfs-service
Start-Service seekfs
.\seekfs.exe service-status
.\seekfs.exe service-index-usn -volume C: -db .\seekfs_c_usn.gsi
Stop-Service seekfs
.\seekfs.exe uninstall-service
```

For development, the same service handler can run in the foreground:

```powershell
.\seekfs.exe service -pipe \\.\pipe\seekfs-service
```

The privileged service IPC now uses a Windows named pipe, not TCP. The default
pipe name is:

```text
\\.\pipe\seekfs-service
```

The default pipe security descriptor is:

```text
D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;IU)
```

That grants full access to LocalSystem and Administrators and read/write access
to interactive users.

This command uses Windows volume control calls and must run elevated or from a
service helper with raw volume access. In the current non-elevated session,
Windows rejected USN access:

```text
query USN journal for C:: Incorrect function.; run elevated or use a service helper for raw volume access
```

`fsutil fsinfo volumeinfo C:` also returned `Access is denied`, so this is an
environment permission boundary, not a normal CLI parsing failure.

The service protocol was smoke-tested non-elevated with a unique test pipe:

```powershell
.\seekfs.exe service -pipe \\.\pipe\seekfs-test-...
.\seekfs.exe service-status -pipe \\.\pipe\seekfs-test-...
.\seekfs.exe service-monitor-start -pipe \\.\pipe\seekfs-test-... -volume C:
.\seekfs.exe service-monitor-stop -pipe \\.\pipe\seekfs-test-... -volume C:
.\seekfs.exe service-index-usn -pipe \\.\pipe\seekfs-test-... -volume C: -db .\seekfs_c_usn.gsi
```

The client reached the service over the named pipe. Status and monitor
start/stop commands worked. `service-index-usn` reached the service and received
the same USN permission failure from the service process, which confirms the IPC
path works. A real USN benchmark requires launching the service elevated.

Current service commands:

```powershell
.\seekfs.exe service-status
.\seekfs.exe service-monitor-start -volume C:
.\seekfs.exe service-monitor-stop -volume C:
.\seekfs.exe service-index-usn -volume C: -db .\seekfs_c_usn.gsi
```

Monitor start/stop now launches/stops a USN worker per volume. Each worker:

- opens the raw volume
- queries `FSCTL_QUERY_USN_JOURNAL`
- starts at the current `NextUsn`
- loops on `FSCTL_READ_USN_JOURNAL`
- tracks last USN, event count, and last error for `service-status`

In the current unelevated shell, the worker reports:

```text
service running; monitors: C:(error: Incorrect function.)
```

That is expected until the service is actually launched elevated.

## Current Index Result

Scope:

- `%USERPROFILE%\Documents`
- `%USERPROFILE%\Downloads`

Result:

- `18,853` entries
- build time: `2.755s`
- database size: `6,087,634` bytes

This is currently a normal directory walk, not USN/MFT-based indexing.

## Binary Format Benchmark

Command:

```powershell
python .\bench_everything.py --tool-kind seekfs --tool .\seekfs.exe --db .\seekfs_docs_downloads.gsi --roots $env:USERPROFILE\Documents $env:USERPROFILE\Downloads --queries 25 --iterations 2 --max-results 100 --max-walk 3000 --out-prefix seekfs_bench_binary
```

Outputs:

- `seekfs_bench_binary.csv`
- `seekfs_bench_binary.summary.json`

Summary:

| Mode | Runs | Median | P90 | P95 | Max | Mean results |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| name | 50 | 78.04 ms | 938.32 ms | 1126.27 ms | 2288.56 ms | 7.32 |
| path | 50 | 77.64 ms | 107.62 ms | 1267.37 ms | 3199.34 ms | 25.68 |
| count | 50 | 77.60 ms | 1115.60 ms | 1218.23 ms | 2186.17 ms | 1 |

## Resident Server Benchmark

CLI-through-server command:

```powershell
python .\bench_everything.py --tool-kind seekfs --tool .\seekfs.exe --db .\seekfs_docs_downloads.gsi --addr 127.0.0.1:47832 --roots $env:USERPROFILE\Documents $env:USERPROFILE\Downloads --queries 25 --iterations 2 --max-results 100 --max-walk 3000 --out-prefix seekfs_bench_server
```

Summary, still including one `seekfs.exe` client process launch per query:

| Mode | Runs | Median | P90 | P95 | Max | Mean results |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| name | 50 | 72.84 ms | 1198.75 ms | 1229.93 ms | 1266.85 ms | 7.32 |
| path | 50 | 73.87 ms | 1162.89 ms | 1272.53 ms | 3112.81 ms | 25.68 |
| count | 50 | 78.09 ms | 1194.25 ms | 2041.57 ms | 2426.67 ms | 1 |

Direct TCP-to-server timing, excluding process startup:

| Mode | Runs | Median | P90 | P95 | Max |
| --- | ---: | ---: | ---: | ---: | ---: |
| name | 50 | 12.03 ms | 19.10 ms | 26.67 ms | 27.06 ms |
| path | 50 | 2.23 ms | 16.94 ms | 22.87 ms | 26.88 ms |
| count | 50 | 1.06 ms | 16.20 ms | 25.00 ms | 25.98 ms |

This confirms the search path itself is fast for the scoped index; the CLI
benchmark is now dominated by Windows process startup and client setup.

## Fresh Everything Rerun

Command:

```powershell
python .\bench_everything.py --tool-kind es --es .\extracted\es.exe --instance 1.5a --roots $env:USERPROFILE\Documents $env:USERPROFILE\Downloads --queries 25 --iterations 2 --max-results 100 --max-walk 3000 --out-prefix everything_bench_rerun
```

Summary:

| Mode | Runs | Median | P90 | P95 | Max | Mean results |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| name | 50 | 180.30 ms | 1236.02 ms | 1354.61 ms | 1516.28 ms | 39.16 |
| path | 50 | 303.87 ms | 450.76 ms | 1422.00 ms | 2473.09 ms | 44.48 |
| count | 50 | 178.86 ms | 1128.92 ms | 1202.86 ms | 1304.03 ms | 1 |

## Previous Gob Benchmark

Command:

```powershell
python .\bench_everything.py --tool-kind seekfs --tool .\seekfs.exe --db .\seekfs_docs_downloads.gob --roots $env:USERPROFILE\Documents $env:USERPROFILE\Downloads --queries 25 --iterations 2 --max-results 100 --max-walk 3000 --out-prefix seekfs_bench_initial
```

Outputs:

- `seekfs_bench_initial.csv`
- `seekfs_bench_initial.summary.json`

Summary:

| Mode | Runs | Median | P90 | P95 | Max | Mean results |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| name | 50 | 109.12 ms | 1168.35 ms | 1301.28 ms | 1426.60 ms | 7.32 |
| path | 50 | 96.37 ms | 947.25 ms | 1410.71 ms | 2240.82 ms | 25.68 |
| count | 50 | 91.95 ms | 1242.11 ms | 1520.98 ms | 2537.01 ms | 1 |

The median already meets the initial target for this indexed scope. Tail latency
is still high because every query launches a process and decodes the full gob
index before scanning.

## Next Implementation Work

1. Run the service from an elevated shell and benchmark `service-index-usn`.
2. Persist journal checkpoint metadata and detect journal ID changes.
3. Apply USN monitor events to the live/persisted index.
4. Add a memory-mapped or zero-copy load path for the binary index.
5. Add incremental refresh based on filesystem timestamps and/or
   `ReadDirectoryChangesW`.
6. Add result agreement checks against Everything for shared indexed roots.
