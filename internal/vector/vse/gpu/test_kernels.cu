// test_kernels.cu - Standalone test program for kernels.cu
// Compile:  nvcc -arch=sm_120 -O2 test_kernels.cu kernels.cu -o test_kernels.exe
// Run:      test_kernels.exe

#include <cstdio>
#include <cstdlib>
#include <cmath>
#include <cstring>
#include <vector>
#include <algorithm>
#include <random>

#include <cuda_runtime.h>

// Forward declarations of kernels defined in kernels.cu
extern "C" {
__global__ void pq_dist_table(const float* q, const float* cb, float* tbl,
    int nq, int dim, int M, int subdim);
__global__ void pq_adc(const float* tbl, const unsigned char* codes, float* dists,
    int nq, int nv, int M);
__global__ void centroid_dist(const float* q, const float* cents, float* dists,
    int nq, int nc, int dim);
__global__ void compute_norms(const float* vecs, float* norms, int nv, int dim);
}

// ---------------------------------------------------------------------------
// Utility
// ---------------------------------------------------------------------------

#define CUDA_CHECK(call) do { \
    cudaError_t e = (call); \
    if (e != cudaSuccess) { \
        fprintf(stderr, "CUDA error %s:%d: %s\n", __FILE__, __LINE__, cudaGetErrorString(e)); \
        exit(1); \
    } \
} while(0)

static int g_pass = 0, g_fail = 0;

void check(const char* name, bool ok) {
    printf("  [%s] %s\n", ok ? "PASS" : "FAIL", name);
    if (ok) g_pass++; else g_fail++;
}

// Compare two float arrays with relative tolerance
bool compareFloat(const float* a, const float* b, int n, float relTol = 1e-3f, float absTol = 1e-4f) {
    for (int i = 0; i < n; i++) {
        float diff = fabsf(a[i] - b[i]);
        float mag  = fmaxf(fabsf(a[i]), fabsf(b[i]));
        if (diff > absTol && diff > relTol * mag) {
            fprintf(stderr, "    mismatch at [%d]: gpu=%.6f cpu=%.6f diff=%.6f\n", i, a[i], b[i], diff);
            return false;
        }
    }
    return true;
}

// ---------------------------------------------------------------------------
// Random data generation
// ---------------------------------------------------------------------------

static std::mt19937 rng(42);

void fillRandom(float* arr, int n, float lo = -1.0f, float hi = 1.0f) {
    std::uniform_real_distribution<float> dist(lo, hi);
    for (int i = 0; i < n; i++) arr[i] = dist(rng);
}

void fillRandomU8(unsigned char* arr, int n) {
    std::uniform_int_distribution<int> dist(0, 255);
    for (int i = 0; i < n; i++) arr[i] = (unsigned char)dist(rng);
}

// ===========================================================================
// Test 1: compute_norms
// ===========================================================================

void test_compute_norms() {
    printf("\n=== test_compute_norms ===\n");
    int nv = 1000, dim = 128;

    std::vector<float> h_vecs(nv * dim);
    fillRandom(h_vecs.data(), nv * dim);

    // CPU reference
    std::vector<float> h_norms_cpu(nv);
    for (int i = 0; i < nv; i++) {
        float s = 0;
        for (int j = 0; j < dim; j++) s += h_vecs[i*dim+j] * h_vecs[i*dim+j];
        h_norms_cpu[i] = s;
    }

    // GPU
    float *d_vecs, *d_norms;
    CUDA_CHECK(cudaMalloc(&d_vecs,  nv * dim * sizeof(float)));
    CUDA_CHECK(cudaMalloc(&d_norms, nv       * sizeof(float)));
    CUDA_CHECK(cudaMemcpy(d_vecs, h_vecs.data(), nv*dim*sizeof(float), cudaMemcpyHostToDevice));

    int threads = 256;
    int blocks  = (nv + threads - 1) / threads;
    compute_norms<<<blocks, threads>>>(d_vecs, d_norms, nv, dim);
    CUDA_CHECK(cudaGetLastError());
    CUDA_CHECK(cudaDeviceSynchronize());

    std::vector<float> h_norms_gpu(nv);
    CUDA_CHECK(cudaMemcpy(h_norms_gpu.data(), d_norms, nv*sizeof(float), cudaMemcpyDeviceToHost));

    check("compute_norms values", compareFloat(h_norms_gpu.data(), h_norms_cpu.data(), nv));

    cudaFree(d_vecs);
    cudaFree(d_norms);
}

// ===========================================================================
// Test 2: centroid_dist
// ===========================================================================

