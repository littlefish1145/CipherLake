// kernels.cu - PQ distance table, ADC, centroid distance, norm computation
// Compile to PTX:  nvcc -ptx -arch=compute_75 kernels.cu -o kernels.ptx
// Compile to OBJ:  nvcc -c    -arch=sm_75    kernels.cu -o kernels.o
// Adjust -arch for your GPU (sm_75=Turing, sm_80=Ampere, sm_89=Ada, sm_90=Hopper, sm_120=Blackwell)

#include <cuda_runtime.h>

// ---------------------------------------------------------------------------
// PQ distance table: for each query q[qid] and each subspace m,
// compute L2^2 distance between q and 256 codewords.
//   grid  = (nq, M, 1)
//   block = (256, 2, 1)   -- threadIdx.x = k(0..255), threadIdx.y = 0|1
// ---------------------------------------------------------------------------
extern "C" __global__ void pq_dist_table(
    const float* __restrict__ q,       // [nq, dim]
    const float* __restrict__ cb,      // [M, 256, subdim]
    float*       __restrict__ tbl,     // [nq, M, 256]
    int nq, int dim, int M, int subdim)
{
    int qid = blockIdx.x;
    int m   = blockIdx.y;
    int k   = threadIdx.x;
    if (k >= 256) return;

    const float* qs = q  + qid * dim + m * subdim;
    const float* c  = cb + (m * 256 + k) * subdim;

    float s = 0.0f;
    for (int i = threadIdx.y; i < subdim; i += blockDim.y) {
        float d = qs[i] - c[i];
        s += d * d;
    }

    __shared__ float sh[256][2];
    sh[k][threadIdx.y] = s;
    __syncthreads();

    if (threadIdx.y == 0)
        tbl[(qid * M + m) * 256 + k] = sh[k][0] + sh[k][1];
}

// ---------------------------------------------------------------------------
// PQ-ADC: lookup table accumulation to get approximate L2^2 distance
// for each query against each vector.
//   grid  = (nq, ceil(nv/threads), 1)
//   block = (threads, 1, 1)
// ---------------------------------------------------------------------------
extern "C" __global__ void pq_adc(
    const float*           __restrict__ tbl,    // [nq, M, 256]
    const unsigned char*  __restrict__ codes,   // [nv, M]
    float*                 __restrict__ dists,  // [nq, nv]
    int nq, int nv, int M)
{
    int q = blockIdx.x;
    int v = blockIdx.y * blockDim.x + threadIdx.x;
    if (v >= nv) return;

    const float* t = tbl + q * M * 256;
    const unsigned char* c = codes + v * M;

    float s = 0.0f;
    for (int m = 0; m < M; m++)
        s += t[m * 256 + c[m]];

    dists[q * nv + v] = s;
}

// ---------------------------------------------------------------------------
// Centroid distance: for each query q[qid] and centroid c,
// compute L2^2 distance (block reduce).
//   grid  = (nq, nc, 1)
//   block = (256, 1, 1)
// ---------------------------------------------------------------------------
extern "C" __global__ void centroid_dist(
    const float* __restrict__ q,        // [nq, dim]
    const float* __restrict__ cents,    // [nc, dim]
    float*       __restrict__ dists,    // [nq, nc]
    int nq, int nc, int dim)
{
    int qid = blockIdx.x;
    int c   = blockIdx.y;
    int t   = threadIdx.x;

    __shared__ float sh[256];

    const float* qv = q     + qid * dim;
    const float* cv = cents + c   * dim;

    float s = 0.0f;
    for (int i = t; i < dim; i += blockDim.x) {
        float d = qv[i] - cv[i];
        s += d * d;
    }

    sh[t] = s;
    __syncthreads();

    for (int n = blockDim.x / 2; n > 0; n >>= 1) {
        if (t < n) sh[t] += sh[t + n];
        __syncthreads();
    }

    if (t == 0)
        dists[qid * nc + c] = sh[0];
}

