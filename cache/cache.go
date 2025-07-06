package cache

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type CacheEntry struct {
	Key       string
	Value     interface{}
	Timestamp time.Time
	TTL       time.Duration
	UseCount  int
}

type Cache struct {
	items      map[string]*list.Element
	evictList  *list.List
	mutex      sync.RWMutex
	capacity   int
	metrics    *CacheMetrics
	ctx        context.Context
	cancel     context.CancelFunc
	cleanupTTL time.Duration
}

type CacheMetrics struct {
	hits   prometheus.Counter
	misses prometheus.Counter
	size   prometheus.Gauge
}

var (
	hits = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cache_hits_total",
		Help: "Total number of cache hits",
	})
	misses = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cache_misses_total",
		Help: "Total number of cache misses",
	})
	size = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cache_size",
		Help: "Current size of the cache",
	})
)

func NewCache(capacity int) *Cache {
	ctx, cancel := context.WithCancel(context.Background())
	metrics := &CacheMetrics{
		hits:   hits,
		misses: misses,
		size:   size,
	}

	c := &Cache{
		items:     make(map[string]*list.Element),
		evictList: list.New(),
		capacity:  capacity,
		metrics:   metrics,
		ctx:       ctx,
		cancel:    cancel,
		cleanupTTL: time.Minute * 5,
	}
	go c.startCleanup()
	return c
}

func (c *Cache) Get(key string) (interface{}, bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if element, exists := c.items[key]; exists {
		entry := element.Value.(*CacheEntry)
		if entry.TTL > 0 && time.Since(entry.Timestamp) > entry.TTL {
			c.evictElement(element)
			c.metrics.misses.Inc()
			return nil, false
		}
		c.evictList.MoveToFront(element)
		entry.UseCount++
		c.metrics.hits.Inc()
		return entry.Value, true
	}

	c.metrics.misses.Inc()
	return nil, false
}

func (c *Cache) Set(key string, value interface{}, ttl time.Duration) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if element, exists := c.items[key]; exists {
		c.evictList.MoveToFront(element)
		entry := element.Value.(*CacheEntry)
		entry.Value = value
		entry.Timestamp = time.Now()
		entry.TTL = ttl
		return
	}

	entry := &CacheEntry{
		Key:       key,
		Value:     value,
		Timestamp: time.Now(),
		TTL:       ttl,
		UseCount:  0,
	}

	element := c.evictList.PushFront(entry)
	c.items[key] = element
	c.metrics.size.Inc()

	if c.evictList.Len() > c.capacity {
		c.evictLRU()
	}
}

func (c *Cache) evictLRU() {
	element := c.evictList.Back()
	if element != nil {
		c.evictElement(element)
	}
}

func (c *Cache) evictElement(element *list.Element) {
	c.evictList.Remove(element)
	entry := element.Value.(*CacheEntry)
	delete(c.items, entry.Key)
	c.metrics.size.Dec()
}

func (c *Cache) Clear() {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.items = make(map[string]*list.Element)
	c.evictList.Init()
	c.metrics.size.Set(0)
}

func (c *Cache) Stop() {
	c.cancel()
}

func (c *Cache) startCleanup() {
	ticker := time.NewTicker(c.cleanupTTL)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.cleanupExpired()
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Cache) cleanupExpired() {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	now := time.Now()
	for key, element := range c.items {
		entry := element.Value.(*CacheEntry)
		if entry.TTL > 0 && now.Sub(entry.Timestamp) > entry.TTL {
			c.evictElement(element)
			c.metrics.misses.Inc()
			delete(c.items, key)
		}
	}
}

func (c *Cache) Size() int {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return c.evictList.Len()
}