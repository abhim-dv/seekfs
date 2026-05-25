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
.\seekfs.exe config set dbs = '["F:\\seekfs_c.gsi", "F:\\seekfs_f.gsi"]'
.\seekfs.exe config get dbs
```