// ---------------------------------------------------------------------------
// PQ-ADC gather: like pq_adc but only computes for selected candidate vectors.
// Takes a list of candidate indices (gather) instead of scanning all nv.
//   grid  = (nq, ceil(nCand/threads), 1)
//   block = (threads, 1, 1)
// ---------------------------------------------------------------------------
extern "C" __global__ void pq_adc_gather(
    const float*           __restrict__ tbl,        // [nq, M, 256]
    const unsigned char*  __restrict__ codes,       // [nv, M]
    const int*            __restrict__ candidates,   // [nCand]
    float*                 __restrict__ dists,       // [nq, nCand]
    int nq, int nCand, int M)
{
    int q = blockIdx.x;
    int t = blockIdx.y * blockDim.x + threadIdx.x;
    if (t >= nCand) return;

    int v = candidates[t];
    const float* tptr = tbl + q * M * 256;
    const unsigned char* c = codes + v * M;

    float s = 0.0f;
    for (int m = 0; m < M; m++)
        s += tptr[m * 256 + c[m]];

    dists[q * nCand + t] = s;
}

// ---------------------------------------------------------------------------
// Vector norm^2: compute ||v||^2 for each vector.
//   grid  = (ceil(nv/256), 1, 1)
//   block = (256, 1, 1)
// ---------------------------------------------------------------------------
extern "C" __global__ void compute_norms(
    const float* __restrict__ vecs,    // [nv, dim]
    float*       __restrict__ norms,    // [nv]
    int nv, int dim)
{
    int i = blockIdx.x * blockDim.x + threadIdx.x;
    if (i >= nv) return;

    float s = 0.0f;
    for (int j = 0; j < dim; j++) {
        float x = vecs[i * dim + j];
        s += x * x;
    }
    norms[i] = s;
}

// ---------------------------------------------------------------------------
// L2 distance from dot products: dist = qNorm2 + vNorm2 - 2*dot
// Computes L2^2 distance for all vectors given precomputed dot products,
// query norm^2, and vector norms^2. Used after cuBLAS Sgemv.
//   grid  = (ceil(nv/256), 1, 1)
//   block = (256, 1, 1)
// ---------------------------------------------------------------------------
extern "C" __global__ void l2_from_dot(
    float*       __restrict__ dists,    // [nv] in/out: dot products -> L2^2 distances
    const float* __restrict__ vNorms2,  // [nv]
    float        qNorm2,
    int nv)
{
    int i = blockIdx.x * blockDim.x + threadIdx.x;
    if (i >= nv) return;
    dists[i] = qNorm2 + vNorms2[i] - 2.0f * dists[i];
}

// ---------------------------------------------------------------------------
// Cosine distance from dot products: dist = 1 - dot / (||q|| * ||v||)
//   grid  = (ceil(nv/256), 1, 1)
//   block = (256, 1, 1)
// ---------------------------------------------------------------------------
extern "C" __global__ void cosine_from_dot(
    float*       __restrict__ dists,    // [nv] in/out: dot products -> cosine distances
    const float* __restrict__ vNorms2,  // [nv]
    float        qNorm2,
    int nv)
{
    int i = blockIdx.x * blockDim.x + threadIdx.x;
    if (i >= nv) return;

    float vn = sqrtf(vNorms2[i]);
    float qn = sqrtf(qNorm2);
    float dn = qn * vn;
    if (dn == 0.0f) {
        dists[i] = 1.0f;
    } else {
        dists[i] = 1.0f - dists[i] / dn;
    }
}

