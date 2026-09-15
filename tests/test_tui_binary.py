"""The `hotseat tui` launcher must find the binary shipped inside platform wheels."""
import hashlib
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from hotseat import __version__, cli


class FindTuiBinary(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.pkg = self.tmp / "site" / "hotseat"
        self.pkg.mkdir(parents=True)
        self.env = {"PATH": str(self.tmp / "empty")}

    def touch(self, path):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("#!/bin/sh\n")
        return str(path)

    def test_nothing_found(self):
        self.assertIsNone(cli.find_tui_binary(self.pkg, self.env))

    def test_bundled_binary_wins_over_checkout_and_path(self):
        bundled = self.touch(self.pkg / "_bin" / "hotseat-tui")
        self.touch(self.pkg.parent / "bin" / "hotseat-tui")
        self.assertEqual(cli.find_tui_binary(self.pkg, self.env), bundled)

    def test_checkout_binary_when_not_bundled(self):
        checkout = self.touch(self.pkg.parent / "bin" / "hotseat-tui")
        self.assertEqual(cli.find_tui_binary(self.pkg, self.env), checkout)

    def test_env_override_beats_everything(self):
        self.touch(self.pkg / "_bin" / "hotseat-tui")
        override = self.touch(self.tmp / "elsewhere" / "tui")
        env = dict(self.env, HOTSEAT_TUI_BIN=override)
        self.assertEqual(cli.find_tui_binary(self.pkg, env), override)

    def test_env_override_ignored_when_missing(self):
        bundled = self.touch(self.pkg / "_bin" / "hotseat-tui")
        env = dict(self.env, HOTSEAT_TUI_BIN=str(self.tmp / "missing"))
        self.assertEqual(cli.find_tui_binary(self.pkg, env), bundled)

    def test_path_fallback(self):
        on_path = self.touch(self.tmp / "pathdir" / "hotseat-tui")
        os.chmod(on_path, 0o755)
        self.assertEqual(cli.find_tui_binary(self.pkg, {"PATH": str(self.tmp / "pathdir")}), on_path)

    def test_version_flag(self):
        with self.assertRaises(SystemExit) as ctx:
            cli.main(["--version"])
        self.assertEqual(ctx.exception.code, 0)


@unittest.skipIf(cli._tui_platform() is None, "no release binary for this platform")
class DownloadTuiBinary(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.env = {"XDG_DATA_HOME": str(self.tmp), "PATH": str(self.tmp / "empty")}
        goos, goarch = cli._tui_platform()
        self.name = f"hotseat-tui-{__version__}-{goos}-{goarch}"
        self.payload = b"#!/bin/sh\necho fake tui\n"
        self.sums = f"{hashlib.sha256(self.payload).hexdigest()}  {self.name}\nabc  other-file\n".encode()

    def fetcher(self, sums=None, payload=None):
        calls = []
        def fetch(url):
            calls.append(url)
            if url.endswith("/SHA256SUMS"):
                return self.sums if sums is None else sums
            return self.payload if payload is None else payload
        fetch.calls = calls
        return fetch

    def test_downloads_verifies_and_caches(self):
        fetch = self.fetcher()
        path = cli.download_tui_binary(self.env, fetch)
        self.assertEqual(Path(path).read_bytes(), self.payload)
        self.assertTrue(os.access(path, os.X_OK))
        self.assertTrue(fetch.calls[0].endswith(f"/v{__version__}/SHA256SUMS"))
        self.assertTrue(fetch.calls[1].endswith(f"/v{__version__}/{self.name}"))
        # The cached copy is what find_tui_binary returns next time.
        pkg = self.tmp / "site" / "hotseat"; pkg.mkdir(parents=True)
        self.assertEqual(cli.find_tui_binary(pkg, self.env), path)

    def test_checksum_mismatch_leaves_nothing(self):
        with self.assertRaises(RuntimeError):
            cli.download_tui_binary(self.env, self.fetcher(payload=b"tampered"))
        cache = self.tmp / "hotseat"
        leftovers = [p for p in cache.rglob("*") if p.is_file()] if cache.exists() else []
        self.assertEqual(leftovers, [])

    def test_missing_from_checksums(self):
        with self.assertRaises(RuntimeError):
            cli.download_tui_binary(self.env, self.fetcher(sums=b"abc  something-else\n"))

    def test_unsupported_platform(self):
        with mock.patch.object(cli, "_tui_platform", return_value=None):
            with self.assertRaises(RuntimeError):
                cli.download_tui_binary(self.env, self.fetcher())


if __name__ == "__main__":
    unittest.main()
