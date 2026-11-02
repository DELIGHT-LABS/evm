package common

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"

	precisebanktypes "github.com/cosmos/evm/x/precisebank/types"
	"github.com/cosmos/evm/x/vm/statedb"

	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
)

// BalanceHandlerFactory is a factory struct to create BalanceHandler instances.
type BalanceHandlerFactory struct {
	bankKeeper BankKeeper
}

// NewBalanceHandler creates a new BalanceHandler instance.
func NewBalanceHandlerFactory(bankKeeper BankKeeper) *BalanceHandlerFactory {
	return &BalanceHandlerFactory{
		bankKeeper: bankKeeper,
	}
}

func (bhf BalanceHandlerFactory) NewBalanceHandler() *BalanceHandler {
	return &BalanceHandler{
		bankKeeper:    bhf.bankKeeper,
		prevEventsLen: 0,
	}
}

// BalanceHandler is a struct that handles balance changes in the Cosmos SDK context.
type BalanceHandler struct {
	bankKeeper    BankKeeper
	prevEventsLen int
}

// BeforeBalanceChange is called before any balance changes by precompile methods.
// It records the current number of events in the context to later process balance changes
// using the recorded events.
func (bh *BalanceHandler) BeforeBalanceChange(ctx sdk.Context) {
	bh.prevEventsLen = len(ctx.EventManager().Events())
}

// AfterBalanceChange validates balance events and imports loaded accounts'
// absolute balances from the active native view.
// Forwarded events can be observed repeatedly without applying a delta twice.
//
// NOTES: Balance change events involving BlockedAddresses are bypassed.
// Native balances are handled separately to prevent cases where a bank coin transfer
// initiated by a precompile is unintentionally overwritten by balance changes from within a contract.
// Typically, accounts registered as BlockedAddresses in app.go—such as module accounts—are not expected to receive coins.
// However, in modules like precisebank, it is common to borrow and repay integer balances
// from the module account to support fractional balance handling.
//
// As a result, even if a module account is marked as a BlockedAddress, a keeper-level SendCoins operation
// can emit an x/bank event in which the module account appears as a spender or receiver.
// If such events are parsed and used to invoke StateDB.AddBalance or StateDB.SubBalance, authorization errors can occur.
//
// To prevent this, balance changes from events involving blocked addresses are not applied to the StateDB.
// Instead, the state changes resulting from the precompile call are applied directly via the MultiStore.
func (bh *BalanceHandler) AfterBalanceChange(ctx sdk.Context, stateDB *statedb.StateDB) error {
	events := ctx.EventManager().Events()
	var addresses []common.Address

	for _, event := range events[bh.prevEventsLen:] {
		switch event.Type {
		case banktypes.EventTypeCoinSpent:
			spenderAddr, err := ParseAddress(event, banktypes.AttributeKeySpender)
			if err != nil {
				return fmt.Errorf("failed to parse spender address from event %q: %w", banktypes.EventTypeCoinSpent, err)
			}
			if len(spenderAddr) != common.AddressLength || bh.bankKeeper.BlockedAddr(spenderAddr) {
				// Bypass blocked addresses
				continue
			}

			amount, err := ParseAmount(event)
			if err != nil {
				return fmt.Errorf("failed to parse amount from event %q: %w", banktypes.EventTypeCoinSpent, err)
			}
			if amount.IsZero() {
				continue
			}

			addresses = append(addresses, common.BytesToAddress(spenderAddr.Bytes()))

		case banktypes.EventTypeCoinReceived:
			receiverAddr, err := ParseAddress(event, banktypes.AttributeKeyReceiver)
			if err != nil {
				return fmt.Errorf("failed to parse receiver address from event %q: %w", banktypes.EventTypeCoinReceived, err)
			}
			if len(receiverAddr) != common.AddressLength || bh.bankKeeper.BlockedAddr(receiverAddr) {
				// Bypass blocked addresses
				continue
			}

			amount, err := ParseAmount(event)
			if err != nil {
				return fmt.Errorf("failed to parse amount from event %q: %w", banktypes.EventTypeCoinReceived, err)
			}
			if amount.IsZero() {
				continue
			}

			addresses = append(addresses, common.BytesToAddress(receiverAddr.Bytes()))

		case precisebanktypes.EventTypeFractionalBalanceChange:
			addr, err := ParseAddress(event, precisebanktypes.AttributeKeyAddress)
			if err != nil {
				return fmt.Errorf("failed to parse address from event %q: %w", precisebanktypes.EventTypeFractionalBalanceChange, err)
			}
			if len(addr) != common.AddressLength || bh.bankKeeper.BlockedAddr(addr) {
				// Bypass blocked addresses
				continue
			}

			delta, err := ParseFractionalAmount(event)
			if err != nil {
				return fmt.Errorf("failed to parse amount from event %q: %w", precisebanktypes.EventTypeFractionalBalanceChange, err)
			}

			if delta.BitLen() > 256 {
				return fmt.Errorf("fractional balance delta exceeds uint256")
			}
			if delta.Sign() == 0 {
				continue
			}
			addresses = append(addresses, common.BytesToAddress(addr.Bytes()))

		default:
			continue
		}
	}
	if len(addresses) != 0 {
		if err := stateDB.ReconcileNativeBalances(ctx, addresses...); err != nil {
			return err
		}
	}
	// Keep the legacy observation API consistent; balance correctness no
	// longer depends on event-manager-local indices surviving forwarding.
	for i := range events {
		if !stateDB.IsEventProcessed(i) {
			stateDB.MarkEventProcessed(i)
		}
	}

	return nil
}