// ---------------------------------------------------------------------------
// Gather + L2 distance: for each candidate i, compute L2^2(vecs[candIdx[i]], query)
//   dist = ||q||^2 + ||v||^2 - 2*dot(q,v)
//   qNorm2 and vNorms2 precomputed, avoids CPU post-processing
//   grid  = (ceil(nCand/256), 1, 1)
//   block = (256, 1, 1)
// ---------------------------------------------------------------------------
extern "C" __global__ void gather_l2(
    const float* __restrict__ vecs,           // [nv, dim]
    const int*   __restrict__ candIndices,    // [nCand]
    const float* __restrict__ query,          // [dim]
    const float* __restrict__ vNorms2,        // [nv] precomputed ||v||^2
    float*       __restrict__ dists,           // [nCand] output L2^2
    float         qNorm2,
    int nCand, int dim)
{
    int i = blockIdx.x * blockDim.x + threadIdx.x;
    if (i >= nCand) return;

    int vecIdx = candIndices[i];
    float dot = 0.0f;
    for (int j = 0; j < dim; j++) {
        dot += vecs[vecIdx * dim + j] * query[j];
    }
    dists[i] = qNorm2 + vNorms2[vecIdx] - 2.0f * dot;
}

// ---------------------------------------------------------------------------
// Gather + cosine distance: for each candidate i, compute cosine dist
//   dist = 1 - dot(q,v) / (||q|| * ||v||)
//   grid  = (ceil(nCand/256), 1, 1)
//   block = (256, 1, 1)
// ---------------------------------------------------------------------------
extern "C" __global__ void gather_cosine(
    const float* __restrict__ vecs,           // [nv, dim]
    const int*   __restrict__ candIndices,    // [nCand]
    const float* __restrict__ query,          // [dim]
    const float* __restrict__ vNorms2,        // [nv] precomputed ||v||^2
    float*       __restrict__ dists,           // [nCand] output cosine distance
    float         qNorm2,
    int nCand, int dim)
{
    int i = blockIdx.x * blockDim.x + threadIdx.x;
    if (i >= nCand) return;

    int vecIdx = candIndices[i];
    float dot = 0.0f;
    for (int j = 0; j < dim; j++) {
        dot += vecs[vecIdx * dim + j] * query[j];
    }
    float vn = sqrtf(vNorms2[vecIdx]);
    float qn = sqrtf(qNorm2);
    float dn = qn * vn;
    if (dn == 0.0f) {
        dists[i] = 1.0f;
    } else {
        dists[i] = 1.0f - dot / dn;
    }
}

// ---------------------------------------------------------------------------
// Gather + dot product: for each candidate i, compute dot(vecs[candIdx[i]], query)
// Avoids D2D copy of candidate vectors into contiguous buffer.
//   grid  = (ceil(nCand/256), 1, 1)
//   block = (256, 1, 1)
// ---------------------------------------------------------------------------
extern "C" __global__ void gather_dot(
    const float* __restrict__ vecs,          // [nv, dim]
    const int*   __restrict__ candIndices,   // [nCand]
    const float* __restrict__ query,         // [dim]
    float*       __restrict__ dots,           // [nCand]
    int nCand, int dim)
{
    int i = blockIdx.x * blockDim.x + threadIdx.x;
    if (i >= nCand) return;

    int vecIdx = candIndices[i];
    float dot = 0.0f;
    for (int j = 0; j < dim; j++) {
        dot += vecs[vecIdx * dim + j] * query[j];
    }
    dots[i] = dot;
}

