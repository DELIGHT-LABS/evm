package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	gethvm "github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/holiman/uint256"

	// Register geth's built-in tracers for trace and replay integration tests.
	_ "github.com/ethereum/go-ethereum/eth/tracers/native"

	precompilecommon "github.com/cosmos/evm/precompiles/common"
	testconstants "github.com/cosmos/evm/testutil/constants"
	"github.com/cosmos/evm/x/erc20/types"
	vmkeeper "github.com/cosmos/evm/x/vm/keeper"
	"github.com/cosmos/evm/x/vm/statedb"
	vmtracer "github.com/cosmos/evm/x/vm/tracer"
	vmglobaltracer "github.com/cosmos/evm/x/vm/tracer/global"
	vmsessiontracer "github.com/cosmos/evm/x/vm/tracer/session"
	evmtypes "github.com/cosmos/evm/x/vm/types"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

type sessionTracerPrecompile struct {
	precompilecommon.Precompile
	target                common.Address
	manager               *vmtracer.Manager
	managers              []*vmtracer.Manager
	installed             bool
	enterCalls            int
	enterDepths           []int
	keeper                *vmkeeper.Keeper
	keeperTarget          common.Address
	activeAfterDirectCall bool
}

func (p *sessionTracerPrecompile) Name() string { return "session-tracer-test" }

func (p *sessionTracerPrecompile) RequiredGas(_ []byte) uint64 { return 0 }

func (p *sessionTracerPrecompile) Run(evm *gethvm.EVM, contract *gethvm.Contract, _ bool) ([]byte, error) {
	return p.RunNativeAction(evm, contract, func(ctx sdk.Context) ([]byte, error) {
		p.manager, _ = vmtracer.FromContext(ctx)
		p.managers = append(p.managers, p.manager)
		uninstall, err := vmsessiontracer.Install(ctx, vmsessiontracer.New(&tracing.Hooks{
			OnEnter: func(depth int, _ byte, _, _ common.Address, _ []byte, _ uint64, _ *big.Int) {
				p.enterCalls++
				p.enterDepths = append(p.enterDepths, depth)
			},
		}))
		p.installed = err == nil
		if err != nil {
			return nil, fmt.Errorf("install session tracer: %w", err)
		}
		defer uninstall()

		_, _, err = evm.Call(contract.Address(), p.target, nil, contract.Gas, uint256.NewInt(0))
		p.activeAfterDirectCall = p.manager.HasActiveCall()
		if err == nil && p.keeper != nil {
			stateDB, ok := evm.StateDB.(*statedb.StateDB)
			if !ok {
				return nil, fmt.Errorf("unexpected StateDB type %T", evm.StateDB)
			}
			// A call rejected before OnEnter must not disturb the enclosing frame.
			_, rejectedErr := p.keeper.CallEVMWithData(ctx, stateDB, p.Address(), &p.keeperTarget, nil, true, true, big.NewInt(1))
			if !errors.Is(rejectedErr, core.ErrIntrinsicGas) {
				return nil, fmt.Errorf("expected intrinsic gas failure, got %v", rejectedErr)
			}
			_, err = p.keeper.CallEVMWithData(ctx, stateDB, p.Address(), &p.keeperTarget, nil, true, true, new(big.Int).SetUint64(contract.Gas))
		}
		return []byte{1}, err
	})
}

