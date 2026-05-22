# Security Notes

## Service Privileges

The Windows service may run with elevated privileges so it can access NTFS/USN
volume APIs. Treat it as a privileged local component.

## Named Pipe Access

The default pipe is:

```text
\\.\pipe\seekfs-service
```

The default SDDL grants access to LocalSystem, built-in administrators, and
interactive users:

```text
D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;IU)
```

Use `-sddl` when installing the service to restrict or customize access.

## Index Files

Index files contain file and directory names from indexed volumes. Store them in
a location with appropriate local filesystem permissions.

Do not publish `.gsi` index files or `.tok` sidecars unless you are comfortable
sharing the indexed path names.
