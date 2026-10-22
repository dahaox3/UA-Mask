#!/usr/bin/env python3
"""Build the nftables IPK for aarch64_cortex-a53 without an OpenWrt SDK."""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import struct
import subprocess
import tarfile

ROOT = Path(__file__).resolve().parents[1]
ARCH = "aarch64_cortex-a53"


def run(*args, env=None):
    return subprocess.check_output(args, cwd=ROOT, env=env, text=True).strip()


def archive(files, epoch):
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode="w", format=tarfile.GNU_FORMAT) as tar:
        directories = {"."}
        for name, _, _ in files:
            directories.update(str(p) for p in PurePosixPath(name).parents if str(p) != ".")
        for name in sorted(directories, key=lambda p: (p.count("/"), p)):
            item = tarfile.TarInfo("./" if name == "." else "./" + name + "/")
            item.type, item.mode, item.mtime = tarfile.DIRTYPE, 0o755, epoch
            item.uid = item.gid = 0
            item.uname = item.gname = "root"
            tar.addfile(item)
        for name, contents, mode in sorted(files):
            item = tarfile.TarInfo("./" + name)
            item.size, item.mode, item.mtime = len(contents), mode, epoch
            item.uid = item.gid = 0
            item.uname = item.gname = "root"
            tar.addfile(item, io.BytesIO(contents))
    compressed = io.BytesIO()
    with gzip.GzipFile(filename="", fileobj=compressed, mode="wb", compresslevel=9, mtime=0) as stream:
        stream.write(raw.getvalue())
    return compressed.getvalue()