func (s *KeeperTestSuite) TestQueryPathsProvideSessionTracerManager() {
	for _, withGlobal := range []bool{false, true} {
		for _, path := range []string{"EstimateGas", "TraceCall", "TraceCallStructLogger", "TraceTx", "TraceTxPredecessor", "TraceBlock"} {
			s.Run(fmt.Sprintf("%s/global=%t", path, withGlobal), func() {
				s.SetupTest()
				ctx := s.Network.GetContext()
				k := s.Network.App.GetEVMKeeper()
				s.Require().False(k.HasHooks(), "exercise estimation without post-tx hooks")
				address := common.HexToAddress("0x000000000000000000000000000000000000ffff")
				precompile := &sessionTracerPrecompile{
					Precompile: precompilecommon.Precompile{
						KvGasConfig: ctx.KVGasConfig(), TransientKVGasConfig: ctx.TransientKVGasConfig(), ContractAddress: address,
					},
					target: s.Keyring.GetAddr(1),
				}
				k.RegisterStaticPrecompile(address, precompile)
				params := k.GetParams(ctx)
				params.ActiveStaticPrecompiles = append(params.ActiveStaticPrecompiles, address.String())
				s.Require().NoError(k.SetParams(ctx, params))

				var managers []*vmtracer.Manager
				var executions []vmtracer.ExecutionInfo
				var starts, ends int
				if withGlobal {
					k.SetGlobalTracerFactories(func(ctx sdk.Context, info vmtracer.ExecutionInfo) (sdk.Context, vmtracer.Tracer) {
						manager, ok := vmtracer.FromContext(ctx)
						s.Require().True(ok)
						managers = append(managers, manager)
						executions = append(executions, info)
						return ctx, vmsessiontracer.New(&tracing.Hooks{
							OnTxStart: func(_ *tracing.VMContext, _ *ethtypes.Transaction, _ common.Address) { starts++ },
							OnTxEnd:   func(_ *ethtypes.Receipt, _ error) { ends++ },
						})
					})
				}

				const gasLimit = uint64(200_000)
				sender := s.Keyring.GetAddr(0)
				zeroGasPrice := hexutil.Big(*big.NewInt(0))
				// Nonempty input avoids EstimateGas's plain-transfer shortcut.
				input := hexutil.Bytes{1}
				args, err := json.Marshal(evmtypes.TransactionArgs{
					From:     &sender,
					To:       &address,
					GasPrice: &zeroGasPrice,
					Input:    &input,
				})
				s.Require().NoError(err)
				newTx := func(nonce uint64) *evmtypes.MsgEthereumTx {
					tx, err := s.Factory.GenerateSignedEthTx(s.Keyring.GetPrivKey(0), evmtypes.EvmTxArgs{
						To: &address, Nonce: nonce, GasLimit: gasLimit, GasPrice: big.NewInt(0),
					})
					s.Require().NoError(err)
					return tx.GetMsgs()[0].(*evmtypes.MsgEthereumTx)
				}
				var traceData []byte
				expectedExecutions := 1
				switch path {
				case "EstimateGas":
					res, err := k.EstimateGas(ctx, &evmtypes.EthCallRequest{
						Args: args, GasCap: gasLimit, ProposerAddress: ctx.BlockHeader().ProposerAddress,
					})
					s.Require().NoError(err)
					s.Require().Positive(res.Gas)
					s.Require().Greater(len(precompile.managers), 1, "exercise multiple gas candidates")
					expectedExecutions = len(precompile.managers)
				case "TraceCall", "TraceCallStructLogger":
					config := &evmtypes.TraceConfig{Tracer: "callTracer"}
					if path == "TraceCallStructLogger" {
						config.Tracer = ""
					}
					res, err := k.TraceCall(ctx, &evmtypes.QueryTraceCallRequest{
						Args: args, GasCap: gasLimit, TraceConfig: config, BlockNumber: ctx.BlockHeight(),
						BlockTime: ctx.BlockTime(), ProposerAddress: ctx.BlockHeader().ProposerAddress,
					})
					s.Require().NoError(err)
					traceData = res.Data
				case "TraceTx", "TraceTxPredecessor":
					req := getDefaultTraceTxRequest(s.Network)
					req.BlockNumber = ctx.BlockHeight()
					req.TraceConfig.Tracer = "callTracer"
					req.Msg = newTx(0)
					if path == "TraceTxPredecessor" {
						req.Predecessors = []*evmtypes.MsgEthereumTx{req.Msg}
						req.Msg = newTx(1)
						expectedExecutions = 2
					}
					res, err := k.TraceTx(ctx, req)
					s.Require().NoError(err)
					traceData = res.Data
				case "TraceBlock":
					req := getDefaultTraceBlockRequest(s.Network)
					req.BlockNumber = ctx.BlockHeight()
					req.TraceConfig = &evmtypes.TraceConfig{Tracer: "callTracer"}
					req.Txs = []*evmtypes.MsgEthereumTx{newTx(0), newTx(1)}
					expectedExecutions = 2
					res, err := k.TraceBlock(ctx, &req)
					s.Require().NoError(err)
					var results []struct {
						Result json.RawMessage `json:"result"`
						Error  string          `json:"error"`
					}
					s.Require().NoError(json.Unmarshal(res.Data, &results))
					s.Require().Len(results, 2)
					for _, result := range results {
						s.Require().Empty(result.Error)
						s.Require().NotContains(string(result.Result), `"error"`)
					}
					traceData = results[1].Result
				}
				if path == "TraceCallStructLogger" {
					var result struct {
						Failed      bool   `json:"failed"`
						ReturnValue string `json:"returnValue"`
					}
					s.Require().NoError(json.Unmarshal(traceData, &result))
					s.Require().False(result.Failed)
					s.Require().Equal("0x01", result.ReturnValue)
				} else if traceData != nil {
					var result struct {
						To     common.Address `json:"to"`
						Error  string         `json:"error"`
						Output string         `json:"output"`
						Calls  []struct {
							To common.Address `json:"to"`
						} `json:"calls"`
					}
					s.Require().NoError(json.Unmarshal(traceData, &result))
					s.Require().Empty(result.Error)
					s.Require().Equal("0x01", result.Output)
					s.Require().Equal(address, result.To)
					s.Require().Len(result.Calls, 1)
					s.Require().Equal(precompile.target, result.Calls[0].To)
				}
				s.Require().Equal(expectedExecutions, precompile.enterCalls)
				s.Require().Len(precompile.managers, expectedExecutions)
				seen := make(map[*vmtracer.Manager]bool)
				for _, manager := range precompile.managers {
					s.Require().NotNil(manager)
					s.Require().False(manager.HasActiveCall())
					s.Require().False(seen[manager], "each candidate/transaction needs a fresh manager")
					seen[manager] = true
				}
				if withGlobal {
					if path != "EstimateGas" {
						s.Require().Equal(precompile.managers, managers)
					}
					factoryManagers := make(map[*vmtracer.Manager]bool)
					for _, manager := range managers {
						s.Require().False(factoryManagers[manager])
						s.Require().False(manager.HasActiveCall())
						factoryManagers[manager] = true
					}
					for manager := range seen {
						s.Require().True(factoryManagers[manager])
					}
					s.Require().Equal(len(managers), starts)
					s.Require().Equal(starts, ends)
					for i, execution := range executions {
						expectedCommit := path == "TraceBlock" || path == "TraceTxPredecessor" && i == 0
						s.Require().Equal(expectedCommit, execution.Commit)
						if expectedExecutions == 2 && path != "EstimateGas" {
							s.Require().Equal(uint64(i), execution.TxIndex)
						}
					}
				}
			})
		}
	}
}

