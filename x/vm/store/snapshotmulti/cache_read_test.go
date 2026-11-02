package snapshotmulti_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/cosmos/evm/x/vm/store/snapshotmulti"

	storetypes "cosmossdk.io/store/types"
)

func TestCacheBranchReadsParentAfterRead(t *testing.T) {
	for _, snapshot := range []bool{false, true} {
		for _, tc := range []struct {
			name   string
			before []byte
			after  []byte
		}{
			{name: "overwrite", before: []byte("40"), after: []byte("20")},
			{name: "insert", after: []byte("20")},
			{name: "delete", before: []byte("40")},
		} {
			t.Run(fmt.Sprintf("%s/snapshot=%t", tc.name, snapshot), func(t *testing.T) {
				// Given a branch which has already read its parent's value.
				store, key := setupStore()
				k := []byte("balance")
				if tc.before != nil {
					store.GetKVStore(key).Set(k, tc.before)
				}
				child := store.CacheMultiStore()
				require.Equal(t, tc.before, child.GetKVStore(key).Get(k))
				require.Equal(t, tc.before != nil, child.GetKVStore(key).Has(k))
				if snapshot {
					child.(*snapshotmulti.Store).Snapshot()
				}
				kv := child.GetKVStore(key)

				// When the immediate parent changes after that read.
				if tc.after == nil {
					store.GetKVStore(key).Delete(k)
				} else {
					store.GetKVStore(key).Set(k, tc.after)
				}

				// Then fresh iterators and point reads agree on the current value.
				for _, reverse := range []bool{false, true} {
					var iter storetypes.Iterator
					if reverse {
						iter = kv.ReverseIterator(nil, nil)
					} else {
						iter = kv.Iterator(nil, nil)
					}
					var value []byte
					for ; iter.Valid(); iter.Next() {
						require.Equal(t, k, iter.Key())
						value = iter.Value()
					}
					require.NoError(t, iter.Close())
					require.Equal(t, tc.after, value)
				}
				require.Equal(t, tc.after, kv.Get(k))
				require.Equal(t, tc.after != nil, kv.Has(k))
			})
		}
	}
}
