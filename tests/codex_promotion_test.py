import copy
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
import zipfile


ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("promotion", ROOT / "scripts/verify-codex-promotion.py")
promotion = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(promotion)
SHA = "a" * 40
LOCK = {"schemaVersion": 1, "version": "0.153.1-mira.4", "upstreamVersion": "0.153.1", "patchSHA256": "b" * 64}
RUN = {"id": 123, "status": "completed", "conclusion": "success", "head_sha": SHA, "event": "push",
       "repository": {"full_name": "fixture/mira"}, "head_repository": {"full_name": "fixture/mira"},
       "path": ".github/workflows/codex-release.yml", "head_branch": "main"}
JOBS = [{"name": name, "status": "completed", "conclusion": "success"} for name in
        ("codex (linux, x86_64-unknown-linux-musl)", "codex (windows, x86_64-pc-windows-msvc)", "release")]
ARTIFACT = {"name": "mira-codex-release", "expired": False, "workflow_run": {"id": 123, "head_sha": SHA},
            "digest": "sha256:" + "c" * 64}


class PromotionTests(unittest.TestCase):
    def test_trusted_source(self):
        promotion.validate_source(RUN, JOBS, ARTIFACT, "fixture/mira", LOCK)

    def test_reject_failed_fork_untrusted_missing_jobs(self):
        for key, value in [("conclusion", "failure"), ("status", "in_progress"), ("event", "pull_request"),
                           ("head_branch", "topic"), ("path", ".github/workflows/other.yml"),
                           ("head_repository", {"full_name": "fork/mira"})]:
            with self.subTest(key=key), self.assertRaises(ValueError):
                promotion.validate_source({**RUN, key: value}, JOBS, ARTIFACT, "fixture/mira", LOCK)
        with self.assertRaises(ValueError):
            promotion.validate_source(RUN, JOBS[:-1], ARTIFACT, "fixture/mira", LOCK)
        for key, value in [("expired", True), ("digest", None), ("workflow_run", {"id": 124, "head_sha": SHA})]:
            with self.subTest(key=key), self.assertRaises((ValueError, TypeError)):
                promotion.validate_source(RUN, JOBS, {**ARTIFACT, key: value}, "fixture/mira", LOCK)

    def test_unsafe_names(self):
        for name in ("../x", "/tmp/x", "C:/x", "a\\b", "a/./b", "a//b", "a./b", "a\n/b"):
            with self.subTest(name=name), self.assertRaises(ValueError):
                promotion.safe_path(name)

    def fixture(self, directory, missing=False, unsafe=False):
        manifest = {**LOCK, "targets": {}}
        assets = {"LICENSE": b"license", "NOTICE": b"notice"}
        for platform in ("linux-amd64", "windows-amd64"):
            windows = platform.startswith("windows")
            suffix = ".exe" if windows else ""
            prefix = f'mira-codex_{LOCK["version"]}_{platform.replace("-", "_")}'
            names = [f"bin/codex{suffix}", f"bin/codex-code-mode-host{suffix}", f"codex-path/rg{suffix}"]
            names += (["codex-resources/codex-command-runner.exe", "codex-resources/codex-windows-sandbox-setup.exe"]
                      if windows else ["codex-resources/bwrap", "codex-resources/zsh/bin/zsh"])
            files = {name: b"fixture binary" for name in names}
            files["codex-package.json"] = json.dumps({"layoutVersion": 1, "variant": "codex", "version": LOCK["upstreamVersion"],
                "entrypoint": "bin/codex" + suffix,
                "target": "x86_64-pc-windows-msvc" if windows else "x86_64-unknown-linux-musl"}).encode()
            if missing:
                del files[f"bin/codex-code-mode-host{suffix}"]
            inventory = [{"path": name, "size": len(raw), "mode": 0o644 if name.endswith(".json") else 0o755,
                          "sha256": hashlib.sha256(raw).hexdigest()} for name, raw in files.items()]
            archive = prefix + (".zip" if windows else ".tar.gz")
            file = directory / archive
            if windows:
                with zipfile.ZipFile(file, "w") as output:
                    for item in inventory:
                        info = zipfile.ZipInfo(prefix + "/" + item["path"])
                        info.create_system = 3
                        info.external_attr = (0o100000 | item["mode"]) << 16
                        output.writestr(info, files[item["path"]])
            else:
                with tarfile.open(file, "w:gz") as output:
                    for item in inventory:
                        info = tarfile.TarInfo(prefix + "/" + item["path"])
                        info.size, info.mode = item["size"], item["mode"]
                        output.addfile(info, io.BytesIO(files[item["path"]]))
                    if unsafe:
                        info = tarfile.TarInfo("../escape")
                        info.size = 1
                        output.addfile(info, io.BytesIO(b"x"))
            assets[archive] = file.read_bytes()
            manifest["targets"][platform] = {"archive": archive, "size": len(assets[archive]),
                "sha256": hashlib.sha256(assets[archive]).hexdigest(), "files": inventory}
        assets["codex-runtime.json"] = json.dumps(manifest).encode()
        assets["SHA256SUMS"] = "".join(hashlib.sha256(raw).hexdigest() + "  " + name + "\n" for name, raw in assets.items()).encode()
        artifact_zip = directory / "artifact.zip"
        with zipfile.ZipFile(artifact_zip, "w") as output:
            for name, raw in assets.items():
                output.writestr(name, raw)
        artifact = {**ARTIFACT, "digest": "sha256:" + promotion.file_digest(artifact_zip), "size_in_bytes": artifact_zip.stat().st_size}
        return artifact_zip, artifact

    def test_complete_package_and_repeat_refusal(self):
        with tempfile.TemporaryDirectory(prefix="mira-promotion-test-") as tmp:
            root = Path(tmp)
            artifact_zip, artifact = self.fixture(root)
            promotion.verify_release(artifact_zip, artifact, LOCK, root / "verified")
            with self.assertRaises(FileExistsError):
                promotion.verify_release(artifact_zip, artifact, LOCK, root / "verified")

    def test_wrong_hash_lock_incomplete_and_traversal(self):
        for defect in ("hash", "lock", "missing", "unsafe"):
            with self.subTest(defect=defect), tempfile.TemporaryDirectory(prefix="mira-promotion-test-") as tmp:
                root = Path(tmp)
                artifact_zip, artifact = self.fixture(root, missing=defect == "missing", unsafe=defect == "unsafe")
                lock = copy.deepcopy(LOCK)
                if defect == "hash":
                    artifact["digest"] = "sha256:" + "0" * 64
                if defect == "lock":
                    lock["patchSHA256"] = "e" * 64
                with self.assertRaises(ValueError):
                    promotion.verify_release(artifact_zip, artifact, lock, root / "verified")

    def test_workflow_never_rebuilds_or_clobbers(self):
        workflow = (ROOT / ".github/workflows/promote-codex-release.yml").read_text()
        self.assertNotIn("--clobber", workflow)
        self.assertNotIn("cargo build", workflow)
        self.assertNotIn("workflow run", workflow)
        self.assertIn("--latest=false", workflow)
        self.assertIn(".draft == true", workflow)
        self.assertIn("--is-ancestor", workflow)
        self.assertIn("github.token", workflow)
        self.assertLess(workflow.index('diff -qr'), workflow.index('--draft=false'))


if __name__ == "__main__":
    unittest.main()
