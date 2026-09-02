package vm

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	gethvm "github.com/ethereum/go-ethereum/core/vm"
	"github.com/holiman/uint256"

	precompilecommon "github.com/cosmos/evm/precompiles/common"
	"github.com/cosmos/evm/x/erc20/types"
	vmkeeper "github.com/cosmos/evm/x/vm/keeper"
	"github.com/cosmos/evm/x/vm/statedb"
	vmtracer "github.com/cosmos/evm/x/vm/tracer"
	vmsessiontracer "github.com/cosmos/evm/x/vm/tracer/session"
	evmtypes "github.com/cosmos/evm/x/vm/types"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

type sessionTracerPrecompile struct {
	precompilecommon.Precompile
	target     common.Address
	manager    *vmtracer.Manager
	installed  bool
	enterCalls int
}

func (p *sessionTracerPrecompile) Name() string { return "session-tracer-test" }

func (p *sessionTracerPrecompile) RequiredGas(_ []byte) uint64 { return 0 }

func (p *sessionTracerPrecompile) Run(evm *gethvm.EVM, contract *gethvm.Contract, _ bool) ([]byte, error) {
	return p.RunNativeAction(evm, contract, func(ctx sdk.Context) ([]byte, error) {
		p.manager, _ = vmtracer.FromContext(ctx)
		uninstall, ok := vmsessiontracer.Install(ctx, vmsessiontracer.New(&tracing.Hooks{
			OnEnter: func(_ int, _ byte, _, _ common.Address, _ []byte, _ uint64, _ *big.Int) {
				p.enterCalls++
			},
		}))
		p.installed = ok
		if !ok {
			return nil, fmt.Errorf("session tracer manager is unavailable")
		}
		defer uninstall()

		_, _, err := evm.Call(contract.Address(), p.target, nil, contract.Gas, uint256.NewInt(0))
		return []byte{1}, err
	})
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
		existingManager bool
	}{
		{
			name: "new manager without global factories",
		},
		{
			name:            "new manager with initialized StateDB cache",
			initializeCache: true,
		},
		{
			name:            "reuse existing manager",
			existingManager: true,
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

			var expectedManager *vmtracer.Manager
			factoryCalls := 0
			if tc.existingManager {
				expectedManager = vmtracer.New(nil)
				ctx = vmtracer.WithManager(ctx, expectedManager)
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
			if tc.existingManager {
				s.Require().Same(expectedManager, precompile.manager)
				s.Require().Zero(factoryCalls)
			}
		})
	}
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
