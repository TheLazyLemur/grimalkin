#!/usr/bin/env python3
"""Semantic scouting trial: Jev vs ripgrep on grimalkin's own files.

For each candidate file, one Jev request asks every benchmark question as a
Noul (speculative fan-out). Files are ranked per question by the Noul value
and compared with a ripgrep baseline ranked by match count.

Usage: python3 example/scout_trial.py   (from the repo root)
Key: JEV_API_KEY=... in ~/.jev. Never printed.
"""
import concurrent.futures as cf
import json
import pathlib
import subprocess
import sys
import time
import urllib.error
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parent.parent
OUT = ROOT / "example" / "results"
API = "https://api.typesafe.ai/v1/systemone"
MODEL = "jev-latest"
PRICE_PER_MTOK = 0.042  # input only; output is free (docs.typesafe.ai/models)
THRESHOLD = 0.5
WORKERS = 8
EXCLUDE_PREFIXES = (".tasks/", ".claude/", "example/")
EXCLUDE_FILES = {"go.sum", "go.mod", ".gitignore", "docs/specs/.gitkeep"}


def api_key():
    for line in (pathlib.Path.home() / ".jev").read_text().splitlines():
        if line.startswith("JEV_API_KEY="):
            return line.split("=", 1)[1].strip().strip('"')
    sys.exit("no JEV_API_KEY= line in ~/.jev")


def candidates():
    files = subprocess.run(["git", "ls-files"], cwd=ROOT, capture_output=True, text=True, check=True).stdout.split()
    return [f for f in files if not f.startswith(EXCLUDE_PREFIXES) and f not in EXCLUDE_FILES]


def questions(bench):
    return {
        q["id"]: {
            "type": "noul",
            "instructions": {
                "task": q["question"],
                "question": "Does the file in `content` (at `path`) itself implement or define what `task` asks about?",
            },
            "criteria": {
                "true": "The file contains the code, script or configuration that does this, or is the main documentation describing it.",
                "false": "The file does not do this: it is unrelated, or only mentions the topic in passing.",
            },
        }
        for q in bench["queries"]
    }


def ask(key, path, qs):
    body = json.dumps({
        "model": MODEL,
        "state": {"path": path, "content": (ROOT / path).read_text()},
        "questions": qs,
    }).encode()
    for attempt in range(6):
        req = urllib.request.Request(API, body, {"Authorization": f"Bearer {key}", "Content-Type": "application/json"})
        start = time.monotonic()
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                data = json.load(resp)
            data["_latency_s"] = time.monotonic() - start
            data["_attempts"] = attempt + 1
            return path, data
        except urllib.error.HTTPError as e:
            if e.code in (429, 529) and attempt < 5:
                time.sleep(float(e.headers.get("retry-after") or 2 ** attempt))
                continue
            raise RuntimeError(f"{path}: HTTP {e.code}: {e.read().decode()[:500]}") from None


def rg_rank(pattern, files):
    out = subprocess.run(["rg", "-c", "-e", pattern, "--", *files], cwd=ROOT, capture_output=True, text=True).stdout
    counts = {}
    for line in out.splitlines():
        f, n = line.rsplit(":", 1)
        counts[f] = int(n)
    return sorted(counts, key=lambda f: (-counts[f], f)), counts


def grade(ranked, selected, q):
    must, ok = set(q["must"]), set(q["ok"])
    ranks = {f: (ranked.index(f) + 1 if f in ranked else None) for f in q["must"]}
    def recall(k):
        return sum(1 for f in q["must"] if f in ranked[:k]) / len(must)
    return {
        "must_ranks": ranks,
        "recall@3": recall(3),
        "recall@5": recall(5),
        "selected": len(selected),
        "selected_recall": sum(1 for f in must if f in selected) / len(must),
        "selected_noise": sorted(f for f in selected if f not in must | ok),
        "selected_bytes": sum((ROOT / f).stat().st_size for f in selected),
    }


def main():
    bench = json.loads((ROOT / "example" / "benchmark.json").read_text())
    files = candidates()
    key = api_key()
    qs = questions(bench)
    OUT.mkdir(exist_ok=True)

    start = time.monotonic()
    raw = {}
    with cf.ThreadPoolExecutor(WORKERS) as pool:
        for path, data in pool.map(lambda f: ask(key, f, qs), files):
            raw[path] = data
    wall = time.monotonic() - start
    (OUT / "raw.json").write_text(json.dumps(raw, indent=2))

    tokens = sum(d["usage"]["input_tokens"] for d in raw.values())
    latencies = sorted(d["_latency_s"] for d in raw.values())
    report = {
        "model": sorted({d["model"] for d in raw.values()}),
        "files": len(files),
        "candidate_bytes": sum((ROOT / f).stat().st_size for f in files),
        "requests": len(raw),
        "retries": sum(d["_attempts"] - 1 for d in raw.values()),
        "input_tokens": tokens,
        "cost_usd": tokens / 1e6 * PRICE_PER_MTOK,
        "wall_s": wall,
        "latency_p50_s": latencies[len(latencies) // 2],
        "latency_max_s": latencies[-1],
        "threshold": THRESHOLD,
        "queries": {},
    }
    for q in bench["queries"]:
        scores = {f: raw[f]["answers"][q["id"]]["noul"] for f in files}
        jev_ranked = sorted(files, key=lambda f: (-scores[f], f))
        jev_selected = [f for f in jev_ranked if scores[f] >= THRESHOLD]
        rg_ranked, rg_counts = rg_rank(q["baseline_rg"], files)
        report["queries"][q["id"]] = {
            "question": q["question"],
            "jev": grade(jev_ranked, jev_selected, q) | {"top5": [(f, round(scores[f], 3)) for f in jev_ranked[:5]]},
            "rg": grade(rg_ranked, rg_ranked, q) | {"pattern": q["baseline_rg"], "top5": [(f, rg_counts[f]) for f in rg_ranked[:5]]},
        }
    (OUT / "report.json").write_text(json.dumps(report, indent=2))

    print(f"{report['files']} files, {report['requests']} requests, {report['retries']} retries, "
          f"{tokens} input tokens, ${report['cost_usd']:.5f}, wall {wall:.1f}s, "
          f"p50 {report['latency_p50_s']:.2f}s, max {report['latency_max_s']:.2f}s, model {report['model']}")
    print(f"\n{'query':<18} {'jev r@3':>7} {'rg r@3':>6} {'jev r@5':>7} {'rg r@5':>6} "
          f"{'jev sel':>7} {'rg sel':>6} {'jev rec':>7} {'rg rec':>6}  jev noise")
    for qid, r in report["queries"].items():
        j, g = r["jev"], r["rg"]
        print(f"{qid:<18} {j['recall@3']:>7.2f} {g['recall@3']:>6.2f} {j['recall@5']:>7.2f} {g['recall@5']:>6.2f} "
              f"{j['selected']:>7} {g['selected']:>6} {j['selected_recall']:>7.2f} {g['selected_recall']:>6.2f}  "
              f"{', '.join(j['selected_noise']) or '-'}")


if __name__ == "__main__":
    main()
