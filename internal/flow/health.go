package flow

import (
	"encoding/json"
	"net/http"
	"runtime"
	"sync"
	"time"
)

type HealthStatus string

const (
	HealthOK      HealthStatus = "ok"
	HealthDegraded HealthStatus = "degraded"
	HealthDown    HealthStatus = "down"
)

type HealthReport struct {
	Status      HealthStatus        `json:"status"`
	Time        time.Time           `json:"time"`
	Uptime      string              `json:"uptime"`
	Version     string              `json:"version,omitempty"`
	Workers     WorkerHealth        `json:"workers"`
	Memory      MemoryHealth        `json:"memory"`
	Goroutines  int                 `json:"goroutines"`
	Components  map[string]HealthStatus `json:"components"`
	Checks      []HealthCheck       `json:"checks,omitempty"`
}

type WorkerHealth struct {
	Active    int32 `json:"active"`
	Max       int32 `json:"max"`
	Utilization float64 `json:"utilization_pct"`
}

type MemoryHealth struct {
	AllocMB   float64 `json:"alloc_mb"`
	TotalMB   float64 `json:"total_mb"`
	SysMB     float64 `json:"sys_mb"`
	HeapMB    float64 `json:"heap_mb"`
}

type HealthCheck struct {
	Name   string      `json:"name"`
	Status HealthStatus `json:"status"`
	Error  string      `json:"error,omitempty"`
	Latency string     `json:"latency,omitempty"`
}

type HealthChecker func() HealthCheck

type Health struct {
	mu         sync.RWMutex
	startedAt  time.Time
	rm         *ResourceManager
	telemetry  *Telemetry
	checkers   map[string]HealthChecker
	version    string
}

func NewHealth(rm *ResourceManager, telemetry *Telemetry) *Health {
	return &Health{
		startedAt: time.Now(),
		rm:        rm,
		telemetry: telemetry,
		checkers:  make(map[string]HealthChecker),
	}
}

func (h *Health) SetVersion(v string) { h.version = v }

func (h *Health) RegisterChecker(name string, fn HealthChecker) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checkers[name] = fn
}

func (h *Health) Report() HealthReport {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	report := HealthReport{
		Time:     time.Now(),
		Uptime:   time.Since(h.startedAt).Round(time.Second).String(),
		Version:  h.version,
		Goroutines: runtime.NumGoroutine(),
		Memory: MemoryHealth{
			AllocMB:  float64(m.Alloc) / 1024 / 1024,
			TotalMB:  float64(m.TotalAlloc) / 1024 / 1024,
			SysMB:    float64(m.Sys) / 1024 / 1024,
			HeapMB:   float64(m.HeapAlloc) / 1024 / 1024,
		},
		Components: make(map[string]HealthStatus),
	}

	if h.rm != nil {
		active := h.rm.Active()
		maxW := h.rm.MaxWorkers()
		util := 0.0
		if maxW > 0 {
			util = float64(active) / float64(maxW) * 100
		}
		report.Workers = WorkerHealth{
			Active:      active,
			Max:         maxW,
			Utilization: util,
		}
	}

	report.Status = HealthOK

	for name, fn := range h.checkers {
		start := time.Now()
		check := fn()
		check.Latency = time.Since(start).String()
		report.Checks = append(report.Checks, check)
		report.Components[name] = check.Status
		if check.Status == HealthDown {
			report.Status = HealthDown
		} else if check.Status == HealthDegraded && report.Status == HealthOK {
			report.Status = HealthDegraded
		}
	}

	return report
}

func (h *Health) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	report := h.Report()
	w.Header().Set("Content-Type", "application/json")
	status := http.StatusOK
	if report.Status == HealthDown {
		status = http.StatusServiceUnavailable
	} else if report.Status == HealthDegraded {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(report)
}

func (h *Health) Live(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "alive"})
}

func (h *Health) Ready(w http.ResponseWriter, r *http.Request) {
	report := h.Report()
	w.Header().Set("Content-Type", "application/json")
	if report.Status == HealthDown {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "not_ready"})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
}

func (h *Health) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", h.ServeHTTP)
	mux.HandleFunc("/live", h.Live)
	mux.HandleFunc("/ready", h.Ready)
	return mux
}

func (h *Health) Print() string {
	report := h.Report()
	data, _ := json.MarshalIndent(report, "", "  ")
	return string(data)
}
