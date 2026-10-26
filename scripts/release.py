"""Build versioned native release archives without publishing them."""

import argparse
import gzip
import hashlib
import io
import json
import os
import re
import subprocess
import tarfile
import tempfile
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
TARGETS = (
    "linux/amd64",
    "linux/arm64",
    "darwin/amd64",
    "darwin/arm64",
    "windows/amd64",
)


def command(*args, env=None):
    return subprocess.check_output(args, cwd=ROOT, env=env, text=True).strip()


def source_info():
    try:
        return {
            "commit": command("git", "rev-parse", "HEAD"),
            "dirty": bool(command("git", "status", "--porcelain")),
        }
    except (subprocess.CalledProcessError, FileNotFoundError):
        return {"commit": None, "dirty": None}


def dependency_licenses(modules):
    go_license = Path(command("go", "env", "GOROOT")) / "LICENSE"
    if not go_license.is_file():
        # Debian places the SDK's license notices in the versioned package docs.
        version = re.match(r"go(\d+\.\d+)", command("go", "env", "GOVERSION"))
        if version:
            go_license = Path(f"/usr/share/doc/golang-{version[1]}-go/copyright")
    if not go_license.is_file():
        raise RuntimeError("Go SDK license notices were not found")
    chunks = [
        b"Go runtime\n==========\n",
        go_license.read_bytes(),
    ]
    for module, version, directory in sorted(modules):
        files = sorted(
            {
                p
                for pattern in ("LICENSE*", "COPYING*", "NOTICE*")
                for p in Path(directory).glob(pattern)
                if p.is_file()
            }
        )
        if not files:
            raise RuntimeError(f"no license notice found for {module}@{version}")
        for path in files:
            chunks += [
                f"\n\n{module}@{version} / {path.name}\n{'=' * 60}\n".encode(),
                path.read_bytes(),
            ]
    return b"".join(chunks)


def package(destination, folder, members, windows):
    if windows:
        with zipfile.ZipFile(
            destination, "w", compression=zipfile.ZIP_DEFLATED
        ) as archive:
            for name, (data, mode) in sorted(members.items()):
                info = zipfile.ZipInfo(f"{folder}/{name}")
                info.create_system = 3
                info.external_attr = (0o100000 | mode) << 16
                info.compress_type = zipfile.ZIP_DEFLATED
                archive.writestr(info, data)
    else:
        with destination.open("wb") as output:
            with gzip.GzipFile(
                filename="", mode="wb", fileobj=output, mtime=0
            ) as compressed:
                with tarfile.open(fileobj=compressed, mode="w") as archive:
                    for name, (data, mode) in sorted(members.items()):
                        info = tarfile.TarInfo(f"{folder}/{name}")
                        info.size, info.mode = len(data), mode
                        archive.addfile(info, io.BytesIO(data))


def build(version, targets, output):
    destination = output.resolve() / version
    if destination.exists():
        raise RuntimeError(f"output already exists: {destination}")
    destination.parent.mkdir(parents=True, exist_ok=True)
    metadata = {
        "version": version,
        "go": command("go", "env", "GOVERSION"),
        "source": source_info(),
    }
    modules = set()
    notes = ROOT / "docs/releases" / f"{version}.md"
    if not notes.is_file():
        raise RuntimeError(f"release notes missing: {notes}")
    with tempfile.TemporaryDirectory(
        prefix=".release-", dir=destination.parent
    ) as temporary:
        staging = Path(temporary)
        binaries = {}
        for target in targets:
            goos, goarch = target.split("/")
            env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED="0")
            for line in command(
                "go",
                "list",
                "-deps",
                "-f",
                "{{if .Module}}{{if not .Module.Main}}{{.Module.Path}}|{{.Module.Version}}|{{.Module.Dir}}{{end}}{{end}}",
                "./cmd/agy2api",
                env=env,
            ).splitlines():
                if line:
                    modules.add(tuple(line.split("|", 2)))
            binary = staging / target.replace("/", "_")
            print(f"Building {version} for {target}", flush=True)
            subprocess.run(
                [
                    "go",
                    "build",
                    "-trimpath",
                    "-ldflags",
                    f"-s -w -X main.version={version}",
                    "-o",
                    str(binary),
                    "./cmd/agy2api",
                ],
                cwd=ROOT,
                env=env,
                check=True,
            )
            info = command("go", "version", "-m", str(binary))
            if (
                "CGO_ENABLED=0" not in info
                or f"GOOS={goos}" not in info
                or f"GOARCH={goarch}" not in info
            ):
                raise RuntimeError(f"unexpected build settings for {target}")
            binaries[target] = binary
        licenses = dependency_licenses(modules)
        assets = []
        for target, binary in binaries.items():
            windows = target.startswith("windows/")
            folder = f"agy2api_{version}_{target.replace('/', '_')}"
            filename = folder + (".zip" if windows else ".tar.gz")
            build_info = dict(metadata, target=target, cgo_enabled=False)
            members = {
                "agy2api.exe" if windows else "agy2api": (binary.read_bytes(), 0o755),
                "README.md": ((ROOT / "README.md").read_bytes(), 0o644),
                "docs/configuration.md": (
                    (ROOT / "docs/configuration.md").read_bytes(),
                    0o644,
                ),
                "docs/api.md": ((ROOT / "docs/api.md").read_bytes(), 0o644),
                "docs/development.md": (
                    (ROOT / "docs/development.md").read_bytes(),
                    0o644,
                ),
                "config.example.toml": (
                    (ROOT / "config.example.toml").read_bytes(),
                    0o644,
                ),
                "THIRD_PARTY_NOTICES": (
                    (ROOT / "THIRD_PARTY_NOTICES").read_bytes(),
                    0o644,
                ),
                "DEPENDENCY_LICENSES.txt": (licenses, 0o644),
                "RELEASE_NOTES.md": (notes.read_bytes(), 0o644),
                "BUILD.json": (
                    (json.dumps(build_info, indent=2) + "\n").encode(),
                    0o644,
                ),
            }
            package(staging / filename, folder, members, windows)
            assets.append(filename)
            binary.unlink()
        manifest = dict(
            metadata,
            targets=targets,
            assets=assets,
            modules=[{"path": name, "version": v} for name, v, _ in sorted(modules)],
        )
        (staging / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
        checksums = [
            f"{hashlib.sha256((staging / name).read_bytes()).hexdigest()}  {name}\n"
            for name in sorted(assets + ["manifest.json"])
        ]
        (staging / "SHA256SUMS").write_text("".join(checksums))
        staging.rename(destination)
    print(f"Release archives ready: {destination}", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--target", action="append", choices=TARGETS)
    parser.add_argument("--output", type=Path, default=ROOT / "dist")
    args = parser.parse_args()
    if not re.fullmatch(
        r"v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)", args.version
    ):
        parser.error("version must be vMAJOR.MINOR.PATCH")
    targets = list(dict.fromkeys(args.target or TARGETS))
    try:
        build(args.version, targets, args.output)
    except (OSError, RuntimeError, subprocess.CalledProcessError) as error:
        parser.exit(1, f"release: {error}\n")


if __name__ == "__main__":
    main()
