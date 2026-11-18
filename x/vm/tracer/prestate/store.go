package prestate

import (
	"bytes"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/holiman/uint256"

	"github.com/cosmos/evm/x/vm/types"

	"cosmossdk.io/collections"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
)

func (s *prestateTrace) observeStore(store storetypes.StoreKey, key []byte) {
	if !s.recording || s.err != nil || s.stopped.Load() != nil {
		return
	}
	switch store.Name() {
	case s.evmStore:
		if bytes.HasPrefix(key, types.KeyPrefixStorage) && len(key) == len(types.KeyPrefixStorage)+common.AddressLength+common.HashLength {
			offset := len(types.KeyPrefixStorage)
			addr, slot := common.BytesToAddress(key[offset:offset+common.AddressLength]), common.BytesToHash(key[offset+common.AddressLength:])
			s.observe(addr, &slot)
		} else if bytes.HasPrefix(key, types.KeyPrefixCodeHash) && len(key) == len(types.KeyPrefixCodeHash)+common.AddressLength {
			s.observe(common.BytesToAddress(key[len(types.KeyPrefixCodeHash):]), nil)
		}
	case authtypes.StoreKey:
		prefix := authtypes.AddressStoreKeyPrefix.Bytes()
		if bytes.HasPrefix(key, prefix) && len(key) == len(prefix)+common.AddressLength {
			s.observe(common.BytesToAddress(key[len(prefix):]), nil)
		}
	case banktypes.StoreKey:
		prefix := banktypes.BalancesPrefix.Bytes()
		if !bytes.HasPrefix(key, prefix) {
			return
		}
		codec := collections.PairKeyCodec(sdk.AccAddressKey, collections.StringKey)
		_, pair, err := codec.Decode(key[len(prefix):])
		if err != nil {
			s.err = fmt.Errorf("decode traced bank balance key: %w", err)
			return
		}
		if pair.K2() == types.GetEVMCoinDenom() && len(pair.K1()) == common.AddressLength {
			s.observe(common.BytesToAddress(pair.K1()), nil)
		}
	}
}

// prestateReadState includes EVM-observed keys as well as SDK writes, so reverted
// attempts and post-hook changes are compared against the same original state.
type prestateReadState struct {
	tracing.StateDB
	trace *prestateTrace
}

func (s *prestateReadState) GetBalance(addr common.Address) *uint256.Int {
	s.trace.observe(addr, nil)
	return s.StateDB.GetBalance(addr)
}

func (s *prestateReadState) GetNonce(addr common.Address) uint64 {
	s.trace.observe(addr, nil)
	return s.StateDB.GetNonce(addr)
}

func (s *prestateReadState) GetCode(addr common.Address) []byte {
	s.trace.observe(addr, nil)
	return s.StateDB.GetCode(addr)
}

func (s *prestateReadState) GetCodeHash(addr common.Address) common.Hash {
	s.trace.observe(addr, nil)
	return s.StateDB.GetCodeHash(addr)
}

func (s *prestateReadState) GetState(addr common.Address, slot common.Hash) common.Hash {
	s.trace.observe(addr, &slot)
	return s.StateDB.GetState(addr, slot)
}
