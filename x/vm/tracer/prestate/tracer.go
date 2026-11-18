package prestate

import (
	"bytes"
	"encoding/json"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/eth/tracers/native"

	vmtracer "github.com/cosmos/evm/x/vm/tracer"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

// Config supplies the pre-execution state and mounted EVM store. Before must
// remain usable while the adapter records original values.
type Config struct {
	Tracer native.PrestateTracerConfig
	Before tracing.StateDB
	Store  string
}

type tracePrestateAccount struct {
	Balance  *hexutil.Big                `json:"balance,omitempty"`
	Code     hexutil.Bytes               `json:"code,omitempty"`
	CodeHash *common.Hash                `json:"codeHash,omitempty"`
	Nonce    uint64                      `json:"nonce,omitempty"`
	Storage  map[common.Hash]common.Hash `json:"storage,omitempty"`
	empty    bool
	exists   bool
}

type prestateTrace struct {
	config    native.PrestateTracerConfig
	before    tracing.StateDB
	after     tracing.StateDB
	accounts  map[common.Address]*tracePrestateAccount
	evmStore  string
	recording bool
	err       error
	stopped   atomic.Pointer[traceStop]
}

type traceStop struct {
	err error
}

// Prepare adapts one fresh native prestate tracer configured for diff mode.
// Call it before constructing the execution's Manager and StateDB. Native start,
// opcode and stop callbacks are reused; the adapter owns completion and results.
//
// After execution and post-processing have published or rolled back their state,
// call the returned finish function with that final state before GetResult.
// Finishing ends observation; it does not publish or roll back any writes.
func Prepare(ctx sdk.Context, config Config, tracer *tracers.Tracer) (sdk.Context, func(tracing.StateDB)) {
	state := &prestateTrace{
		config: config.Tracer, before: config.Before,
		accounts: make(map[common.Address]*tracePrestateAccount), evmStore: config.Store, recording: true,
	}
	onStart, stop := tracer.OnTxStart, tracer.Stop
	tracer.OnTxStart = func(env *tracing.VMContext, tx *ethtypes.Transaction, from common.Address) {
		copyEnv := *env
		copyEnv.StateDB = &prestateReadState{StateDB: env.StateDB, trace: state}
		onStart(&copyEnv, tx, from)
	}
	tracer.OnTxEnd = nil
	tracer.GetResult = func() (json.RawMessage, error) {
		if err := state.traceError(); err != nil {
			return nil, err
		}
		raw, err := state.result()
		if stopped := state.traceError(); stopped != nil {
			return nil, stopped
		}
		return raw, err
	}
	tracer.Stop = func(err error) {
		if state.stopped.CompareAndSwap(nil, &traceStop{err: err}) {
			stop(err)
		}
	}
	return ctx.WithMultiStore(vmtracer.ObserveStoreWrites(ctx.MultiStore(), state.observeStore)), func(final tracing.StateDB) {
		state.recording = false
		state.after = final
	}
}

func (s *prestateTrace) traceError() error {
	if stopped := s.stopped.Load(); stopped != nil && stopped.err != nil {
		return stopped.err
	}
	return s.err
}

func (s *prestateTrace) snapshot(db tracing.StateDB, addr common.Address) *tracePrestateAccount {
	balance, code := db.GetBalance(addr).ToBig(), common.CopyBytes(db.GetCode(addr))
	account := &tracePrestateAccount{
		Balance: (*hexutil.Big)(balance), Nonce: db.GetNonce(addr), Code: code,
		Storage: make(map[common.Hash]common.Hash), exists: db.Exist(addr),
	}
	hash := db.GetCodeHash(addr)
	if hash != (common.Hash{}) && hash != ethtypes.EmptyCodeHash {
		account.CodeHash = &hash
	}
	account.empty = balance.Sign() == 0 && account.Nonce == 0 && len(code) == 0
	if s.config.DisableCode {
		account.Code = nil
	}
	return account
}

func (s *prestateTrace) observe(addr common.Address, slot *common.Hash) {
	if !s.recording || s.stopped.Load() != nil {
		return
	}
	account := s.accounts[addr]
	if account == nil {
		account = s.snapshot(s.before, addr)
		s.accounts[addr] = account
	}
	if slot != nil && !s.config.DisableStorage {
		if _, ok := account.Storage[*slot]; !ok {
			account.Storage[*slot] = s.before.GetState(addr, *slot)
		}
	}
}

func (s *prestateTrace) result() (json.RawMessage, error) {
	pre, post := make(map[common.Address]*tracePrestateAccount), make(map[common.Address]*tracePrestateAccount)
	for addr, original := range s.accounts {
		if err := s.traceError(); err != nil {
			return nil, err
		}
		before := *original
		before.Storage = make(map[common.Hash]common.Hash)
		after := s.snapshot(s.after, addr)
		if !after.exists {
			if !before.empty {
				for key, value := range original.Storage {
					if value != (common.Hash{}) {
						before.Storage[key] = value
					}
				}
				pre[addr] = &before
			}
			continue
		}
		delta := &tracePrestateAccount{Storage: make(map[common.Hash]common.Hash)}
		modified := false
		if before.Balance.ToInt().Cmp(after.Balance.ToInt()) != 0 {
			delta.Balance, modified = after.Balance, true
		}
		if before.Nonce != after.Nonce {
			delta.Nonce, modified = after.Nonce, true
		}
		var beforeHash, afterHash common.Hash
		if before.CodeHash != nil {
			beforeHash = *before.CodeHash
		}
		if after.CodeHash != nil {
			afterHash = *after.CodeHash
		}
		if beforeHash != afterHash {
			delta.CodeHash, modified = &afterHash, true
		}
		if !bytes.Equal(before.Code, after.Code) {
			delta.Code, modified = after.Code, true
		}
		for key, value := range original.Storage {
			if err := s.traceError(); err != nil {
				return nil, err
			}
			next := s.after.GetState(addr, key)
			if value == next {
				continue
			}
			modified = true
			if value != (common.Hash{}) {
				before.Storage[key] = value
			}
			if next != (common.Hash{}) {
				delta.Storage[key] = next
			}
		}
		if modified {
			if !before.empty {
				pre[addr] = &before
			}
			post[addr] = delta
		}
	}
	return json.Marshal(struct {
		Pre  map[common.Address]*tracePrestateAccount `json:"pre"`
		Post map[common.Address]*tracePrestateAccount `json:"post"`
	}{pre, post})
}