func (s *KeeperTestSuite) TestSameVMReentryPreservesSessionTracerForKeeperCalls() {
	s.SetupTest()
	ctx := s.Network.GetContext()
	evmKeeper := s.Network.App.GetEVMKeeper()
	outerAddress := common.HexToAddress("0x000000000000000000000000000000000000fffe")
	innerAddress := common.HexToAddress("0x000000000000000000000000000000000000ffff")
	target := s.Keyring.GetAddr(1)
	precompiles := make([]*sessionTracerPrecompile, 0, 2)
	params := evmKeeper.GetParams(ctx)
	for _, address := range []common.Address{outerAddress, innerAddress} {
		precompile := &sessionTracerPrecompile{
			Precompile: precompilecommon.Precompile{
				KvGasConfig:          ctx.KVGasConfig(),
				TransientKVGasConfig: ctx.TransientKVGasConfig(),
				ContractAddress:      address,
			},
			target:       target,
			keeper:       evmKeeper,
			keeperTarget: target,
		}
		precompiles = append(precompiles, precompile)
		evmKeeper.RegisterStaticPrecompile(address, precompile)
		params.ActiveStaticPrecompiles = append(params.ActiveStaticPrecompiles, address.String())
	}
	// Both the top-level VM and the nested VM first reenter their own VM,
	// then invoke CallEVMWithData while their session tracer is still installed.
	precompiles[0].keeperTarget = innerAddress
	s.Require().NoError(evmKeeper.SetParams(ctx, params))

	var callTracer *tracers.Tracer
	var manager *vmtracer.Manager
	var factoryCalls, txStarts, txEnds int
	evmKeeper.SetGlobalTracerFactories(func(factoryCtx sdk.Context, _ vmtracer.ExecutionInfo) (sdk.Context, vmtracer.Tracer) {
		factoryCalls++
		manager, _ = vmtracer.FromContext(factoryCtx)
		ct, err := tracers.DefaultDirectory.New("callTracer", &tracers.Context{}, nil, evmtypes.GetEthChainConfig())
		s.Require().NoError(err)
		callTracer = ct
		hooks := *ct.Hooks
		hooks.OnTxStart = func(vm *tracing.VMContext, tx *ethtypes.Transaction, from common.Address) {
			txStarts++
			ct.OnTxStart(vm, tx, from)
		}
		hooks.OnTxEnd = func(receipt *ethtypes.Receipt, err error) {
			txEnds++
			ct.OnTxEnd(receipt, err)
		}
		return factoryCtx, vmsessiontracer.New(&hooks)
	})

	tx, err := s.Factory.GenerateSignedEthTx(s.Keyring.GetPrivKey(0), evmtypes.EvmTxArgs{
		To:       &outerAddress,
		GasLimit: 500_000,
		GasPrice: big.NewInt(0),
	})
	s.Require().NoError(err)
	res, err := evmKeeper.ApplyTransaction(ctx.WithGasMeter(storetypes.NewGasMeter(1_000_000)), tx.GetMsgs()[0].(*evmtypes.MsgEthereumTx).AsTransaction())
	s.Require().NoError(err)
	s.Require().False(res.Failed())
	s.Require().Equal(1, factoryCalls)
	s.Require().Equal(1, txStarts)
	s.Require().Equal(1, txEnds)
	for _, precompile := range precompiles {
		s.Require().Same(manager, precompile.manager)
		s.Require().True(precompile.activeAfterDirectCall)
	}
	s.Require().False(manager.HasActiveCall())
	s.Require().Equal([]int{1, 1, 2, 2}, precompiles[0].enterDepths)
	s.Require().Equal([]int{2, 2}, precompiles[1].enterDepths)

	result, err := callTracer.GetResult()
	s.Require().NoError(err)
	type callFrame struct {
		To    common.Address `json:"to"`
		Calls []callFrame    `json:"calls"`
	}
	var root callFrame
	s.Require().NoError(json.Unmarshal(result, &root))
	s.Require().Equal(callFrame{To: outerAddress, Calls: []callFrame{
		{To: target},
		{
			To:    innerAddress,
			Calls: []callFrame{{To: target}, {To: target}},
		},
	}}, root)
}

type nestedCallEVMPrecompile struct {
	precompilecommon.Precompile
	keeper  *vmkeeper.Keeper
	target  common.Address
	manager *vmtracer.Manager
}

func (p *nestedCallEVMPrecompile) Name() string { return "nested-call-evm-test" }

