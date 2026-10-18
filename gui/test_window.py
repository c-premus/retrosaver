"""Window tests for retrosaver-settings: a real GTK window, driven by a script.

They need GTK 4, libadwaita 1.2 or newer, a display (a real session or
`xvfb-run`) and a retrosaver binary new enough to have the `config` command:
$RETROSAVER_BIN, or `retrosaver` on PATH. Without any of these they skip, so
`python3 -m unittest discover -s gui` stays green anywhere.

Nothing here touches the session. Each test gets a scratch XDG_CONFIG_HOME, a
stub `retrosaver` that passes config and list through to the real binary but
only records setup and teardown, and a stub `systemctl` that reports a state
kept in a file.

Run them with:  xvfb-run python3 -m unittest discover -s gui -v
"""

import json
import os
import shutil
import subprocess
import tempfile
import textwrap
import time
import unittest
import warnings

from test_settings import settings

# Adw.MessageDialog is deprecated from libadwaita 1.6, but it is the dialog the
# 1.2 floor has, so the app uses it on purpose. PyGObject's asyncio shims warn
# on Python 3.14. Neither is something these tests can act on.
warnings.filterwarnings("ignore", category=DeprecationWarning, message=r"Adw\.MessageDialog")
warnings.filterwarnings("ignore", category=DeprecationWarning, module=r"gi\.events")

# How long one step may take before the test fails. A save is debounced by
# SAVE_DELAY_MS and then runs a subprocess, so this is generous on purpose: a
# slow runner must not flake, and a hang must still end.
STEP_TIMEOUT_S = 15


def _gtk():
    """Return (Adw, Gio, GLib, Gtk), or a reason string when unusable."""
    try:
        import gi

        gi.require_version("Gtk", "4.0")
        gi.require_version("Adw", "1")
        from gi.repository import Adw, Gio, GLib, Gtk
    except (ImportError, ValueError) as exc:
        return f"GTK 4 / libadwaita bindings unavailable: {exc}"
    if (Adw.get_major_version(), Adw.get_minor_version()) < (1, 2):
        return "libadwaita is older than 1.2, the app's floor"
    if not Gtk.init_check():
        return "no display (run under a session or xvfb-run)"
    return Adw, Gio, GLib, Gtk


def _binary():
    """(path to the real retrosaver binary, None), or (None, a reason)."""
    path = os.environ.get("RETROSAVER_BIN") or shutil.which("retrosaver")
    if not path:
        return None, "no retrosaver binary: set RETROSAVER_BIN"
    with tempfile.TemporaryDirectory() as cfg:
        env = dict(os.environ, XDG_CONFIG_HOME=cfg)
        probe = subprocess.run([path, "config", "show", "--json"],
                               env=env, capture_output=True)
    if probe.returncode != 0:
        return None, f"{path} has no `config show --json`: set RETROSAVER_BIN to a newer build"
    return os.path.abspath(path), None


GTK = _gtk()
if isinstance(GTK, str):
    BINARY, SKIP = None, GTK
else:
    BINARY, SKIP = _binary()

STUB_RETROSAVER = textwrap.dedent("""\
    #!/bin/sh
    # Records setup and teardown instead of running them, and fails on request.
    case "$1" in
      setup|teardown)
        echo "$1" >> "$STUB_DIR/calls"
        if [ -e "$STUB_DIR/fail-$1" ]; then echo "$1 failed (test)" >&2; exit 1; fi
        if [ "$1" = setup ]; then
          echo enabled > "$STUB_DIR/enabled"; echo active > "$STUB_DIR/active"
        else
          echo disabled > "$STUB_DIR/enabled"; echo inactive > "$STUB_DIR/active"
        fi
        exit 0 ;;
      preview)
        if [ -e "$STUB_DIR/fail-preview" ]; then echo "no display (test)" >&2; exit 1; fi
        echo "preview $2 $$" >> "$STUB_DIR/previews"
        exec sleep 60 ;;
      config)
        if [ "$2" = set ] && [ -e "$STUB_DIR/fail-set" ]; then
          echo "config set failed (test)" >&2; exit 1
        fi ;;
    esac
    exec "$REAL_RETROSAVER" "$@"
""")

