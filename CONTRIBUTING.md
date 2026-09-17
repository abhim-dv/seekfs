# Contributing

## Development

Use Go on Windows for the primary development path. `scripts/ci.ps1` is the
source of truth for CI: it runs the same gates the `Windows CI` workflow does.
Run it before pushing, and the race stage for concurrency changes. Local
toolchain versions can still differ from the runner's, so these reduce
surprises rather than guarantee a green run.

```powershell
./scripts/ci.ps1                  # every stage (what CI runs, across its jobs)
./scripts/ci.ps1 -Quick           # same, minus the slower end-to-end CLI test
./scripts/ci.ps1 -Stage analysis  # staticcheck + govulncheck only
./scripts/ci.ps1 -Stage test      # tests only
./scripts/ci.ps1 -Stage race      # the -race subset (needs a C compiler, e.g. mingw)
```

Keep the workflow jobs delegating to that script. A gate that lives in only one
of the two places will eventually stop running in the other.

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
