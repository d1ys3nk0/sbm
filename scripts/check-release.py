#!/usr/bin/env python3
"""Validate macOS release archives produced by GoReleaser."""

import argparse
import hashlib
from pathlib import Path
import platform
import struct
import subprocess
import tarfile
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", type=Path, default=Path("dist"))
    parser.add_argument("--version", help="Require this release version in both archive names")
    args = parser.parse_args()
    dist = args.directory
    archives = sorted(dist.glob("*.tar.gz"))
    targets = {"darwin_amd64": 0x01000007, "darwin_arm64": 0x0100000C}
    checksums = {}
    for line in (dist / "checksums.txt").read_text().splitlines():
        digest, name = line.split()
        name = name.lstrip("*")
        if name in checksums:
            raise ValueError(f"Duplicate checksum: {name}")
        checksums[name] = digest
    if len(archives) != len(targets) or set(checksums) != {p.name for p in archives}:
        raise ValueError("Expected exactly two archives and matching checksum entries")

    host_arch = {"x86_64": "amd64", "arm64": "arm64"}.get(platform.machine())
    if host_arch is None:
        raise ValueError("Unsupported macOS host architecture")
    versions = set()
    found = set()
    host_binary = None
    for archive in archives:
        target = next((t for t in targets if archive.name.endswith(f"_{t}.tar.gz")), None)
        if target is None or target in found or not archive.name.startswith("sbm_"):
            raise ValueError(f"Unexpected archive: {archive.name}")
        found.add(target)
        version = archive.name[len("sbm_"):-len(f"_{target}.tar.gz")]
        if not version:
            raise ValueError("Missing archive version")
        versions.add(version)
        if hashlib.sha256(archive.read_bytes()).hexdigest() != checksums[archive.name]:
            raise ValueError(f"Checksum mismatch: {archive.name}")
        with tarfile.open(archive, "r:gz") as contents:
            members = contents.getmembers()
            if len(members) != 3 or {m.name for m in members} != {"sbm", "README.md", "LICENSE"}:
                raise ValueError(f"Unexpected archive contents: {archive.name}")
            if not all(m.isfile() and m.size > 0 for m in members):
                raise ValueError(f"Expected nonempty regular files: {archive.name}")
            for member in members:
                data = contents.extractfile(member).read()
                if (b"apex" + b"plane") in data.lower():
                    raise ValueError(f"Obsolete branding in {archive.name}: {member.name}")
            executable = contents.getmember("sbm")
            if not executable.mode & 0o111:
                raise ValueError(f"Nonexecutable sbm: {archive.name}")
            binary = contents.extractfile(executable).read()
            if len(binary) < 8 or struct.unpack("<II", binary[:8]) != (0xFEEDFACF, targets[target]):
                raise ValueError(f"Unexpected Mach-O architecture: {archive.name}")
            if target == f"darwin_{host_arch}":
                host_binary = binary

    if found != set(targets) or len(versions) != 1 or host_binary is None:
        raise ValueError("Archive platforms or versions disagree")
    if args.version and versions != {args.version}:
        raise ValueError(f"Expected release version {args.version}")
    commit = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    with tempfile.TemporaryDirectory() as directory:
        binary = Path(directory) / "sbm"
        binary.write_bytes(host_binary)
        binary.chmod(0o755)
        output = subprocess.check_output([str(binary), "--version"], text=True).strip()
        expected = f"sbm {versions.pop()} ({commit})"
        if output != expected:
            raise ValueError(f"Unexpected version metadata: {output}")
    print("Verified both archives, SHA-256 checksums, architectures, and version metadata")


if __name__ == "__main__":
    main()