STUB_SYSTEMCTL = textwrap.dedent("""\
    #!/bin/sh
    # systemctl --user is-enabled|is-active retrosaver.service
    case "$2" in
      is-enabled) cat "$STUB_DIR/enabled"; grep -qx enabled "$STUB_DIR/enabled" ;;
      is-active) cat "$STUB_DIR/active"; grep -qx active "$STUB_DIR/active" ;;
      *) exit 1 ;;
    esac
""")


@unittest.skipIf(SKIP, SKIP)
class WindowTest(unittest.TestCase):
    def setUp(self):
        self.Adw, self.Gio, self.GLib, self.Gtk = GTK
        self.App, self.Window = settings.gtk_classes(*GTK)

        self.tmp = tempfile.mkdtemp(prefix="retrosaver-gui-test-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.stub = os.path.join(self.tmp, "stub")
        os.mkdir(self.stub)
        for name, body in (("retrosaver", STUB_RETROSAVER), ("systemctl", STUB_SYSTEMCTL)):
            path = os.path.join(self.stub, name)
            with open(path, "w") as f:
                f.write(body)
            os.chmod(path, 0o755)
        self.set_service("enabled", "active")

        self.config_dir = os.path.join(self.tmp, "config")
        self.conf = os.path.join(self.config_dir, "retrosaver", "retrosaver.conf")
        old = dict(os.environ)
        self.addCleanup(lambda: (os.environ.clear(), os.environ.update(old)))
        os.environ.update(
            XDG_CONFIG_HOME=self.config_dir,
            RETROSAVER=os.path.join(self.stub, "retrosaver"),
            REAL_RETROSAVER=BINARY,
            STUB_DIR=self.stub,
            PATH=self.stub + os.pathsep + os.environ.get("PATH", ""),
        )

        names = [m["name"] for m in json.loads(self.real("list", "--all", "--json"))]
        if len(names) < 3:
            self.skipTest("fewer than three display modules installed")
        self.names = names

    # ---- helpers --------------------------------------------------------

    def real(self, *args):
        return subprocess.run([BINARY, *args], check=True,
                              capture_output=True, text=True).stdout

    def set_service(self, enabled, active):
        for name, value in (("enabled", enabled), ("active", active)):
            with open(os.path.join(self.stub, name), "w") as f:
                f.write(value + "\n")

    def calls(self):
        try:
            with open(os.path.join(self.stub, "calls")) as f:
                return f.read().split()
        except FileNotFoundError:
            return []

    def write_config(self, text):
        os.makedirs(os.path.dirname(self.conf), exist_ok=True)
        with open(self.conf, "w") as f:
            f.write(text)

    def config(self):
        try:
            with open(self.conf) as f:
                return f.read()
        except FileNotFoundError:
            return ""

    def has_line(self, line):
        return lambda: line in self.config().splitlines()

    def dialog(self, heading):
        """The open message dialog with this heading, if any."""
        for w in self.Gtk.Window.list_toplevels():
            if isinstance(w, self.Adw.MessageDialog) and w.get_heading() == heading:
                return w
        return None

    def drive(self, scenario):
        """Run the app and step through scenario(window).

        The scenario is a generator. Each value it yields is a condition to
        wait for; it resumes once the condition holds. A condition still false
        after STEP_TIMEOUT_S, or any exception, fails the test and quits the
        loop, so a test can fail but cannot hang.
        """
        GLib = self.GLib
        state = {"error": None, "window": None}
        test = self
        app = self.App()
        app.set_flags(self.Gio.ApplicationFlags.NON_UNIQUE)

        def finish(error=None):
            state["error"] = state["error"] or error
            app.quit()
            return GLib.SOURCE_REMOVE

        def activate(application):
            window = test.Window(application)
            window.present()
            state["window"] = window
            steps = scenario(window)
            current = {"cond": None, "deadline": 0}

            def advance():
                try:
                    current["cond"] = next(steps)
                    current["deadline"] = time.monotonic() + STEP_TIMEOUT_S
                    return True
                except StopIteration:
                    return False

            def tick():
                try:
                    if current["cond"] is None or current["cond"]():
                        if not advance():
                            return finish()
                    elif time.monotonic() > current["deadline"]:
                        code = getattr(current["cond"], "__code__", None)
                        where = f" (line {code.co_firstlineno})" if code else ""
                        return finish(AssertionError(
                            f"timed out after {STEP_TIMEOUT_S}s waiting for a condition{where}"))
                except Exception as exc:  # noqa: BLE001 - re-raised below
                    return finish(exc)
                return GLib.SOURCE_CONTINUE

            GLib.timeout_add(50, tick)

        # PyGObject prints an exception raised in a signal handler or other
        # callback and carries on, so a broken callback would leave the test
        # green. Route those to the test instead.
        import sys
        previous_hook = sys.excepthook
        sys.excepthook = lambda kind, value, tb: state.update(error=state["error"] or value)
        self.addCleanup(setattr, sys, "excepthook", previous_hook)

        # The class's own do_activate would make a second window.
        app.do_activate = lambda: None
        app.connect("activate", activate)
        app.run([])
        if state["error"] is not None:
            raise state["error"]

    @staticmethod
    def loaded(w):
        return lambda: w.outer.get_visible_child_name() == "pages" and w.service is not None

    # ---- the General page -----------------------------------------------

    def test_timings_and_monitors_write_only_what_changed(self):
        self.write_config("# my note\nSAVER_DELAY=300\nFUTURE_KEY=kept\n")

        def scenario(w):
            yield self.loaded(w)
            w.saver_spin.set_value(10)
            w.cycle_row.set_enable_expansion(False)
            w.monitors_row.set_selected(1)
            yield self.has_line("SAVER_DELAY=600")
            yield self.has_line("CYCLE_AFTER=0")
            yield self.has_line("MONITORS=different")
            text = self.config()
            self.assertIn("# my note", text)
            self.assertIn("FUTURE_KEY=kept", text)
            self.assertNotIn("LOCK_AFTER", text, "an untouched key was written")

            w.lock_row.set_enable_expansion(False)
            self.assertFalse(w.blank_row.get_sensitive(), "blank stays usable without lock")
            yield self.has_line("LOCK_AFTER=0")
            w.lock_row.set_enable_expansion(True)
            self.assertTrue(w.blank_row.get_sensitive())
            # Back on with the spin button's value, which defaults to 15 minutes.
            yield self.has_line("LOCK_AFTER=900")

        self.drive(scenario)

    def test_a_value_in_seconds_is_shown_and_left_alone(self):
        self.write_config("LOCK_AFTER=500\n")

        def scenario(w):
            yield self.loaded(w)
            inner = w.lock_spin.get_ancestor(self.Adw.ActionRow)
            self.assertIn("500", inner.get_subtitle())
            self.assertEqual(w.lock_spin.get_value(), 8)
            w.saver_spin.set_value(4)
            yield self.has_line("SAVER_DELAY=240")
            self.assertIn("LOCK_AFTER=500", self.config().splitlines())

        self.drive(scenario)

    def test_a_change_just_before_closing_is_saved(self):
        def scenario(w):
            yield self.loaded(w)
            w.saver_spin.set_value(7)
            w.close()  # inside the save delay
            yield lambda: True

        self.drive(scenario)
        self.assertIn("SAVER_DELAY=420", self.config().splitlines())

    # ---- the Modules page -----------------------------------------------

    def test_both_modes_select_exactly_the_ticked_modules(self):
        keep = set(self.names[:2])

        def scenario(w):
            yield self.loaded(w)
            w.on_tick_all(None, False)
            for name in keep:
                w.checks[name].set_active(True)
            self.assertIn("2 of", w.count_label.get_text())
            yield lambda: set(self.real("list").split()) == keep
            self.assertIn('INCLUDE=""', self.config())

            w.mode_row.set_selected(1)
            yield self.has_line('EXCLUDE=""')
            self.assertEqual(set(self.real("list").split()), keep)

        self.drive(scenario)

    def test_search_and_the_empty_selection_warning(self):
        target = self.names[-1]

        def scenario(w):
            yield self.loaded(w)
            w.search.set_text(target)
            # The entry applies the filter after GTK's search delay, so wait
            # for the filtered list rather than reading it straight away.
            index = self.names.index(target)
            yield lambda: (
                len(self.visible(w)) < len(self.names)
                and index in [r.get_index() for r in self.visible(w)])

            w.on_tick_all(None, False)
            self.assertIn("No modules", w.count_label.get_text())

        self.drive(scenario)

    @staticmethod
    def visible(w):
        rows, row = [], w.list.get_first_child()
        while row is not None:
            if row.get_child_visible():
                rows.append(row)
            row = row.get_next_sibling()
        return rows

    # ---- preview --------------------------------------------------------

    def previews(self):
        """[(module, pid)] for every preview the stub has started."""
        try:
            with open(os.path.join(self.stub, "previews")) as f:
                return [(line.split()[1], int(line.split()[2])) for line in f]
        except FileNotFoundError:
            return []

    @staticmethod
    def alive(pid):
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            return False
        # A reaped child is gone; an unreaped one shows as a zombie.
        with open(f"/proc/{pid}/stat") as f:
            return f.read().split(") ", 1)[1][0] != "Z"

    def gone(self, pid):
        return lambda: not os.path.exists(f"/proc/{pid}") or not self.alive(pid)

    def test_preview_starts_stops_and_switches(self):
        a, b = self.names[0], self.names[1]

        def scenario(w):
            yield self.loaded(w)
            play_a, play_b = w.play_buttons[a], w.play_buttons[b]
            play_a.emit("clicked")
            yield lambda: len(self.previews()) == 1
            name, pid_a = self.previews()[0]
            self.assertEqual(name, a)
            self.assertTrue(self.alive(pid_a))
            self.assertEqual(play_a.get_icon_name(), "media-playback-stop-symbolic")

            # Another module's button stops the first and starts the second.
            play_b.emit("clicked")
            yield lambda: len(self.previews()) == 2
            yield self.gone(pid_a)
            name, pid_b = self.previews()[1]
            self.assertEqual(name, b)
            yield lambda: play_a.get_icon_name() == "media-playback-start-symbolic"
            self.assertEqual(play_b.get_icon_name(), "media-playback-stop-symbolic")

            # The same button again stops it, and lets the screensaver back
            # at once rather than whenever the process gets round to exiting.
            # (Under Xvfb there is no session manager, so the cookie is 0
            # throughout; on a real session this is a live inhibitor.)
            running = w.preview
            play_b.emit("clicked")
            self.assertEqual(running["cookie"], 0, "the idle inhibitor outlived Stop")
            yield self.gone(pid_b)
            yield lambda: play_b.get_icon_name() == "media-playback-start-symbolic"
            self.assertIsNone(w.preview)

        self.drive(scenario)

    def test_closing_the_window_stops_the_preview(self):
        pids = []

        def scenario(w):
            yield self.loaded(w)
            w.play_buttons[self.names[0]].emit("clicked")
            yield lambda: self.previews()
            pids.append(self.previews()[0][1])
            w.close()
            yield lambda: True

        self.drive(scenario)
        deadline = time.monotonic() + STEP_TIMEOUT_S
        while not self.gone(pids[0])() and time.monotonic() < deadline:
            time.sleep(0.05)
        self.assertTrue(self.gone(pids[0])(), "the preview outlived the window")

    def test_a_preview_that_cannot_start_is_reported(self):
        open(os.path.join(self.stub, "fail-preview"), "w").close()
        toasts = []

        def scenario(w):
            yield self.loaded(w)
            original = w.toast
            w.toast = lambda message: (toasts.append(message), original(message))
            play = w.play_buttons[self.names[0]]
            play.emit("clicked")
            yield lambda: toasts
            self.assertIn("no display (test)", toasts[0])
            yield lambda: play.get_icon_name() == "media-playback-start-symbolic"
            self.assertIsNone(w.preview)

        self.drive(scenario)

    # ---- the service switch ---------------------------------------------

    def test_turning_the_screensaver_on_runs_setup(self):
        self.set_service("disabled", "inactive")

        def scenario(w):
            yield self.loaded(w)
            self.assertFalse(w.service_switch.get_active())
            self.assertEqual(w.service, "not-set-up")
            w.service_switch.set_active(True)
            yield lambda: w.service == "running"
            self.assertEqual(self.calls(), ["setup"])

        self.drive(scenario)

    def test_turning_it_off_asks_first(self):
        def scenario(w):
            yield self.loaded(w)
            w.service_switch.set_active(False)
            yield lambda: self.dialog("Turn off the screensaver?") is not None
            self.dialog("Turn off the screensaver?").response("cancel")
            yield lambda: w.service_switch.get_active()
            self.assertEqual(self.calls(), [], "Cancel still ran something")

            w.service_switch.set_active(False)
            yield lambda: self.dialog("Turn off the screensaver?") is not None
            self.dialog("Turn off the screensaver?").response("off")
            yield lambda: w.service == "not-set-up"
            self.assertEqual(self.calls(), ["teardown"])
            self.assertFalse(w.service_switch.get_active())

        self.drive(scenario)

    def test_a_failed_setup_is_reported_and_the_switch_reverts(self):
        self.set_service("disabled", "inactive")
        open(os.path.join(self.stub, "fail-setup"), "w").close()

        def scenario(w):
            yield self.loaded(w)
            w.service_switch.set_active(True)
            yield lambda: self.dialog("Setup failed") is not None
            self.assertIn("setup failed (test)", self.dialog("Setup failed").get_body())
            yield lambda: not w.service_switch.get_active() and w.service_switch.get_sensitive()

        self.drive(scenario)

    # ---- failures -------------------------------------------------------

    def test_a_failed_save_is_reported_and_reloads(self):
        self.write_config("SAVER_DELAY=300\n")
        toasts = []

        def scenario(w):
            yield self.loaded(w)
            original = w.toast
            w.toast = lambda message: (toasts.append(message), original(message))
            open(os.path.join(self.stub, "fail-set"), "w").close()
            w.saver_spin.set_value(9)
            yield lambda: toasts
            self.assertIn("config set failed (test)", toasts[0])
            self.assertEqual(self.config(), "SAVER_DELAY=300\n")
            # Reloaded from the file: the control shows what the file holds.
            yield self.loaded(w)
            yield lambda: w.saver_spin.get_value() == 5

        self.drive(scenario)

    def test_a_broken_file_is_shown_and_never_overwritten(self):
        self.write_config("# mine\nMONITORS=both\n")

        def scenario(w):
            yield lambda: "could not be read" in (w.status.get_title() or "")
            self.assertIn("MONITORS", w.status.get_description())
            self.assertEqual(w.outer.get_visible_child_name(), "status")
            self.write_config("# mine\nSAVER_DELAY=120\n")
            w.load()
            yield self.loaded(w)
            self.assertEqual(w.saver_spin.get_value(), 2)

        self.drive(scenario)
        self.assertEqual(self.config(), "# mine\nSAVER_DELAY=120\n")


if __name__ == "__main__":
    unittest.main()
