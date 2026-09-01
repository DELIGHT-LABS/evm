// Package tracer provides composable EVM tracing hooks with global and session
// lifecycles. Cosmos EVM does not enable a global tracer by default; an
// embedding application must opt in during app wiring.
//
// An embedding application can enable the built-in transaction trace collector
// during app wiring using only the package's public API:
//
//	import vmglobaltracer "github.com/cosmos/evm/x/vm/tracer/global"
//
//	evmKeeper.SetGlobalTracerFactories(vmglobaltracer.TxTraceFactory)
//
// TxTraceFactory is registered as a function value. The keeper invokes it once
// for each top-level EVM execution, producing an execution-local collector and
// hooks.
//
// A post-tx hook running with the execution context can then read its trace:
//
//	touches, transfers, erc20Transfers := vmglobaltracer.GetTxTrace(
//		ctx,
//		uint64(receipt.TransactionIndex),
//	)
//
// Applications can replace the built-in collection policy while retaining the
// standard GetTxTrace result API by implementing global.Collector. A custom
// collector provides Hooks, Touches, Transfers, and ERC20Transfers; it can
// decide which calls to retain, how to handle reverted frames, and which value
// movements qualify as native or ERC20 transfers.
//
// The custom collector and its constructor belong to the embedding application:
//
//	import (
//		"github.com/cosmos/cosmos-sdk/types"
//
//		vmtracer "github.com/cosmos/evm/x/vm/tracer"
//		vmglobaltracer "github.com/cosmos/evm/x/vm/tracer/global"
//	)
//
//	func policyTraceFactory(
//		ctx types.Context,
//		execution vmtracer.ExecutionInfo,
//	) (types.Context, vmtracer.Tracer) {
//		// newPolicyCollector is implemented by the embedding application and
//		// returns a value satisfying vmglobaltracer.Collector.
//		collector := newPolicyCollector()
//		ctx = vmglobaltracer.WithCollector(
//			ctx,
//			execution.TxIndex,
//			collector,
//		)
//		return ctx, collector
//	}
//
//	evmKeeper.SetGlobalTracerFactories(policyTraceFactory)
//
// Collectors with application-specific result types do not need to implement
// global.Collector. They can return any vmtracer.Tracer from a GlobalFactory and
// store their typed result in an application-owned context value instead.
//
// Session tracers are installed dynamically through the session package.
// sessionHooks below is owned by the embedding application. Callers must retain
// and invoke the returned cleanup function:
//
//	import vmsessiontracer "github.com/cosmos/evm/x/vm/tracer/session"
//
//	uninstall, ok := vmsessiontracer.Install(
//		ctx,
//		vmsessiontracer.WithDepthOffset(sessionHooks, 1),
//	)
//	if ok {
//		defer uninstall()
//	}
//
// Managers belong to one EVM execution and are not safe for concurrent use,
// matching vm.EVM's concurrency guarantees.
package tracer
