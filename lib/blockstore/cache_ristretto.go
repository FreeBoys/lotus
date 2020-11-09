package blockstore

import (
	"context"
	"io"
	"time"

	"github.com/dgraph-io/ristretto"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"golang.org/x/xerrors"
)

type RistrettoCachingBlockstore struct {
	blockCache  *ristretto.Cache
	existsCache *ristretto.Cache

	inner blockstore.Blockstore
}

var _ blockstore.Blockstore = (*RistrettoCachingBlockstore)(nil)

func WrapRistrettoCache(ctx context.Context, inner blockstore.Blockstore) (*RistrettoCachingBlockstore, error) {
	blockCache, err := ristretto.NewCache(&ristretto.Config{
		NumCounters: 10_000_000, // assumes we're going to be storing 1MM objects (docs say to x10 that)
		MaxCost:     1 << 27,    // 256MiB.
		BufferItems: 64,
		Metrics:     true,
	})
	if err != nil {
		return nil, xerrors.Errorf("failed to create ristretto block cache: %w", err)
	}

	existsCache, err := ristretto.NewCache(&ristretto.Config{
		NumCounters: 10_000_000, // assumes we're going to be storing 1MM objects (docs say to x10 that)
		MaxCost:     1 << 20,    // 1MiB.
		BufferItems: 64,
		Metrics:     true,
	})
	if err != nil {
		return nil, xerrors.Errorf("failed to create ristretto has cache: %w", err)
	}

	c := &RistrettoCachingBlockstore{
		blockCache:  blockCache,
		existsCache: existsCache,
		inner:       inner,
	}

	go func() {
		blockCacheTag, err := tag.New(ctx, tag.Insert(CacheName, "block_cache"))
		if err != nil {
			log.Warnf("blockstore metrics: failed to instantiate block cache tag: %s", err)
			return
		}
		existsCacheTag, err := tag.New(ctx, tag.Insert(CacheName, "exists_cache"))
		if err != nil {
			log.Warnf("blockstore metrics: failed to instantiate exists cache tag: %s", err)
			return
		}
		recordMetrics := func(ctx context.Context, m *ristretto.Metrics) {
			hits, misses := int64(m.Hits()), int64(m.Misses()) // to get a consistent view for the ratio.
			stats.Record(ctx,
				CacheMeasures.HitRatio.M(float64(hits)/(float64(hits)+float64(misses))),
				CacheMeasures.Hits.M(hits),
				CacheMeasures.Misses.M(misses),
				CacheMeasures.Adds.M(int64(m.KeysAdded())),
				CacheMeasures.Updates.M(int64(m.KeysUpdated())),
				CacheMeasures.Evictions.M(int64(m.KeysEvicted())),
				CacheMeasures.CostAdded.M(int64(m.CostAdded())),
				CacheMeasures.CostEvicted.M(int64(m.CostEvicted())),
				CacheMeasures.SetsDropped.M(int64(m.SetsDropped())),
				CacheMeasures.SetsRejected.M(int64(m.SetsRejected())),
				CacheMeasures.QueriesDropped.M(int64(m.GetsDropped())),
				CacheMeasures.QueriesServed.M(int64(m.GetsKept())),
			)
		}

		for {
			select {
			case <-time.After(CacheMetricsEmitInterval):
				recordMetrics(blockCacheTag, blockCache.Metrics)
				recordMetrics(existsCacheTag, existsCache.Metrics)
			case <-ctx.Done():
				return // yield
			}
		}
	}()

	return c, nil
}

