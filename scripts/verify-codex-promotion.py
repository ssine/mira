"""Verify an existing CI release without compiling or executing its binaries."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import shutil
import stat
import tarfile
import zipfile


def require(condition, message):
    if not condition:
        raise ValueError(message)


def digest(stream):
    return hashlib.file_digest(stream, "sha256").hexdigest()


def file_digest(file):
    with Path(file).open("rb") as stream:
        return digest(stream)


def safe_path(name):
    require(isinstance(name, str) and name and not re.search(r"[\\:\x00-\x1f]", name), "Unsafe path")
    require(all(part not in ("", ".", "..") and not part.endswith((".", " "))
                for part in name.split("/")), "Unsafe path component")
    return name


def validate_source(run, jobs, artifact, repo, lock):
    require(run["status"] == "completed" and run["conclusion"] == "success", "Source run did not pass")
    require(run["repository"]["full_name"] == repo and run["head_repository"]["full_name"] == repo,
            "Source repository mismatch")
    require(run["path"] == ".github/workflows/codex-release.yml" and run["head_branch"] == "main"
            and run["event"] in ("push", "workflow_dispatch"), "Only trusted main Codex builds can be promoted")
    require(re.fullmatch(r"[0-9a-f]{40}", run["head_sha"]), "Invalid source SHA")
    require(len(jobs) == 3 and all(j["status"] == "completed" and j["conclusion"] == "success" for j in jobs),
            "All platform and packaging jobs must pass")
    require(sum(j["name"] == "release" for j in jobs) == 1, "Missing packaging job")
    for target in ("x86_64-unknown-linux-musl", "x86_64-pc-windows-msvc"):
        require(sum(target in j["name"] for j in jobs) == 1, "Missing or duplicate target job")
    require(artifact["name"] == "mira-codex-release" and not artifact["expired"], "Missing or expired release artifact")
    require(artifact["workflow_run"]["id"] == run["id"]
            and artifact["workflow_run"]["head_sha"] == run["head_sha"], "Artifact provenance mismatch")
    require(re.fullmatch(r"sha256:[0-9a-f]{64}", artifact["digest"]), "Missing independent artifact digest")
    require(lock["schemaVersion"] == 1 and re.fullmatch(r"\d+\.\d+\.\d+-mira\.[1-9]\d*", lock["version"]),
            "Invalid runtime lock")


def verify_package(file, platform, target, lock):
    prefix = f'mira-codex_{lock["version"]}_{platform.replace("-", "_")}'
    windows = platform == "windows-amd64"
    suffix = ".exe" if windows else ""
    extension = ".zip" if windows else ".tar.gz"
    require(target["archive"] == prefix + extension, "Unexpected archive name")
    require(file.stat().st_size == target["size"] and file_digest(file) == target["sha256"], "Archive hash/size mismatch")
    expected = {}
    for item in target["files"]:
        name = safe_path(item["path"])
        require(name not in expected and item["mode"] in (0o644, 0o755), "Duplicate file or invalid mode")
        require(isinstance(item["size"], int) and item["size"] >= 0
                and re.fullmatch(r"[0-9a-f]{64}", item["sha256"]), "Invalid file inventory")
        expected[name] = item
    required = ["codex-package.json", f"bin/codex{suffix}", f"bin/codex-code-mode-host{suffix}", f"codex-path/rg{suffix}"]
    required += (["codex-resources/codex-command-runner.exe", "codex-resources/codex-windows-sandbox-setup.exe"]
                 if windows else ["codex-resources/bwrap", "codex-resources/zsh/bin/zsh"])
    require(all(name in expected and expected[name]["size"] > 0 for name in required), "Incomplete canonical package")
    seen = set()
    canonical = None

    def check(name, size, mode, stream):
        nonlocal canonical
        safe_path(name)
        require(name.startswith(prefix + "/"), "Unexpected package root")
        relative = name[len(prefix) + 1:]
        require(relative in expected and relative not in seen, "Unexpected or duplicate package file")
        item = expected[relative]
        require(size == item["size"] and mode == item["mode"], "File size/mode mismatch")
        if relative == "codex-package.json":
            raw = stream.read()
            require(hashlib.sha256(raw).hexdigest() == item["sha256"], "Canonical manifest hash mismatch")
            canonical = json.loads(raw)
        else:
            require(digest(stream) == item["sha256"], "Package file hash mismatch")
        seen.add(relative)

    if windows:
        with zipfile.ZipFile(file) as archive:
            for member in archive.infolist():
                safe_path(member.filename.rstrip("/"))
                mode = member.external_attr >> 16
                if member.is_dir():
                    require(member.filename == prefix + "/" or member.filename.startswith(prefix + "/"), "Unexpected directory")
                    continue
                require(stat.S_ISREG(mode), "ZIP special file not allowed")
                with archive.open(member) as stream:
                    check(member.filename, member.file_size, stat.S_IMODE(mode), stream)
    else:
        with tarfile.open(file, "r:gz") as archive:
            for member in archive:
                safe_path(member.name.rstrip("/"))
                if member.isdir():
                    require(member.name == prefix or member.name.startswith(prefix + "/"), "Unexpected directory")
                    continue
                require(member.isfile(), "TAR links/special files not allowed")
                with archive.extractfile(member) as stream:
                    check(member.name, member.size, member.mode, stream)
    require(seen == expected.keys(), "Package inventory mismatch")
    require(canonical["layoutVersion"] == 1 and canonical["variant"] == "codex"
            and canonical["version"] == lock["upstreamVersion"] and canonical["entrypoint"] == f"bin/codex{suffix}"
            and canonical["target"] == ("x86_64-pc-windows-msvc" if windows else "x86_64-unknown-linux-musl"),
            "Canonical package metadata mismatch")


def verify_release(artifact_zip, artifact, lock, output):
    require(artifact_zip.stat().st_size == artifact["size_in_bytes"]
            and "sha256:" + file_digest(artifact_zip) == artifact["digest"], "GitHub artifact hash/size mismatch")
    output.mkdir()  # Fresh directory only; never overwrite a previous staging area.
    prefix = f'mira-codex_{lock["version"]}_'
    assets = {"SHA256SUMS", "LICENSE", "NOTICE", "codex-runtime.json",
              prefix + "linux_amd64.tar.gz", prefix + "windows_amd64.zip"}
    with zipfile.ZipFile(artifact_zip) as archive:
        require(len(archive.infolist()) == len(assets) and set(archive.namelist()) == assets, "Unexpected release assets")
        for member in archive.infolist():
            require(not member.is_dir() and not stat.S_ISLNK(member.external_attr >> 16), "Invalid artifact member")
            with archive.open(member) as src, (output / member.filename).open("xb") as dest:
                shutil.copyfileobj(src, dest)
    checksums = {}
    for line in (output / "SHA256SUMS").read_text().splitlines():
        match = re.fullmatch(r"([0-9a-f]{64})  ([^/\\]+)", line)
        require(match is not None and match[2] not in checksums, "Malformed or duplicate checksum")
        checksums[match[2]] = match[1]
    require(checksums.keys() == assets - {"SHA256SUMS"}, "Incomplete checksum list")
    for name, sha in checksums.items():
        require(file_digest(output / name) == sha, "Release checksum mismatch: " + name)
    manifest = json.loads((output / "codex-runtime.json").read_text())
    require({key: value for key, value in manifest.items() if key != "targets"} == lock, "Source lock mismatch")
    require(set(manifest["targets"]) == {"linux-amd64", "windows-amd64"}, "Unexpected targets")
    for platform, target in manifest["targets"].items():
        require(target["archive"] in assets, "Unexpected target asset")
        verify_package(output / target["archive"], platform, target, lock)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--metadata", type=Path, required=True, help="Directory containing run/jobs/artifact/lock.json")
    parser.add_argument("--repo", required=True)
    parser.add_argument("--archive", type=Path)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    data = {name: json.loads((args.metadata / (name + ".json")).read_text()) for name in ("run", "jobs", "artifact", "lock")}
    validate_source(data["run"], data["jobs"], data["artifact"], args.repo, data["lock"])
    if args.archive:
        require(args.output is not None, "An output directory is required")
        verify_release(args.archive, data["artifact"], data["lock"], args.output)
    print(f'Verified Codex {data["lock"]["version"]} from run {data["run"]["id"]} ({data["run"]["head_sha"]})')


if __name__ == "__main__":
    main()
