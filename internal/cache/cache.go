package cache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

type ObjectCache struct {
	mu       sync.RWMutex
	items    map[string]*list.Element
	lruList  *list.List
	maxBytes int64
	ttl      time.Duration
	size     int64
	hits     int64
	misses   int64
}

type CacheItem struct {
	Key         string
	Value       []byte
	Size        int64
	CreatedAt   time.Time
	AccessedAt  time.Time
	AccessCount int64
}

func NewObjectCache(maxBytes int64, ttl time.Duration) (*ObjectCache, error) {
	if maxBytes <= 0 {
		maxBytes = 30 << 30
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}

	return &ObjectCache{
		items:    make(map[string]*list.Element),
		lruList:  list.New(),
		maxBytes: maxBytes,
		ttl:      ttl,
	}, nil
}

func (c *ObjectCache) Get(ctx context.Context, key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, exists := c.items[key]
	if !exists {
		c.misses++
		return nil, false
	}

	item := elem.Value.(*CacheItem)

	if time.Since(item.CreatedAt) > c.ttl {
		c.deleteElement(elem)
		c.misses++
		return nil, false
	}

	item.AccessedAt = time.Now()
	item.AccessCount++
	c.lruList.MoveToFront(elem)

	c.hits++
	return item.Value, true
}

func (c *ObjectCache) Set(ctx context.Context, key string, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	itemSize := int64(len(value))

	if elem, exists := c.items[key]; exists {
		oldItem := elem.Value.(*CacheItem)
		c.size -= oldItem.Size
		c.lruList.Remove(elem)
	}

	for c.size+itemSize > c.maxBytes && c.lruList.Len() > 0 {
		c.evictOldest()
	}

	item := &CacheItem{
		Key:         key,
		Value:       value,
		Size:        itemSize,
		CreatedAt:   time.Now(),
		AccessedAt:  time.Now(),
		AccessCount: 1,
	}

	elem := c.lruList.PushFront(item)
	c.items[key] = elem
	c.size += itemSize
}

func (c *ObjectCache) Delete(ctx context.Context, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, exists := c.items[key]; exists {
		c.deleteElement(elem)
	}
}

func (c *ObjectCache) Clear(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items = make(map[string]*list.Element)
	c.lruList = list.New()
	c.size = 0
}

func (c *ObjectCache) evictOldest() {
	elem := c.lruList.Back()
	if elem != nil {
		c.deleteElement(elem)
	}
}

func (c *ObjectCache) deleteElement(elem *list.Element) {
	item := elem.Value.(*CacheItem)
	delete(c.items, item.Key)
	c.lruList.Remove(elem)
	c.size -= item.Size
}

func (c *ObjectCache) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return CacheStats{
		ItemCount: int64(len(c.items)),
		SizeBytes: c.size,
		MaxBytes:  c.maxBytes,
		Hits:      c.hits,
		Misses:    c.misses,
		HitRate:   c.calculateHitRate(),
	}
}

func (c *ObjectCache) calculateHitRate() float64 {
	total := c.hits + c.misses
	if total == 0 {
		return 0
	}
	return float64(c.hits) / float64(total)
}

type CacheStats struct {
	ItemCount int64   `json:"item_count"`
	SizeBytes int64   `json:"size_bytes"`
	MaxBytes  int64   `json:"max_bytes"`
	Hits      int64   `json:"hits"`
	Misses    int64   `json:"misses"`
	HitRate   float64 `json:"hit_rate"`
}

type MetadataCache struct {
	mu    sync.RWMutex
	items map[string]*MetadataCacheItem
	ttl   time.Duration
}

type MetadataCacheItem struct {
	Value     map[string]interface{}
	CreatedAt time.Time
}

func NewMetadataCache(ttl time.Duration) *MetadataCache {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}

	return &MetadataCache{
		items: make(map[string]*MetadataCacheItem),
		ttl:   ttl,
	}
}

func (m *MetadataCache) Get(key string) (map[string]interface{}, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	item, exists := m.items[key]
	if !exists {
		return nil, false
	}

	if time.Since(item.CreatedAt) > m.ttl {
		return nil, false
	}

	return item.Value, true
}

func (m *MetadataCache) Set(key string, value map[string]interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.items[key] = &MetadataCacheItem{
		Value:     value,
		CreatedAt: time.Now(),
	}
}

func (m *MetadataCache) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.items, key)
}

func (m *MetadataCache) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.items = make(map[string]*MetadataCacheItem)
}

type QueryResultCache struct {
	mu      sync.RWMutex
	items   map[string]*QueryCacheItem
	ttl     time.Duration
	maxSize int
}

type QueryCacheItem struct {
	Results   interface{}
	CreatedAt time.Time
}

func NewQueryResultCache(ttl time.Duration, maxSize int) *QueryResultCache {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if maxSize <= 0 {
		maxSize = 10000
	}

	return &QueryResultCache{
		items:   make(map[string]*QueryCacheItem),
		ttl:     ttl,
		maxSize: maxSize,
	}
}

func (q *QueryResultCache) Get(key string) (interface{}, bool) {
	q.mu.RLock()
	defer q.mu.RUnlock()

	item, exists := q.items[key]
	if !exists {
		return nil, false
	}

	if time.Since(item.CreatedAt) > q.ttl {
		return nil, false
	}

	return item.Results, true
}

func (q *QueryResultCache) Set(key string, results interface{}) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.items) >= q.maxSize {
		q.evictOldest()
	}

	q.items[key] = &QueryCacheItem{
		Results:   results,
		CreatedAt: time.Now(),
	}
}

func (q *QueryResultCache) evictOldest() {
	var oldestKey string
	var oldestTime time.Time

	for k, v := range q.items {
		if oldestTime.IsZero() || v.CreatedAt.Before(oldestTime) {
			oldestKey = k
			oldestTime = v.CreatedAt
		}
	}

	if oldestKey != "" {
		delete(q.items, oldestKey)
	}
}

func (q *QueryResultCache) Clear() {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.items = make(map[string]*QueryCacheItem)
}
