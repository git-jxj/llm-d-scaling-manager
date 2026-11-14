package whatifapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The cache drops the least recently used entry: a get counts as a use.
func TestLRURecencyAndEviction(t *testing.T) {
	c := newLRU[int](2)
	c.add("a", 1)
	c.add("b", 2)
	_, _ = c.get("a") // a is now the most recent
	c.add("c", 3)     // so b goes
	_, okA := c.get("a")
	_, okB := c.get("b")
	_, okC := c.get("c")
	assert.True(t, okA, "a was used, and dropped")
	assert.False(t, okB, "b was the least used, and kept")
	assert.True(t, okC)
	assert.Equal(t, 2, c.len())
}

// add keeps an entry's first value; put replaces it.
func TestLRUAddKeepsPutReplaces(t *testing.T) {
	c := newLRU[int](2)
	assert.Equal(t, 1, c.add("a", 1))
	assert.Equal(t, 1, c.add("a", 9), "add replaced an immutable entry")
	c.put("a", 7)
	v, _ := c.get("a")
	assert.Equal(t, 7, v)
	c.put("b", 2)
	c.put("c", 3)
	assert.Equal(t, 2, c.len(), "put grew the cache past its size")
}

// A weighed cache drops the least used entries past its weight, keeps the
// newest whatever it weighs, and counts a replaced entry's new weight.
func TestLRUBoundedInBytes(t *testing.T) {
	c := newWeighedLRU(100, 10, bodyBytes)
	c.add("a", []byte("aaaa"))
	c.add("b", []byte("bbbb"))
	assert.Equal(t, 8, c.bytes())
	c.add("c", []byte("cccc"))
	_, okA := c.get("a")
	assert.False(t, okA, "12 bytes kept in a 10-byte cache")
	assert.Equal(t, 8, c.bytes())
	c.add("big", make([]byte, 20))
	assert.Equal(t, 1, c.len(), "the newest is kept alone, past the bound")
	assert.Equal(t, 20, c.bytes())
	c.put("big", []byte("x"))
	assert.Equal(t, 1, c.bytes(), "put counts the new value")
	assert.Equal(t, defaultResponseBytes, New(Config{}).plans.maxWeight, "the plans are bounded in bytes")
}

// A put that grows an entry evicts others past the bound; a new key adds its
// weight; exactly the bound is kept; both response caches are weighed.
func TestLRUBytesEdges(t *testing.T) {
	c := newWeighedLRU(100, 10, bodyBytes)
	c.add("a", []byte("aaaa"))
	assert.Equal(t, 4, c.bytes())
	c.add("b", []byte("bbbb"))
	assert.Equal(t, 8, c.bytes(), "a new key adds its weight")
	c.put("a", []byte("aaaaaaaa"))
	_, okB := c.get("b")
	assert.False(t, okB, "a grown entry left the cache past its bound")
	assert.Equal(t, 8, c.bytes())
	c.add("c", []byte("cc"))
	assert.Equal(t, 10, c.bytes(), "exactly the bound is kept")
	assert.Equal(t, 2, c.len())
	s := New(Config{})
	for name, l := range map[string]*lru[[]byte]{"plans": s.plans, "sizes": s.sizes} {
		assert.Equal(t, defaultResponseBytes, l.maxWeight, name)
		assert.NotNil(t, l.weigh, "%s: no weight, no bound", name)
	}
}
