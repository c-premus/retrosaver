"""Tests for the GTK-free helpers in retrosaver-settings.

Run with: python3 -m unittest discover -s gui
"""

import importlib.machinery
import importlib.util
import os
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))


def load():
    # The program has no .py extension, as an installed command should not,
    # so it is loaded by path rather than imported.
    path = os.path.join(HERE, "retrosaver-settings")
    loader = importlib.machinery.SourceFileLoader("retrosaver_settings", path)
    spec = importlib.util.spec_from_loader(loader.name, loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


settings = load()


class Minutes(unittest.TestCase):
    def test_to_minutes(self):
        self.assertEqual(settings.to_minutes(300), 5)
        self.assertEqual(settings.to_minutes(90), 2)  # rounds
        self.assertEqual(settings.to_minutes(15), 1)  # never below one
        self.assertEqual(settings.to_minutes(0), 1)

    def test_is_whole_minutes(self):
        self.assertTrue(settings.is_whole_minutes(600))
        self.assertFalse(settings.is_whole_minutes(15))


class SetArgs(unittest.TestCase):
    def test_sorted_and_formatted(self):
        self.assertEqual(
            settings.set_args({"SAVER_DELAY": 600, "EXCLUDE": "a b"}),
            ["config", "set", "EXCLUDE=a b", "SAVER_DELAY=600"],
        )


class Selection(unittest.TestCase):
    names = ["atlantis", "flame", "ifs", "sonar"]

    def test_all_except_unticked(self):
        got = settings.selection_updates(False, {"atlantis", "ifs"}, self.names)
        self.assertEqual(got, {"INCLUDE": "", "EXCLUDE": "flame sonar"})

    def test_only_ticked(self):
        got = settings.selection_updates(True, {"ifs", "atlantis"}, self.names)
        # Discovery order, not set order.
        self.assertEqual(got, {"INCLUDE": "atlantis ifs", "EXCLUDE": ""})

    def test_switching_mode_keeps_the_same_selection(self):
        # Whichever mode writes the file, the modules the daemon may pick are
        # exactly the ticked ones.
        ticked = {"flame", "sonar"}
        for only in (False, True):
            u = settings.selection_updates(only, ticked, self.names)
            include, exclude = u["INCLUDE"].split(), u["EXCLUDE"].split()
            picked = {n for n in self.names
                      if (not include or n in include) and n not in exclude}
            self.assertEqual(picked, ticked, f"only_ticked={only}")


class Preview(unittest.TestCase):
    def test_args(self):
        self.assertEqual(settings.preview_args("atlantis"), ["preview", "atlantis"])

    def test_only_an_early_unexpected_exit_is_a_failure(self):
        self.assertTrue(settings.preview_failed(False, 0.3, False))
        self.assertFalse(settings.preview_failed(True, 0.3, False))   # exited cleanly
        self.assertFalse(settings.preview_failed(False, 0.3, True))   # we stopped it
        self.assertFalse(settings.preview_failed(False, 30, False))   # user closed it


class Service(unittest.TestCase):
    def test_states(self):
        self.assertEqual(settings.service_state("enabled\n", "active\n"), "running")
        self.assertEqual(settings.service_state("enabled\n", "failed\n"), "stopped")
        self.assertEqual(settings.service_state("disabled\n", "inactive\n"), "not-set-up")
        # A missing unit prints nothing on stdout.
        self.assertEqual(settings.service_state("", ""), "not-set-up")


class Search(unittest.TestCase):
    module = {"name": "atlantis", "label": "Atlantis",
              "description": "Sharks, dolphins and whales."}

    def test_matches(self):
        self.assertTrue(settings.matches("", self.module))
        self.assertTrue(settings.matches("ATL", self.module))
        self.assertTrue(settings.matches("dolphin whale", self.module))
        self.assertFalse(settings.matches("dolphin toaster", self.module))


class Binary(unittest.TestCase):
    def test_environment_override(self):
        old = os.environ.get("RETROSAVER")
        try:
            os.environ["RETROSAVER"] = "/tmp/x/retrosaver"
            self.assertEqual(settings.retrosaver_binary(), "/tmp/x/retrosaver")
            del os.environ["RETROSAVER"]
            self.assertEqual(settings.retrosaver_binary(), "retrosaver")
        finally:
            if old is not None:
                os.environ["RETROSAVER"] = old


if __name__ == "__main__":
    unittest.main()
