"""Unit tests of rerun.py, the upgrade's rerun of join checked against the
home's files (task 01a2113d-fa1e).

    python3 e2e/pilotsim/rerun_test.py

The fixtures in testdata/rerun are real: the worker's report of
`aicrew-agent join --json` and its home listings before and after, from the
pilot-sim-upgrade (same.*, from v0.11.5) and the below-minimum run (older.*,
from v0.11.0) of this tree's join, reduced to the fields rerun.py reads.
Each other case edits one of them the way the product would differ.
"""
import json
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import rerun  # noqa: E402


def fixture(name):
    base = os.path.join(HERE, "testdata", "rerun", name)
    with open(base + ".json", encoding="utf-8") as f:
        rep = json.load(f)
    with open(base + ".before", encoding="utf-8") as f:
        before = rerun.digests(f.read())
    with open(base + ".after", encoding="utf-8") as f:
        after = rerun.digests(f.read())
    return rep, before, after


def row(rep, path, where="changes"):
    rows = rep["check"]["wiring"] if where == "wiring" else rep["changes"]
    return next(c for c in rows if c["path"] == path)


MCP = ".mcp.json (mcpServers.aimem)"


class Rerun(unittest.TestCase):
    def errs(self, rep, before, after):
        return rerun.check(rep, before, after)[0]

    def assertFails(self, errs, text):
        self.assertTrue(any(text in e for e in errs), errs)

    def test_real_same(self):
        self.assertEqual(self.errs(*fixture("same")), [])

    def test_real_older(self):
        self.assertEqual(self.errs(*fixture("older")), [])

    def test_omitted_row(self):
        rep, before, after = fixture("older")
        rep["changes"] = [c for c in rep["changes"] if c["path"] != "docs/ROLES.md"]
        self.assertFails(self.errs(rep, before, after), "docs/ROLES.md changed, but no row")

    def test_no_rows_at_all(self):
        rep, before, after = fixture("older")
        rep["changes"], rep["status"] = [], "ready"
        self.assertFails(self.errs(rep, before, after), "docs/ROLES.md changed, but no row")

    def test_wiring_only_update(self):
        rep, before, after = fixture("same")
        row(rep, MCP, "wiring")["action"] = "update"
        after = dict(after, **{".mcp.json": "0" * 64})
        rep["status"] = "restart_required"
        self.assertEqual(self.errs(rep, before, after), [])
        rep["status"] = "ready"
        self.assertFails(self.errs(rep, before, after), "replaced or rewired: " + MCP)

    def test_wiring_create_in_an_existing_file(self):
        rep, before, after = fixture("same")
        row(rep, MCP, "wiring")["action"] = "create"
        after = dict(after, **{".mcp.json": "0" * 64})
        rep["status"] = "restart_required"
        self.assertEqual(self.errs(rep, before, after), [])

    def test_wiring_conflict(self):
        rep, before, after = fixture("same")
        row(rep, MCP, "wiring")["action"] = "conflict"
        self.assertFails(self.errs(rep, before, after), "check.wiring " + MCP + ": a conflict")

    def test_wiring_changed_unreported(self):
        rep, before, after = fixture("same")
        after = dict(after, **{".mcp.json": "0" * 64})
        self.assertFails(self.errs(rep, before, after), ".mcp.json changed, but no row")

    def test_create_alone_is_ready(self):
        rep, before, after = fixture("same")
        del before[".claude/commands/crew-handoff.md"]
        row(rep, ".claude/commands/crew-handoff.md")["action"] = "create"
        self.assertEqual(self.errs(rep, before, after), [])
        rep["status"] = "restart_required"
        self.assertFails(self.errs(rep, before, after), "nothing was replaced or rewired: ready")

    def test_create_of_a_file_that_was_there(self):
        rep, before, after = fixture("same")
        row(rep, "AGENTS.md")["action"] = "create"
        self.assertFails(self.errs(rep, before, after), "the file was not there before")

    def test_update_without_a_change(self):
        rep, before, _ = fixture("older")
        self.assertFails(self.errs(rep, before, dict(before)), "the file was there and changed")

    def test_unchanged_but_changed(self):
        rep, before, after = fixture("same")
        after = dict(after, **{"AGENTS.md": "0" * 64})
        self.assertFails(self.errs(rep, before, after), "reported unchanged, but not so")

    def test_kept_handoff_changed(self):
        rep, before, after = fixture("same")
        after = dict(after, **{"docs/HANDOFF.md": "0" * 64})
        self.assertFails(self.errs(rep, before, after), "reported kept, but not so")

    def test_file_conflict(self):
        rep, before, after = fixture("same")
        row(rep, "docs/ROLES.md")["action"] = "conflict"
        self.assertFails(self.errs(rep, before, after), "changes docs/ROLES.md: a conflict")

    def test_missing_and_unknown_action(self):
        for action in (None, "replaced"):
            rep, before, after = fixture("same")
            r = row(rep, "AGENTS.md")
            if action is None:
                del r["action"]
            else:
                r["action"] = action
            self.assertFails(self.errs(rep, before, after), "is not one join reports there")

    def test_wiring_takes_no_kept(self):
        rep, before, after = fixture("same")
        row(rep, MCP, "wiring")["action"] = "kept"
        self.assertFails(self.errs(rep, before, after), "is not one join reports there")

    def test_agent_json_and_aimem_are_not_guidance(self):
        rep, before, after = fixture("same")
        after = dict(after, **{"agent.json": "0" * 64, "aimem/hub.json": "1" * 64})
        self.assertEqual(self.errs(rep, before, after), [])

    def test_listing(self):
        self.assertEqual(rerun.digests("aa  ./docs/ROLES.md\nbb *./x y.md\n\n"),
                         {"docs/ROLES.md": "aa", "x y.md": "bb"})


if __name__ == "__main__":
    unittest.main()