// ---------------------------------------------------------------------------
// Select top-N smallest distances and output their indices.
// Uses block-level shared memory reduction, thread 0 sequential scan.
// Output: outIndices[numBlocks * topK], outCounts[numBlocks]
//   grid  = (ceil(n/256), 1, 1)
//   block = (256, 1, 1)
// ---------------------------------------------------------------------------
extern "C" __global__ void topk_select_indices(
    const float* __restrict__ dists,             // [n]
    int*         __restrict__ outIndices,        // [numBlocks, topK]
    int*         __restrict__ outCounts,        // [numBlocks]
    int n, int topK)
{
    extern __shared__ char smem[];
    float* sDists = reinterpret_cast<float*>(smem);              // [topK]
    int*   sIdx   = reinterpret_cast<int*>(sDists + topK);       // [topK]

    int tid = threadIdx.x;
    int blockStart = blockIdx.x * blockDim.x;
    int chunkEnd = min(blockStart + blockDim.x, n);

    if (tid < topK) {
        sDists[tid] = INFINITY;
        sIdx[tid] = -1;
    }
    __syncthreads();

    if (tid == 0) {
        int count = 0;
        for (int i = blockStart; i < chunkEnd; i++) {
            float d = dists[i];
            if (count < topK) {
                int pos = count;
                for (int j = 0; j < count; j++) {
                    if (d < sDists[j]) { pos = j; break; }
                }
                for (int j = count; j > pos; j--) {
                    sDists[j] = sDists[j - 1];
                    sIdx[j] = sIdx[j - 1];
                }
                sDists[pos] = d;
                sIdx[pos] = i;
                count++;
            } else if (d < sDists[topK - 1]) {
                int pos = topK - 1;
                for (int j = 0; j < topK - 1; j++) {
                    if (d < sDists[j]) { pos = j; break; }
                }
                for (int j = topK - 1; j > pos; j--) {
                    sDists[j] = sDists[j - 1];
                    sIdx[j] = sIdx[j - 1];
                }
                sDists[pos] = d;
                sIdx[pos] = i;
            }
        }

        outCounts[blockIdx.x] = count;
        for (int j = 0; j < count; j++) {
            outIndices[blockIdx.x * topK + j] = sIdx[j];
        }
    }
}

// ---------------------------------------------------------------------------
// Gather IVF candidates: given selected centroid indices, collect all vector
// indices from those clusters into a flat output array.
// Uses atomicAdd for parallel write — each thread processes one centroid.
//
//   grid  = (ceil(nProbe/256), 1, 1)
//   block = (256, 1, 1)
// ---------------------------------------------------------------------------
extern "C" __global__ void gather_ivf_candidates(
    const int*   __restrict__ probeCentroids,    // [nProbe] centroid indices
    const int*   __restrict__ clusterOffsets,    // [nc+1] CSR offsets
    const int*   __restrict__ clusterMembers,    // [total] flattened vector indices
    int*         __restrict__ outCandidates,      // [maxCands] output vector indices
    int*         __restrict__ outCount,          // [1] actual count
    int nProbe)
{
    // Shared: each thread loads its centroid's range
    __shared__ int sStart[256];
    __shared__ int sLen[256];
    __shared__ int sOffset[256];
    __shared__ int sTotal;

    int tid = threadIdx.x;

    if (tid == 0) sTotal = 0;
    __syncthreads();

    // Each thread loads one centroid's range
    int myCentroid = -1;
    int myStart = 0, myLen = 0;
    if (tid < nProbe) {
        myCentroid = probeCentroids[tid];
        myStart = clusterOffsets[myCentroid];
        myLen = clusterOffsets[myCentroid + 1] - myStart;
    }

    // Prefix sum (exclusive) via shared memory
    // Simple sequential prefix sum by thread 0 (nProbe <= 256)
    sStart[tid] = myStart;
    sLen[tid] = myLen;
    __syncthreads();

    if (tid == 0) {
        int exclusive = 0;
        for (int i = 0; i < nProbe; i++) {
            sOffset[i] = exclusive;
            exclusive += sLen[i];
        }
        sTotal = exclusive;
        *outCount = exclusive;
    }
    __syncthreads();

    // Each thread copies its centroid's members to output
    if (myCentroid >= 0 && myLen > 0) {
        int outOff = sOffset[tid];
        for (int j = 0; j < myLen; j++) {
            outCandidates[outOff + j] = clusterMembers[myStart + j];
        }
    }
}

