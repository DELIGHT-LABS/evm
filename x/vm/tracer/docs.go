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
// # Recommended EVM execution APIs
//
// Prefer keeper entry points that manage tracing for the caller:
//
//   - ApplyTransaction for Ethereum transaction execution, after the required
//     ante-handler validation and preprocessing.
//   - CallEVM or CallEVMWithData for native module and precompile contract calls.
//     When called from a precompile, pass its existing StateDB and set
//     callFromPrecompile=true. These methods reuse an active enclosing manager;
//     sequential top-level calls receive separate managers.
//   - CallEVMViewWithData for isolated, non-committing reads. These
//     calls use an independent manager, not the parent's session tracers.
//   - EthCall, EstimateGas, TraceCall, TraceTx, and TraceBlock for query and debug
//     execution. Gas-estimation candidates and replayed transactions each get
//     their own manager. Debug tracers are preserved as the manager's base hooks.
//
// These entry points provide a manager even without global factories, so a
// precompile can install a session tracer. Enabling global factories adds the
// application's tracers to those managed executions. Factories also run during
// simulation and replay, potentially multiple times per RPC request; they must
// not assume that every invocation represents a newly accepted transaction.
//
// # Low-level APIs require explicit tracing setup
//
// ApplyMessage, ApplyMessageWithConfig, NewEVM, and NewEVMWithOverridePrecompiles
// do not create a manager or invoke the registered global tracer factories.
// SetGlobalTracerFactories alone does not enable composable tracing for direct
// calls to these APIs. Passing nil hooks may select the keeper's configured
// legacy tracer, but does not make session.Install work.
//
// Direct callers must connect BOTH sides of the same manager: put it in the
// execution context used by StateDB, and pass manager.Hooks() as tracingHooks
// (vm.Config.Tracer when constructing a VM directly). A context-only manager
// receives no events; hooks alone leave session.Install without a manager.
// Preserve any desired base tracer by passing its hooks to New(baseHooks).
//
// For example, a standalone top-level ApplyMessage call with the built-in
// transaction collector can be wired explicitly using public APIs:
//
//	import (
//		"github.com/cosmos/evm/x/vm/statedb"
//		vmtracer "github.com/cosmos/evm/x/vm/tracer"
//		vmglobaltracer "github.com/cosmos/evm/x/vm/tracer/global"
//	)
//
//	// ctx and msg are prepared by the caller; baseHooks may be nil.
//	txConfig := statedb.NewEmptyTxConfig()
//	manager := vmtracer.New(baseHooks)
//	execCtx := vmtracer.WithManager(ctx, manager)
//	execCtx, collector := vmglobaltracer.TxTraceFactory(execCtx, vmtracer.ExecutionInfo{
//		Message: msg,
//		TxHash:  txConfig.TxHash,
//		TxIndex: uint64(txConfig.TxIndex),
//		Commit:  true,
//	})
//	manager.InstallGlobal(collector)
//	stateDB := statedb.New(execCtx, evmKeeper, txConfig)
//	res, err := evmKeeper.ApplyMessage(execCtx, stateDB, msg, manager.Hooks(), true, false, false)
//
// This example explicitly invokes one factory; it does not consult the keeper's
// registered factory list. For custom factories, invoke the application-owned
// factories with the execution metadata, retain each returned context, and
// install each returned tracer before constructing StateDB. ApplyMessage uses
// an empty TxConfig; use matching explicit metadata with ApplyMessageWithConfig.
// This tracing setup does not replace ante validation, gas/context preparation,
// or the other responsibilities of a low-level execution caller.
//
// If StateDB already exists, AttachTracerManager updates its manager reference,
// including an initialized cache context. It does not copy other context values
// returned by global factories; creating StateDB from the final execution
// context is preferred for a fresh execution.
//
// For nested execution, prefer CallEVM/CallEVMWithData. A low-level nested caller
// must deliberately reuse the active enclosing manager and pass NestedHooks(),
// which suppresses duplicate transaction-boundary events. Do not infer activity
// solely from FromContext succeeding: a completed manager can remain in StateDB.
// Use HasActiveFrame to distinguish an active parent from a sequential call.
// NewEVM callers also own transaction-boundary hook dispatch; constructing the
// VM alone does not emit OnTxStart or OnTxEnd.
//
// # Reading global traces
//
// A post-tx hook reads the ordered trace and checks collection errors:
//
//	observations, err := vmglobaltracer.GetTxTrace(ctx, uint64(receipt.TransactionIndex))
//
// Call enter/exit, native transfers, token-call requests and raw logs share one
// occurrence sequence. Reverted attempts remain present. ERC20 request records
// have separate IDs, as required by the consumer's raw observation contract;
// calldata and logs are not decoded into token movements by the collector.
// Balance-change records are additional diagnostics, not guessed transfers.
//
// Custom collectors implement tracer.Journal. Collection errors report missing
// facts; explicit enforcement errors must be checked before committing. Keeper
// handles that check on managed execution paths. Low-level VM callers own it.
//
// Session tracers are installed dynamically through the session package.
// sessionHooks below is owned by the embedding application. Callers must retain
// and invoke the returned cleanup function:
//
//	import vmsessiontracer "github.com/cosmos/evm/x/vm/tracer/session"
//
//	uninstall, err := vmsessiontracer.Install(
//		ctx,
//		vmsessiontracer.New(sessionHooks),
//	)
//	if err != nil {
//		return err
//	}
//	defer uninstall()
//
// The manager reports logical call depths across precompile reentry and nested
// EVM instances. Applications do not need to add an offset for these calls.
// session.WithDepthOffset is available when a consumer needs an additional,
// explicit offset in its own trace representation.
//
// Managers belong to one EVM execution and are not safe for concurrent use,
// matching vm.EVM's concurrency guarantees.
package tracer
