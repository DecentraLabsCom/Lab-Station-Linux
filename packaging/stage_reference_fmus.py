#!/usr/bin/env python3
"""Validate and stage reference FMUs supported by a Linux Station architecture."""

from __future__ import annotations

import argparse
import shutil
import zipfile
from pathlib import Path


LINUX_BINARIES = {
    "amd64": {
        "BinaryClockTest": "binaries/x86_64-linux/BinaryClockTest.so",
        "BouncingBall": "binaries/linux64/BouncingBall.so",
        "Feedthrough": "binaries/linux64/Feedthrough.so",
        "Stair": "binaries/x86_64-linux/Stair.so",
        "StateSpace": "binaries/x86_64-linux/StateSpace.so",
        "VanDerPol": "binaries/x86_64-linux/VanDerPol.so",
    },
    "arm64": {
        "Stair": "binaries/aarch64-linux/Stair.so",
        "StateSpace": "binaries/aarch64-linux/StateSpace.so",
        "VanDerPol": "binaries/aarch64-linux/VanDerPol.so",
    },
}


def stage(source_dir: Path, output_dir: Path, architecture: str) -> None:
    if source_dir.is_symlink() or not source_dir.is_dir():
        raise ValueError("Reference FMU source must be a real directory")
    required_license = source_dir / "LICENSE.txt"
    if required_license.is_symlink() or not required_license.is_file():
        raise ValueError("Reference FMU license is missing")

    output_dir.mkdir(parents=True, exist_ok=True)
    for model_name, binary_path in LINUX_BINARIES[architecture].items():
        source = source_dir / f"{model_name}.fmu"
        if source.is_symlink() or not source.is_file():
            raise ValueError(f"Reference FMU is missing or is not a regular file: {model_name}.fmu")
        try:
            with zipfile.ZipFile(source) as archive:
                names = archive.namelist()
                if len(names) != len(set(names)) or archive.testzip() is not None:
                    raise ValueError(f"Reference FMU contains duplicate or corrupt entries: {model_name}.fmu")
                if "modelDescription.xml" not in names or binary_path not in names:
                    raise ValueError(
                        f"Reference FMU {model_name}.fmu does not contain its {architecture} Linux binary"
                    )
        except (OSError, zipfile.BadZipFile) as error:
            raise ValueError(f"Reference FMU is not a valid archive: {model_name}.fmu: {error}") from error
        shutil.copyfile(source, output_dir / source.name)

    shutil.copyfile(required_license, output_dir / "Reference-FMUs.LICENSE.txt")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-dir", type=Path, required=True)
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument("--architecture", choices=tuple(LINUX_BINARIES), required=True)
    args = parser.parse_args()
    stage(args.source_dir, args.output_dir, args.architecture)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
