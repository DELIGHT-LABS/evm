package keeper

import (
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	sdkmath "cosmossdk.io/math"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	sdktestutil "github.com/cosmos/cosmos-sdk/testutil"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

func TestCalculateChargedGas(t *testing.T) {
	testCases := []struct {
		name       string
		gasLimit   uint64
		rawEVMGas  uint64
		hookGas    uint64
		multiplier sdkmath.LegacyDec
		expected   uint64
		expectErr  bool
	}{
		{
			name:       "no hook returns raw EVM gas",
			gasLimit:   100_000,
			rawEVMGas:  21_000,
			multiplier: sdkmath.LegacyZeroDec(),
			expected:   21_000,
		},
		{
			name:       "no hook preserves multiplier floor",
			gasLimit:   100_000,
			rawEVMGas:  21_000,
			multiplier: sdkmath.LegacyMustNewDecFromStr("0.5"),
			expected:   50_000,
		},
		{
			name:       "hook raw total wins",
			gasLimit:   100_000,
			rawEVMGas:  21_000,
			hookGas:    30_000,
			multiplier: sdkmath.LegacyMustNewDecFromStr("0.5"),
			expected:   51_000,
		},
		{
			name:       "multiplier floor wins without adding hook twice",
			gasLimit:   100_000,
			rawEVMGas:  21_000,
			hookGas:    10_000,
			multiplier: sdkmath.LegacyMustNewDecFromStr("0.5"),
			expected:   50_000,
		},
		{
			name:       "raw total overflow",
			gasLimit:   math.MaxUint64,
			rawEVMGas:  math.MaxUint64,
			hookGas:    1,
			multiplier: sdkmath.LegacyZeroDec(),
			expectErr:  true,
		},
		{
			name:       "multiplier conversion overflow",
			gasLimit:   math.MaxUint64,
			rawEVMGas:  1,
			multiplier: sdkmath.LegacyNewDec(2),
			expectErr:  true,
		},
		{
			name:       "charged gas above candidate limit",
			gasLimit:   100,
			rawEVMGas:  90,
			hookGas:    11,
			multiplier: sdkmath.LegacyZeroDec(),
			expectErr:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual, err := calculateChargedGas(tc.gasLimit, tc.rawEVMGas, tc.hookGas, tc.multiplier)
			if tc.expectErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expected, actual)
		})
	}
}

func TestExecutePostTxHooks(t *testing.T) {
	t.Run("standard KV read and write gas is included", func(t *testing.T) {
		key := storetypes.NewKVStoreKey("hook-gas")
		transientKey := storetypes.NewTransientStoreKey("hook-gas-transient")
		ctx := sdktestutil.DefaultContext(key, transientKey)
		gasUsed, outOfGas, err := executePostTxHooks(ctx, 10_000, func(hookCtx sdk.Context) error {
			store := hookCtx.KVStore(key)
			store.Set([]byte{1}, []byte{2})
			require.Equal(t, []byte{2}, store.Get([]byte{1}))
			return nil
		})

		config := storetypes.KVGasConfig()
		expected := config.WriteCostFlat + 2*config.WriteCostPerByte +
			config.ReadCostFlat + 2*config.ReadCostPerByte
		require.NoError(t, err)
		require.False(t, outOfGas)
		require.Equal(t, expected, gasUsed)
	})

	t.Run("ordinary error preserves consumed gas", func(t *testing.T) {
		expectedErr := errors.New("hook rejected")
		gasUsed, outOfGas, err := executePostTxHooks(sdk.Context{}, 1_000, func(ctx sdk.Context) error {
			ctx.GasMeter().ConsumeGas(123, "hook work")
			return expectedErr
		})

		require.Equal(t, uint64(123), gasUsed)
		require.ErrorIs(t, err, expectedErr)
		require.False(t, outOfGas)
	})

	t.Run("SDK out of gas is retryable and charged to limit", func(t *testing.T) {
		gasUsed, outOfGas, err := executePostTxHooks(sdk.Context{}, 100, func(ctx sdk.Context) error {
			ctx.GasMeter().ConsumeGas(101, "hook work")
			return nil
		})

		require.Equal(t, uint64(100), gasUsed)
		require.NoError(t, err)
		require.True(t, outOfGas)
	})

	t.Run("SDK overflow is retryable and charged to limit", func(t *testing.T) {
		gasUsed, outOfGas, err := executePostTxHooks(sdk.Context{}, math.MaxUint64, func(ctx sdk.Context) error {
			ctx.GasMeter().ConsumeGas(math.MaxUint64, "first")
			ctx.GasMeter().ConsumeGas(1, "overflow")
			return nil
		})

		require.Equal(t, uint64(math.MaxUint64), gasUsed)
		require.NoError(t, err)
		require.True(t, outOfGas)
	})
}
