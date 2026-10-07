#!/usr/bin/env python3
"""Semantic scouting trial against another repository.

Each source file is split into line-aligned chunks small enough for Jev's state
budget. Every chunk receives every benchmark question as a Noul, and a file's
score is its highest chunk score. Results are compared with ripgrep ranked by
match count.

Usage:
  python3 example/scout_repo.py REPO BENCHMARK OUT_DIR [--prefix SUBDIR/]
"""
import argparse
import math
import concurrent.futures as cf
import json
import pathlib
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

API = "https://api.typesafe.ai/v1/systemone"
MODEL = "jev-latest"
PRICE_PER_MTOK = 0.042
THRESHOLD = 0.5
WORKERS = 8
CHUNK_CHARS = 40_000
SUFFIXES = {".cs", ".ts", ".tsx", ".py"}
EXCLUDED_PARTS = (
    "/Migrations/", "/client/lib/api/", "/Generated/", "/solver/tests/", "/__tests__/",
    "/__mocks__/", "/test-utils/", "/obj/", "/bin/", "/node_modules/",
)
EXCLUDED_NAMES = re.compile(r"(\.test\.|\.spec\.|\.g\.cs$)")
SECRET = re.compile(
    r"(-----BEGIN [A-Z ]*PRIVATE KEY-----|AKIA[0-9A-Z]{16}|sk-ant-[A-Za-z0-9_-]{20,}|"
    r"ghp_[A-Za-z0-9]{30,}|xox[baprs]-[A-Za-z0-9-]{20,})"
)


def api_key():
    for line in (pathlib.Path.home() / ".jev").read_text().splitlines():
        if line.startswith("JEV_API_KEY="):
            return line.split("=", 1)[1].strip().strip('"')
    sys.exit("no JEV_API_KEY= line in ~/.jev")


def candidates(repo, prefix=""):
    raw = subprocess.check_output(["git", "-C", str(repo), "ls-files", "-z"]).decode().split("\0")
    files = []
    for f in raw:
        p = repo / f
        if (f.startswith(prefix) and p.suffix in SUFFIXES and p.is_file() and not p.is_symlink()
                and p.resolve().is_relative_to(repo)
                and not any(part in f for part in EXCLUDED_PARTS) and not EXCLUDED_NAMES.search(p.name)):
            files.append(f)
    return files


def chunks(text):
    lines = text.splitlines(keepends=True)
    out, current, size, start = [], [], 0, 1
    for number, line in enumerate(lines, 1):
        if current and size + len(line) > CHUNK_CHARS:
            out.append((start, number - 1, "".join(current)))
            current, size, start = [], 0, number
        while len(line) > CHUNK_CHARS:
            out.append((number, number, line[:CHUNK_CHARS]))
            line = line[CHUNK_CHARS:]
        current.append(line)
        size += len(line)
    if current:
        out.append((start, len(lines), "".join(current)))
    return out or [(1, 1, "")]


def questions(bench):
    return {
        q["id"]: {
            "type": "noul",
            "instructions": {
                "task": q["question"],
                "question": "Does `content` from the source file at `path` itself implement or define part of what `task` asks about?",
            },
            "criteria": {
                "true": "This file excerpt contains code that performs, defines, enforces or directly participates in this behaviour.",
                "false": "This excerpt is unrelated, or merely references the topic without implementing it.",
            },
        }
        for q in bench["queries"]
    }


def ask(key, item, qs):
    path, index, first, last, text = item
    body = json.dumps({
        "model": MODEL,
        "state": {"path": path, "lines": f"{first}-{last}", "content": text},
        "questions": qs,
    }).encode()
    for attempt in range(7):
        request = urllib.request.Request(API, body, {"Authorization": f"Bearer {key}", "Content-Type": "application/json"})
        started = time.monotonic()
        try:
            with urllib.request.urlopen(request, timeout=90) as response:
                data = json.load(response)
            for qid in qs:
                value = data["answers"][qid]["noul"]
                if not isinstance(value, (int, float)) or not math.isfinite(value) or not 0 <= value <= 1:
                    raise ValueError(f"invalid Noul for {path}, question {qid}")
            data["_path"], data["_chunk"], data["_lines"] = path, index, [first, last]
            data["_latency_s"], data["_attempts"] = time.monotonic() - started, attempt + 1
            return data
        except urllib.error.HTTPError as error:
            if error.code in (429, 529) and attempt < 6:
                time.sleep(float(error.headers.get("retry-after") or min(2 ** attempt, 30)))
                continue
            raise RuntimeError(f"{path}:{first}-{last}: HTTP {error.code}: {error.read().decode()[:500]}") from None


def rg_rank(repo, pattern, files):
    result = subprocess.run(["rg", "-c", "-e", pattern, "--", *files], cwd=repo, capture_output=True, text=True)
    if result.returncode > 1:
        raise RuntimeError(result.stderr)
    counts = {}
    for line in result.stdout.splitlines():
        path, count = line.rsplit(":", 1)
        counts[path] = int(count)
    return sorted(counts, key=lambda f: (-counts[f], f)), counts


