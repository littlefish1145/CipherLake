"""Generate FAISS CPU HNSW baseline JSON for SIFT1M.

Output: data/faiss_baseline_sift1m.json
"""
import json
import sys
import time
import struct
import numpy as np
import faiss

# Parameters matching our Go HNSW defaults
M = 16
EF_CONSTRUCTION = 200
TOP_K = 10
METRIC = "l2"
EF_SWEEP = [64, 96, 128, 160, 192, 256]
QUERIES = 100
WARMUP = 16
SUBSET = None  # use full 1M base vectors; set to int for quick test


def read_fvecs(path):
    """Read .fvecs file, return (vectors, dim)."""
    with open(path, 'rb') as f:
        raw = f.read()
    dim = struct.unpack('I', raw[:4])[0]
    vec_bytes = 4 + dim * 4
    n = len(raw) // vec_bytes
    vecs = np.frombuffer(raw, dtype=np.float32).reshape(n, dim + 1)[:, 1:]
    return vecs.copy(), dim


def read_ivecs(path):
    """Read .ivecs file, return int32 array (n_queries, dim)."""
    with open(path, 'rb') as f:
        raw = f.read()
    dim = struct.unpack('I', raw[:4])[0]
    vec_bytes = 4 + dim * 4
    n = len(raw) // vec_bytes
    data = np.frombuffer(raw, dtype=np.int32).reshape(n, dim + 1)[:, 1:]
    return data.copy()


def compute_recall(labels, groundtruth, k=10):
    """Compute Recall@k."""
    hits = 0
    for i in range(len(labels)):
        gt_set = set(groundtruth[i][:k].tolist())
        for r in labels[i][:k]:
            if r in gt_set:
                hits += 1
    return hits / (len(labels) * k)


def main():
    data_dir = sys.argv[1] if len(sys.argv) > 1 else "data"

    print("Loading SIFT1M...")
    base_vecs, dim = read_fvecs(f"{data_dir}/sift_base.fvecs")
    query_vecs, _ = read_fvecs(f"{data_dir}/sift_query.fvecs")
    groundtruth = read_ivecs(f"{data_dir}/sift_groundtruth.ivecs")

    base_vecs = base_vecs.astype(np.float32)
    query_vecs = query_vecs.astype(np.float32)

    base_count = base_vecs.shape[0]
    nq = min(QUERIES, len(query_vecs))
    query_vecs = query_vecs[:nq]
    groundtruth = groundtruth[:nq]

    print(f"Base: {base_count} vectors, dim={dim}")
    print(f"Queries: {nq}, topk={TOP_K}")
    print(f"Building FAISS HNSW index (M={M}, efConstruction={EF_CONSTRUCTION})...")

    index = faiss.IndexFlatL2(dim)
    hnsw = faiss.IndexHNSWFlat(dim, M)
    hnsw.hnsw.efConstruction = EF_CONSTRUCTION
    hnsw.verbose = False

    build_start = time.time()
    hnsw.train(base_vecs)
    hnsw.add(base_vecs)
    build_time = time.time() - build_start
    print(f"Build time: {build_time:.3f}s")

    import os
    import psutil
    mem = psutil.Process(os.getpid()).memory_info().rss

    entries = []
    for ef in EF_SWEEP:
        hnsw.hnsw.efSearch = ef

        # warmup
        for i in range(min(WARMUP, nq)):
            _, _ = hnsw.search(query_vecs[i:i+1], TOP_K)

        latencies = []
        start = time.time()
        all_labels = []
        for i in range(nq):
            qs = time.time()
            distances, labels = hnsw.search(query_vecs[i:i+1], TOP_K)
            lat_us = (time.time() - qs) * 1_000_000
            latencies.append(lat_us)
            all_labels.append(labels[0])
        elapsed = time.time() - start

        recall = compute_recall(all_labels, groundtruth, TOP_K)
        qps = nq / elapsed
        latencies.sort()
        p50 = latencies[int(len(latencies) * 0.5)]
        p95 = latencies[int(len(latencies) * 0.95)]
        avg_lat = sum(latencies) / len(latencies)

        entries.append({
            "ef_search": ef,
            "recall_at_10": round(recall, 6),
            "qps": round(qps, 2),
            "p50_us": round(p50, 2),
            "p95_us": round(p95, 2),
            "avg_us": round(avg_lat, 2),
        })
        print(f"  ef={ef}: recall={recall:.4f}, qps={qps:.0f}, p50={p50:.0f}us")

    report = {
        "dataset": "SIFT1M",
        "metric": METRIC,
        "dimension": dim,
        "base_count": base_count,
        "query_count": nq,
        "topk": TOP_K,
        "m": M,
        "ef_construction": EF_CONSTRUCTION,
        "threads": 1,
        "warmup": WARMUP,
        "cpu_isa": "FAISS CPU (auto)",
        "build_seconds": round(build_time, 3),
        "memory_bytes": mem,
        "entries": entries,
    }

    out_path = f"{data_dir}/faiss_baseline_sift1m.json"
    with open(out_path, "w") as f:
        json.dump(report, f, indent=2)
    print(f"\nFAISS baseline saved to {out_path}")
    print(json.dumps(report, indent=2))


if __name__ == "__main__":
    main()
