package statedb

import (
	"slices"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/cosmos/evm/x/vm/types"

	errorsmod "cosmossdk.io/errors"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

// ReconcileNativeBalances imports the current native view before resuming EVM
// execution. Callers must first export pending EVM changes before giving control
// to native code; importing before that export would overwrite EVM changes.
// Events are not a balance ledger: forwarding them must not apply a delta twice.
// With no addresses it reconciles every loaded object, as needed on SDK-to-EVM
// reentry. Supplying addresses restricts work to those already-loaded objects;
// cold objects read the live native view when first accessed.
func (s *StateDB) ReconcileNativeBalances(ctx sdk.Context, only ...common.Address) error {
	if ctx.MultiStore() != s.nativeContext().MultiStore() {
		return types.ErrNativeScopeContext
	}
	addresses := slices.Clone(only)
	if len(only) == 0 {
		addresses = make([]common.Address, 0, len(s.stateObjects))
		for address := range s.stateObjects {
			addresses = append(addresses, address)
		}
	}
	slices.SortFunc(addresses, func(a, b common.Address) int { return a.Cmp(b) })
	addresses = slices.Compact(addresses)
	for _, address := range addresses {
		object := s.stateObjects[address]
		if object == nil {
			continue
		}
		nativeBalance := s.keeper.GetBalance(ctx, address)
		if nativeBalance == nil {
			return errorsmod.Wrapf(types.ErrNativeBalance, "%s", address)
		}
		total := nativeBalance.ToBig()
		if locked := object.account.LockedBalanceSnapshot(); locked != nil {
			if locked.Sign() < 0 {
				return errorsmod.Wrapf(types.ErrNativeBalance, "%s", address)
			}
			total.Sub(total, locked)
		}
		if total.Sign() < 0 || total.BitLen() > 256 {
			return errorsmod.Wrapf(types.ErrNativeBalance, "%s", address)
		}
		balance := new(uint256.Int).SetBytes(total.Bytes())
		if !balance.Eq(object.Balance()) {
			s.journal.append(nativeBalanceChange{
				account: address,
				prev:    new(uint256.Int).Set(object.Balance()),
			})
			object.setBalance(balance)
		}
	}
	return nil
}

// Imported observations are reversible, but are not new EVM writes. Marking
// them dirty would re-export native-owned account metadata and module balances.
type nativeBalanceChange struct {
	account common.Address
	prev    *uint256.Int
}

func (change nativeBalanceChange) Revert(s *StateDB) {
	s.stateObjects[change.account].setBalance(change.prev)
}

func (nativeBalanceChange) Dirtied() *common.Address {
	return nil
}
