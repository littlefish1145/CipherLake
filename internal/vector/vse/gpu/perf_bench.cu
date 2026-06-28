// perf_bench.cu - Benchmark GPU operations to compare with Go+cgo overhead
// Compile: nvcc -arch=sm_120 -O2 perf_bench.cu kernels.cu -o perf_bench.exe

#include <cstdio>
#include <cstdlib>
#include <cmath>
#include <cstring>
#include <vector>
#include <chrono>
#include <random>

#include <cuda_runtime.h>

extern "C" {
__global__ void pq_dist_table(const float* q, const float* cb, float* tbl,
    int nq, int dim, int M, int subdim);
__global__ void pq_adc(const float* tbl, const unsigned char* codes, float* dists,
    int nq, int nv, int M);
__global__ void centroid_dist(const float* q, const float* cents, float* dists,
    int nq, int nc, int dim);
}

// Timer helper
struct Timer {
    std::chrono::high_resolution_clock::time_point start;
    void begin() { start = std::chrono::high_resolution_clock::now(); }
    double end() {
        auto end = std::chrono::high_resolution_clock::now();
        return std::chrono::duration<double, std::milli>(end - start).count();
    }
};

#define CUDA_CHECK(call) do { \
    cudaError_t e = (call); \
    if (e != cudaSuccess) { \
        fprintf(stderr, "CUDA error %s:%d: %s\n", __FILE__, __LINE__, cudaGetErrorString(e)); \
        exit(1); \
    } \
} while(0)

static std::mt19937 rng(42);

void fillRandom(float* arr, int n, float lo = -1.0f, float hi = 1.0f) {
    std::uniform_real_distribution<float> dist(lo, hi);
    for (int i = 0; i < n; i++) arr[i] = dist(rng);
}
void fillRandomU8(unsigned char* arr, int n) {
    std::uniform_int_distribution<int> dist(0, 255);
    for (int i = 0; i < n; i++) arr[i] = (unsigned char)dist(rng);
}

