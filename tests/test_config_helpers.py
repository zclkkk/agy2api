import json
import pathlib
import tempfile
import unittest

from config_helpers import account_id, read_config, write_account, write_config


class TOMLFixtures(unittest.TestCase):
    def test_scalar_config_round_trip(self):
        values = {
            "listen": "127.0.0.1:8080",
            "data_dir": r'C:\用户\path with "quotes"',
            "escaped": "fixture\tvalue\nwith\\escapes",
            "proxy_url": "http://localhost:8081",
            "max_body_bytes": 33_554_432,
            "sanitize_client_markers": False,
        }
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "config.toml"
            write_config(path, values)
            self.assertEqual(read_config(path), values)

    def test_rejects_non_scalar_fixture(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "config.toml"
            with self.assertRaises(TypeError):
                write_config(path, {"nested": {"value": 1}})


class AccountFixtures(unittest.TestCase):
    def test_account_layout_matches_the_service(self):
        # internal/account pins the same value for this Google account ID.
        self.assertEqual(account_id("104"), "7d63f052")
        with tempfile.TemporaryDirectory() as directory:
            data = pathlib.Path(directory)
            account = write_account(
                data, "104", "key", {"project_id": "p"}, "a@example.com"
            )
            self.assertEqual(account, data / "accounts" / "7d63f052")
            info = json.loads((account / "account.json").read_text())
            self.assertEqual((info["api_key"], info["user_id"]), ("key", "104"))
            for path in (
                account,
                account / "account.json",
                account / "credentials.json",
            ):
                self.assertEqual(
                    path.stat().st_mode & 0o777, 0o700 if path.is_dir() else 0o600
                )
