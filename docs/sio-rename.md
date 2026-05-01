# sio Rename

`sio` is now the canonical executable name for this serial I/O CLI.

The old short command, `gs`, is retained as a compatibility alias during the
transition. New scripts, documentation, and agent instructions should call
`sio`. Existing `gs` scripts continue to work when the alias binary is installed.

The user data directory is unchanged. Session state, caches, and worker logs
continue to live under:

```text
%AppData%/gs/sessions/<session>/
```

This avoids breaking existing named sessions while the executable name changes.

The rename reduces collision risk with other common `gs` tools such as
Ghostscript and git-spice while keeping a short serial I/O-oriented command.

Windows verification:

```powershell
Get-Command sio
sio version

# Optional legacy alias:
Get-Command gs
gs version
```
