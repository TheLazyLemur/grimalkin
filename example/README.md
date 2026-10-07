# Semantic scouting trial (Jev)

Does a cheap classifier find the files that answer a question about the
codebase, without the main model reading them?

```sh
python3 example/scout_trial.py   # key: JEV_API_KEY=... in ~/.jev
```

`benchmark.json` holds 10 questions about grimalkin, labelled before any Jev
call: `must` files a scout has to surface, `ok` files that are related and
not penalised, and the regex an informed agent might grep first. Each of the
39 tracked source and doc files gets one request asking all 10 questions as
Nouls. Results land in `results/` (`report.json`, `raw.json`).

## Result (jev-1.13.0, 2026-10-07)

39 requests, 120k input tokens, $0.005, 2.2–2.4 s wall at 8 concurrent
requests (p50 0.4 s per request). A second run moved no score by more than
0.09; 2 of 390 answers crossed the 0.5 threshold.

| Question | Jev recall@3 | rg recall@3 | Jev picked (≥0.5) | rg matched |
| --- | --- | --- | --- | --- |
| cancel_kill | 0.33 | 0.33 | 4 | 9 |
| api_key_command | 1.00 | 1.00 | 3 | 7 |
| result_truncation | 1.00 | 1.00 | 5 | 8 |
| markdown_render | 1.00 | 1.00 | 4 | 5 |
| prompt_cache | 1.00 | 1.00 | 1 | 4 |
| skill_arguments | 1.00 | 1.00 | 4 | 4 |
| clipboard_yank | 1.00 | 1.00 | 3 | 6 |
| import_rules | 1.00 | 1.00 | 2 | 34 |
| no_terminal_hang | 1.00 | 0.00 | 3 | 10 |
| skill_dedup | 1.00 | 0.00 | 5 | 10 |

- Jev wins where the code doesn't use the question's words
  (`no_terminal_hang`: `Setsid` never says "password") and where keywords are
  everywhere (`import_rules`: 2 picks against 34 grep hits).
- Jev misses call chains. For `cancel_kill` it found `shell.go` (0.90) but
  scored `core/agent.go` 0.12 and `tui/tui.go` 0.05: each holds one link
  (`Cancel()`, the Ctrl-C key), and judged alone neither "kills the command".
  A per-file question can't see links between files.
- Docs score high: README and specs describe most behaviour, so they crowd
  the top 5 on code questions.

## Caveats

10 questions on one small repo, labelled by the person who wrote the
benchmark, with grep patterns chosen knowing the code. Indicative, not proof.