def grade(ranked, selected, must):
    rank = {path: index + 1 for index, path in enumerate(ranked)}
    def recall(k):
        return sum(rank.get(path, 10**9) <= k for path in must) / len(must)
    return {
        "must_ranks": {path: rank.get(path) for path in must},
        **{f"recall@{k}": recall(k) for k in (5, 10, 20)},
        "selected": len(selected),
        "selected_recall": sum(path in selected for path in must) / len(must),
    }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("repo", type=pathlib.Path)
    parser.add_argument("benchmark", type=pathlib.Path)
    parser.add_argument("out", type=pathlib.Path)
    parser.add_argument("--prefix", default="", help="only scan tracked files under this path")
    args = parser.parse_args()
    repo = args.repo.expanduser().resolve()
    bench = json.loads(args.benchmark.read_text())
    files = candidates(repo, args.prefix)
    if not files or not bench["queries"]:
        sys.exit("no source candidates or benchmark questions")
    missing = [path for q in bench["queries"] for path in q["must"] if path not in files]
    if missing:
        sys.exit("labelled files outside candidate corpus: " + ", ".join(missing))
    if any(not q["must"] for q in bench["queries"]):
        sys.exit("every benchmark question needs at least one must-file")

    leaked = [f for f in files if SECRET.search((repo / f).read_text(errors="replace"))]
    if leaked:
        sys.exit("refusing to send files matching secret patterns: " + ", ".join(leaked))

    items = []
    for path in files:
        for index, (first, last, text) in enumerate(chunks((repo / path).read_text(errors="replace"))):
            items.append((path, index, first, last, text))

    args.out.mkdir(parents=True, exist_ok=True)
    started = time.monotonic()
    key, qs = api_key(), questions(bench)
    with cf.ThreadPoolExecutor(WORKERS) as pool:
        raw = list(pool.map(lambda item: ask(key, item, qs), items))
    wall = time.monotonic() - started
    (args.out / "raw.json").write_text(json.dumps(raw, indent=2))

    scores = {q["id"]: {path: 0.0 for path in files} for q in bench["queries"]}
    for response in raw:
        for qid, answer in response["answers"].items():
            scores[qid][response["_path"]] = max(scores[qid][response["_path"]], answer["noul"])

    tokens = sum(response["usage"]["input_tokens"] for response in raw)
    latencies = sorted(response["_latency_s"] for response in raw)
    report = {
        "repo": str(repo),
        "commit": subprocess.check_output(["git", "-C", str(repo), "rev-parse", "HEAD"], text=True).strip(),
        "protocol": {
            "source_prefix": args.prefix, "suffixes": sorted(SUFFIXES),
            "excluded_parts": EXCLUDED_PARTS, "excluded_names_regex": EXCLUDED_NAMES.pattern,
            "threshold": THRESHOLD, "chunk_chars": CHUNK_CHARS, "workers": WORKERS,
            "price_per_million_input_tokens_usd": PRICE_PER_MTOK,
            "selection": "Full source scan, not lexical prefilter. Max score across chunks.",
            "no_precision_claim": "must-files are anchors, not exhaustive relevance labels",
        },
        "model": sorted({response["model"] for response in raw}),
        "files": len(files),
        "chunks": len(items),
        "candidate_bytes": sum((repo / path).stat().st_size for path in files),
        "input_tokens": tokens,
        "cost_usd": tokens / 1e6 * PRICE_PER_MTOK,
        "wall_s": wall,
        "latency_p50_s": latencies[len(latencies) // 2],
        "latency_p95_s": latencies[int(len(latencies) * 0.95)],
        "retries": sum(response["_attempts"] - 1 for response in raw),
        "queries": {},
    }
    rg_wall = 0.0
    for q in bench["queries"]:
        q_scores = scores[q["id"]]
        jev_ranked = sorted(files, key=lambda path: (-q_scores[path], path))
        jev_selected = {path for path in files if q_scores[path] >= THRESHOLD}
        rg_started = time.monotonic()
        rg_ranked, rg_counts = rg_rank(repo, q["baseline_rg"], files)
        rg_wall += time.monotonic() - rg_started
        hybrid = set(jev_ranked[:10]) | set(rg_ranked[:10])
        report["queries"][q["id"]] = {
            "question": q["question"],
            "jev": grade(jev_ranked, jev_selected, q["must"]) | {
                "top10": [(path, round(q_scores[path], 3)) for path in jev_ranked[:10]],
            },
            "rg": grade(rg_ranked, set(rg_ranked), q["must"]) | {
                "pattern": q["baseline_rg"],
                "top10": [(path, rg_counts[path]) for path in rg_ranked[:10]],
            },
            "hybrid_top10_union": {
                "files": len(hybrid),
                "recall": sum(path in hybrid for path in q["must"]) / len(q["must"]),
            },
        }
    report["rg_all_queries_wall_s"] = rg_wall
    (args.out / "report.json").write_text(json.dumps(report, indent=2))

    print(f"{report['files']} files, {report['chunks']} chunks, {tokens:,} input tokens, ${report['cost_usd']:.4f}, "
          f"wall {wall:.1f}s, p50 {report['latency_p50_s']:.2f}s, p95 {report['latency_p95_s']:.2f}s, "
          f"retries {report['retries']}, model {report['model']}")
    print(f"\n{'query':<21} {'jev@5':>5} {'rg@5':>5} {'jev@10':>6} {'rg@10':>6} {'jev@20':>6} {'rg@20':>6} {'jev sel':>7} {'rg hits':>7} {'union':>5}")
    for qid, result in report["queries"].items():
        jev, rg, union = result["jev"], result["rg"], result["hybrid_top10_union"]
        print(f"{qid:<21} {jev['recall@5']:>5.2f} {rg['recall@5']:>5.2f} {jev['recall@10']:>6.2f} {rg['recall@10']:>6.2f} "
              f"{jev['recall@20']:>6.2f} {rg['recall@20']:>6.2f} {jev['selected']:>7} {rg['selected']:>7} {union['recall']:>5.2f}")


if __name__ == "__main__":
    main()
