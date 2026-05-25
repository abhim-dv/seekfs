# Service Pipe Protocol

The service listens on a Windows named pipe. The default pipe is:

```text
\\.\pipe\seekfs-service
```

Requests and responses are newline-delimited JSON objects encoded as UTF-8.

## Search Request

For fast executable or exact filename lookup, leave `match_path` false:

CLI equivalent:

```powershell
.\seekfs.exe search "gh.exe"
```

```json
{
  "command": "search",
  "query": "gh.exe",
  "match_path": false,
  "limit": 20,
  "count_only": false
}
```

Set `match_path` to true only when path context is required.

```json
{
  "command": "search",
  "query": "ext:go dir:cmd main",
  "match_path": true,
  "limit": 20,
  "count_only": false
}
```

## Search Response

```json
{
  "ok": true,
  "count": 2,
  "results": [
    "C:\\path\\file.txt",
    "F:\\path\\other.txt"
  ]
}
```

## Count Response

```json
{
  "ok": true,
  "count": 9
}
```

## Info Response

`info` is what `seekfs loaded --json` uses. It reports the process serving the
pipe plus each loaded database and its incremental state.

```json
{
  "ok": true,
  "pid": 45668,
  "entries": 22499191,
  "dbs": [
    {
      "path": "C:\\ProgramData\\seekfs\\indexes\\seekfs_c.gsi",
      "entries": 8166043,
      "source": "usn",
      "built_at": "2026-05-25T11:33:15.3230812-07:00",
      "volume": "C:",
      "journal_id": 133234659009607417,
      "checkpoint_usn": 903307872656,
      "state": "ready",
      "frn_records": 8166043
    }
  ]
}
```

`state` is `ready` when journal replay is active. It is `stale` when the
service can still answer from the loaded index but could not validate or read
the NTFS journal; `stale_reason` contains the failure.

## Error Response

```json
{
  "ok": false,
  "message": "service has no search indexes loaded"
}
```

## Service Commands

Current service commands:

- `search`
- `index-usn`
- `info`
- `status`

The protocol is not yet frozen. Public automation should prefer the CLI until a
stable protocol version field is added.