// ---------------------------------------------------------------------------
// Top-K selection for candidate subset: same as topk_select but
// uses candIndices[] to map local candidate index → global vector index
//   grid  = (ceil(nCand/blockDim.x), 1, 1)
//   block = (256, 1, 1)
//   topK  <= blockDim.x
// ---------------------------------------------------------------------------
extern "C" __global__ void topk_select_gather(
    const float* __restrict__ dists,             // [nCand]
    const int*   __restrict__ candIndices,       // [nCand] local→global index mapping
    const unsigned long long* __restrict__ gids,  // [nv] global IDs
    float*       __restrict__ outDists,          // [numBlocks, topK]
    unsigned long long* __restrict__ outGids,    // [numBlocks, topK]
    int*         __restrict__ outCounts,        // [numBlocks]
    int nCand, int topK)
{
    extern __shared__ char smem[];
    float*              sDists = reinterpret_cast<float*>(smem);
    unsigned long long* sGids  = reinterpret_cast<unsigned long long*>(sDists + topK);

    int tid = threadIdx.x;
    int blockStart = blockIdx.x * blockDim.x;
    int chunkEnd = min(blockStart + blockDim.x, nCand);

    if (tid < topK) {
        sDists[tid] = INFINITY;
        sGids[tid] = 0ULL;
    }
    __syncthreads();

    if (tid == 0) {
        int count = 0;
        for (int i = blockStart; i < chunkEnd; i++) {
            float d = dists[i];
            if (count < topK) {
                int pos = count;
                for (int j = 0; j < count; j++) {
                    if (d < sDists[j]) { pos = j; break; }
                }
                for (int j = count; j > pos; j--) {
                    sDists[j] = sDists[j - 1];
                    sGids[j] = sGids[j - 1];
                }
                sDists[pos] = d;
                sGids[pos] = gids[candIndices[i]];
                count++;
            } else if (d < sDists[topK - 1]) {
                int pos = topK - 1;
                for (int j = 0; j < topK - 1; j++) {
                    if (d < sDists[j]) { pos = j; break; }
                }
                for (int j = topK - 1; j > pos; j--) {
                    sDists[j] = sDists[j - 1];
                    sGids[j] = sGids[j - 1];
                }
                sDists[pos] = d;
                sGids[pos] = gids[candIndices[i]];
            }
        }

        outCounts[blockIdx.x] = count;
        for (int j = 0; j < count; j++) {
            outDists[blockIdx.x * topK + j] = sDists[j];
            outGids[blockIdx.x * topK + j] = sGids[j];
        }
    }
}
// Each block scans its chunk sequentially (thread 0) and keeps the topK
// smallest distances. Final merge done on CPU (numBlocks * topK entries).
//
//   grid  = (ceil(nv/blockDim.x), 1, 1)
//   block = (256, 1, 1)
//   topK  <= blockDim.x (shared memory: topK * (4+8) bytes)
// ---------------------------------------------------------------------------
extern "C" __global__ void topk_select(
    const float* __restrict__ dists,             // [nv]
    const unsigned long long* __restrict__ gids, // [nv]
    float*       __restrict__ outDists,          // [numBlocks, topK]
    unsigned long long* __restrict__ outGids,    // [numBlocks, topK]
    int*         __restrict__ outCounts,        // [numBlocks]
    int nv, int topK)
{
    extern __shared__ char smem[];
    float*              sDists = reinterpret_cast<float*>(smem);              // [topK]
    unsigned long long* sGids  = reinterpret_cast<unsigned long long*>(sDists + topK); // [topK]

    int tid = threadIdx.x;
    int blockStart = blockIdx.x * blockDim.x;
    int chunkEnd = min(blockStart + blockDim.x, nv);

    // Initialize shared topK with +infinity
    if (tid < topK) {
        sDists[tid] = INFINITY;
        sGids[tid] = 0ULL;
    }
    __syncthreads();

    // Thread 0 scans the chunk and maintains sorted topK (ascending)
    if (tid == 0) {
        int count = 0;
        for (int i = blockStart; i < chunkEnd; i++) {
            float d = dists[i];
            if (count < topK) {
                // Insert sorted ascending
                int pos = count;
                for (int j = 0; j < count; j++) {
                    if (d < sDists[j]) { pos = j; break; }
                }
                for (int j = count; j > pos; j--) {
                    sDists[j] = sDists[j - 1];
                    sGids[j] = sGids[j - 1];
                }
                sDists[pos] = d;
                sGids[pos] = gids[i];
                count++;
            } else if (d < sDists[topK - 1]) {
                // Replace worst (last), re-sort
                int pos = topK - 1;
                for (int j = 0; j < topK - 1; j++) {
                    if (d < sDists[j]) { pos = j; break; }
                }
                for (int j = topK - 1; j > pos; j--) {
                    sDists[j] = sDists[j - 1];
                    sGids[j] = sGids[j - 1];
                }
                sDists[pos] = d;
                sGids[pos] = gids[i];
            }
        }

        outCounts[blockIdx.x] = count;
        for (int j = 0; j < count; j++) {
            outDists[blockIdx.x * topK + j] = sDists[j];
            outGids[blockIdx.x * topK + j] = sGids[j];
        }
    }
}