void test_centroid_dist() {
    printf("\n=== test_centroid_dist ===\n");
    int nq = 8, nc = 64, dim = 128;

    std::vector<float> h_q(nq * dim);
    std::vector<float> h_cents(nc * dim);
    fillRandom(h_q.data(), nq * dim);
    fillRandom(h_cents.data(), nc * dim);

    // CPU reference
    std::vector<float> h_dists_cpu(nq * nc);
    for (int q = 0; q < nq; q++) {
        for (int c = 0; c < nc; c++) {
            float s = 0;
            for (int d = 0; d < dim; d++) {
                float diff = h_q[q*dim+d] - h_cents[c*dim+d];
                s += diff * diff;
            }
            h_dists_cpu[q*nc+c] = s;
        }
    }

    // GPU
    float *d_q, *d_cents, *d_dists;
    CUDA_CHECK(cudaMalloc(&d_q,      nq * dim * sizeof(float)));
    CUDA_CHECK(cudaMalloc(&d_cents,  nc * dim * sizeof(float)));
    CUDA_CHECK(cudaMalloc(&d_dists,  nq * nc  * sizeof(float)));
    CUDA_CHECK(cudaMemcpy(d_q,     h_q.data(),     nq*dim*sizeof(float), cudaMemcpyHostToDevice));
    CUDA_CHECK(cudaMemcpy(d_cents, h_cents.data(),  nc*dim*sizeof(float), cudaMemcpyHostToDevice));

    dim3 grid(nq, nc);
    dim3 block(256);
    centroid_dist<<<grid, block>>>(d_q, d_cents, d_dists, nq, nc, dim);
    CUDA_CHECK(cudaGetLastError());
    CUDA_CHECK(cudaDeviceSynchronize());

    std::vector<float> h_dists_gpu(nq * nc);
    CUDA_CHECK(cudaMemcpy(h_dists_gpu.data(), d_dists, nq*nc*sizeof(float), cudaMemcpyDeviceToHost));

    check("centroid_dist values", compareFloat(h_dists_gpu.data(), h_dists_cpu.data(), nq*nc));

    cudaFree(d_q); cudaFree(d_cents); cudaFree(d_dists);
}

// ===========================================================================
// Test 3: pq_dist_table
// ===========================================================================

void test_pq_dist_table() {
    printf("\n=== test_pq_dist_table ===\n");
    int nq = 4, dim = 128, M = 32, subdim = 4;  // dim = M * subdim

    std::vector<float> h_q(nq * dim);
    std::vector<float> h_cb(M * 256 * subdim);
    fillRandom(h_q.data(),  nq * dim);
    fillRandom(h_cb.data(), M * 256 * subdim);

    // CPU reference
    std::vector<float> h_tbl_cpu(nq * M * 256);
    for (int qid = 0; qid < nq; qid++) {
        for (int m = 0; m < M; m++) {
            for (int k = 0; k < 256; k++) {
                float s = 0;
                for (int i = 0; i < subdim; i++) {
                    float d = h_q[qid*dim + m*subdim + i] - h_cb[(m*256+k)*subdim + i];
                    s += d * d;
                }
                h_tbl_cpu[(qid*M+m)*256 + k] = s;
            }
        }
    }

    // GPU
    float *d_q, *d_cb, *d_tbl;
    CUDA_CHECK(cudaMalloc(&d_q,   nq * dim * sizeof(float)));
    CUDA_CHECK(cudaMalloc(&d_cb,  M * 256 * subdim * sizeof(float)));
    CUDA_CHECK(cudaMalloc(&d_tbl, nq * M * 256 * sizeof(float)));
    CUDA_CHECK(cudaMemcpy(d_q,  h_q.data(),  nq*dim*sizeof(float), cudaMemcpyHostToDevice));
    CUDA_CHECK(cudaMemcpy(d_cb, h_cb.data(),  M*256*subdim*sizeof(float), cudaMemcpyHostToDevice));

    dim3 grid(nq, M);
    dim3 block(256, 2);
    pq_dist_table<<<grid, block>>>(d_q, d_cb, d_tbl, nq, dim, M, subdim);
    CUDA_CHECK(cudaGetLastError());
    CUDA_CHECK(cudaDeviceSynchronize());

    std::vector<float> h_tbl_gpu(nq * M * 256);
    CUDA_CHECK(cudaMemcpy(h_tbl_gpu.data(), d_tbl, nq*M*256*sizeof(float), cudaMemcpyDeviceToHost));

    check("pq_dist_table values", compareFloat(h_tbl_gpu.data(), h_tbl_cpu.data(), nq*M*256));

    cudaFree(d_q); cudaFree(d_cb); cudaFree(d_tbl);
}

// ===========================================================================
// Test 4: pq_adc
// ===========================================================================