func (p *nestedCallEVMPrecompile) RequiredGas(_ []byte) uint64 { return 0 }

func (p *nestedCallEVMPrecompile) Run(evm *gethvm.EVM, contract *gethvm.Contract, _ bool) ([]byte, error) {
	return p.RunNativeAction(evm, contract, func(ctx sdk.Context) ([]byte, error) {
		p.manager, _ = vmtracer.FromContext(ctx)
		stateDB, ok := evm.StateDB.(*statedb.StateDB)
		if !ok {
			return nil, fmt.Errorf("unexpected StateDB type %T", evm.StateDB)
		}
		response, err := p.keeper.CallEVMWithData(
			ctx,
			stateDB,
			p.Address(),
			&p.target,
			nil,
			true,
			true,
			new(big.Int).SetUint64(contract.Gas),
		)
		if err != nil {
			return nil, err
		}
		return response.Ret, nil
	})
}

func (s *KeeperTestSuite) TestCallEVMWithDataProvidesSessionTracerManager() {
	testCases := []struct {
		name            string
		initializeCache bool
		staleManager    bool
	}{
		{
			name: "new manager without global factories",
		},
		{
			name:            "new manager with initialized StateDB cache",
			initializeCache: true,
		},
		{
			name:         "replace inactive manager",
			staleManager: true,
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			s.SetupTest()

			ctx := s.Network.GetContext()
			evmKeeper := s.Network.App.GetEVMKeeper()
			precompileAddress := common.HexToAddress("0x000000000000000000000000000000000000ffff")
			precompile := &sessionTracerPrecompile{
				Precompile: precompilecommon.Precompile{
					KvGasConfig:          ctx.KVGasConfig(),
					TransientKVGasConfig: ctx.TransientKVGasConfig(),
					ContractAddress:      precompileAddress,
				},
				target: s.Keyring.GetAddr(1),
			}
			evmKeeper.RegisterStaticPrecompile(precompileAddress, precompile)
			params := evmKeeper.GetParams(ctx)
			params.ActiveStaticPrecompiles = append(params.ActiveStaticPrecompiles, precompileAddress.String())
			s.Require().NoError(evmKeeper.SetParams(ctx, params))

			stateDB := statedb.New(ctx, evmKeeper, statedb.NewEmptyTxConfig())
			if tc.initializeCache {
				_, err := stateDB.GetCacheContext()
				s.Require().NoError(err)
			}

			var oldManager *vmtracer.Manager
			factoryCalls := 0
			if tc.staleManager {
				oldManager = vmtracer.New(nil)
				ctx = vmtracer.WithManager(ctx, oldManager)
				evmKeeper.SetGlobalTracerFactories(func(factoryCtx sdk.Context, _ vmtracer.ExecutionInfo) (sdk.Context, vmtracer.Tracer) {
					factoryCalls++
					return factoryCtx, nil
				})
			}

			res, err := evmKeeper.CallEVMWithData(
				ctx,
				stateDB,
				types.ModuleAddress,
				&precompileAddress,
				nil,
				false,
				false,
				nil,
			)
			s.Require().NoError(err)
			s.Require().False(res.Failed())
			s.Require().True(precompile.installed)
			s.Require().NotNil(precompile.manager)
			s.Require().Equal(1, precompile.enterCalls)
			if tc.staleManager {
				s.Require().NotSame(oldManager, precompile.manager)
				s.Require().Equal(1, factoryCalls)
			}
		})
	}
}

func (s *KeeperTestSuite) TestSequentialCallEVMWithDataStartsSeparateTracingLifecycles() {
	s.SetupTest()

	ctx := s.Network.GetContext()
	evmKeeper := s.Network.App.GetEVMKeeper()
	stateDB := statedb.New(ctx, evmKeeper, statedb.NewEmptyTxConfig())
	var managers []*vmtracer.Manager
	var txStarts, txEnds int
	evmKeeper.SetGlobalTracerFactories(func(factoryCtx sdk.Context, _ vmtracer.ExecutionInfo) (sdk.Context, vmtracer.Tracer) {
		manager, ok := vmtracer.FromContext(factoryCtx)
		s.Require().True(ok)
		managers = append(managers, manager)
		return factoryCtx, vmsessiontracer.New(&tracing.Hooks{
			OnTxStart: func(_ *tracing.VMContext, _ *ethtypes.Transaction, _ common.Address) {
				txStarts++
			},
			OnTxEnd: func(_ *ethtypes.Receipt, _ error) {
				txEnds++
			},
		})
	})

	recipient := s.Keyring.GetAddr(1)
	for range 2 {
		res, err := evmKeeper.CallEVMWithData(ctx, stateDB, types.ModuleAddress, &recipient, nil, true, false, nil)
		s.Require().NoError(err)
		s.Require().False(res.Failed())
	}

	s.Require().Len(managers, 2)
	s.Require().NotSame(managers[0], managers[1])
	s.Require().Equal(2, txStarts)
	s.Require().Equal(2, txEnds)
}

