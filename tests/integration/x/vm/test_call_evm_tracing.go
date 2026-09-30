package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	gethvm "github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/tracers"
	_ "github.com/ethereum/go-ethereum/eth/tracers/native"
	"github.com/holiman/uint256"

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
		uninstall, ok := vmsessiontracer.Install(ctx, vmsessiontracer.New(&tracing.Hooks{
			OnEnter: func(depth int, _ byte, _, _ common.Address, _ []byte, _ uint64, _ *big.Int) {
				p.enterCalls++
				p.enterDepths = append(p.enterDepths, depth)
			},
		}))
		p.installed = ok
		if !ok {
			return nil, fmt.Errorf("session tracer manager is unavailable")
		}
		defer uninstall()

		_, _, err := evm.Call(contract.Address(), p.target, nil, contract.Gas, uint256.NewInt(0))
		p.activeAfterDirectCall = p.manager.HasActiveFrame()
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
	s.Require().False(manager.HasActiveFrame())
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
		{To: innerAddress, Calls: []callFrame{{To: target}, {To: target}}},
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
	s.Require().Len(collectors[0].Touches(), 1)
	s.Require().Equal(recipient, collectors[0].Touches()[0].To)
	s.Require().NotEmpty(collectors[1].Touches())
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
