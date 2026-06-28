package cache

import (
	"container/list"
	"sync"
)

type Cache struct {
	mu       sync.RWMutex
	maxSize  int
	items    map[string]*entry
	lruList  *list.List
	hitCount int64
	getCount int64
}

type entry struct {
	key   string
	value interface{}
	el    *list.Element
}

func NewCache(maxSize int) *Cache {
	if maxSize <= 0 {
		maxSize = 10000
	}
	return &Cache{
		maxSize: maxSize,
		items:   make(map[string]*entry),
		lruList: list.New(),
	}
}

func (c *Cache) Get(key string) (interface{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.getCount++
	if e, ok := c.items[key]; ok {
		c.lruList.MoveToFront(e.el)
		c.hitCount++
		return e.value, true
	}
	return nil, false
}

func (c *Cache) Set(key string, value interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.items[key]; ok {
		c.lruList.MoveToFront(e.el)
		e.value = value
		return
	}

	if len(c.items) >= c.maxSize {
		c.evictOne()
	}

	el := c.lruList.PushFront(key)
	c.items[key] = &entry{
		key:   key,
		value: value,
		el:    el,
	}
}

func (c *Cache) evictOne() {
	back := c.lruList.Back()
	if back == nil {
		return
	}
	key := back.Value.(string)
	delete(c.items, key)
	c.lruList.Remove(back)
}

func (c *Cache) Remove(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.items[key]; ok {
		c.lruList.Remove(e.el)
		delete(c.items, key)
	}
}

func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items = make(map[string]*entry)
	c.lruList = list.New()
	c.hitCount = 0
	c.getCount = 0
}

func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

func (c *Cache) HitRate() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.getCount == 0 {
		return 1.0
	}
	return float64(c.hitCount) / float64(c.getCount)
}

type arcEntry struct {
	key   string
	value interface{}
	el    *list.Element
	inT2  bool
}

type ARC struct {
	mu       sync.Mutex
	p        int
	c        int
	t1       *list.List
	t2       *list.List
	b1       *list.List
	b2       *list.List
	items    map[string]*arcEntry
	hitCount int64
	getCount int64
}

func NewARC(capacity int) *ARC {
	if capacity <= 0 {
		capacity = 10000
	}
	return &ARC{
		c:     capacity,
		t1:    list.New(),
		t2:    list.New(),
		b1:    list.New(),
		b2:    list.New(),
		items: make(map[string]*arcEntry),
	}
}

func (a *ARC) Get(key string) (interface{}, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.getCount++
	e, ok := a.items[key]
	if !ok {
		return nil, false
	}
	a.hitCount++

	if !e.inT2 {
		a.t1.Remove(e.el)
		e.inT2 = true
	} else {
		a.t2.Remove(e.el)
	}
	e.el = a.t2.PushFront(e.key)
	return e.value, true
}

func (a *ARC) Set(key string, value interface{}) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.getCount++

	// Case 1: already in cache (T1 ∪ T2)
	if e, ok := a.items[key]; ok {
		a.hitCount++
		e.value = value
		if !e.inT2 {
			a.t1.Remove(e.el)
			e.inT2 = true
		} else {
			a.t2.Remove(e.el)
		}
		e.el = a.t2.PushFront(key)
		return
	}

	// Case 2: in B1 (ghost hit — recently evicted from T1)
	if a.removeGhost(a.b1, key) {
		b1Len := a.b1.Len()
		b2Len := a.b2.Len()
		delta := 1
		if b1Len > 0 {
			delta = max(1, b2Len/b1Len)
		}
		a.p = min(a.p+delta, a.c)
		a.replace()
		el := a.t2.PushFront(key)
		a.items[key] = &arcEntry{key: key, value: value, el: el, inT2: true}
		return
	}

	// Case 3: in B2 (ghost hit — recently evicted from T2)
	if a.removeGhost(a.b2, key) {
		b1Len := a.b1.Len()
		b2Len := a.b2.Len()
		delta := 1
		if b2Len > 0 {
			delta = max(1, b1Len/b2Len)
		}
		a.p = max(a.p-delta, 0)
		a.replace()
		el := a.t2.PushFront(key)
		a.items[key] = &arcEntry{key: key, value: value, el: el, inT2: true}
		return
	}

	// Case 4: complete miss
	l1Len := a.t1.Len() + a.b1.Len()
	l2Len := a.t2.Len() + a.b2.Len()

	if l1Len == a.c {
		if a.t1.Len() < a.c {
			a.evictB1LRU()
			a.replace()
		} else {
			a.evictT1LRU()
		}
	} else if l1Len+l2Len >= a.c {
		if l1Len+l2Len >= 2*a.c {
			a.evictB2LRU()
		}
		a.replace()
	}

	el := a.t1.PushFront(key)
	a.items[key] = &arcEntry{key: key, value: value, el: el, inT2: false}
}

func (a *ARC) replace() {
	if a.t1.Len() > 0 && (a.t1.Len() > a.p || (a.t1.Len() == a.p && a.b2.Len() > 0)) {
		a.evictT1ToB1()
	} else {
		a.evictT2ToB2()
	}
}

func (a *ARC) evictT1ToB1() {
	el := a.t1.Back()
	if el == nil {
		return
	}
	key := el.Value.(string)
	delete(a.items, key)
	a.t1.Remove(el)
	a.b1.PushFront(key)
}

func (a *ARC) evictT2ToB2() {
	el := a.t2.Back()
	if el == nil {
		return
	}
	key := el.Value.(string)
	delete(a.items, key)
	a.t2.Remove(el)
	a.b2.PushFront(key)
}

func (a *ARC) evictT1LRU() {
	el := a.t1.Back()
	if el == nil {
		return
	}
	key := el.Value.(string)
	delete(a.items, key)
	a.t1.Remove(el)
}

func (a *ARC) evictB1LRU() {
	el := a.b1.Back()
	if el != nil {
		a.b1.Remove(el)
	}
}

func (a *ARC) evictB2LRU() {
	el := a.b2.Back()
	if el != nil {
		a.b2.Remove(el)
	}
}

func (a *ARC) removeGhost(lst *list.List, key string) bool {
	for el := lst.Front(); el != nil; el = el.Next() {
		if el.Value.(string) == key {
			lst.Remove(el)
			return true
		}
	}
	return false
}

func (a *ARC) HitRate() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.getCount == 0 {
		return 1.0
	}
	return float64(a.hitCount) / float64(a.getCount)
}