func (s *KeeperTestSuite) TestCallEVMViewWithDataStartsIndependentTracingLifecycle() {
	s.SetupTest()

	ctx := s.Network.GetContext()
	evmKeeper := s.Network.App.GetEVMKeeper()
	var managers []*vmtracer.Manager
	var collectors []vmglobaltracer.Collector
	evmKeeper.SetGlobalTracerFactories(func(factoryCtx sdk.Context, _ vmtracer.ExecutionInfo) (sdk.Context, vmtracer.Tracer) {
		manager, ok := vmtracer.FromContext(factoryCtx)
		s.Require().True(ok)
		managers = append(managers, manager)
		collector := vmglobaltracer.NewCollector()
		collectors = append(collectors, collector)
		return factoryCtx, collector
	})

	hook := &NestedEVMGasHook{
		keeper:   evmKeeper,
		contract: common.HexToAddress(testconstants.WEVMOSContractMainnet),
	}
	evmKeeper.SetHooks(vmkeeper.NewMultiEvmHooks(hook))

	recipient := s.Keyring.GetAddr(1)
	tx, err := s.Factory.GenerateSignedEthTx(s.Keyring.GetPrivKey(0), evmtypes.EvmTxArgs{
		To:       &recipient,
		GasLimit: 500_000,
		GasPrice: big.NewInt(0),
	})
	s.Require().NoError(err)
	response, err := evmKeeper.ApplyTransaction(
		ctx.WithGasMeter(storetypes.NewGasMeter(1_000_000)),
		tx.GetMsgs()[0].(*evmtypes.MsgEthereumTx).AsTransaction(),
	)

	s.Require().NoError(err)
	s.Require().False(response.Failed())
	s.Require().Len(managers, 2)
	s.Require().NotSame(managers[0], managers[1])
	s.Require().Len(collectors, 2)
	s.Require().Len(collectors[0].Observations(), 2)
	s.Require().Equal(recipient, collectors[0].Observations()[0].Target)
	s.Require().NotEmpty(collectors[1].Observations())
}

func (s *KeeperTestSuite) TestNestedCallEVMWithDataReusesTransactionTracing() {
	s.SetupTest()

	ctx := s.Network.GetContext()
	evmKeeper := s.Network.App.GetEVMKeeper()
	outerAddress := common.HexToAddress("0x000000000000000000000000000000000000fffe")
	sessionAddress := common.HexToAddress("0x000000000000000000000000000000000000ffff")
	sessionPrecompile := &sessionTracerPrecompile{
		Precompile: precompilecommon.Precompile{
			KvGasConfig:          ctx.KVGasConfig(),
			TransientKVGasConfig: ctx.TransientKVGasConfig(),
			ContractAddress:      sessionAddress,
		},
		target: s.Keyring.GetAddr(1),
	}
	outerPrecompile := &nestedCallEVMPrecompile{
		Precompile: precompilecommon.Precompile{
			KvGasConfig:          ctx.KVGasConfig(),
			TransientKVGasConfig: ctx.TransientKVGasConfig(),
			ContractAddress:      outerAddress,
		},
		keeper: evmKeeper,
		target: sessionAddress,
	}
	evmKeeper.RegisterStaticPrecompile(outerAddress, outerPrecompile)
	evmKeeper.RegisterStaticPrecompile(sessionAddress, sessionPrecompile)
	params := evmKeeper.GetParams(ctx)
	params.ActiveStaticPrecompiles = append(
		params.ActiveStaticPrecompiles,
		outerAddress.String(),
		sessionAddress.String(),
	)
	s.Require().NoError(evmKeeper.SetParams(ctx, params))

	factoryCalls := 0
	txStarts := 0
	txEnds := 0
	globalEnters := 0
	var factoryManager *vmtracer.Manager
	evmKeeper.SetGlobalTracerFactories(func(factoryCtx sdk.Context, _ vmtracer.ExecutionInfo) (sdk.Context, vmtracer.Tracer) {
		factoryCalls++
		factoryManager, _ = vmtracer.FromContext(factoryCtx)
		return factoryCtx, vmsessiontracer.New(&tracing.Hooks{
			OnTxStart: func(_ *tracing.VMContext, _ *ethtypes.Transaction, _ common.Address) {
				txStarts++
			},
			OnTxEnd: func(_ *ethtypes.Receipt, _ error) {
				txEnds++
			},
			OnEnter: func(_ int, _ byte, _, _ common.Address, _ []byte, _ uint64, _ *big.Int) {
				globalEnters++
			},
		})
	})

	tx, err := s.Factory.GenerateSignedEthTx(s.Keyring.GetPrivKey(0), evmtypes.EvmTxArgs{
		To:       &outerAddress,
		GasLimit: 500_000,
		GasPrice: big.NewInt(0),
	})
	s.Require().NoError(err)
	response, err := evmKeeper.ApplyTransaction(
		ctx.WithGasMeter(storetypes.NewGasMeter(1_000_000)),
		tx.GetMsgs()[0].(*evmtypes.MsgEthereumTx).AsTransaction(),
	)

	s.Require().NoError(err)
	s.Require().False(response.Failed())
	s.Require().Equal(1, factoryCalls)
	s.Require().Equal(1, txStarts)
	s.Require().Equal(1, txEnds)
	s.Require().GreaterOrEqual(globalEnters, 3)
	s.Require().NotNil(factoryManager)
	s.Require().NotNil(outerPrecompile.manager)
	s.Require().Same(factoryManager, outerPrecompile.manager)
	s.Require().Same(outerPrecompile.manager, sessionPrecompile.manager)
	s.Require().True(sessionPrecompile.installed)
	s.Require().Equal(1, sessionPrecompile.enterCalls)
}

