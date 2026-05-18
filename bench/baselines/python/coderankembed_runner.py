#!/usr/bin/env python3
"""CodeRankEmbed Hybrid baseline runner.

Wrapper that lets the Go-side bench/baselines harness invoke
CodeRankEmbed Hybrid without per-baseline Go code growing model-
download logic. Usage:

    python3 bench/baselines/python/coderankembed_runner.py \\
        --repo PATH --query "validateToken" --top-k 10

Emits one repo-relative path per line on stdout. Errors go to
stderr; non-zero exit when the model isn't available.

Install: `pip install sentence-transformers transformers torch`.
First run downloads the CodeRankEmbed model (~440 MB).
"""

import argparse
import os
import sys
from pathlib import Path


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", required=True, help="indexed corpus path")
    ap.add_argument("--query", required=True, help="single query string")
    ap.add_argument("--top-k", type=int, default=10)
    args = ap.parse_args()

    try:
        from sentence_transformers import SentenceTransformer
    except ImportError as e:
        print(
            f"coderankembed_runner: missing dependency ({e}). "
            "pip install sentence-transformers transformers torch",
            file=sys.stderr,
        )
        return 2

    model = SentenceTransformer("nomic-ai/CodeRankEmbed", trust_remote_code=True)

    # Index every file under repo (cheap for sub-million LoC; the
    # ground-truth fixture is the gortex repo itself).
    repo = Path(args.repo).resolve()
    paths: list[Path] = []
    texts: list[str] = []
    for p in repo.rglob("*"):
        if not p.is_file():
            continue
        if any(seg.startswith(".") for seg in p.relative_to(repo).parts):
            continue
        if p.suffix.lower() not in {
            ".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".rs",
            ".java", ".kt", ".swift", ".rb", ".cs", ".cpp", ".c",
            ".h", ".hpp", ".md", ".yaml", ".yml", ".json",
        }:
            continue
        try:
            text = p.read_text(errors="ignore")
        except OSError:
            continue
        if not text.strip():
            continue
        paths.append(p)
        texts.append(text[:8000])  # truncate to keep the embed cheap

    if not texts:
        print(
            "coderankembed_runner: no indexable files under repo",
            file=sys.stderr,
        )
        return 1

    embeds = model.encode(texts, show_progress_bar=False, convert_to_numpy=True)
    qe = model.encode([args.query], show_progress_bar=False, convert_to_numpy=True)[0]

    # Cosine similarity → rank.
    import numpy as np

    sims = embeds @ qe / (
        (np.linalg.norm(embeds, axis=1) * np.linalg.norm(qe)) + 1e-12
    )
    order = np.argsort(-sims)[: args.top_k]
    for idx in order:
        rel = paths[idx].relative_to(repo)
        print(rel.as_posix())
    return 0


if __name__ == "__main__":
    sys.exit(main())