void test_pq_adc() {
    printf("\n=== test_pq_adc ===\n");
    int nq = 4, nv = 500, M = 32;

    // Distance table [nq, M, 256]
    std::vector<float> h_tbl(nq * M * 256);
    fillRandom(h_tbl.data(), nq * M * 256, 0.0f, 10.0f);

    // PQ codes [nv, M]
    std::vector<unsigned char> h_codes(nv * M);
    fillRandomU8(h_codes.data(), nv * M);

    // CPU reference
    std::vector<float> h_dists_cpu(nq * nv);
    for (int q = 0; q < nq; q++) {
        for (int v = 0; v < nv; v++) {
            float s = 0;
            for (int m = 0; m < M; m++) {
                s += h_tbl[(q*M+m)*256 + h_codes[v*M+m]];
            }
            h_dists_cpu[q*nv+v] = s;
        }
    }

    // GPU
    float *d_tbl;
    unsigned char *d_codes;
    float *d_dists;
    CUDA_CHECK(cudaMalloc(&d_tbl,   nq * M * 256 * sizeof(float)));
    CUDA_CHECK(cudaMalloc(&d_codes, nv * M * sizeof(unsigned char)));
    CUDA_CHECK(cudaMalloc(&d_dists, nq * nv * sizeof(float)));
    CUDA_CHECK(cudaMemcpy(d_tbl,   h_tbl.data(),   nq*M*256*sizeof(float), cudaMemcpyHostToDevice));
    CUDA_CHECK(cudaMemcpy(d_codes, h_codes.data(), nv*M*sizeof(unsigned char), cudaMemcpyHostToDevice));

    int threads = 128;
    int blocks  = (nv + threads - 1) / threads;
    dim3 grid(nq, blocks);
    dim3 block(threads);
    pq_adc<<<grid, block>>>(d_tbl, d_codes, d_dists, nq, nv, M);
    CUDA_CHECK(cudaGetLastError());
    CUDA_CHECK(cudaDeviceSynchronize());

    std::vector<float> h_dists_gpu(nq * nv);
    CUDA_CHECK(cudaMemcpy(h_dists_gpu.data(), d_dists, nq*nv*sizeof(float), cudaMemcpyDeviceToHost));

    check("pq_adc values", compareFloat(h_dists_gpu.data(), h_dists_cpu.data(), nq*nv, 1e-2f, 1e-2f));

    cudaFree(d_tbl); cudaFree(d_codes); cudaFree(d_dists);
}

// ===========================================================================
// Test 5: End-to-end PQ search pipeline
// ===========================================================================