// Close clears and closes all caches. It also closes the underlying blockstore,
// if it implements io.Closer.
func (c *RistrettoCachingBlockstore) Close() error {
	for _, cache := range []*ristretto.Cache{
		c.blockCache,
		c.existsCache,
	} {
		cache.Clear()
		cache.Close()
	}
	if closer, ok := c.inner.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (c *RistrettoCachingBlockstore) Get(cid cid.Cid) (blocks.Block, error) {
	k := []byte(cid.Hash())
	// check the has cache.
	if has, ok := c.existsCache.Get(k); ok && !has.(bool) {
		// we know we don't have the item; short-circuit.
		return nil, ErrNotFound
	}
	// check the block cache.
	if obj, ok := c.blockCache.Get(k); ok {
		return obj.(blocks.Block), nil
	}
	// fall back to the inner store.
	res, err := c.inner.Get(cid)
	if err != nil {
		if err == ErrNotFound {
			// inform the has cache that the item does not exist.
			_ = c.existsCache.Set(k, false, 1)
		}
		return res, err
	}
	l := len(res.RawData())
	c.existsCache.Del(k)              // evict the item immediately in case it was added concurrently.
	_ = c.existsCache.Set(k, true, 1) // set is asynchronous.
	_ = c.blockCache.Set(k, res, int64(l))
	return res, err
}

func (c *RistrettoCachingBlockstore) GetSize(cid cid.Cid) (int, error) {
	k := []byte(cid.Hash())
	// check the has cache.
	if has, ok := c.existsCache.Get(k); ok && !has.(bool) {
		// we know we don't have the item; short-circuit.
		return -1, ErrNotFound
	}
	res, err := c.inner.GetSize(cid)
	if err != nil {
		if err == ErrNotFound {
			// inform the exists cache that the item does not exist.
			_ = c.existsCache.Set(k, false, 1)
		}
		return res, err
	}
	_ = c.existsCache.Set(k, true, 1)
	return res, err
}

func (c *RistrettoCachingBlockstore) Has(cid cid.Cid) (bool, error) {
	k := []byte(cid.Hash())
	if has, ok := c.existsCache.Get(k); ok {
		return has.(bool), nil
	}
	has, err := c.inner.Has(cid)
	if err != nil {
		return has, err
	}
	_ = c.existsCache.Set(k, has, 1)
	return has, err
}

func (c *RistrettoCachingBlockstore) Put(block blocks.Block) error {
	k := []byte(block.Cid().Hash())
	if exists := c.probabilisticExists(k); exists {
		return nil
	}
	err := c.inner.Put(block)
	if err != nil {
		return err
	}
	l := len(block.RawData())
	_ = c.blockCache.Set(k, block, int64(l))
	c.existsCache.Del(k)              // evict the item immediately in case it exists.
	_ = c.existsCache.Set(k, true, 1) // set is asynchronous.
	return err
}

func (c *RistrettoCachingBlockstore) PutMany(blks []blocks.Block) error {
	miss := make([]blocks.Block, 0, len(blks))
	for _, b := range blks {
		k := []byte(b.Cid().Hash())
		if c.probabilisticExists(k) {
			continue
		}
		miss = append(miss, b)
	}
	if len(miss) == 0 {
		// nothing to add.
		return nil
	}

	err := c.inner.PutMany(miss)
	if err != nil {
		return err
	}
	for _, b := range miss {
		k := []byte(b.Cid().Hash())
		l := len(b.RawData())
		_ = c.blockCache.Set(k, b, int64(l))
		c.existsCache.Del(k)              // evict the item immediately in case it exists.
		_ = c.existsCache.Set(k, true, 1) // set is asynchronous.
	}
	return err
}

func (c *RistrettoCachingBlockstore) AllKeysChan(ctx context.Context) (<-chan cid.Cid, error) {
	return c.inner.AllKeysChan(ctx)
}

func (c *RistrettoCachingBlockstore) DeleteBlock(cid cid.Cid) error {
	k := []byte(cid.Hash())
	err := c.inner.DeleteBlock(cid)
	if err != nil {
		return err
	}
	c.blockCache.Del(k)
	c.existsCache.Del(k)           // evict the item immediately in case it exists.
	c.existsCache.Set(k, false, 1) // set is asynchronous.
	return err
}

func (c *RistrettoCachingBlockstore) probabilisticExists(k []byte) bool {
	if has, ok := c.existsCache.Get(k); ok {
		return has.(bool)
	}
	// may have paged out of the exists cache, but still present in the block cache.
	if _, ok := c.blockCache.Get(k); ok {
		c.existsCache.Del(k)              // play it safe, just in case the value was added interim.
		_ = c.existsCache.Set(k, true, 1) // update the exists cache.
		return true
	}
	return false
}

func (c *RistrettoCachingBlockstore) HashOnRead(enabled bool) {
	c.inner.HashOnRead(enabled)
}
