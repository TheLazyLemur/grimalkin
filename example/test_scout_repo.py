"""Offline checks; do not send data or spend API credit."""
import pathlib
import tempfile
import unittest
from unittest.mock import patch

import scout_repo as scout


class TrialTests(unittest.TestCase):
    def test_chunks_keep_all_content_and_line_ranges(self):
        with patch.object(scout, "CHUNK_CHARS", 10):
            text = "abc\ndefgh\nijkl\nmno"
            chunks = scout.chunks(text)
        self.assertEqual("".join(c[2] for c in chunks), text)
        self.assertEqual(chunks, [(1, 2, "abc\ndefgh\n"), (3, 4, "ijkl\nmno")])
        self.assertTrue(all(len(c[2]) <= 10 for c in chunks))

    def test_long_line_is_not_lost_or_exceeded(self):
        with patch.object(scout, "CHUNK_CHARS", 10):
            text = "x" * 35 + "\n"
            chunks = scout.chunks(text)
        self.assertEqual("".join(c[2] for c in chunks), text)
        self.assertTrue(all(len(c[2]) <= 10 for c in chunks))
        self.assertTrue(all(c[:2] == (1, 1) for c in chunks))

    def test_recall_does_not_count_missing_files(self):
        result = scout.grade(["a", "b"], {"a"}, ["a", "c"])
        self.assertEqual(result["recall@5"], .5)
        self.assertEqual(result["selected_recall"], .5)
        self.assertEqual(result["must_ranks"], {"a": 1, "c": None})

    def test_candidates_exclude_tests_generated_env_and_symlinks(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory).resolve()
            prefix = "src/"
            paths = [prefix + name for name in (
                "client/thing.ts", "client/thing.test.ts", "client/thing.spec.ts",
                "client/__tests__/thing.ts", "client/lib/api/models/Thing.ts",
                "solver/tests/test_thing.py", "X/Generated/X.cs", "X/Migrations/X.cs",
                ".env", "X/appsettings.json", "client/link.ts",
            )]
            for name in paths[:-1]:
                p = root / name
                p.parent.mkdir(parents=True, exist_ok=True)
                p.touch()
            (root / paths[-1]).symlink_to(root / paths[0])
            with patch.object(scout.subprocess, "check_output", return_value="\0".join(paths).encode()):
                self.assertEqual(scout.candidates(root, prefix), [paths[0]])

    def test_rg_errors_are_not_silently_treated_as_no_results(self):
        with patch.object(scout.subprocess, "run") as run:
            run.return_value.returncode = 2
            run.return_value.stderr = "bad expression"
            with self.assertRaisesRegex(RuntimeError, "bad expression"):
                scout.rg_rank(pathlib.Path("."), "[", ["test.py"])

    def test_questions_do_not_leak_labels(self):
        benchmark = {"queries": [{"id": "x", "question": "Where are errors handled?", "must": ["secret-answer.ts"]}]}
        self.assertNotIn("secret-answer", str(scout.questions(benchmark)))


if __name__ == "__main__":
    unittest.main()
