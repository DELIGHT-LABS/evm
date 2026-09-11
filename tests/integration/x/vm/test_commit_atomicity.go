package vm

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/holiman/uint256"

	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
)

func (s *KeeperTestSuite) TestStateDBCommitAtomicityRejectsModuleAccount() {
	s.SetupTest()

	ctx := s.Network.GetContext()
	accountKeeper := s.Network.App.GetAccountKeeper()
	evmKeeper := s.Network.App.GetEVMKeeper()

	moduleAccount := authtypes.NewEmptyModuleAccount("statedb-atomicity", authtypes.Minter)
	accountKeeper.NewAccount(ctx, moduleAccount)
	accountKeeper.SetAccount(ctx, moduleAccount)
	moduleAddr := common.BytesToAddress(moduleAccount.GetAddress().Bytes())

	// The zero address sorts before every non-zero module address, ensuring its
	// successful keeper write is staged before the module-account rejection.
	eoaAddr := common.Address{}
	s.Require().NotEqual(eoaAddr, moduleAddr)
	s.Require().True(evmKeeper.GetBalance(ctx, eoaAddr).IsZero())
	moduleBalanceBefore := evmKeeper.GetBalance(ctx, moduleAddr)

	db := s.StateDB()
	db.SetBalance(eoaAddr, uint256.NewInt(123), tracing.BalanceChangeUnspecified)
	db.SetBalance(moduleAddr, new(uint256.Int).Add(moduleBalanceBefore, uint256.NewInt(1)), tracing.BalanceChangeUnspecified)

	err := db.Commit()
	s.Require().ErrorContains(err, "is not allowed to receive funds")
	s.Require().True(evmKeeper.GetBalance(ctx, eoaAddr).IsZero())
	s.Require().Equal(moduleBalanceBefore, evmKeeper.GetBalance(ctx, moduleAddr))
	s.Require().NotNil(accountKeeper.GetAccount(ctx, sdk.AccAddress(moduleAddr.Bytes())))
}
