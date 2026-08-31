package keeper

import (
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/tracing"

	"github.com/cosmos/evm/x/vm/statedb"
	vmtracer "github.com/cosmos/evm/x/vm/tracer"
	"github.com/cosmos/evm/x/vm/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

// SetGlobalTracerFactories configures application-provided tracers that are
// installed for every top-level EVM execution. Cosmos EVM does not install a
// global tracer unless the embedding application calls this method.
func (k *Keeper) SetGlobalTracerFactories(factories ...vmtracer.GlobalFactory) *Keeper {
	k.globalTracerFactories = append([]vmtracer.GlobalFactory(nil), factories...)
	return k
}

func (k Keeper) prepareTracing(
	ctx sdk.Context,
	msg core.Message,
	txConfig statedb.TxConfig,
	commit bool,
) (sdk.Context, *tracing.Hooks) {
	var base *tracing.Hooks
	if k.tracer != "" {
		base = k.Tracer(ctx, msg, types.GetEthChainConfig())
	}

	manager := vmtracer.New(base)
	ctx = vmtracer.WithManager(ctx, manager)
	execution := vmtracer.ExecutionInfo{
		Message: msg,
		TxHash:  txConfig.TxHash,
		TxIndex: uint64(txConfig.TxIndex),
		Commit:  commit,
	}
	for _, factory := range k.globalTracerFactories {
		if factory == nil {
			continue
		}
		var executionTracer vmtracer.Tracer
		ctx, executionTracer = factory(ctx, execution)
		manager.InstallGlobal(executionTracer)
	}
	return ctx, manager.Hooks()
}