// Exercise the production keeper boundary with real StateDB movements rather
// than treating OnEnter(value) as proof of a transfer.
func (s *KeeperTestSuite) TestCollectorActualMovementAndObservationRejection() {
	for _, reject := range []bool{false, true} {
		s.Run(fmt.Sprintf("reject=%v", reject), func() {
			s.SetupTest()
			ctx := s.Network.GetContext()
			keeper := s.Network.App.GetEVMKeeper()
			var collector vmglobaltracer.Collector
			rejection := errors.New("observation policy rejected")
			keeper.SetGlobalTracerFactories(func(factoryCtx sdk.Context, _ vmtracer.ExecutionInfo) (sdk.Context, vmtracer.Tracer) {
				collector = vmglobaltracer.NewCollector()
				if reject {
					_, err := collector.Observe(func(e vmtracer.Observation) error {
						if e.Kind == vmtracer.NativeTransfer {
							return rejection
						}
						return nil
					})
					s.Require().NoError(err)
				}
				return factoryCtx, collector
			})
			sender, recipient := s.Keyring.GetAddr(0), s.Keyring.GetAddr(1)
			before := keeper.GetBalance(ctx, recipient)
			tx, err := s.Factory.GenerateSignedEthTx(s.Keyring.GetPrivKey(0), evmtypes.EvmTxArgs{
				To:       &recipient,
				Amount:   big.NewInt(7),
				GasLimit: 100_000,
				GasPrice: big.NewInt(0),
			})
			s.Require().NoError(err)
			response, err := keeper.ApplyTransaction(ctx.WithGasMeter(storetypes.NewGasMeter(1_000_000)), tx.GetMsgs()[0].(*evmtypes.MsgEthereumTx).AsTransaction())
			s.Require().NotNil(collector)
			facts := collector.Observations()
			s.Require().NotEmpty(facts)
			transfers := nativeObservations(collector.Observations())
			s.Require().Len(transfers, 1)
			s.Require().Equal(sender, transfers[0].From)
			s.Require().Equal(recipient, transfers[0].To)
			s.Require().Equal(int64(7), transfers[0].Amount.Int64())
			balances := 0
			for _, e := range facts {
				if e.Kind == vmtracer.BalanceChange {
					balances++
				}
			}
			s.Require().Equal(2, balances)
			if reject {
				s.Require().ErrorIs(err, rejection)
				s.Require().Equal(before, keeper.GetBalance(ctx, recipient))
			} else {
				s.Require().NoError(err)
				s.Require().False(response.Failed())
				s.Require().NoError(collector.Error())
				s.Require().True(transfers[0].Status.Success)
			}
		})
	}
}

func (s *KeeperTestSuite) TestCollectorRetainsChildMovementAfterParentRevert() {
	s.SetupTest()
	ctx := s.Network.GetContext()
	keeper := s.Network.App.GetEVMKeeper()
	sender, recipient := s.Keyring.GetAddr(0), s.Keyring.GetAddr(1)
	parent := common.HexToAddress("0x000000000000000000000000000000000000eeee")
	// CALL recipient with value 7, consume its success flag, then REVERT.
	code := common.FromHex("0x6000600060006000600773")
	code = append(code, recipient.Bytes()...)
	code = append(code, common.FromHex("0x61fffff15060006000fd")...)
	db := statedb.New(ctx, keeper, statedb.NewEmptyTxConfig())
	db.SetCode(parent, code, tracing.CodeChangeUnspecified)
	s.Require().NoError(db.Commit())
	var collector vmglobaltracer.Collector
	keeper.SetGlobalTracerFactories(func(factoryCtx sdk.Context, _ vmtracer.ExecutionInfo) (sdk.Context, vmtracer.Tracer) {
		collector = vmglobaltracer.NewCollector()
		return factoryCtx, collector
	})
	before := keeper.GetBalance(ctx, recipient)
	tx, err := s.Factory.GenerateSignedEthTx(s.Keyring.GetPrivKey(0), evmtypes.EvmTxArgs{
		To:       &parent,
		Amount:   big.NewInt(9),
		GasLimit: 200_000,
		GasPrice: big.NewInt(0),
	})
	s.Require().NoError(err)
	response, err := keeper.ApplyTransaction(ctx.WithGasMeter(storetypes.NewGasMeter(1_000_000)), tx.GetMsgs()[0].(*evmtypes.MsgEthereumTx).AsTransaction())
	s.Require().NoError(err)
	s.Require().True(response.Failed())
	s.Require().NoError(collector.Error())
	transfers := nativeObservations(collector.Observations())
	s.Require().Len(transfers, 2)
	s.Require().Equal(sender, transfers[0].From)
	s.Require().Equal(parent, transfers[0].To)
	s.Require().Equal(parent, transfers[1].From)
	s.Require().Equal(recipient, transfers[1].To)
	s.Require().Equal(int64(7), transfers[1].Amount.Int64())
	s.Require().NotEqual(transfers[0].ObservationID, transfers[1].ObservationID)
	for _, transfer := range transfers {
		s.Require().True(transfer.Status.Reverted)
		s.Require().False(transfer.Status.Success)
	}
	childSucceeded := false
	for _, e := range collector.Observations() {
		if e.Kind == vmtracer.CallExit && e.CallID == transfers[1].CallID {
			s.Require().NoError(e.Err)
			s.Require().False(e.Reverted)
			s.Require().True(e.Status.Reverted)
			childSucceeded = true
		}
	}
	s.Require().True(childSucceeded)
	s.Require().Equal(before, keeper.GetBalance(ctx, recipient))
}

