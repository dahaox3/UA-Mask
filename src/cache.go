package main

import (
	"strings"
	"sync"

	"github.com/hashicorp/golang-lru/v2/simplelru"
)

const (
	defaultCacheBytes = 512 * 1024
	maxCachedUABytes  = 2048
	cacheEntryCharge  = 128 // conservative allowance for entry/map bookkeeping
)

// UACache bounds both entry count and charged bytes. It owns its strings so a
// short substring cannot retain an entire HTTP header allocation. A nil cache
// is a supported disabled cache, including when cache-size=0.
type UACache struct {
	mu           sync.Mutex
	lru          *simplelru.LRU[string, string]
	bytes, limit int
}

func cacheCharge(key, value string) int { return len(key) + len(value) + cacheEntryCharge }

func NewUACache(entries, byteLimit int) *UACache {
	if entries <= 0 || byteLimit <= 0 {
		return nil
	}
	c := &UACache{limit: byteLimit}
	c.lru, _ = simplelru.NewLRU[string, string](entries, func(key, value string) {
		c.bytes -= cacheCharge(key, value)
	})
	return c
}

func (c *UACache) Get(key string) (string, bool) {
	if c == nil || len(key) > maxCachedUABytes {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Get(key)
}

func (c *UACache) Add(key, value string) {
	if c == nil || len(key) > maxCachedUABytes || len(value) > maxCachedUABytes {
		return
	}
	charge := cacheCharge(key, value)
	if charge > c.limit {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lru.Remove(key)
	for c.bytes+charge > c.limit {
		c.lru.RemoveOldest()
	}
	ownedKey := strings.Clone(key)
	ownedValue := ownedKey
	if key != value {
		ownedValue = strings.Clone(value)
	}
	c.bytes += charge
	c.lru.Add(ownedKey, ownedValue)
}

func (c *UACache) Snapshot() (entries, chargedBytes int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len(), c.bytes
}
