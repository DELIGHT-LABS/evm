package keeper

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/cosmos/evm/x/vm/types"

	sdkmath "cosmossdk.io/math"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

func calculateChargedGas(gasLimit, rawEVMGas, hookGas uint64, minGasMultiplier sdkmath.LegacyDec) (uint64, error) {
	if rawEVMGas > math.MaxUint64-hookGas {
		return 0, fmt.Errorf("%w: raw EVM gas plus hook gas", types.ErrGasOverflow)
	}
	rawTotal := rawEVMGas + hookGas

	if minGasMultiplier.IsNil() {
		// in case we are executing eth_call on a legacy block, returns a zero value.
		minGasMultiplier = sdkmath.LegacyZeroDec()
	}
	minimumGasUsed := sdkmath.LegacyNewDecFromInt(sdkmath.NewIntFromUint64(gasLimit)).Mul(minGasMultiplier).TruncateInt()
	if !minimumGasUsed.IsUint64() {
		return 0, fmt.Errorf("%w: minimum gas used %s is not a uint64", types.ErrGasOverflow, minimumGasUsed.String())
	}

	charged := max(rawTotal, minimumGasUsed.Uint64())
	if charged > gasLimit {
		return 0, fmt.Errorf("gas required exceeds allowance (%d)", gasLimit)
	}
	return charged, nil
}

func buildPostTxHookContext(ctx sdk.Context, gasLimit uint64) sdk.Context {
	if ctx.Context() == nil {
		ctx = ctx.WithContext(context.Background())
	}
	return ctx.
		WithGasMeter(storetypes.NewGasMeter(gasLimit)).
		WithKVGasConfig(storetypes.KVGasConfig()).
		WithTransientKVGasConfig(storetypes.TransientGasConfig())
}

func executePostTxHooks(ctx sdk.Context, gasLimit uint64, run func(sdk.Context) error) (gasUsed uint64, outOfGas bool, err error) {
	hookCtx := buildPostTxHookContext(ctx, gasLimit)
	defer func() {
		if recovered := recover(); recovered != nil {
			switch recovered.(type) {
			case storetypes.ErrorOutOfGas, storetypes.ErrorGasOverflow, types.ErrorGasOverflow:
				gasUsed = gasLimit
				outOfGas = true
			default:
				panic(recovered)
			}
			return
		}
		if !outOfGas {
			gasUsed = hookCtx.GasMeter().GasConsumedToLimit()
		}
	}()

	err = run(hookCtx)
	if errors.Is(err, sdkerrors.ErrOutOfGas) {
		gasUsed = gasLimit
		err = nil
		outOfGas = true
	}
	return gasUsed, outOfGas, err
}
