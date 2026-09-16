from __future__ import annotations

import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import skill_scaffold


class SkillScaffoldTests(unittest.TestCase):
    def test_build_entry_enforces_portable_limits(self) -> None:
        entry = skill_scaffold.build_entry("a" * 64, "é" * 1024, ["workflow"])
        self.assertIn("      claude:\n        enabled: true\n        type: skill\n", entry)
        for name in ["", "../escape", "-name", "name-", "two--hyphens", "a" * 65]:
            with self.subTest(name=name), self.assertRaises(SystemExit):
                skill_scaffold.build_entry(name, "Description", ["workflow"])
        with self.assertRaisesRegex(SystemExit, "1025 characters; maximum is 1024"):
            skill_scaffold.build_entry("example", "é" * 1025, ["workflow"])

    def test_default_description_is_nonempty(self) -> None:
        entry = skill_scaffold.build_entry("example", "   ", ["workflow"])
        self.assertIn("Scaffolded Loom skill for example workflows.", entry)

    def test_dry_run_and_invalid_apply_preserve_registry(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            registry = root / "mcp" / "context" / "skills-registry.yaml"
            registry.parent.mkdir(parents=True)
            original = "version: 1\nupdated: 2026-01-01\nskills:\n"
            registry.write_text(original, encoding="utf-8")
            command = [sys.executable, "-B", str(Path(skill_scaffold.__file__)), "--root", str(root)]
            for args, expected_code in [
                (["--name", "Example Skill"], 0),
                (["--name", "a" * 65, "--apply"], 1),
                (["--name", "example", "--description", "é" * 1025, "--apply"], 1),
            ]:
                with self.subTest(args=args):
                    result = subprocess.run(command + args, capture_output=True, text=True, timeout=10)
                    self.assertEqual(result.returncode, expected_code, result.stderr)
                    self.assertEqual(registry.read_text(encoding="utf-8"), original)
                    self.assertFalse((root / "mcp" / "skills").exists())


if __name__ == "__main__":
    unittest.main()
