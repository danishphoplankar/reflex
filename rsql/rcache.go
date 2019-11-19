package rsql

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/luno/jettison/errors"
	"github.com/luno/reflex"
)

const defaultRCacheLimit = 10000

// rcache provides a read-through cache for the head of an events table.
// Note that only monotonic incremental int64 event ids are supported.
type rcache struct {
	cache []*reflex.Event
	mu    sync.RWMutex

	name   string
	loader Loader
	limit  int
}

// newRCache returns a new read-through cache.
func newRCache(loader Loader, name string) *rcache {
	return &rcache{
		name:   name,
		loader: loader,
		limit:  defaultRCacheLimit,
	}
}

func (c *rcache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lenUnsafe()
}

func (c *rcache) lenUnsafe() int {
	return len(c.cache)
}

func (c *rcache) emptyUnsafe() bool {
	return c.lenUnsafe() == 0
}

func (c *rcache) headUnsafe() int64 {
	if c.emptyUnsafe() {
		return 0
	}
	return c.cache[0].IDInt()
}

func (c *rcache) tailUnsafe() int64 {
	if c.emptyUnsafe() {
		return 0
	}
	return c.cache[len(c.cache)-1].IDInt()
}

func (c *rcache) Load(ctx context.Context, dbc *sql.DB,
	after int64, lag time.Duration) ([]*reflex.Event, int64, error) {

	if res, ok := c.maybeHit(after+1, lag); ok {
		rcacheHitsCounter.WithLabelValues(c.name).Inc()
		return res, getLastID(res), nil
	}

	rcacheMissCounter.WithLabelValues(c.name).Inc()
	return c.readThrough(ctx, dbc, after, lag)
}

func (c *rcache) maybeHit(from int64, lag time.Duration) ([]*reflex.Event, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.maybeHitUnsafe(from, lag)
}

// maybeHitUnsafe returns a list of events from id (inclusive).
// Note it is unsafe, locks are managed outside.
func (c *rcache) maybeHitUnsafe(from int64, lag time.Duration) ([]*reflex.Event, bool) {
	if from < c.headUnsafe() || from > c.tailUnsafe() {
		return nil, false
	}

	offset := int(from - c.headUnsafe())

	if lag == 0 {
		return c.cache[offset:], true
	}

	cutOff := time.Now().Add(-lag)

	var res []*reflex.Event
	for i := offset; i < c.lenUnsafe(); i++ {
		if c.cache[i].Timestamp.After(cutOff) {
			// Events too new
			break
		}
		res = append(res, c.cache[i])
	}

	return res, true
}

// readThrough returns the next events from the DB as well as updating the cache.
func (c *rcache) readThrough(ctx context.Context, dbc *sql.DB,
	after int64, lag time.Duration) ([]*reflex.Event, int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Recheck cache after waiting for lock
	if res, ok := c.maybeHitUnsafe(after+1, lag); ok {
		return res, getLastID(res), nil
	}

	res, next, err := c.loader(ctx, dbc, after, lag)
	if err != nil {
		return nil, 0, err
	}
	if len(res) == 0 {
		return nil, after, nil
	}

	// Sanity check: Validate consecutive event ids and next cursor.
	for i := 1; i < len(res); i++ {
		if res[i].IDInt() != res[i-1].IDInt()+1 {
			return nil, 0, ErrConsecEvent
		}
	}
	if next != res[len(res)-1].IDInt() {
		return nil, 0, errors.Wrap(ErrNextCursorMismatch, "")
	}

	c.maybeUpdateUnsafe(res)
	c.maybeTrimUnsafe()

	return res, next, nil
}

func (c *rcache) maybeUpdateUnsafe(el []*reflex.Event) {
	if len(el) == 0 {
		return
	}

	next := el[0].IDInt()

	// If empty, init
	if c.emptyUnsafe() {
		c.cache = el
		return
	}

	// If gap, re-init
	if c.tailUnsafe()+1 < next {
		c.cache = el
		return
	}

	// If consecutive, append
	if c.tailUnsafe()+1 == next {
		c.cache = append(c.cache, el...)
		return
	}

	// Else ignore
}

func (c *rcache) maybeTrimUnsafe() {
	if c.lenUnsafe() > c.limit {
		offset := c.lenUnsafe() - c.limit
		c.cache = c.cache[offset:]
	}
}

func getLastID(el []*reflex.Event) int64 {
	if len(el) == 0 {
		return 0
	}
	return el[len(el)-1].IDInt()
}
