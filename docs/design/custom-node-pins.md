# Pinning custom node packs

`comfy lock` pins each node class's interface hash and python module, but not
which version of the pack provides it. Two servers can report the same
interface for different code. This adds pack pins to the lockfile.

## What servers look like

Surveyed on a real server (42 pack directories):

- **git clones**: commit + remote URL.
- **Comfy Registry installs**: `.cnr-version` holds the version; the id is
  `[project].name` in `pyproject.toml`. ComfyUI-Manager's
  `/v2/customnode/installed` omits these.
- **local packs**: no source information; only a content hash identifies them.
- The same pack can be installed twice (Registry and git). A class's
  `python_module` (`custom_nodes.<dir>`) says which directory it came from, so
  pins are keyed by directory.

## Design

**Helper pack** (`helper/comfyform_helper`): registers no nodes, adds
`GET /comfyform/packs`:

```json
{"packs": {"<dir>": {"kind": "git|registry|local", "remote": "", "commit": "",
                     "id": "", "version": "", "content": "sha256:..."}}}
```

`content` hashes the pack's code and config files (not `.git`, `__pycache__`,
`node_modules`, or weights), cached by file mtimes.

**Fallback**: without the helper, `comfy` reads Manager's
`/v2/customnode/installed` (git packs only). Packs it can't identify lock as
`unpinned`.

**Lockfile**: `packs` maps each directory that a locked class comes from to
`{"source": "git+<remote>@<commit>" | "registry:<id>@<version>" | "local" |
"unpinned", "content": "sha256:..."}`.

**Plan**: warns when a used pack's source or content differs from the lock,
like interface drift. Packs are not checked when the server reports nothing.

## Not in scope

Model file hashes (the helper is the natural place); `comfy sync` to install
locked versions on another server.
