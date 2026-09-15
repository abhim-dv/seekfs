# Configuration

`seekfs` can read a small `seekfs.toml` file from the current directory or from:

```text
%AppData%\seekfs\seekfs.toml
```

You can also pass an explicit path:

```powershell
.\seekfs.exe search -config .\seekfs.toml -service "main"
```

Supported keys:

```toml
dbs = ["F:\\seekfs_c.gsi", "F:\\seekfs_f.gsi"]
volumes = ["C:", "F:"]
service_pipe = "\\\\.\\pipe\\seekfs-service"
default_limit = 100
output_format = "json"
```

With `output_format = "json"` and `default_limit` set, agent calls can stay
short:

```powershell
.\seekfs.exe search "gh.exe"
.\seekfs.exe search -path "ext:go dir:cmd main"
```

When no `-db` is supplied, `search` and `count` use the resident service by
default. Pass `-local` to skip the service and load the configured/default DB
from disk.

Single-value aliases are also accepted:

```toml
db = "F:\\seekfs_c.gsi"
volume = "C:"
db_path = "F:\\seekfs_c.gsi"
db_paths = ["F:\\seekfs_c.gsi", "F:\\seekfs_f.gsi"]
```

This parser intentionally supports only the simple string, integer, and string
array forms used by `seekfs`.

## Editing Config

Use `seekfs config` so agents and users do not need to locate the file manually:

```powershell
.\seekfs.exe config path
.\seekfs.exe config show
.\seekfs.exe config set output_format json
.\seekfs.exe config set default_limit 20
.\seekfs.exe config set dbs = '["F:\\seekfs_c.gsi", "F:\\seekfs_f.gsi"]'
.\seekfs.exe config get dbs
```

## Build tuning (name trigrams)

Index builds are bounded and parallel by default. These environment variables
exist for low-memory or unusual environments; the defaults are fine otherwise.

| Variable | Meaning | Default |
| --- | --- | --- |
| `SEEKFS_NAME_GRAM_EXTERNAL` | `0` forces the in-memory name-gram builder; any other value forces the external, bounded-memory builder | external for real volumes, in-memory for small indexes |
| `SEEKFS_NAME_GRAM_EXTERNAL_MIN_RECORDS` | Record-count floor below which the in-memory builder is used | `250000` |
| `SEEKFS_NAME_GRAM_SPOOL_DIR` | Directory for external-builder spill files | `%ProgramData%\seekfs\gram-spool` (the seekfs dir, which is never indexed) |
| `SEEKFS_GRAM_SPILL_BYTES` | Per-worker spill buffer budget, in bytes | `67108864` (64 MiB) |
| `SEEKFS_GRAM_SPILL_WORKERS` | Parallel spill workers | `GOMAXPROCS`, capped at 8 |
| `SEEKFS_GRAM_MERGE_WORKERS` | Parallel merge workers | `GOMAXPROCS`, capped at 8 |
| `SEEKFS_GRAM_MERGE_FANIN` | Maximum runs merged per pass (minimum 2) | `32` |