void test_pq_end_to_end() {
    printf("\n=== test_pq_end_to_end (PQ search pipeline) ===\n");
    int nq = 2, nv = 200, dim = 64, M = 16, subdim = 4;  // dim = M * subdim
    int topK = 5;

    // Generate vectors
    std::vector<float> h_vecs(nv * dim);
    fillRandom(h_vecs.data(), nv * dim);

    std::vector<float> h_q(nq * dim);
    fillRandom(h_q.data(), nq * dim);

    // Simple PQ: use 256 centroids per subspace (random codebook)
    std::vector<float> h_cb(M * 256 * subdim);
    fillRandom(h_cb.data(), M * 256 * subdim);

    // Generate PQ codes for each vector (nearest codeword)
    std::vector<unsigned char> h_codes(nv * M);
    for (int v = 0; v < nv; v++) {
        for (int m = 0; m < M; m++) {
            float best = 1e30f; int bestK = 0;
            for (int k = 0; k < 256; k++) {
                float s = 0;
                for (int i = 0; i < subdim; i++) {
                    float d = h_vecs[v*dim + m*subdim + i] - h_cb[(m*256+k)*subdim + i];
                    s += d * d;
                }
                if (s < best) { best = s; bestK = k; }
            }
            h_codes[v*M + m] = (unsigned char)bestK;
        }
    }

    // CPU reference: PQ-ADC approximate distances
    std::vector<float> h_tbl_cpu(nq * M * 256);
    for (int qid = 0; qid < nq; qid++) {
        for (int m = 0; m < M; m++) {
            for (int k = 0; k < 256; k++) {
                float s = 0;
                for (int i = 0; i < subdim; i++) {
                    float d = h_q[qid*dim + m*subdim + i] - h_cb[(m*256+k)*subdim + i];
                    s += d * d;
                }
                h_tbl_cpu[(qid*M+m)*256 + k] = s;
            }
        }
    }

    std::vector<float> h_dists_cpu(nq * nv);
    for (int q = 0; q < nq; q++) {
        for (int v = 0; v < nv; v++) {
            float s = 0;
            for (int m = 0; m < M; m++)
                s += h_tbl_cpu[(q*M+m)*256 + h_codes[v*M+m]];
            h_dists_cpu[q*nv+v] = s;
        }
    }

    // GPU: full pipeline
    float *d_q, *d_cb, *d_tbl, *d_dists;
    unsigned char *d_codes;
    CUDA_CHECK(cudaMalloc(&d_q,     nq * dim * sizeof(float)));
    CUDA_CHECK(cudaMalloc(&d_cb,    M * 256 * subdim * sizeof(float)));
    CUDA_CHECK(cudaMalloc(&d_tbl,   nq * M * 256 * sizeof(float)));
    CUDA_CHECK(cudaMalloc(&d_codes, nv * M * sizeof(unsigned char)));
    CUDA_CHECK(cudaMalloc(&d_dists, nq * nv * sizeof(float)));
    CUDA_CHECK(cudaMemcpy(d_q,     h_q.data(),     nq*dim*sizeof(float), cudaMemcpyHostToDevice));
    CUDA_CHECK(cudaMemcpy(d_cb,    h_cb.data(),    M*256*subdim*sizeof(float), cudaMemcpyHostToDevice));
    CUDA_CHECK(cudaMemcpy(d_codes, h_codes.data(), nv*M*sizeof(unsigned char), cudaMemcpyHostToDevice));

    // Step 1: pq_dist_table
    {
        dim3 grid(nq, M);
        dim3 block(256, 2);
        pq_dist_table<<<grid, block>>>(d_q, d_cb, d_tbl, nq, dim, M, subdim);
        CUDA_CHECK(cudaGetLastError());
    }

    // Step 2: pq_adc
    {
        int threads = 128;
        int blocks  = (nv + threads - 1) / threads;
        dim3 grid(nq, blocks);
        dim3 block(threads);
        pq_adc<<<grid, block>>>(d_tbl, d_codes, d_dists, nq, nv, M);
        CUDA_CHECK(cudaGetLastError());
    }

    CUDA_CHECK(cudaDeviceSynchronize());

    std::vector<float> h_dists_gpu(nq * nv);
    CUDA_CHECK(cudaMemcpy(h_dists_gpu.data(), d_dists, nq*nv*sizeof(float), cudaMemcpyDeviceToHost));

    // Verify distances
    check("end-to-end PQ distances", compareFloat(h_dists_gpu.data(), h_dists_cpu.data(), nq*nv, 1e-2f, 1e-2f));

    // Verify topK ordering
    for (int q = 0; q < nq; q++) {
        std::vector<std::pair<float,int>> cpu_sorted, gpu_sorted;
        for (int v = 0; v < nv; v++) {
            cpu_sorted.push_back({h_dists_cpu[q*nv+v], v});
            gpu_sorted.push_back({h_dists_gpu[q*nv+v], v});
        }
        std::sort(cpu_sorted.begin(), cpu_sorted.end());
        std::sort(gpu_sorted.begin(), gpu_sorted.end());

        bool topKMatch = true;
        for (int i = 0; i < topK && i < nv; i++) {
            if (cpu_sorted[i].second != gpu_sorted[i].second) { topKMatch = false; break; }
        }
        char buf[64];
        snprintf(buf, sizeof(buf), "topK indices (query %d)", q);
        check(buf, topKMatch);
    }

    cudaFree(d_q); cudaFree(d_cb); cudaFree(d_tbl);
    cudaFree(d_codes); cudaFree(d_dists);
}

// ===========================================================================
// main
// ===========================================================================

int main() {
    // Detect GPU
    int devCount = 0;
    cudaGetDeviceCount(&devCount);
    if (devCount == 0) {
        fprintf(stderr, "No CUDA device found!\n");
        return 1;
    }

    cudaDeviceProp prop;
    cudaGetDeviceProperties(&prop, 0);
    int rtVer = 0;
    cudaRuntimeGetVersion(&rtVer);
    printf("GPU: %s (SM %d.%d, %d MB, %d cores)\n",
           prop.name, prop.major, prop.minor,
           (int)(prop.totalGlobalMem / 1048576),
           prop.multiProcessorCount * 128);
    printf("CUDA Runtime: %d.%d\n", rtVer / 1000, (rtVer % 1000) / 10);

    // Run tests
    test_compute_norms();
    test_centroid_dist();
    test_pq_dist_table();
    test_pq_adc();
    test_pq_end_to_end();

    // Summary
    printf("\n========================================\n");
    printf("Results: %d passed, %d failed\n", g_pass, g_fail);
    printf("========================================\n");
    return g_fail > 0 ? 1 : 0;
}
