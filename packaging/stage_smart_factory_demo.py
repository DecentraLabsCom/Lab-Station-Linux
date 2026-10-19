#!/usr/bin/env python3
"""Verify and stage the canonical Linux Smart Factory demo assets."""

from __future__ import annotations

import argparse
import hashlib
import json
import shutil
import zipfile
from pathlib import Path


ASSET_OUTPUTS = {
    "SmartFactoryCell-linux64.fmu": "SmartFactoryCell.fmu",
    "smart-factory-cell.glb": "SmartFactoryCell.glb",
    "LICENSE.txt": "SmartFactoryCell.LICENSE.txt",
}
SCENE_ARCHIVE_PATH = "resources/smart-factory-cell.glb"
LINUX_BINARY_PATH = "binaries/linux64/SmartFactoryCell.so"


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def stage_demo(example_root: Path, lock_path: Path, output_dir: Path) -> None:
    if example_root.is_symlink() or not example_root.is_dir():
        raise ValueError("Smart Factory example source must be a real directory")
    lock = json.loads(lock_path.read_text(encoding="utf-8"))
    if lock.get("repository") != "DecentraLabsCom/FMU-Executor":
        raise ValueError("Smart Factory asset lock names an unexpected source repository")
    if lock.get("assetSet") != "examples/smart-factory-cell":
        raise ValueError("Smart Factory asset lock names an unexpected asset set")

    expected_hashes = lock.get("files")
    if not isinstance(expected_hashes, dict) or set(expected_hashes) != set(ASSET_OUTPUTS):
        raise ValueError("Smart Factory asset lock must pin the FMU, GLB, and license")

    source_files: dict[str, Path] = {}
    for name, expected_hash in expected_hashes.items():
        source = example_root / name
        if source.is_symlink() or not source.is_file():
            raise ValueError(f"Smart Factory asset is missing or is a symlink: {name}")
        actual_hash = sha256_file(source)
        if actual_hash != expected_hash:
            raise ValueError(f"Smart Factory asset does not match its SHA-256 pin: {name}")
        source_files[name] = source

    fmu_path = source_files["SmartFactoryCell-linux64.fmu"]
    scene_bytes = source_files["smart-factory-cell.glb"].read_bytes()
    try:
        with zipfile.ZipFile(fmu_path) as archive:
            names = archive.namelist()
            if len(names) != len(set(names)):
                raise ValueError("Smart Factory FMU contains duplicate archive paths")
            if archive.testzip() is not None:
                raise ValueError("Smart Factory FMU has a corrupt archive entry")
            for required in ("modelDescription.xml", LINUX_BINARY_PATH, SCENE_ARCHIVE_PATH):
                if required not in names:
                    raise ValueError(f"Smart Factory Linux FMU is missing {required}")
            embedded_scene = archive.read(SCENE_ARCHIVE_PATH)
    except (OSError, zipfile.BadZipFile) as error:
        raise ValueError(f"Smart Factory Linux FMU is not a valid ZIP archive: {error}") from error

    if embedded_scene != scene_bytes:
        raise ValueError("Standalone GLB differs from the scene embedded in the Smart Factory FMU")

    output_dir.mkdir(parents=True, exist_ok=True)
    for source_name, output_name in ASSET_OUTPUTS.items():
        shutil.copyfile(source_files[source_name], output_dir / output_name)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--example-root", type=Path, required=True)
    parser.add_argument("--lock-file", type=Path, required=True)
    parser.add_argument("--output-dir", type=Path, required=True)
    args = parser.parse_args()
    stage_demo(args.example_root, args.lock_file, args.output_dir)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
