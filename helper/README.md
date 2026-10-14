# comfyform helper

A ComfyUI custom node pack that registers no nodes. It adds one route,
`GET /comfyform/packs`, which `comfy lock` and `comfy plan` use to pin the
custom node packs a workflow depends on:

```json
{"packs": {
  "comfyui-impact-pack": {"kind": "registry", "id": "comfyui-impact-pack", "version": "8.28.3", "content": "sha256:..."},
  "ComfyUI-KJNodes":     {"kind": "git", "remote": "https://github.com/kijai/ComfyUI-KJNodes", "commit": "d764153...", "content": "sha256:..."},
  "my_local_nodes":      {"kind": "local", "content": "sha256:..."}
}}
```

`content` hashes each pack's code and config files (`.py`, `.js`, `.json`,
`.toml`, ...), skipping `.git`, `__pycache__`, `node_modules` and weights. It is
the only pin for packs with no git or Registry source, and it catches edits to
the others. Hashes are cached by file size and mtime.

Without the helper, `comfy` falls back to ComfyUI-Manager's
`/v2/customnode/installed`, which reports git packs only; the rest lock as
`unpinned`.

## Install

Copy `comfyform_helper/` into ComfyUI's `custom_nodes/` and restart ComfyUI.
It needs Python 3.11+ (for `tomllib`) and `git` on the PATH.

## Test

```sh
cd helper && python3 -m unittest test_packs
```
