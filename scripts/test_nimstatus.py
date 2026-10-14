#!/usr/bin/env python3
import contextlib
import datetime
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location("nimstatus", Path(__file__).resolve().parents[1] / "nimstatus.py")
status = importlib.util.module_from_spec(spec)
spec.loader.exec_module(status)


class UsageTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / "nim-usage.jsonl"
        self.root = mock.patch.object(status, "ROOT", self.tmp.name)
        self.root.start()
        self.addCleanup(self.root.stop)
        status._jsonl_cache = {"mtime": 0, "size": 0, "out": None}

    def append(self, tokens=3, end="\n"):
        with self.path.open("a") as f:
            f.write(json.dumps({"ts": status.today_str()+"T00:00:00Z", "total_tokens": tokens}) + end)

    def test_append_reads_only_new_records(self):
        self.append()
        self.assertEqual(status.jsonl_stats()["all"], {"req": 1, "tok": 3})
        self.append(5)
        original = json.loads
        with mock.patch.object(status.json, "loads", wraps=original) as loads:
            result = status.jsonl_stats()
        self.assertEqual(result["all"], {"req": 2, "tok": 8})
        self.assertEqual(loads.call_count, 1, "status reread old records after an append")

    def test_partial_write_is_not_counted_until_complete(self):
        self.append(end="")
        self.assertEqual(status.jsonl_stats()["all"]["req"], 0)
        with self.path.open("a") as f:
            f.write("\n")
        self.assertEqual(status.jsonl_stats()["all"]["req"], 1)

    def test_rotation_and_truncation_reset_totals(self):
        self.append(90)
        status.jsonl_stats()
        self.path.rename(self.path.with_suffix(".old"))
        self.append(7)
        self.assertEqual(status.jsonl_stats()["all"], {"req": 1, "tok": 7})
        self.path.write_text("")
        self.assertEqual(status.jsonl_stats()["all"], {"req": 0, "tok": 0})

    def test_midnight_recomputes_today_without_file_change(self):
        self.append()
        status.jsonl_stats()
        with mock.patch.object(status, "today_str", return_value="2099-01-01"):
            result = status.jsonl_stats()
        self.assertEqual(result["all"]["req"], 1)
        self.assertEqual(result["today"]["req"], 0)

    def test_non_object_and_malformed_lines_are_ignored(self):
        self.path.write_text('null\n[]\ninvalid\n')
        self.append()
        self.assertEqual(status.jsonl_stats()["all"]["req"], 1)


class RendererTests(unittest.TestCase):
    def setUp(self):
        self.data = {"version": "test", "uptime": "1h", "concurrent": 30,
                     "concurrent_opencode": 30, "concurrent_nvidia": 0,
                     "sem_limit": 30, "lane_cap": 30,
                     "zen_lanes": [{"id": "shared:%02d" % i, "requests": i,
                                    "rate_limited": 2, "total_tokens": 100,
                                    "avg_ms": 1200, "last_used_ago": "1s"} for i in range(30)],
                     "pool": {"verified": 12, "dropped_total": 3, "last_refresh_ago": "2m"},
                     "opencode": {"models": ["opencode/step-5-preview-free"]},
                     "antseed": {"models": [], "stale_for": "12s"},
                     "usage": {"by_model": {"opencode/step-5-preview-free": {
                         "requests": 10, "prompt_tokens": 100, "completion_tokens": 20,
                         "duration_ms": 10000, "errors": 2}}}}
        self.stats = {"all": {"req": 10, "tok": 120}, "today": {"req": 10, "tok": 120}, "since": ""}

    def test_plain_status_shows_backend_load_and_capacity(self):
        with mock.patch.object(status, "jsonl_stats", return_value=self.stats):
            out = status.plain_status(self.data)
        self.assertIn("opencode 30", out)
        self.assertIn("30/30", out)
        self.assertIn("nim lanes", out)

    def test_rich_status_fits_normal_terminal_and_has_single_model_table(self):
        if not status.RICH:
            self.skipTest("rich optional dependency unavailable")
        from rich.console import Console
        for width in (80, 120):
            out = io.StringIO()
            Console(file=out, width=width, color_system=None).print(status.build_renderable(self.data, self.stats))
            text = out.getvalue()
            self.assertIn("opencode 30", text)
            self.assertIn("nim lanes", text)
            self.assertLess(len(text.splitlines()), 35)
            self.assertTrue(all(len(line) <= width for line in text.splitlines()))

    def test_model_table_shows_full_ids_and_no_backend_column(self):
        if not status.RICH:
            self.skipTest("rich optional dependency unavailable")
        from rich.console import Console
        out = io.StringIO()
        Console(file=out, width=120, color_system=None).print(status.build_renderable(self.data, self.stats))
        text = out.getvalue()
        # owner: the status model table. one column, the id a client sends.
        self.assertIn("opencode/step-5-preview-free", text)
        self.assertIn("antseed/no free models", text)
        self.assertNotIn("backend", text)
        self.assertNotIn("\n step-5-preview-free", text)


if __name__ == "__main__":
    unittest.main()
