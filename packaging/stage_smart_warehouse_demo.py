#!/usr/bin/env python3
"""Verify and stage the canonical Linux Smart Warehouse demo assets."""

from __future__ import annotations

import argparse
import hashlib
import json
import shutil
import zipfile
from pathlib import Path


ASSET_OUTPUTS = {
    "SmartWarehouseCell-linux64.fmu": "SmartWarehouseCell.fmu",
    "smart-warehouse-cell.glb": "SmartWarehouseCell.glb",
    "SmartWarehouseCell.webapp.zip": "SmartWarehouseCell.webapp.zip",
    "LICENSE.txt": "SmartWarehouseCell.LICENSE.txt",
}
SCENE_ARCHIVE_PATH = "resources/smart-warehouse-cell.glb"
LINUX_BINARY_PATH = "binaries/linux64/SmartWarehouseCell.so"
REQUIRED_APP_PROTOCOL_MESSAGES = (
    "decentralabs:digital-twin-host-ready",
    "decentralabs:digital-twin-ready",
    "decentralabs:digital-twin-error",
    "decentralabs:fmi-sample",
    "decentralabs:fmi-reset",
    "decentralabs:fmi-simulation-state",
    "decentralabs:fmi-samples",
)


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def stage_demo(example_root: Path, lock_path: Path, output_dir: Path) -> None:
    if example_root.is_symlink() or not example_root.is_dir():
        raise ValueError("Smart Warehouse example source must be a real directory")
    lock = json.loads(lock_path.read_text(encoding="utf-8"))
    if lock.get("repository") != "DecentraLabsCom/FMU-Executor" or lock.get("assetSet") != "examples/smart-warehouse-cell":
        raise ValueError("Smart Warehouse asset lock names an unexpected source")
    expected_hashes = lock.get("files")
    if not isinstance(expected_hashes, dict) or set(expected_hashes) != set(ASSET_OUTPUTS):
        raise ValueError("Smart Warehouse asset lock must pin the FMU, GLB, app and license")

    sources: dict[str, Path] = {}
    for name, expected_hash in expected_hashes.items():
        source = example_root / name
        if source.is_symlink() or not source.is_file() or sha256_file(source) != expected_hash:
            raise ValueError(f"Smart Warehouse asset is missing or does not match its SHA-256 pin: {name}")
        sources[name] = source

    scene_bytes = sources["smart-warehouse-cell.glb"].read_bytes()
    try:
        with zipfile.ZipFile(sources["SmartWarehouseCell-linux64.fmu"]) as archive:
            names = archive.namelist()
            if len(names) != len(set(names)) or archive.testzip() is not None:
                raise ValueError("Smart Warehouse FMU contains duplicate or corrupt entries")
            for required in ("modelDescription.xml", LINUX_BINARY_PATH, SCENE_ARCHIVE_PATH):
                if required not in names:
                    raise ValueError(f"Smart Warehouse Linux FMU is missing {required}")
            embedded_scene = archive.read(SCENE_ARCHIVE_PATH)
    except (OSError, zipfile.BadZipFile) as error:
        raise ValueError(f"Smart Warehouse Linux FMU is not a valid ZIP archive: {error}") from error
    if embedded_scene != scene_bytes:
        raise ValueError("Standalone GLB differs from the scene embedded in the Smart Warehouse FMU")
    with zipfile.ZipFile(sources["SmartWarehouseCell.webapp.zip"]) as webapp:
        if any(required not in webapp.namelist() for required in ("index.html", "app.js", "warehouse.glb")):
            raise ValueError("Smart Warehouse app bundle is missing index.html, app.js or warehouse.glb")
        if webapp.read("warehouse.glb") != scene_bytes:
            raise ValueError("Smart Warehouse app bundle uses a different warehouse scene")
        app_script = webapp.read("app.js").decode("utf-8")
        if any(message not in app_script for message in REQUIRED_APP_PROTOCOL_MESSAGES):
            raise ValueError("Smart Warehouse app bundle is missing the Marketplace host protocol")

    output_dir.mkdir(parents=True, exist_ok=True)
    for source_name, output_name in ASSET_OUTPUTS.items():
        shutil.copyfile(sources[source_name], output_dir / output_name)


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
