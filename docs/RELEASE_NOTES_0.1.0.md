# seekfs 0.1.0 Release Notes

`seekfs` is an agent-first indexed file search CLI for local filesystems. This
first release targets Windows and NTFS/USN indexing with a resident service mode
for low-latency repeated searches.

## Highlights

- Service-backed CLI search with JSON output.
- Compact C: + F: indexes measured under the Everything DB size baseline on the
  development machine.
- Agent query filters for common coding workflows.
- Machine-readable service info and benchmark output.

## Install

Download `seekfs-windows-amd64.zip`, extract it, and run:

```powershell
.\seekfs.exe version
```

## Service Setup

```powershell
.\seekfs.exe index-volumes -volume C: -volume F: -launch
```

## Smoke Test

```powershell
.\seekfs.exe loaded --json
.\seekfs.exe search -service --json -path -n 20 "ext:go"
.\seekfs.exe bench -service --json -iterations 100
.\seekfs.exe doctor --json
```

## Signing

The 0.1.0 artifact is unsigned. Windows may show standard warnings for unsigned
executables.