func (s *KeeperTestSuite) TestCollectorTokenRequestAndRawLog() {
	s.SetupTest()
	sender := s.Keyring.GetKey(0)
	recipient := s.Keyring.GetAddr(1)
	token, err := deployErc20Contract(sender, s.Factory)
	s.Require().NoError(err)
	s.Require().NoError(s.Network.NextBlock())
	var collector vmglobaltracer.Collector
	s.Network.App.GetEVMKeeper().SetGlobalTracerFactories(func(ctx sdk.Context, _ vmtracer.ExecutionInfo) (sdk.Context, vmtracer.Tracer) {
		collector = vmglobaltracer.NewCollector()
		return ctx, collector
	})
	_, err = executeTransferCall(transferParams{
		senderKey:     sender,
		contractAddr:  token,
		recipientAddr: recipient,
	}, s.Factory)
	s.Require().NoError(err)
	s.Require().NoError(collector.Error())
	var enter, request, emission *vmtracer.Observation
	for _, e := range collector.Observations() {
		switch e.Kind {
		case vmtracer.CallEnter:
			if e.Target == token {
				observation := e
				enter = &observation
			}
		case vmtracer.ERC20Call:
			observation := e
			request = &observation
		case vmtracer.Log:
			if len(e.Log.Topics) == 3 && e.Log.Address == token {
				observation := e
				emission = &observation
			}
		case vmtracer.NativeTransfer:
			s.FailNow("token log must not become a native transfer")
		}
	}
	s.Require().NotNil(enter)
	s.Require().NotNil(request)
	s.Require().NotNil(emission)
	s.Require().Equal(enter.CallID, request.CallID)
	s.Require().Equal(enter.CallID, emission.CallID)
	s.Require().Equal(enter.Input, request.Input)
	s.Require().Equal(sender.Addr, request.Caller)
	s.Require().Equal(token, request.Target)
	s.Require().Less(enter.ObservationID, request.ObservationID)
	s.Require().Less(request.ObservationID, emission.ObservationID)
	s.Require().Equal(common.BytesToHash(sender.Addr.Bytes()), emission.Log.Topics[1])
	s.Require().Equal(common.BytesToHash(recipient.Bytes()), emission.Log.Topics[2])
	s.Require().Equal(int64(1000), new(big.Int).SetBytes(emission.Log.Data).Int64())
	s.Require().True(emission.Status.Success)
}

func nativeObservations(events []vmtracer.Observation) []vmtracer.Observation {
	var out []vmtracer.Observation
	for _, e := range events {
		if e.Kind == vmtracer.NativeTransfer {
			out = append(out, e)
		}
	}
	return out
}

