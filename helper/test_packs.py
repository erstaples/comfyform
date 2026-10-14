import os
import subprocess
import tempfile
import sys
import unittest

# Import packs.py directly: the package __init__ needs ComfyUI's server module.
sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "comfyform_helper"))
from packs import content_hash, scan  # noqa: E402


def write(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(text)


class ScanTest(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp()

    def test_kinds(self):
        r = self.root
        write(f"{r}/reg/.cnr-version", "8.28.3\n")
        write(f"{r}/reg/pyproject.toml", '[project]\nname = "comfyui-impact-pack"\nversion = "8.28.3"\n')
        write(f"{r}/reg/__init__.py", "")
        write(f"{r}/loose/__init__.py", "")
        write(f"{r}/single.py", "NODE_CLASS_MAPPINGS = {}")
        write(f"{r}/off.disabled/__init__.py", "")
        write(f"{r}/repo/__init__.py", "")
        git = lambda *a: subprocess.run(["git", "-C", f"{r}/repo", *a], check=True, capture_output=True)
        git("init", "-q")
        git("remote", "add", "origin", "https://example.com/repo.git")
        git("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x")

        packs = scan([r, f"{r}/missing"])["packs"]
        self.assertEqual(sorted(packs), ["loose", "reg", "repo", "single"])
        self.assertEqual(packs["reg"]["kind"], "registry")
        self.assertEqual(packs["reg"]["id"], "comfyui-impact-pack")
        self.assertEqual(packs["reg"]["version"], "8.28.3")
        self.assertEqual(packs["loose"]["kind"], "local")
        self.assertEqual(packs["single"]["kind"], "local")
        self.assertEqual(packs["repo"]["kind"], "git")
        self.assertEqual(len(packs["repo"]["commit"]), 40)
        self.assertEqual(packs["repo"]["remote"], "https://example.com/repo.git")
        for p in packs.values():
            self.assertTrue(p["content"].startswith("sha256:"))

    def test_content_hash_tracks_code_only(self):
        p = f"{self.root}/pack"
        write(f"{p}/__init__.py", "a = 1")
        write(f"{p}/web/app.js", "x")
        h1 = content_hash(p)
        write(f"{p}/model.safetensors", "weights")
        write(f"{p}/__pycache__/x.py", "junk")
        self.assertEqual(content_hash(p), h1, "weights and caches must not change the hash")
        write(f"{p}/__init__.py", "a = 2")
        self.assertNotEqual(content_hash(p), h1, "an edit must change the hash")


if __name__ == "__main__":
    unittest.main()
