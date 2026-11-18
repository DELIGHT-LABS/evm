package keeper

import (
	"encoding/json"

	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/eth/tracers/native"

	"github.com/cosmos/evm/x/vm/statedb"
	"github.com/cosmos/evm/x/vm/tracer/prestate"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

func (k *Keeper) preparePrestateTrace(ctx sdk.Context, txConfig statedb.TxConfig, rawConfig string, tracer *tracers.Tracer) (sdk.Context, func(tracing.StateDB), error) {
	var config native.PrestateTracerConfig
	if rawConfig != "" {
		if err := json.Unmarshal([]byte(rawConfig), &config); err != nil {
			return ctx, nil, err
		}
	}
	if !config.DiffMode {
		return ctx, nil, nil
	}
	observed, finish := prestate.Prepare(ctx, prestate.Config{
		Tracer: config,
		Before: statedb.New(ctx, k, txConfig),
		Store:  k.storeKey.Name(),
	}, tracer)
	return observed, finish, nil
}