// ---------------------------------------------------------------------------
// Batched top-K selection: one block per query row, thread 0 scans all nv
// elements, outputs sorted topK distances + global IDs per row.
//   grid  = (nq, 1, 1)
//   block = (256, 1, 1)
//   shared memory: topK * (sizeof(float) + sizeof(uint64))
// ---------------------------------------------------------------------------
extern "C" __global__ void batched_topk_select_gids(
    const float*           __restrict__ dists,     // [nq, nv]
    const unsigned long long* __restrict__ gids,   // [nv]
    float*                 __restrict__ outDists,   // [nq, topK]
    unsigned long long*    __restrict__ outGids,    // [nq, topK]
    int nq, int nv, int topK)
{
    int q = blockIdx.x;
    if (q >= nq) return;

    extern __shared__ char smem[];
    float* sDists = (float*)smem;
    unsigned long long* sGids = (unsigned long long*)(sDists + topK);

    const float* rowDists = dists + q * nv;

    if (threadIdx.x < topK) {
        sDists[threadIdx.x] = INFINITY;
        sGids[threadIdx.x] = 0ULL;
    }
    __syncthreads();

    if (threadIdx.x == 0) {
        int count = 0;
        for (int i = 0; i < nv; i++) {
            float d = rowDists[i];
            if (count < topK) {
                int pos = count;
                for (int j = 0; j < count; j++) {
                    if (d < sDists[j]) { pos = j; break; }
                }
                for (int j = count; j > pos; j--) {
                    sDists[j] = sDists[j - 1];
                    sGids[j] = sGids[j - 1];
                }
                sDists[pos] = d;
                sGids[pos] = gids[i];
                count++;
            } else if (d < sDists[topK - 1]) {
                int pos = topK - 1;
                for (int j = 0; j < topK - 1; j++) {
                    if (d < sDists[j]) { pos = j; break; }
                }
                for (int j = topK - 1; j > pos; j--) {
                    sDists[j] = sDists[j - 1];
                    sGids[j] = sGids[j - 1];
                }
                sDists[pos] = d;
                sGids[pos] = gids[i];
            }
        }

        for (int j = 0; j < topK; j++) {
            outDists[q * topK + j] = sDists[j];
            outGids[q * topK + j] = sGids[j];
        }
    }
}

