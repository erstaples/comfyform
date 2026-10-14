"""Describe installed custom node packs: where each came from and a hash of
its code. Plain Python, no ComfyUI imports, so it can be tested on its own."""

import hashlib
import os
import subprocess
import tomllib

# Files that make up a pack's behavior. Weights and media are left out: they
# are large and belong to model pinning, not code pinning.
CODE_EXTS = {
    ".py", ".js", ".mjs", ".cjs", ".ts", ".css", ".html", ".json", ".toml",
    ".yaml", ".yml", ".txt", ".cfg", ".ini", ".sh",
}
SKIP_DIRS = {".git", "__pycache__", "node_modules", ".venv", "venv"}

_cache = {}  # path -> (fingerprint, content hash)


def scan(custom_nodes_dirs):
    packs = {}
    for root in custom_nodes_dirs:
        if not os.path.isdir(root):
            continue
        for name in sorted(os.listdir(root)):
            path = os.path.join(root, name)
            if name.startswith(".") or name.endswith(".disabled") or name == "__pycache__":
                continue
            if os.path.isdir(path):
                packs[name] = describe(path)
            elif name.endswith(".py"):
                packs[name[:-3]] = {"kind": "local", "content": content_hash(path)}
    return {"packs": packs}


def describe(path):
    info = {"kind": "local"}
    if os.path.exists(os.path.join(path, ".git")):
        commit = _git(path, "rev-parse", "HEAD")
        if commit:
            info = {"kind": "git", "commit": commit, "remote": _git(path, "config", "--get", "remote.origin.url")}
    elif os.path.isfile(os.path.join(path, ".cnr-version")):
        with open(os.path.join(path, ".cnr-version")) as f:
            version = f.read().strip()
        info = {"kind": "registry", "id": _project_name(path) or os.path.basename(path), "version": version}
    info["content"] = content_hash(path)
    return info


def content_hash(path):
    files = _code_files(path)
    fingerprint = tuple((rel, st.st_size, st.st_mtime_ns) for rel, st in files)
    cached = _cache.get(path)
    if cached and cached[0] == fingerprint:
        return cached[1]
    h = hashlib.sha256()
    base = path if os.path.isdir(path) else os.path.dirname(path)
    for rel, _ in files:
        with open(os.path.join(base, rel), "rb") as f:
            digest = hashlib.sha256(f.read()).hexdigest()
        h.update(f"{rel}\0{digest}\n".encode())
    result = "sha256:" + h.hexdigest()
    _cache[path] = (fingerprint, result)
    return result


def _code_files(path):
    if os.path.isfile(path):
        return [(os.path.basename(path), os.stat(path))]
    out = []
    for dirpath, dirnames, filenames in os.walk(path):
        dirnames[:] = sorted(d for d in dirnames if d not in SKIP_DIRS)
        for fn in filenames:
            if os.path.splitext(fn)[1].lower() in CODE_EXTS:
                full = os.path.join(dirpath, fn)
                rel = os.path.relpath(full, path).replace(os.sep, "/")
                out.append((rel, os.stat(full)))
    out.sort(key=lambda e: e[0])
    return out


def _project_name(path):
    try:
        with open(os.path.join(path, "pyproject.toml"), "rb") as f:
            return tomllib.load(f).get("project", {}).get("name")
    except (OSError, tomllib.TOMLDecodeError):
        return None


def _git(path, *args):
    try:
        r = subprocess.run(["git", "-C", path, *args], capture_output=True, text=True, timeout=10)
    except (OSError, subprocess.TimeoutExpired):
        return ""
    return r.stdout.strip() if r.returncode == 0 else ""