func (s *KeeperTestSuite) TestCollectorEnforcementCoverageAndFinalization() {
	for _, mode := range []string{"budget", "observer", "passive", "txend-reject", "txend-accept"} {
		s.Run(mode, func() {
			s.SetupTest()
			ctx := s.Network.GetContext()
			keeper := s.Network.App.GetEVMKeeper()
			recipient := common.HexToAddress("0x000000000000000000000000000000000000eeef")
			// Write 1 to storage slot 0, then STOP.
			db := statedb.New(ctx, keeper, statedb.NewEmptyTxConfig())
			db.SetCode(recipient, common.FromHex("0x600160005500"), tracing.CodeChangeUnspecified)
			s.Require().NoError(db.Commit())
			before := keeper.GetBalance(ctx, recipient)
			rejection := errors.New("final observation policy rejected")
			var collector vmglobaltracer.Collector
			var manager *vmtracer.Manager
			observed, ended := false, 0
			keeper.SetGlobalTracerFactories(func(factoryCtx sdk.Context, _ vmtracer.ExecutionInfo) (sdk.Context, vmtracer.Tracer) {
				manager, _ = vmtracer.FromContext(factoryCtx)
				s.Require().NotNil(manager)
				budget := uint64(100)
				if mode == "budget" || mode == "passive" {
					budget = 3
				}
				var err error
				collector, err = vmglobaltracer.NewCollectorWithBudget(budget)
				s.Require().NoError(err)
				if mode == "budget" || mode == "observer" {
					_, err = collector.Observe(func(e vmtracer.Observation) error {
						if e.Kind == vmtracer.NativeTransfer {
							observed = true
							return rejection
						}
						return nil
					})
					s.Require().NoError(err)
				}
				manager.InstallGlobal(vmsessiontracer.New(&tracing.Hooks{
					OnTxEnd: func(receipt *ethtypes.Receipt, _ error) {
						ended++
						s.Require().Positive(receipt.GasUsed)
						// No economic or storage effects may be committed yet.
						s.Require().Equal(before, keeper.GetBalance(ctx, recipient))
						s.Require().Equal(common.Hash{}, statedb.New(ctx, keeper, statedb.NewEmptyTxConfig()).GetState(recipient, common.Hash{}))
						if mode == "txend-reject" {
							manager.LatchError(rejection)
						}
					},
				}))
				return factoryCtx, collector
			})
			tx, err := s.Factory.GenerateSignedEthTx(s.Keyring.GetPrivKey(0), evmtypes.EvmTxArgs{
				To:       &recipient,
				Amount:   big.NewInt(7),
				GasLimit: 100_000,
				GasPrice: big.NewInt(0),
			})
			s.Require().NoError(err)
			response, err := keeper.ApplyTransaction(ctx.WithGasMeter(storetypes.NewGasMeter(1_000_000)), tx.GetMsgs()[0].(*evmtypes.MsgEthereumTx).AsTransaction())
			s.Require().Equal(1, ended)
			switch mode {
			case "budget":
				s.Require().ErrorContains(err, "budget")
				s.Require().False(observed)
			case "observer", "txend-reject":
				s.Require().ErrorIs(err, rejection)
			default:
				s.Require().NoError(err)
				s.Require().False(response.Failed())
			}
			after := statedb.New(ctx, keeper, statedb.NewEmptyTxConfig())
			if mode == "passive" || mode == "txend-accept" {
				s.Require().Equal(new(uint256.Int).Add(before, uint256.NewInt(7)), keeper.GetBalance(ctx, recipient))
				s.Require().Equal(common.BigToHash(big.NewInt(1)), after.GetState(recipient, common.Hash{}))
			} else {
				s.Require().Equal(before, keeper.GetBalance(ctx, recipient))
				s.Require().Equal(common.Hash{}, after.GetState(recipient, common.Hash{}))
			}
			if mode == "observer" {
				s.Require().True(observed)
			}
		})
	}
}

func (s *KeeperTestSuite) TestCallEVMChargesGasOnPolicyRejection() {
	for _, view := range []bool{false, true} {
		var acceptedGas uint64
		for _, reject := range []bool{false, true} {
			s.Run(fmt.Sprintf("view=%t/reject=%t", view, reject), func() {
				s.SetupTest()
				ctx := s.Network.GetContext().
					WithGasMeter(storetypes.NewGasMeter(1_000_000)).
					WithKVGasConfig(storetypes.GasConfig{}).
					WithTransientKVGasConfig(storetypes.GasConfig{})
				keeper := s.Network.App.GetEVMKeeper()
				sender := s.Keyring.GetAddr(0)
				recipient := common.HexToAddress("0x000000000000000000000000000000000000eeef")
				db := statedb.New(ctx, keeper, statedb.NewEmptyTxConfig())
				// Write 1 to storage slot 0, then STOP.
				db.SetCode(recipient, common.FromHex("0x600160005500"), tracing.CodeChangeUnspecified)
				s.Require().NoError(db.Commit())
				rejection := errors.New("final policy rejected")
				var executionGas uint64
				ends := 0
				keeper.SetGlobalTracerFactories(func(factoryCtx sdk.Context, _ vmtracer.ExecutionInfo) (sdk.Context, vmtracer.Tracer) {
					manager, ok := vmtracer.FromContext(factoryCtx)
					s.Require().True(ok)
					return factoryCtx, vmsessiontracer.New(&tracing.Hooks{
						OnTxEnd: func(receipt *ethtypes.Receipt, _ error) {
							ends++
							executionGas = receipt.GasUsed
							if reject {
								manager.LatchError(rejection)
							}
						},
					})
				})
				beforeGas := ctx.GasMeter().GasConsumed()
				var response *evmtypes.MsgEthereumTxResponse
				var err error
				if view {
					response, err = keeper.CallEVMViewWithData(ctx, sender, &recipient, nil, big.NewInt(100_000))
				} else {
					response, err = keeper.CallEVMWithData(ctx, statedb.New(ctx, keeper, statedb.NewEmptyTxConfig()), sender, &recipient, nil, true, false, big.NewInt(100_000))
				}
				s.Require().NotNil(response)
				s.Require().Equal(1, ends)
				s.Require().Positive(executionGas)
				s.Require().Equal(executionGas, response.GasUsed)
				s.Require().Equal(executionGas, ctx.GasMeter().GasConsumed()-beforeGas)
				if reject {
					s.Require().Equal(acceptedGas, executionGas)
					s.Require().ErrorIs(err, rejection)
					s.Require().Empty(response.Logs)
				} else {
					acceptedGas = executionGas
					s.Require().NoError(err)
				}
				expected := common.Hash{}
				if !view && !reject {
					expected = common.BigToHash(big.NewInt(1))
				}
				s.Require().Equal(expected, statedb.New(ctx, keeper, statedb.NewEmptyTxConfig()).GetState(recipient, common.Hash{}))
			})
		}
	}
}
