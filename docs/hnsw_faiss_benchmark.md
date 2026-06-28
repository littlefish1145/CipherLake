# HNSW vs FAISS Benchmark

`cmd/hnswbench` is the local CPU benchmark harness for comparing this repository's HNSW implementation against a FAISS baseline on `SIFT1M`.

## Inputs

Place the standard SIFT1M files under `data/` by default:

- `sift_base.fvecs`
- `sift_query.fvecs`
- `sift_groundtruth.ivecs`

You can override the directory with `--data-dir`.

## Run

```powershell
go run ./cmd/hnswbench --data-dir data --m 16 --ef-construction 200 --queries 100 --warmup 16 --threads 24 --metric l2 --ef-sweep 64,96,128,160,192,256
```

Optional baseline comparison:

```powershell
go run ./cmd/hnswbench --data-dir data --m 16 --ef-construction 200 --queries 100 --warmup 16 --threads 24 --metric l2 --baseline faiss_hnsw_baseline.json --output hnsw_report.json
```

## Baseline format

The baseline file is a JSON report with the same shape as the harness output. The important fields are:

- `entries[].ef_search`
- `entries[].recall_at_10`
- `entries[].qps`

The harness also records:

- `threads`
- `warmup`
- `cpu_isa`
- `memory_bytes`
- `comparisons[].ratio`

The harness matches each current run to the nearest baseline recall point and reports the observed `current_qps / baseline_qps` ratios, plus the best acceptance ratio. The acceptance target is `>= 0.97`.
