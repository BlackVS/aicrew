"""Unit tests of cycle.py's rule for who submits (task 01a2113d-fa1e).

    python3 e2e/pilotsim/cycle_test.py
"""
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import cycle  # noqa: E402

# A copy of v0.11.3's worker guidance on submitting, which predates
# `forge pr review` (#285): the worker submits itself.
EARLIER_ROLES = """## Work turn

- Submit the pull request with `aicrew-agent step submit` once its gates are green.
"""

# This tree's and v0.11.5's wording, as the release's join writes it.
LAUNCHER_ROLES = """## Work turn

- Post your own review with `aicrew-agent forge pr` (%s), and leave the
  submit to the launcher where gates-green is in the set.
""" % cycle.REVIEW_GUIDANCE

BUILD = "1111111111111111111111111111111111111111"
RELEASE = "6c2aeb51fbc41abb2efd553af37ac76da659b62f"


def home(root, roles, commit, version, gates_green="release"):
    """A cycle's submit snapshot with the worker's check report and
    docs/ROLES.md; gates_green is the automation's source, or None for a
    report that lists no gates-green."""
    snap = os.path.join(root, "cycle-1", "submit")
    worker = os.path.join(snap, "homes", "worker")
    os.makedirs(worker)
    entries = [{"name": "review-request", "source": "release"}]
    if gates_green is not None:
        entries.append({"name": "gates-green", "source": gates_green})
    rep = {"aicrew_agent": {"version": version, "commit": commit}, "automations": {"entries": entries}}
    with open(os.path.join(worker, "check.json"), "w", encoding="utf-8") as f:
        json.dump(rep, f)
    if roles is not None:
        with open(os.path.join(worker, "ROLES.md"), "w", encoding="utf-8") as f:
            f.write(roles)
    return snap


class LauncherSubmits(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp()
        os.environ["PILOTSIM_BUILD_COMMIT"] = BUILD

    def tearDown(self):
        import shutil
        shutil.rmtree(self.root, ignore_errors=True)

    def check(self, snap, case, want, why=""):
        got, reason = cycle.launcher_submits(snap, case)
        self.assertEqual((got, reason == ""), (want, why == ""), reason)
        if why:
            self.assertIn(why, reason)

    def test_earlier_release_submits_itself(self):
        # v0.11.3: gates-green is in its set, but its guidance has the
        # worker submit.
        self.check(home(self.root, EARLIER_ROLES, "a" * 40, "v0.11.3"), "", False)

    def test_previous_release_leaves_it_to_the_launcher(self):
        # v0.11.5, the release the upgrade scenario starts from: not this
        # tree's build, and the launcher submits.
        self.check(home(self.root, LAUNCHER_ROLES, RELEASE, "v0.11.5"), "", True)

    def test_this_build_leaves_it_to_the_launcher(self):
        self.check(home(self.root, LAUNCHER_ROLES, BUILD, "v0.11.5-dev"), "", True)

    def test_f1_submits_itself(self):
        self.check(home(self.root, LAUNCHER_ROLES, BUILD, "v0.11.5-dev"), "F1", False)

    def test_gates_green_disabled_or_absent(self):
        self.check(home(os.path.join(self.root, "a"), LAUNCHER_ROLES, BUILD, "x", gates_green="disabled"), "", False)
        self.check(home(os.path.join(self.root, "b"), LAUNCHER_ROLES, BUILD, "x", gates_green=None), "", False)

    def test_no_guidance_read_is_unknown(self):
        self.check(home(self.root, None, RELEASE, "v0.11.5"), "", False, why="no docs/ROLES.md of the worker")

    def test_a_later_snapshot_reads_the_submit_snapshot(self):
        # A step after the submit (here the cycle's done) reads the worker
        # as the submit snapshot saw it, as after an upgrade.
        home(self.root, LAUNCHER_ROLES, RELEASE, "v0.11.5")
        done = os.path.join(self.root, "cycle-1", "done")
        os.makedirs(done)
        self.check(done, "", True)


if __name__ == "__main__":
    unittest.main()
