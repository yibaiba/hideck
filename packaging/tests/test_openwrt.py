"""Fast packaging contract checks; full builds use the pinned SDK workflow."""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
PACKAGING = ROOT / "packaging/openwrt"


class OpenWrtTests(unittest.TestCase):
    def test_sdk_matrix_pins_each_release_and_architecture(self):
        targets = json.loads((PACKAGING / "sdk-matrix.json").read_text())["include"]
        self.assertEqual(len({entry["id"] for entry in targets}), 6)
        for entry in targets:
            self.assertRegex(entry["sha256"], r"^[a-f0-9]{64}$")
            self.assertIn(entry["release"], entry["sdk"])
            self.assertIn(entry["target"].replace("/", "-"), entry["sdk"])
        self.assertEqual({entry["arch"] for entry in targets}, {"amd64", "arm64", "armv7"})

    def test_private_adb_is_not_installed_over_system_binary(self):
        recipe = (PACKAGING / "hideck-adb/Makefile").read_text()
        self.assertIn("$(1)/usr/libexec/hideck/adb", recipe)
        self.assertNotIn("$(1)/usr/bin/adb", recipe)
        self.assertNotIn("define Package/hideck-adb/postinst", recipe)
        self.assertRegex(recipe, r"PKG_HASH:=[a-f0-9]{64}\n")
        self.assertIn("Build/Compile/Default,-j$(HIDECK_ADB_JOBS) adb", recipe)
        self.assertIn("CMAKE_BINARY_SUBDIR:=build", recipe)

    def test_sdk_downloads_use_http1_and_verify_before_extraction(self):
        fetch = (PACKAGING / "fetch-sdk.sh").read_text()
        start = fetch.index("curl --http1.1 -sS -fL -C -")
        download = fetch[start:fetch.index('"$source"', start)]
        for option in ("-fL", "-C -", "--proto =https", "--connect-timeout 20", "--speed-limit",
                       "--speed-time", "--max-time"):
            self.assertIn(option, download)
        self.assertNotIn("--insecure", download)
        self.assertNotIn(" -k", download)
        self.assertIn("https://downloads.openwrt.org/releases/*/openwrt-sdk-*.tar.zst", fetch)
        sources = [ROOT / ".github/workflows/binary-release.yml",
                   PACKAGING / "Dockerfile.sdk"]
        for path in sources:
            with self.subTest(source=path.name):
                source = path.read_text()
                self.assertNotRegex(source, r"\bcurl\s+-")
                start = source.index("fetch-")
                verify = source.index("sha256sum -c -", start)
                extract = source.index("tar --zstd -xf", start)
                self.assertLess(verify, extract)

    def test_fetch_sdk_keeps_verified_archive_and_rejects_bad_input(self):
        with tempfile.TemporaryDirectory(prefix="hideck-fetch-sdk-") as directory:
            archive = Path(directory) / "sdk.tar.zst"
            archive.write_bytes(b"cached sdk")
            digest = subprocess.run(["sha256sum", str(archive)], check=True, capture_output=True,
                                    text=True).stdout.split()[0]
            url = "https://downloads.openwrt.org/releases/24.10.8/targets/x86/64/openwrt-sdk-x.tar.zst"
            script = str(PACKAGING / "fetch-sdk.sh")
            result = subprocess.run(["sh", script, url, str(archive), digest],
                                    capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("using verified archive", result.stdout)
            for bad_url, bad_digest in (("https://example.com/openwrt-sdk-x.tar.zst", digest),
                                        (url, "not-a-digest")):
                result = subprocess.run(["sh", script, bad_url, str(archive), bad_digest],
                                        capture_output=True, text=True, timeout=10)
                self.assertNotEqual(result.returncode, 0)

    def test_shell_scripts_parse(self):
        for script in PACKAGING.glob("*.sh"):
            with self.subTest(script=script.name):
                subprocess.run(["sh", "-n", str(script)], check=True, timeout=10)

    def test_rejects_invalid_binary_without_starting_build(self):
        with tempfile.TemporaryDirectory(prefix="hideck-sdk-test-") as directory:
            sdk = Path(directory)
            (sdk / "include").mkdir()
            (sdk / "include/package.mk").touch()
            (sdk / ".config").write_text('CONFIG_ARCH="x86_64"\n')
            binary = sdk / "hideck"
            binary.write_text("not a static ELF executable")
            environment = dict(os.environ, SDK_DIR=directory, HIDECK_BINARY=str(binary),
                               HIDECK_VERSION="v2.1.23", OUTPUT_DIR=str(sdk / "output"))
            result = subprocess.run(["sh", str(PACKAGING / "build-packages.sh")],
                                    env=environment, capture_output=True, text=True, timeout=10)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Expected a static Linux ELF binary", result.stderr)
            self.assertFalse((sdk / "output").exists())

    def test_apk_signature_is_verified_and_private_key_not_exported(self):
        source = (PACKAGING / "sign-apks.sh").read_text()
        self.assertIn('adbsign --allow-untrusted --sign-key "$sign_dir/key.pem"', source)
        self.assertIn('--keys-dir "$OUTPUT_DIR/keys" verify', source)
        self.assertIn('for package in "$@"; do', source)
        self.assertIn('--sign-key "$sign_dir/key.pem" "$package"', source)
        self.assertEqual(source.count("--allow-untrusted"), 1)
        self.assertIn('trap \'rm -f "$sign_dir/key.pem"', source)
        self.assertNotIn('cp "$sign_dir/key.pem"', source)

    def test_prebuilt_binary_requires_its_hash(self):
        recipe = (PACKAGING / "hideck/Makefile").read_text()
        self.assertIn("PKG_HASH:=$(HIDECK_BINARY_SHA256)", recipe)
        self.assertIn("sha256sum -c -", recipe)
        self.assertNotIn("PKG_HASH:=skip", recipe)

    def test_architecture_validation_checks_elf_class_not_filename(self):
        script = (PACKAGING / "build-packages.sh").read_text()
        self.assertIn('file -b "$HIDECK_BINARY"', script)
        self.assertIn(r"arm:*ELF\ 32-bit*ARM*", script)
        self.assertIn(r"aarch64:*ELF\ 64-bit*aarch64*", script)

    def test_release_publishes_dynamic_binary_without_changing_package_input(self):
        release = (ROOT / ".github/workflows/binary-release.yml").read_text()
        packages = (ROOT / ".github/workflows/openwrt-packages.yml").read_text()

        self.assertIn("LINK_MODE=dynamic", release)
        self.assertIn("openwrt_dynamic_${{ matrix.artifact_arch }}", release)
        for sdk_id in ("24.10.8-x86-64", "24.10.8-armsr-armv8", "24.10.8-mvebu-cortexa9"):
            self.assertIn(f"sdk_id: {sdk_id}", release)
        self.assertIn("*-openwrt-linux-musl*-gcc", release)
        self.assertNotIn("zig-linux", release)
        self.assertIn('asset="hideck_${HIDECK_VERSION}_openwrt_${BINARY_ARCH}"', packages)
        self.assertNotIn("openwrt_dynamic_${BINARY_ARCH}", packages)


if __name__ == "__main__":
    unittest.main()