// ---------------------------------------------------------------------------
// Merge partial top-K from topk_select_indices back into final topK,
// then gather vecIdx from the original candIdx array.
// Input:  blockPositions[numBlocks * topK] — local positions within dists
//         blockCounts[numBlocks]           — valid count per block
//         origDists[nCand]                 — original distance array
//         candIdx[nCand]                   — vecIdx per candidate
// Output: outDists[topK], outVecIdx[topK]  — merged, sorted ascending
//   grid  = (1, 1, 1)
//   block = (256, 1, 1)
//   shared memory: topK * (sizeof(float) + sizeof(int))
// ---------------------------------------------------------------------------
extern "C" __global__ void merge_topk_positions(
    const int*   __restrict__ blockPositions,  // [numBlocks, topK]
    const int*   __restrict__ blockCounts,     // [numBlocks]
    const float* __restrict__ origDists,       // [nCand]
    const int*   __restrict__ candIdx,         // [nCand]
    float*       __restrict__ outDists,        // [topK]
    int*         __restrict__ outVecIdx,       // [topK]
    int numBlocks, int topK)
{
    extern __shared__ char smem[];
    float* sDists = (float*)smem;
    int* sVecIdx = (int*)(sDists + topK);

    if (threadIdx.x < topK) {
        sDists[threadIdx.x] = INFINITY;
        sVecIdx[threadIdx.x] = -1;
    }
    __syncthreads();

    if (threadIdx.x == 0) {
        int count = 0;
        for (int b = 0; b < numBlocks; b++) {
            int bc = blockCounts[b];
            for (int j = 0; j < bc; j++) {
                int pos = blockPositions[b * topK + j];
                float d = origDists[pos];

                if (count < topK) {
                    int ins = count;
                    for (int k = 0; k < count; k++) {
                        if (d < sDists[k]) { ins = k; break; }
                    }
                    for (int k = count; k > ins; k--) {
                        sDists[k] = sDists[k - 1];
                        sVecIdx[k] = sVecIdx[k - 1];
                    }
                    sDists[ins] = d;
                    sVecIdx[ins] = candIdx[pos];
                    count++;
                } else if (d < sDists[topK - 1]) {
                    int ins = topK - 1;
                    for (int k = 0; k < topK - 1; k++) {
                        if (d < sDists[k]) { ins = k; break; }
                    }
                    for (int k = topK - 1; k > ins; k--) {
                        sDists[k] = sDists[k - 1];
                        sVecIdx[k] = sVecIdx[k - 1];
                    }
                    sDists[ins] = d;
                    sVecIdx[ins] = candIdx[pos];
                }
            }
        }

        for (int j = 0; j < topK; j++) {
            outDists[j] = sDists[j];
            outVecIdx[j] = sVecIdx[j];
        }
    }
}

// ---------------------------------------------------------------------------
// Select top-K centroid indices from distance array.
// Single block: thread 0 scans all nc elements, outputs sorted centroid
// indices. Used to eliminate D2H→CPU→H2D round trip in centroid selection.
//   grid  = (1, 1, 1)
//   block = (256, 1, 1)
//   shared memory: topK * (sizeof(float) + sizeof(int))
// ---------------------------------------------------------------------------
extern "C" __global__ void centroid_select_topk(
    const float* __restrict__ cdists,   // [nc]
    int*         __restrict__ outIndices,// [topK]
    int nc, int topK)
{
    extern __shared__ char smem[];
    float* sDists = (float*)smem;
    int* sIdx = (int*)(sDists + topK);

    if (threadIdx.x < topK) {
        sDists[threadIdx.x] = INFINITY;
        sIdx[threadIdx.x] = -1;
    }
    __syncthreads();

    if (threadIdx.x == 0) {
        int count = 0;
        for (int i = 0; i < nc; i++) {
            float d = cdists[i];
            if (count < topK) {
                int ins = count;
                for (int j = 0; j < count; j++) {
                    if (d < sDists[j]) { ins = j; break; }
                }
                for (int j = count; j > ins; j--) {
                    sDists[j] = sDists[j - 1];
                    sIdx[j] = sIdx[j - 1];
                }
                sDists[ins] = d;
                sIdx[ins] = i;
                count++;
            } else if (d < sDists[topK - 1]) {
                int ins = topK - 1;
                for (int j = 0; j < topK - 1; j++) {
                    if (d < sDists[j]) { ins = j; break; }
                }
                for (int j = topK - 1; j > ins; j--) {
                    sDists[j] = sDists[j - 1];
                    sIdx[j] = sIdx[j - 1];
                }
                sDists[ins] = d;
                sIdx[ins] = i;
            }
        }

        for (int j = 0; j < topK; j++) {
            outIndices[j] = sIdx[j];
        }
    }
}
