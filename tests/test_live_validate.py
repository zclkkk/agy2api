"""Local checks for the acceptance runner; never accesses an account."""

import contextlib
import io
import threading
import unittest
from unittest.mock import patch

import live_validate


class FakeLive:
    run_case = live_validate.Live.run_case
    fail = False

    models = ["gemini-a", "gemini-b"]
    model = "gemini-a"

    def __init__(self, *_):
        self.results = []
        self.lock = threading.Lock()

    def __getattr__(self, _):
        return lambda *args, **kwargs: None

    def negative(self):
        if self.fail:
            raise AssertionError("synthetic acceptance failure")


class AcceptanceExitStatus(unittest.TestCase):
    def test_arithmetic_checks_accept_latex_grouping(self):
        for text in ["34,013", r"\mathbf{34{,}013}", r"34\,013", "34013"]:
            with self.subTest(text=text):
                self.assertIn("34013", live_validate.ungroup_numbers(text))
        self.assertNotIn("34013", live_validate.ungroup_numbers(r"34{,}103"))

    def test_failed_cases_produce_a_nonzero_exit_status(self):
        for failed, expected in [(False, 0), (True, 1)]:
            with self.subTest(failed=failed):
                with (
                    patch.object(FakeLive, "fail", failed),
                    patch.object(live_validate, "Live", FakeLive),
                    patch("sys.argv", ["live_validate.py", "--only", "http/negative"]),
                    contextlib.redirect_stdout(io.StringIO()),
                ):
                    self.assertEqual(live_validate.main(), expected)


if __name__ == "__main__":
    unittest.main()
