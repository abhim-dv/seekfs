# Service Pipe Protocol

The service listens on a Windows named pipe. The default pipe is:

```text
\\.\pipe\seekfs-service
```

Requests and responses are newline-delimited JSON objects encoded as UTF-8.

## Search Request

```json
{
  "command": "search",
  "query": "linkmerge w6 full e10",
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
- `monitor-start`
- `monitor-stop`
- `status`

The protocol is not yet frozen. Public automation should prefer the CLI until a
stable protocol version field is added.
