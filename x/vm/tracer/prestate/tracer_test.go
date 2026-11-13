package prestate_test

import (
	"encoding/json"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/eth/tracers/native"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/evm/x/vm/tracer/prestate"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	sdktestutil "github.com/cosmos/cosmos-sdk/testutil"
)

type balanceReadState struct {
	tracing.StateDB
	onRead func()
}

func (s *balanceReadState) GetBalance(addr common.Address) *uint256.Int {
	if s.onRead != nil {
		s.onRead()
	}
	return s.StateDB.GetBalance(addr)
}

func TestAdapterCancellation(t *testing.T) {
	for _, phase := range []string{"before result", "during result"} {
		t.Run(phase, func(t *testing.T) {
			target := common.HexToAddress("0x1234")
			before, err := state.New(ethtypes.EmptyRootHash, state.NewDatabaseForTesting())
			require.NoError(t, err)
			before.SetBalance(target, uint256.NewInt(3), tracing.BalanceChangeUnspecified)
			live := before.Copy()
			reader := &balanceReadState{StateDB: live}
			config := native.PrestateTracerConfig{DiffMode: true}
			tracer := newNativeTracer(t, config)
			key := storetypes.NewKVStoreKey("evm")
			ctx := sdktestutil.DefaultContext(key, storetypes.NewTransientStoreKey("transient"))
			_, finish := prestate.Prepare(ctx, prestate.Config{Tracer: config, Before: before, Store: key.Name()}, tracer)
			tracer.OnTxStart(&tracing.VMContext{StateDB: reader, BlockNumber: big.NewInt(1)}, ethtypes.NewTx(&ethtypes.LegacyTx{To: &target}), common.Address{1})
			live.SetBalance(target, uint256.NewInt(7), tracing.BalanceChangeUnspecified)
			finish(reader)
			cancelled := errors.New("trace cancelled")
			if phase == "before result" {
				tracer.Stop(cancelled)
			} else {
				reader.onRead = func() {
					reader.onRead = nil
					done := make(chan struct{})
					go func() {
						tracer.Stop(cancelled)
						close(done)
					}()
					<-done
				}
			}

			raw, err := tracer.GetResult()

			require.ErrorIs(t, err, cancelled)
			require.Empty(t, raw)
		})
	}
}

func TestAdapterUsesExplicitFinalState(t *testing.T) {
	target := common.HexToAddress("0x1234")
	before, err := state.New(ethtypes.EmptyRootHash, state.NewDatabaseForTesting())
	require.NoError(t, err)
	before.SetBalance(target, uint256.NewInt(3), tracing.BalanceChangeUnspecified)
	live, final := before.Copy(), before.Copy()
	config := native.PrestateTracerConfig{DiffMode: true}
	tracer := newNativeTracer(t, config)
	key := storetypes.NewKVStoreKey("evm")
	ctx := sdktestutil.DefaultContext(key, storetypes.NewTransientStoreKey("transient"))
	_, finish := prestate.Prepare(ctx, prestate.Config{Tracer: config, Before: before, Store: key.Name()}, tracer)
	tracer.OnTxStart(&tracing.VMContext{StateDB: live, BlockNumber: big.NewInt(1)}, ethtypes.NewTx(&ethtypes.LegacyTx{To: &target}), common.Address{1})
	live.SetBalance(target, uint256.NewInt(7), tracing.BalanceChangeUnspecified)
	final.SetBalance(target, uint256.NewInt(11), tracing.BalanceChangeUnspecified)

	finish(final)
	raw, err := tracer.GetResult()

	require.NoError(t, err)
	var diff struct {
		Pre  map[common.Address]struct{ Balance *hexutil.Big }
		Post map[common.Address]struct{ Balance *hexutil.Big }
	}
	require.NoError(t, json.Unmarshal(raw, &diff))
	require.Contains(t, diff.Pre, target)
	require.Contains(t, diff.Post, target)
	require.Equal(t, uint64(3), diff.Pre[target].Balance.ToInt().Uint64())
	require.Equal(t, uint64(11), diff.Post[target].Balance.ToInt().Uint64())
}

func TestAdapterMatchesNativeAccountDiff(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config native.PrestateTracerConfig
	}{
		{"default", native.PrestateTracerConfig{DiffMode: true}},
		{"without code", native.PrestateTracerConfig{DiffMode: true, DisableCode: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := common.HexToAddress("0x1234")
			before, err := state.New(ethtypes.EmptyRootHash, state.NewDatabaseForTesting())
			require.NoError(t, err)
			before.SetBalance(target, uint256.NewInt(3), tracing.BalanceChangeUnspecified)
			before.SetCode(target, []byte{0x60, 0x00}, tracing.CodeChangeUnspecified)
			live := before.Copy()
			baseline, adapted := newNativeTracer(t, tc.config), newNativeTracer(t, tc.config)
			key := storetypes.NewKVStoreKey("evm")
			ctx := sdktestutil.DefaultContext(key, storetypes.NewTransientStoreKey("transient"))
			_, finish := prestate.Prepare(ctx, prestate.Config{Tracer: tc.config, Before: before, Store: key.Name()}, adapted)
			env := &tracing.VMContext{StateDB: live, BlockNumber: big.NewInt(1)}
			tx := ethtypes.NewTx(&ethtypes.LegacyTx{To: &target})
			baseline.OnTxStart(env, tx, common.Address{1})
			adapted.OnTxStart(env, tx, common.Address{1})
			live.SetBalance(target, uint256.NewInt(7), tracing.BalanceChangeUnspecified)
			live.SetCode(target, []byte{0x00}, tracing.CodeChangeUnspecified)
			live.SetNonce(target, 2, tracing.NonceChangeEoACall)
			baseline.OnTxEnd(&ethtypes.Receipt{}, nil)
			finish(live)
			want, err := baseline.GetResult()
			require.NoError(t, err)

			got, err := adapted.GetResult()

			require.NoError(t, err)
			require.JSONEq(t, string(want), string(got))
		})
	}
}

func newNativeTracer(t *testing.T, config native.PrestateTracerConfig) *tracers.Tracer {
	t.Helper()
	raw, err := json.Marshal(config)
	require.NoError(t, err)
	tracer, err := tracers.DefaultDirectory.New("prestateTracer", nil, raw, params.AllDevChainProtocolChanges)
	require.NoError(t, err)
	return tracer
}
