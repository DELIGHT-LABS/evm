package keeper

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/eth/tracers/logger"
	ethparams "github.com/ethereum/go-ethereum/params"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	evmtrace "github.com/cosmos/evm/trace"
	"github.com/cosmos/evm/x/vm/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

type queryTrace struct {
	ctx      sdk.Context
	tracer   *tracers.Tracer
	deadline context.Context
	cancel   context.CancelFunc
	span     trace.Span
}

func prepareQueryTrace(ctx sdk.Context, msg *core.Message, config traceTxConfig) (_ *queryTrace, err error) {
	traceConfig := config.traceConfig
	if traceConfig == nil {
		traceConfig = &types.TraceConfig{}
	}
	ctx, span := ctx.StartSpan(tracer, "traceTxWithMsg", trace.WithAttributes(
		attribute.String("tx_hash", config.txConfig.TxHash.Hex()),
		attribute.Int("tx_index", int(config.txConfig.TxIndex)), //#nosec G115 -- int overflow is not a concern here
		attribute.String("from", msg.From.Hex()),
	))
	defer func() {
		if err != nil {
			evmtrace.EndSpanErr(span, err)
		}
	}()

	var chainOverrides *ethparams.ChainConfig
	if traceConfig.Overrides != nil {
		chainOverrides = traceConfig.Overrides.EthereumConfig(types.GetEthChainConfig().ChainID)
	}
	sLogger := logger.NewStructLogger(&logger.Config{
		EnableMemory:     traceConfig.EnableMemory,
		DisableStorage:   traceConfig.DisableStorage,
		DisableStack:     traceConfig.DisableStack,
		EnableReturnData: traceConfig.EnableReturnData,
		Limit:            int(traceConfig.Limit),
		Overrides:        chainOverrides,
	})
	evmTracer := &tracers.Tracer{
		Hooks:     sLogger.Hooks(),
		GetResult: sLogger.GetResult,
		Stop:      sLogger.Stop,
	}
	if traceConfig.Tracer != "" {
		var cfg json.RawMessage
		if traceConfig.TracerJsonConfig != "" {
			cfg = json.RawMessage(traceConfig.TracerJsonConfig)
		}
		evmTracer, err = tracers.DefaultDirectory.New(traceConfig.Tracer, &tracers.Context{
			BlockHash: common.BytesToHash(ctx.HeaderHash()),
			TxIndex:   int(config.txConfig.TxIndex), //#nosec G115 -- int overflow is not a concern here
			TxHash:    config.txConfig.TxHash,
		}, cfg, types.GetEthChainConfig())
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	if traceConfig.Tracer == "callTracerLegacy" {
		finalizeLegacyCallTrace(evmTracer)
	}

	timeout := defaultTraceTimeout
	if traceConfig.Timeout != "" {
		timeout, err = time.ParseDuration(traceConfig.Timeout)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "timeout value: %s", err.Error())
		}
	}
	deadline, cancel := context.WithTimeout(ctx.Context(), timeout)
	return &queryTrace{
		ctx:      buildTraceCtx(ctx, msg.GasLimit),
		tracer:   evmTracer,
		deadline: deadline,
		cancel:   cancel,
		span:     span,
	}, nil
}

// start watches the timeout after the caller has finished decorating the tracer.
func (t *queryTrace) start() {
	stop := t.tracer.Stop
	go func() {
		<-t.deadline.Done()
		if errors.Is(t.deadline.Err(), context.DeadlineExceeded) {
			stop(errors.New("execution timeout"))
		}
	}()
}

func (t *queryTrace) close(err error) {
	t.cancel()
	evmtrace.EndSpanErr(t.span, err)
}

func (t *queryTrace) result() (*any, error) {
	var result any
	result, err := t.tracer.GetResult()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &result, nil
}
