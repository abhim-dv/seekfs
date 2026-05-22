# Contributing

## Development

Use Go on Windows for the primary development path:

```powershell
go test ./...
go vet ./...
go build -o seekfs.exe ./cmd/seekfs
powershell -ExecutionPolicy Bypass -File .\test_seekfs_cli.ps1
```

Do not commit generated indexes, sidecars, benchmark outputs, logs, extracted
third-party binaries, or built executables.

## Pull Requests

Keep changes focused. Include:

- A short explanation of the behavior change.
- Test or benchmark output for search, indexing, or service changes.
- Notes about Windows privilege requirements if the change touches service or
  USN code.

## Security

Do not include private paths, local index contents, or machine-specific service
credentials in issues or pull requests.
