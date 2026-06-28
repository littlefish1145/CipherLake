package prefetch

import (
	"container/heap"
	"container/list"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Priority int

const (
	PriorityLow    Priority = 0
	PriorityNormal Priority = 1
	PriorityHigh   Priority = 2
	PriorityUrgent Priority = 3
)

type FetchJob struct {
	Key       string
	URL       string
	LocalPath string
	Size      int64
	Priority  Priority
	Index     int
	done      chan struct{}
	err       error
}

type priorityQueue []*FetchJob

func (pq priorityQueue) Len() int { return len(pq) }

func (pq priorityQueue) Less(i, j int) bool {
	if pq[i].Priority != pq[j].Priority {
		return pq[i].Priority > pq[j].Priority
	}
	return pq[i].Index < pq[j].Index
}

func (pq priorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].Index = i
	pq[j].Index = j
}

func (pq *priorityQueue) Push(x interface{}) {
	n := len(*pq)
	job := x.(*FetchJob)
	job.Index = n
	*pq = append(*pq, job)
}

func (pq *priorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	job := old[n-1]
	old[n-1] = nil
	job.Index = -1
	*pq = old[:n-1]
	return job
}

type Engine struct {
	mu         sync.Mutex
	cond       *sync.Cond
	queue      priorityQueue
	pending    map[string]*FetchJob
	workers    int
	maxBytes   int64
	cachedSize int64
	cacheDir   string
	client     *http.Client
	active     int32
	stopped    bool
	stopCh     chan struct{}
	wg         sync.WaitGroup

	// LRU 缓存追踪：记录已下载文件按访问顺序淘汰
	lruList  *list.List
	lruIndex map[string]*list.Element

	// 统计指标
	fetchCount int64
}

// lruEntry 记录一个已缓存文件的元数据
type lruEntry struct {
	key  string
	path string
	size int64
}

func NewEngine(workers int, cacheDir string, maxBytes int64) *Engine {
	e := &Engine{
		queue:    make(priorityQueue, 0),
		pending:  make(map[string]*FetchJob),
		workers:  workers,
		maxBytes: maxBytes,
		cacheDir: cacheDir,
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: workers,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		stopCh:    make(chan struct{}),
		lruList:   list.New(),
		lruIndex:  make(map[string]*list.Element),
	}
	e.cond = sync.NewCond(&e.mu)
	heap.Init(&e.queue)
	os.MkdirAll(cacheDir, 0755)

	for i := 0; i < workers; i++ {
		e.wg.Add(1)
		go e.worker()
	}
	return e
}

func (e *Engine) Enqueue(key, url, localPath string, size int64, pri Priority) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.stopped {
		return
	}
	if _, ok := e.pending[key]; ok {
		return
	}
	if _, err := os.Stat(localPath); err == nil {
		return
	}

	job := &FetchJob{
		Key:       key,
		URL:       url,
		LocalPath: localPath,
		Size:      size,
		Priority:  pri,
		done:      make(chan struct{}),
	}
	e.pending[key] = job
	heap.Push(&e.queue, job)
	e.cond.Signal()
}

func (e *Engine) Wait(key string) error {
	e.mu.Lock()
	job, ok := e.pending[key]
	e.mu.Unlock()
	if !ok {
		return nil
	}
	<-job.done
	return job.err
}

func (e *Engine) Stop() {
	e.mu.Lock()
	e.stopped = true
	e.cond.Broadcast()
	e.mu.Unlock()
	e.wg.Wait()
}

func (e *Engine) worker() {
	defer e.wg.Done()

	for {
		e.mu.Lock()
		for e.queue.Len() == 0 && !e.stopped {
			e.cond.Wait()
		}
		if e.stopped {
			e.mu.Unlock()
			return
		}
		job := heap.Pop(&e.queue).(*FetchJob)
		e.mu.Unlock()

		err := e.fetch(job)
		job.err = err

		e.mu.Lock()
		delete(e.pending, job.Key)
		close(job.done)
		e.mu.Unlock()
	}
}

func (e *Engine) fetch(job *FetchJob) error {
	os.MkdirAll(filepath.Dir(job.LocalPath), 0755)

	tmpPath := job.LocalPath + ".tmp"

	req, err := http.NewRequest("GET", job.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", "bytes=0-")

	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	_, err = io.Copy(out, resp.Body)
	out.Close()
	if err != nil {
		os.Remove(tmpPath)
		return err
	}

	os.Rename(tmpPath, job.LocalPath)

	e.mu.Lock()
	e.cachedSize += job.Size
	e.fetchCount++
	// 新文件加入 LRU 头部
	entry := &lruEntry{key: job.Key, path: job.LocalPath, size: job.Size}
	e.lruIndex[job.Key] = e.lruList.PushFront(entry)
	// 超过容量时从尾部淘汰最久未使用的文件
	e.evictLocked()
	e.mu.Unlock()

	return nil
}

// FetchCount 返回已完成的预取次数
func (e *Engine) FetchCount() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.fetchCount
}

// evictLocked 在持有 mu 的情况下淘汰 LRU 尾部文件直到 cachedSize <= maxBytes
func (e *Engine) evictLocked() {
	for e.cachedSize > e.maxBytes && e.lruList.Len() > 0 {
		elem := e.lruList.Back()
		entry := elem.Value.(*lruEntry)
		e.lruList.Remove(elem)
		delete(e.lruIndex, entry.key)
		// 删除磁盘文件（忽略错误：文件可能已被外部移除）
		os.Remove(entry.path)
		e.cachedSize -= entry.size
		if e.cachedSize < 0 {
			e.cachedSize = 0
		}
	}
}

// Touch 将 key 对应的缓存项移动到 LRU 头部，标记为最近访问
func (e *Engine) Touch(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if elem, ok := e.lruIndex[key]; ok {
		e.lruList.MoveToFront(elem)
	}
}

func (e *Engine) CachedSize() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cachedSize
}
