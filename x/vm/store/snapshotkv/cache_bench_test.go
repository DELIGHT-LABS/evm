package snapshotkv_test

import (
	"fmt"
	"testing"

	"github.com/cosmos/evm/x/vm/store/snapshotkv"

	"cosmossdk.io/store/cachekv"
	storetypes "cosmossdk.io/store/types"
)

func BenchmarkCacheIterator(b *testing.B) {
	for _, name := range []string{"sdk", "fixed_parent", "live_parent"} {
		b.Run(name, func(b *testing.B) {
			parent := newCacheParent()
			for i := 0; i < 1000; i++ {
				parent.Set([]byte(fmt.Sprintf("%04d", i)), []byte("parent"))
			}
			var cache storetypes.CacheKVStore = cachekv.NewStore(parent)
			switch name {
			case "fixed_parent":
				cache = snapshotkv.NewCache(parent)
			case "live_parent":
				cache = snapshotkv.NewCacheWithParent(func() storetypes.KVStore { return parent })
			}
			for i := 0; i < 100; i++ {
				cache.Set([]byte(fmt.Sprintf("%04d", i)), []byte("private"))
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				iter := cache.Iterator(nil, nil)
				for ; iter.Valid(); iter.Next() {
					_ = iter.Value()
				}
				if err := iter.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