def verify_elf(binary):
    assert binary[:6] == b"\x7fELF\x02\x01", "Expected little-endian 64-bit ELF"
    assert struct.unpack_from("<H", binary, 18)[0] == 183, "Expected AArch64 ELF"
    phoff = struct.unpack_from("<Q", binary, 32)[0]
    entry_size, entries = struct.unpack_from("<HH", binary, 54)
    assert all(struct.unpack_from("<I", binary, phoff + i * entry_size)[0] != 3 for i in range(entries)), "Binary must be statically linked (no PT_INTERP)"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", default=shutil.which("go") or "go")
    parser.add_argument("--output", type=Path, default=ROOT / "dist")
    parser.add_argument("--allow-dirty", action="store_true", help="Allow a development build from uncommitted sources")
    args = parser.parse_args()
    if not args.allow_dirty and run("git", "status", "--porcelain", "--untracked-files=normal"):
        raise SystemExit("Commit the source changes before building a release (or use --allow-dirty).")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    makefile = (ROOT / "Makefile").read_text(encoding="utf-8")
    version = re.search(r"^PKG_VERSION:=(.+)$", makefile, re.M)[1].strip()
    release = re.search(r"^PKG_RELEASE:=(.+)$", makefile, re.M)[1].strip()
    epoch = int(os.environ.get("SOURCE_DATE_EPOCH") or run("git", "show", "-s", "--format=%ct", "HEAD"))
    commit = run("git", "rev-parse", "HEAD")
    env = os.environ.copy()
    env.update(GOOS="linux", GOARCH="arm64", GOARM64="v8.0", CGO_ENABLED="0")
    executable = output / "UAmask.aarch64_cortex-a53"
    subprocess.run([args.go, "build", "-trimpath", "-buildvcs=false", "-ldflags", "-s -w -buildid= -X main.version=" + version, "-o", str(executable), "./src"], cwd=ROOT, env=env, check=True)
    binary = executable.read_bytes()
    verify_elf(binary)
    mappings = [
        ("usr/bin/UAmask", binary, 0o755),
        ("etc/init.d/UAmask", (ROOT / "files/UAmask.init").read_bytes(), 0o755),
        ("etc/config/UAmask", (ROOT / "files/UAmask.uci").read_bytes(), 0o600),
        ("usr/lib/lua/luci/model/cbi/UAmask.lua", (ROOT / "files/luci/cbi.lua").read_bytes(), 0o644),
        ("usr/lib/lua/luci/controller/UAmask.lua", (ROOT / "files/luci/controller.lua").read_bytes(), 0o644),
        ("usr/share/licenses/UAmask/LICENSE", (ROOT / "LICENSE").read_bytes(), 0o644),
    ]
    for name, contents, _ in mappings[1:]:
        assert b"\r\n" not in contents, "Unix files must use LF: " + name
    control = f"""Package: UAmask
Version: {version}-{release}
Depends: libc, luci-compat
Conflicts: ua3f-tproxy, ua3f-tproxy-ipt, UAmask-ipt
Source: https://github.com/dahaox3/UA-Mask
SourceName: UAmask
License: GPL-3.0-only
LicenseFiles: LICENSE
Section: net
SourceDateEpoch: {epoch}
Maintainer: Zesuy <hongri580@gmail.com>
Architecture: {ARCH}
Installed-Size: {sum(len(contents) for _, contents, _ in mappings)}
Description: UA-Mask transparent HTTP UA proxy with bounded TCP lifecycle.
 Cortex-A53 ARM64 nftables/firewall4 build with idle and queue protection.
""".encode()
    # Same OpenWrt default package hooks and conffile semantics as v0.4.3.
    postinst = b'#!/bin/sh\n[ "${IPKG_NO_SCRIPT}" = "1" ] && exit 0\n[ -s "${IPKG_INSTROOT}/lib/functions.sh" ] || exit 0\n. "${IPKG_INSTROOT}/lib/functions.sh"\ndefault_postinst "$0" "$@"\n'
    prerm = b'#!/bin/sh\n[ -s "${IPKG_INSTROOT}/lib/functions.sh" ] || exit 0\n. "${IPKG_INSTROOT}/lib/functions.sh"\ndefault_prerm "$0" "$@"\n'
    controls = [("control", control, 0o644), ("conffiles", b"/etc/config/UAmask\n", 0o644), ("postinst", postinst, 0o755), ("prerm", prerm, 0o755)]
    package = archive([("debian-binary", b"2.0\n", 0o644), ("control.tar.gz", archive(controls, epoch), 0o644), ("data.tar.gz", archive(mappings, epoch), 0o644)], epoch)
    path = output / f"UAmask_{version}-{release}_{ARCH}.ipk"
    path.write_bytes(package)
    # Independently read back the finished nested package before publishing.
    with tarfile.open(fileobj=io.BytesIO(package)) as outer:
        assert outer.extractfile("./debian-binary").read() == b"2.0\n"
        with tarfile.open(fileobj=io.BytesIO(outer.extractfile("./control.tar.gz").read())) as inner:
            assert inner.extractfile("./conffiles").read() == b"/etc/config/UAmask\n"
            assert f"Architecture: {ARCH}".encode() in inner.extractfile("./control").read()
        with tarfile.open(fileobj=io.BytesIO(outer.extractfile("./data.tar.gz").read())) as inner:
            assert inner.extractfile("./usr/bin/UAmask").read() == binary
            assert inner.getmember("./usr/bin/UAmask").mode == 0o755
    digest = hashlib.sha256(package).hexdigest()
    (output / "SHA256SUMS").write_text(f"{digest}  {path.name}\n", encoding="utf-8", newline="\n")
    info = {"package": path.name, "sha256": digest, "binary_sha256": hashlib.sha256(binary).hexdigest(), "version": version + "-" + release, "architecture": ARCH, "goos": "linux", "goarch": "arm64", "goarm64": "v8.0", "cgo_enabled": False, "go_version": run(args.go, "version"), "commit": commit, "source_date_epoch": epoch}
    (output / "BUILDINFO.json").write_text(json.dumps(info, indent=2) + "\n", encoding="utf-8", newline="\n")
    print(json.dumps(info, indent=2))


if __name__ == "__main__":
    main()