void bench_pq(int nv, int dim, int M, int subdim, int nc, int nProbe, int warmup) {
    int topK = 10;

    // Generate data
    std::vector<float> h_vecs(nv * dim);
    fillRandom(h_vecs.data(), nv * dim);

    std::vector<float> h_q(dim);
    fillRandom(h_q.data(), dim);

    // Random centroids
    std::vector<float> h_cents(nc * dim);
    fillRandom(h_cents.data(), nc * dim);

    // Random codebook
    std::vector<float> h_cb(M * 256 * subdim);
    fillRandom(h_cb.data(), M * 256 * subdim);

    // PQ codes
    std::vector<unsigned char> h_codes(nv * M);
    fillRandomU8(h_codes.data(), nv * M);

    // Allocate GPU
    float *d_q, *d_cents, *d_cdists, *d_cb, *d_tbl, *d_dists;
    unsigned char *d_codes;
    CUDA_CHECK(cudaMalloc(&d_q,      dim * sizeof(float)));
    CUDA_CHECK(cudaMalloc(&d_cents,  nc * dim * sizeof(float)));
    CUDA_CHECK(cudaMalloc(&d_cdists, nc * sizeof(float)));
    CUDA_CHECK(cudaMalloc(&d_cb,     M * 256 * subdim * sizeof(float)));
    CUDA_CHECK(cudaMalloc(&d_tbl,    M * 256 * sizeof(float)));
    CUDA_CHECK(cudaMalloc(&d_dists,  nv * sizeof(float)));
    CUDA_CHECK(cudaMalloc(&d_codes,  nv * M * sizeof(unsigned char)));

    CUDA_CHECK(cudaMemcpy(d_q,     h_q.data(),     dim * sizeof(float), cudaMemcpyHostToDevice));
    CUDA_CHECK(cudaMemcpy(d_cents, h_cents.data(), nc * dim * sizeof(float), cudaMemcpyHostToDevice));
    CUDA_CHECK(cudaMemcpy(d_cb,    h_cb.data(),    M * 256 * subdim * sizeof(float), cudaMemcpyHostToDevice));
    CUDA_CHECK(cudaMemcpy(d_codes, h_codes.data(), nv * M * sizeof(unsigned char), cudaMemcpyHostToDevice));

    std::vector<float> h_cd(nc);
    std::vector<float> h_dists(nv);

    // Warmup
    for (int i = 0; i < warmup; i++) {
        centroid_dist<<<dim3(1, nc), 256>>>(d_q, d_cents, d_cdists, 1, nc, dim);
        CUDA_CHECK(cudaGetLastError());
        CUDA_CHECK(cudaMemcpy(h_cd.data(), d_cdists, nc * sizeof(float), cudaMemcpyDeviceToHost));

        pq_dist_table<<<dim3(1, M), dim3(256, 2)>>>(d_q, d_cb, d_tbl, 1, dim, M, subdim);
        CUDA_CHECK(cudaGetLastError());

        int threads = 128;
        int blocks = (nv + threads - 1) / threads;
        pq_adc<<<dim3(1, blocks), threads>>>(d_tbl, d_codes, d_dists, 1, nv, M);
        CUDA_CHECK(cudaGetLastError());
        CUDA_CHECK(cudaMemcpy(h_dists.data(), d_dists, nv * sizeof(float), cudaMemcpyDeviceToHost));
    }
    CUDA_CHECK(cudaDeviceSynchronize());

    // Benchmark centroid + D2H
    Timer t;
    t.begin();
    for (int i = 0; i < 100; i++) {
        centroid_dist<<<dim3(1, nc), 256>>>(d_q, d_cents, d_cdists, 1, nc, dim);
        CUDA_CHECK(cudaMemcpy(h_cd.data(), d_cdists, nc * sizeof(float), cudaMemcpyDeviceToHost));
    }
    CUDA_CHECK(cudaDeviceSynchronize());
    double centroidTime = t.end() / 100.0;

    // Benchmark PQ dist table + ADC + D2H
    t.begin();
    for (int i = 0; i < 100; i++) {
        pq_dist_table<<<dim3(1, M), dim3(256, 2)>>>(d_q, d_cb, d_tbl, 1, dim, M, subdim);
        CUDA_CHECK(cudaGetLastError());
        int threads = 128;
        int blocks = (nv + threads - 1) / threads;
        pq_adc<<<dim3(1, blocks), threads>>>(d_tbl, d_codes, d_dists, 1, nv, M);
        CUDA_CHECK(cudaGetLastError());
        CUDA_CHECK(cudaMemcpy(h_dists.data(), d_dists, nv * sizeof(float), cudaMemcpyDeviceToHost));
    }
    CUDA_CHECK(cudaDeviceSynchronize());
    double pqTime = t.end() / 100.0;

    // Benchmark all CUDA ops (including malloc/free) to simulate Go path
    t.begin();
    for (int i = 0; i < 100; i++) {
        float *tmp_q, *tmp_cd, *tmp_tbl, *tmp_d;
        CUDA_CHECK(cudaMalloc(&tmp_q,  dim * sizeof(float)));
        CUDA_CHECK(cudaMalloc(&tmp_cd, nc * sizeof(float)));
        CUDA_CHECK(cudaMalloc(&tmp_tbl, M * 256 * sizeof(float)));
        CUDA_CHECK(cudaMalloc(&tmp_d, nv * sizeof(float)));
        CUDA_CHECK(cudaMemcpy(tmp_q, h_q.data(), dim * sizeof(float), cudaMemcpyHostToDevice));

        centroid_dist<<<dim3(1, nc), 256>>>(tmp_q, d_cents, tmp_cd, 1, nc, dim);
        CUDA_CHECK(cudaMemcpy(h_cd.data(), tmp_cd, nc * sizeof(float), cudaMemcpyDeviceToHost));

        pq_dist_table<<<dim3(1, M), dim3(256, 2)>>>(tmp_q, d_cb, tmp_tbl, 1, dim, M, subdim);
        CUDA_CHECK(cudaGetLastError());
        int threads = 128;
        int blocks = (nv + threads - 1) / threads;
        pq_adc<<<dim3(1, blocks), threads>>>(tmp_tbl, d_codes, tmp_d, 1, nv, M);
        CUDA_CHECK(cudaGetLastError());
        CUDA_CHECK(cudaMemcpy(h_dists.data(), tmp_d, nv * sizeof(float), cudaMemcpyDeviceToHost));

        cudaFree(tmp_q);
        cudaFree(tmp_cd);
        cudaFree(tmp_tbl);
        cudaFree(tmp_d);
    }
    CUDA_CHECK(cudaDeviceSynchronize());
    double fullTime = t.end() / 100.0;

    // Benchmark full malloc-free cycle (no GPU work) to measure driver overhead
    t.begin();
    for (int i = 0; i < 100; i++) {
        float *a, *b;
        CUDA_CHECK(cudaMalloc(&a, nv * sizeof(float)));
        CUDA_CHECK(cudaMalloc(&b, nv * sizeof(float)));
        CUDA_CHECK(cudaMemcpy(a, h_dists.data(), nv * sizeof(float), cudaMemcpyHostToDevice));
        CUDA_CHECK(cudaMemcpy(b, a, nv * sizeof(float), cudaMemcpyDeviceToHost));
        cudaFree(a);
        cudaFree(b);
    }
    CUDA_CHECK(cudaDeviceSynchronize());
    double allocTime = t.end() / 100.0;

    printf("%5d x %3dd | M=%-2d nc=%-3d | centroid=%7.3fms  pq=%7.3fms  full=%7.3fms  alloc=%7.3fms\n",
           nv, dim, M, nc, centroidTime, pqTime, fullTime, allocTime);

    cudaFree(d_q); cudaFree(d_cents); cudaFree(d_cdists);
    cudaFree(d_cb); cudaFree(d_tbl); cudaFree(d_dists); cudaFree(d_codes);
}

int main() {
    int devCount = 0;
    cudaGetDeviceCount(&devCount);
    if (devCount == 0) {
        fprintf(stderr, "No CUDA device!\n");
        return 1;
    }
    cudaDeviceProp prop;
    cudaGetDeviceProperties(&prop, 0);
    printf("GPU: %s (SM %d.%d)\n", prop.name, prop.major, prop.minor);

    printf("\n--- Perf: PQ pipeline breakdown ---\n");
    printf("       config       | centroid   pq(2kern+D2H)  full(+alloc)  alloc+copy\n");
    printf("-------------------|----------------------------------------------------\n");

    // Matches our Go benchmark configs
    bench_pq(10000, 128, 32, 4, 256, 64, 3);
    bench_pq(10000, 384, 96, 4, 256, 64, 3);
    bench_pq(50000, 128, 32, 4, 256, 64, 3);
    bench_pq(100000, 128, 32, 4, 256, 64, 3);

    return 0;
}
