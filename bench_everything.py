#!/usr/bin/env python3
import argparse
import csv
import json
import random
import re
import statistics
import subprocess
import time
from dataclasses import asdict, dataclass
from pathlib import Path


WORD_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{2,}")


@dataclass
class QueryResult:
    query: str
    mode: str
    iteration: int
    elapsed_ms: float
    exit_code: int
    result_count: int
    output_bytes: int
    stderr: str


def run_cmd(args, timeout):
    start = time.perf_counter()
    proc = subprocess.run(
        args,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        errors="replace",
        timeout=timeout,
    )
    elapsed_ms = (time.perf_counter() - start) * 1000
    return proc, elapsed_ms


def sample_terms(roots, limit, seed, max_walk):
    rng = random.Random(seed)
    terms = set()
    files_seen = 0
    for root in roots:
        root_path = Path(root)
        if not root_path.exists():
            continue
        for path in root_path.rglob("*"):
            files_seen += 1
            if files_seen > max_walk:
                break
            if files_seen % 11 != 0 and len(terms) > limit * 4:
                continue
            for part in path.parts[-3:]:
                for match in WORD_RE.findall(part):
                    token = match.strip("._-")
                    if 3 <= len(token) <= 48:
                        terms.add(token)
            if len(terms) >= limit * 8:
                break
        if len(terms) >= limit * 8:
            break

    terms = sorted(terms)
    if len(terms) <= limit:
        return terms
    return rng.sample(terms, limit)


def count_results(stdout):
    if not stdout:
        return 0
    return sum(1 for line in stdout.splitlines() if line.strip())


def percentile(values, pct):
    if not values:
        return None
    ordered = sorted(values)
    idx = round((len(ordered) - 1) * pct)
    return ordered[idx]


def summarize(rows):
    groups = {}
    for row in rows:
        groups.setdefault(row.mode, []).append(row)

    summary = {}
    for mode, items in groups.items():
        latencies = [x.elapsed_ms for x in items]
        ok = [x for x in items if x.exit_code == 0]
        summary[mode] = {
            "runs": len(items),
            "ok": len(ok),
            "errors": len(items) - len(ok),
            "min_ms": min(latencies),
            "median_ms": statistics.median(latencies),
            "p90_ms": percentile(latencies, 0.90),
            "p95_ms": percentile(latencies, 0.95),
            "max_ms": max(latencies),
            "mean_ms": statistics.mean(latencies),
            "mean_results": statistics.mean([x.result_count for x in items]),
        }
    return summary


