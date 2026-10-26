import pathlib
import tempfile
import unittest

from config_helpers import read_config, write_config


class TOMLFixtures(unittest.TestCase):
    def test_scalar_config_round_trip(self):
        values = {
            "listen": "127.0.0.1:8080",
            "data_dir": r'C:\用户\path with "quotes"',
            "api_key": "fixture\tkey\nwith\\escapes",
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
