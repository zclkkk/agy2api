"""Local checks for the acceptance runner; never accesses an account."""

import contextlib
import io
import threading
import types
import unittest
from unittest.mock import patch

import live_validate


class FakeLive:
    run_case = live_validate.Live.run_case
    requiring = live_validate.Live.requiring
    fail = False

    models = ["gemini-a", "gemini-b"]
    model, other, image_model = "gemini-a", "gemini-b", None

    def __init__(self, *_):
        self.results = []
        self.lock = threading.Lock()

    def __enter__(self):
        return self

    def __exit__(self, *_):
        pass

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

    def run_main(self, only):
        with (
            patch.object(live_validate, "Live", FakeLive),
            patch("sys.argv", ["live_validate.py", "--only", only]),
            contextlib.redirect_stdout(io.StringIO()) as out,
        ):
            return live_validate.main(), out.getvalue()

    def test_cases_the_account_cannot_run_are_skipped(self):
        # FakeLive lists no image model.
        status, out = self.run_main("images/generate")
        self.assertEqual(status, 0)
        self.assertIn('"status": "skipped"', out)
        self.assertIn("no image model", out)

    def test_a_failed_start_is_cleaned_up(self):
        live = live_validate.Live.__new__(live_validate.Live)
        live.requested_model = None
        closed = []

        def listing():
            raise RuntimeError("listing failed")

        live.start = lambda: None
        live.a = types.SimpleNamespace(models=types.SimpleNamespace(list=listing))
        live.close = lambda: closed.append(True)
        with self.assertRaises(RuntimeError):
            live.__enter__()
        self.assertEqual(closed, [True])

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
