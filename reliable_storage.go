package utahfs

import (
	"bytes"
	"context"
	"log"
	"sync"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// ReliableStorage is an extension of the ObjectStorage interface that provides
// distributed locking (if necessary) and atomic transactions.
type ReliableStorage interface {
	// Start begins a new transaction. The methods below will not work until
	// this is called, and will stop working again after Commit is called.
	//
	// Transactions are isolated and atomic.
	Start(ctx context.Context) error

	Get(ctx context.Context, key string) (data []byte, err error)

	// Commit persists the changes in `writes` to the backend, atomically. If
	// the value of a key is nil, then that key is deleted.
	Commit(ctx context.Context, writes map[string][]byte) error
}

type simpleStorage struct {
	store ObjectStorage
}

// NewSimpleStorage returns a ReliableStorage implementation, intended for
// testing. It simply panics if the atomicity of a transaction is broken.
func NewSimpleStorage(store ObjectStorage) ReliableStorage {
	return &simpleStorage{store}
}

func (ss *simpleStorage) Start(ctx context.Context) error { return nil }

func (ss *simpleStorage) Get(ctx context.Context, key string) ([]byte, error) {
	return ss.store.Get(ctx, key)
}

func (ss *simpleStorage) Commit(ctx context.Context, writes map[string][]byte) error {
	for key, val := range writes {
		if err := ss.store.Set(ctx, key, val); err != nil {
			panic(err)
		}
	}
	return nil
}

type reliableStub struct {
	base ReliableStorage
	data map[string][]byte
}

var _ ObjectStorage = reliableStub{}

func (rs reliableStub) Get(ctx context.Context, key string) ([]byte, error) {
	raw, ok := rs.data[key]
	if !ok {
		return rs.base.Get(ctx, key)
	} else if raw == nil {
		return nil, ErrObjectNotFound
	}
	return dup(raw), nil
}

func (rs reliableStub) Set(ctx context.Context, key string, data []byte) error {
	rs.data[key] = dup(data)
	return nil
}

func (rs reliableStub) Delete(ctx context.Context, key string) error {
	rs.data[key] = nil
	return nil
}

// ReliableObjectStorage provides a way for an ObjectStorage implementation to
// be composed in front of a ReliableStorage implementation.
//
// Take the Store field of the exposed struct, set it as the base storage of any
// other ObjectStorage implementations, and then replace it with the result. For
// example:
//   ros := utahfs.NewReliableObjectStorage(base)
//   ros.Store, _ = storage.NewCache(ros.Store, 4096)
// and now any reads will possibly be answered from cached before hitting
// `base`, and any redundant writes will be omitted.
type ReliableObjectStorage struct {
	Store ObjectStorage
	stub  reliableStub
}

var _ ReliableStorage = &ReliableObjectStorage{}

func NewReliableObjectStorage(base ReliableStorage) *ReliableObjectStorage {
	stub := reliableStub{
		base: base,
		data: make(map[string][]byte),
	}
	return &ReliableObjectStorage{
		Store: stub,
		stub:  stub,
	}
}

func (ros *ReliableObjectStorage) Start(ctx context.Context) error {
	return ros.stub.base.Start(ctx)
}

func (ros *ReliableObjectStorage) Get(ctx context.Context, key string) ([]byte, error) {
	return ros.Store.Get(ctx, key)
}

func (ros *ReliableObjectStorage) Commit(ctx context.Context, writes map[string][]byte) error {
	for key, val := range writes {
		if val == nil {
			if err := ros.Store.Delete(ctx, key); err != nil {
				return err
			}
		} else {
			if err := ros.Store.Set(ctx, key, val); err != nil {
				return err
			}
		}
	}

	finalWrites := make(map[string][]byte)
	for key, val := range ros.stub.data {
		finalWrites[key] = val
		delete(ros.stub.data, key)
	}

	return ros.stub.base.Commit(ctx, finalWrites)
}

type localWAL struct {
	mu sync.Mutex

	remote ObjectStorage
	local  *leveldb.DB

	maxSize   int
	currSize  int
	lastCount time.Time
	wake      chan struct{}
}

// NewLocalWAL returns a ReliableStorage implementation that achieves reliable
// writes over a remote object storage provider by buffering writes in a
// Write-Ahead Log (WAL) stored at `path`.
//
// The WAL may have at least `maxSize` buffered entries before new writes start
// blocking on old writes being flushed.
func NewLocalWAL(remote ObjectStorage, path string, maxSize int) (ReliableStorage, error) {
	local, err := leveldb.OpenFile(path, nil)
	if err != nil {
		return nil, err
	}
	wal := &localWAL{
		remote: remote,
		local:  local,

		maxSize:   maxSize,
		currSize:  0,
		lastCount: time.Time{},
		wake:      make(chan struct{}),
	}
	go wal.drain()

	return wal, nil
}

func (lw *localWAL) drain() {
	tick := time.Tick(5 * time.Second)

	for {
		select {
		case <-tick:
		case <-lw.wake:
		}

		if err := lw.drainOnce(); err != nil {
			log.Println(err)
		}
	}
}

func (lw *localWAL) drainOnce() error {
	iter := lw.local.NewIterator(&util.Range{nil, nil}, nil)
	for i := 0; iter.Next(); i++ {
		key, val := iter.Key(), iter.Value()

		if val != nil {
			if err := lw.remote.Set(context.Background(), string(key), val); err != nil {
				return err
			}
		} else {
			if err := lw.remote.Delete(context.Background(), string(key)); err != nil {
				return err
			}
		}
		if err := lw.drop(key, val); err != nil {
			return err
		}
	}
	iter.Release()
	if err := iter.Error(); err != nil {
		return err
	}

	return nil
}

func (lw *localWAL) drop(key, val []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()

	cand, err := lw.local.Get(key, nil)
	if err == leveldb.ErrNotFound {
		return nil
	} else if err != nil {
		return err
	} else if !bytes.Equal(val, cand) {
		return nil
	}
	return lw.local.Delete(key, nil)
}

func (lw *localWAL) count() (int, error) {
	lw.mu.Lock()
	if time.Since(lw.lastCount) < 10*time.Second {
		curr := lw.currSize
		lw.mu.Unlock()
		return curr, nil
	}
	lw.mu.Unlock()

	count := 0
	iter := lw.local.NewIterator(&util.Range{nil, nil}, nil)
	for iter.Next() {
		count++
	}
	iter.Release()
	if err := iter.Error(); err != nil {
		return 0, err
	}

	lw.mu.Lock()
	lw.lastCount = time.Now()
	lw.currSize = count
	lw.mu.Unlock()

	return count, nil
}

func (lw *localWAL) Start(ctx context.Context) error {
	// Block until the database has drained enough to accept new writes.
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		count, err := lw.count()
		if err != nil {
			return err
		}

		if count > lw.maxSize {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case lw.wake <- struct{}{}:
			case <-ticker.C:
			}
			continue
		}
		return nil
	}
}

func (lw *localWAL) Get(ctx context.Context, key string) ([]byte, error) {
	data, err := lw.local.Get([]byte(key), nil)
	if err == leveldb.ErrNotFound {
		return lw.remote.Get(ctx, key)
	} else if data == nil {
		return nil, ErrObjectNotFound
	} else if err != nil {
		return nil, err
	}
	return data, nil
}

func (lw *localWAL) Commit(ctx context.Context, writes map[string][]byte) error {
	if len(writes) == 0 {
		return nil
	}

	batch := new(leveldb.Batch)
	for key, val := range writes {
		batch.Put([]byte(key), dup(val))
	}
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if err := lw.local.Write(batch, &opt.WriteOptions{Sync: true}); err != nil {
		return err
	}
	return nil
}
