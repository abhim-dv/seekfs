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
