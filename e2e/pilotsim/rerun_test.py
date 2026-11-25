"""Unit tests of rerun.py, the upgrade's rerun of join checked against the
home's files (task 01a2113d-fa1e).

    python3 e2e/pilotsim/rerun_test.py
"""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import rerun  # noqa: E402

BEFORE = {"docs/ROLES.md": "a1", "docs/START.md": "b1", ".mcp.json": "c1", "docs/HANDOFF.md": "d1"}


def changes(status, pairs):
    return {"status": status, "changes": [{"path": p, "action": a} for p, a in pairs]}


class Rerun(unittest.TestCase):
    def errs(self, report, after):
        return rerun.check(report, BEFORE, after)[0]

    def test_same_files_ready(self):
        # The previous release's files are this tree's: nothing to update.
        r = changes("ready", [("docs/ROLES.md", "unchanged"), ("docs/START.md", "unchanged"),
                              (".mcp.json (mcpServers.aimem)", "unchanged"), ("docs/HANDOFF.md", "kept")])
        self.assertEqual(self.errs(r, dict(BEFORE)), [])

    def test_older_guidance_updated(self):
        after = dict(BEFORE, **{"docs/ROLES.md": "a2"})
        r = changes("restart_required", [("docs/ROLES.md", "update"), ("docs/START.md", "unchanged")])
        self.assertEqual(self.errs(r, after), [])

    def test_a_new_managed_file(self):
        after = dict(BEFORE, **{".claude/turn-settings.json": "e1"})
        r = changes("restart_required", [(".claude/turn-settings.json", "create")])
        self.assertEqual(self.errs(r, after), [])

    def test_changed_file_reported_unchanged(self):
        after = dict(BEFORE, **{"docs/ROLES.md": "a2"})
        r = changes("restart_required", [("docs/ROLES.md", "unchanged")])
        self.assertTrue(any("reported unchanged, but the file changed" in e for e in self.errs(r, after)))

    def test_update_reported_without_a_change(self):
        r = changes("restart_required", [("docs/ROLES.md", "update")])
        errs = self.errs(r, dict(BEFORE))
        self.assertTrue(any("did not change" in e for e in errs), errs)
        self.assertTrue(any("no file changed: ready" in e for e in errs), errs)

    def test_restart_without_a_change(self):
        r = changes("restart_required", [("docs/ROLES.md", "unchanged")])
        self.assertTrue(any("no file changed" in e for e in self.errs(r, dict(BEFORE))))

    def test_change_without_restart(self):
        after = dict(BEFORE, **{"docs/ROLES.md": "a2"})
        r = changes("ready", [("docs/ROLES.md", "update")])
        self.assertTrue(any("files changed: docs/ROLES.md" in e for e in self.errs(r, after)))

    def test_conflict_fails(self):
        r = changes("ready", [("docs/ROLES.md", "conflict")])
        self.assertTrue(any("a conflict" in e for e in self.errs(r, dict(BEFORE))))

    def test_entry_compared_by_its_file(self):
        after = dict(BEFORE, **{".mcp.json": "c2"})
        r = changes("restart_required", [(".mcp.json (mcpServers.aimem)", "update")])
        self.assertEqual(self.errs(r, after), [])

    def test_listing(self):
        self.assertEqual(rerun.digests("aa  ./docs/ROLES.md\nbb *./x y.md\n\n"),
                         {"docs/ROLES.md": "aa", "x y.md": "bb"})


if __name__ == "__main__":
    unittest.main()