def main():
    parser = argparse.ArgumentParser(description="Benchmark Everything/es-compatible file search tools")
    parser.add_argument("--tool-kind", choices=["es", "seekfs"], default="es")
    parser.add_argument("--es", default=str(Path("extracted") / "es.exe"))
    parser.add_argument("--tool", default="", help="Tool path. Defaults to --es for es or .\\seekfs.exe for seekfs.")
    parser.add_argument("--db", default="", help="seekfs index database path")
    parser.add_argument("--addr", default="", help="seekfs resident server address")
    parser.add_argument("--instance", default="", help="Everything instance name, for example 1.5a")
    parser.add_argument("--roots", nargs="+", default=[str(Path.home())])
    parser.add_argument("--queries", type=int, default=100)
    parser.add_argument("--iterations", type=int, default=3)
    parser.add_argument("--max-results", type=int, default=100)
    parser.add_argument("--seed", type=int, default=20260521)
    parser.add_argument("--max-walk", type=int, default=20000)
    parser.add_argument("--timeout", type=float, default=10)
    parser.add_argument("--out-prefix", default="everything_bench")
    parser.add_argument("--include-empty", action="store_true")
    parser.add_argument("--reindex", action="store_true", help="Also time 'es.exe -reindex'. This can be disruptive.")
    args = parser.parse_args()

    tool_path = args.tool or (args.es if args.tool_kind == "es" else str(Path("seekfs.exe")))
    tool = str(Path(tool_path).resolve())
    out_prefix = Path(args.out_prefix)

    instance_args = ["-instance", args.instance] if args.instance else []
    if args.tool_kind == "es":
        version, _ = run_cmd([tool, "-version"], args.timeout)
        backend_version, _ = run_cmd([tool, *instance_args, "-get-everything-version"], args.timeout)
    else:
        version, _ = run_cmd([tool, "version"], args.timeout)
        backend_version = version
    if backend_version.returncode != 0:
        raise SystemExit(
            "Everything IPC is not available. Start Everything first. "
            f"stderr/stdout: {backend_version.stderr or backend_version.stdout}".strip()
        )

    terms = sample_terms(args.roots, args.queries, args.seed, args.max_walk)
    if not terms:
        raise SystemExit("No sample terms found under roots: " + ", ".join(args.roots))

    if args.include_empty:
        terms.extend(["zzzz_no_expected_match_zzzz", "unlikely_everything_benchmark_token"])

    rows = []

    if args.reindex:
        if args.tool_kind != "es":
            raise SystemExit("--reindex is currently supported only for --tool-kind es")
        proc, elapsed_ms = run_cmd([tool, *instance_args, "-reindex"], timeout=max(args.timeout, 3600))
        rows.append(QueryResult(
            query="-reindex",
            mode="reindex",
            iteration=0,
            elapsed_ms=elapsed_ms,
            exit_code=proc.returncode,
            result_count=0,
            output_bytes=len(proc.stdout.encode("utf-8", errors="replace")),
            stderr=proc.stderr.strip() or proc.stdout.strip(),
        ))

    modes = [
        ("name", []),
        ("path", ["-match-path"]),
        ("count", ["-get-result-count"]),
    ]

    for iteration in range(args.iterations):
        random.Random(args.seed + iteration).shuffle(terms)
        for query in terms:
            for mode, extra in modes:
                if args.tool_kind == "es":
                    cmd = [tool, *instance_args, *extra, "-n", str(args.max_results), query]
                    if mode == "count":
                        cmd = [tool, *instance_args, *extra, query]
                else:
                    db_args = ["-db", args.db] if args.db else []
                    addr_args = ["-addr", args.addr] if args.addr else []
                    if mode == "name":
                        cmd = [tool, "search", *db_args, *addr_args, "-n", str(args.max_results), query]
                    elif mode == "path":
                        cmd = [tool, "search", *db_args, *addr_args, "-path", "-n", str(args.max_results), query]
                    else:
                        cmd = [tool, "count", *db_args, *addr_args, query]
                proc, elapsed_ms = run_cmd(cmd, args.timeout)
                rows.append(QueryResult(
                    query=query,
                    mode=mode,
                    iteration=iteration,
                    elapsed_ms=elapsed_ms,
                    exit_code=proc.returncode,
                    result_count=count_results(proc.stdout),
                    output_bytes=len(proc.stdout.encode("utf-8", errors="replace")),
                    stderr=proc.stderr.strip(),
                ))

    csv_path = out_prefix.with_suffix(".csv")
    json_path = out_prefix.with_suffix(".summary.json")

    with csv_path.open("w", newline="", encoding="utf-8") as f:
        writer = csv.DictWriter(f, fieldnames=list(asdict(rows[0]).keys()))
        writer.writeheader()
        for row in rows:
            writer.writerow(asdict(row))

    metadata = {
        "tool_kind": args.tool_kind,
        "tool": tool,
        "tool_version": version.stdout.strip(),
        "backend_version": backend_version.stdout.strip(),
        "instance": args.instance,
        "roots": args.roots,
        "queries": len(terms),
        "iterations": args.iterations,
        "max_results": args.max_results,
        "summary": summarize(rows),
    }
    json_path.write_text(json.dumps(metadata, indent=2), encoding="utf-8")
    print(json.dumps(metadata, indent=2))
    print(f"Wrote {csv_path}")
    print(f"Wrote {json_path}")


if __name__ == "__main__":
    main()
