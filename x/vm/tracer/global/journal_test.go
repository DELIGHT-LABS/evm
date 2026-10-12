package global

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/stretchr/testify/require"

	vmtracer "github.com/cosmos/evm/x/vm/tracer"
	vmsession "github.com/cosmos/evm/x/vm/tracer/session"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

func rawManager(t *testing.T, budget uint64) (*vmtracer.Manager, *txTraceCollector) {
	t.Helper()
	collector, err := NewCollectorWithBudget(budget)
	require.NoError(t, err)
	c := collector.(*txTraceCollector)
	m := vmtracer.New(nil)
	m.InstallGlobal(c)
	m.SetCodeReader(func(common.Address) []byte { return nil })
	return m, c
}

func TestObservationIdentitySessionAndAncestorRollback(t *testing.T) {
	m, collector := rawManager(t, 100)
	a, b := common.HexToAddress("0xaa"), common.HexToAddress("0xbb")
	var global, local []vmtracer.Observation
	_, err := collector.Observe(func(e vmtracer.Observation) error { global = append(global, e); return nil })
	require.NoError(t, err)
	m.Hooks().OnEnter(0, byte(vm.CALL), a, b, nil, 100, big.NewInt(1))
	viewRange, err := vmsession.Begin(vmtracer.WithManager(sdk.Context{}.WithContext(context.Background()), m))
	require.NoError(t, err)
	stop, err := collector.Observe(func(e vmtracer.Observation) error { local = append(local, e); return nil })
	require.NoError(t, err)
	m.NestedHooks().OnEnter(0, byte(vm.CALL), b, a, nil, 100, big.NewInt(3))
	m.RecordNativeTransfer(b, a, big.NewInt(3))
	m.RecordNativeTransfer(b, a, big.NewInt(3))
	m.Hooks().OnLog(&ethtypes.Log{
		Address: a,
		Data:    []byte{1},
	})
	m.NestedHooks().OnExit(0, nil, 10, nil, false)
	viewRange.Close()
	stop()
	stop()
	require.False(t, collector.status(2).Reverted)
	m.Hooks().OnExit(0, nil, 10, errors.New("parent revert"), true)
	require.True(t, collector.status(2).Reverted)
	require.Len(t, collector.Observations(), 7)
	require.Equal(t, global[1:6], local)
	view := viewRange.Observations()
	require.Len(t, view, len(local))
	for i := range local {
		require.Equal(t, local[i].OccurrenceID, view[i].OccurrenceID)
		require.True(t, view[i].Status.Reverted)
	}
	require.Equal(t, uint64(1), viewRange.FrameID)
	require.Equal(t, uint64(2), local[0].FrameID)
	require.Equal(t, uint64(1), local[0].ParentFrameID)
	require.NotEqual(t, local[1].OccurrenceID, local[2].OccurrenceID)
	local[1].Amount.SetInt64(999)
	local[3].Log.Data[0] = 99
	require.Equal(t, int64(3), collector.Observations()[2].Amount.Int64())
	require.Equal(t, byte(1), collector.Observations()[4].Log.Data[0])
	require.NoError(t, m.ExecutionError())
}

func TestObservationRequestsAreNotMovements(t *testing.T) {
	m, collector := rawManager(t, 100)
	a, b := common.HexToAddress("0xaa"), common.HexToAddress("0xbb")
	input := make([]byte, 68)
	copy(input, []byte{0xa9, 5, 0x9c, 0xbb})
	input[35] = 1
	input[67] = 7
	m.Hooks().OnEnter(0, byte(vm.CALL), a, b, input, 100, big.NewInt(5))
	m.Hooks().OnExit(0, nil, 1, errors.New("insufficient funds"), false)
	facts := collector.Observations()
	require.Len(t, facts, 3)
	require.Equal(t, vmtracer.ERC20Call, facts[1].Kind)
	require.NotEqual(t, facts[0].OccurrenceID, facts[1].OccurrenceID)
	require.Equal(t, input, facts[0].Input)
	require.Empty(t, nativeRecords(collector.Observations()))
	for _, op := range []vm.OpCode{vm.DELEGATECALL, vm.CALLCODE, vm.STATICCALL} {
		m.Hooks().OnEnter(0, byte(op), a, b, nil, 100, big.NewInt(5))
		m.Hooks().OnExit(0, nil, 1, nil, false)
	}
	for _, e := range collector.Observations() {
		require.NotEqual(t, vmtracer.NativeTransfer, e.Kind)
	}
	require.NoError(t, m.ExecutionError())
}

func TestObservationLatchSurvivesCaughtRevertAndBudget(t *testing.T) {
	m, collector := rawManager(t, 3)
	violation := errors.New("policy violation")
	_, err := collector.Observe(func(e vmtracer.Observation) error {
		if e.FrameID == 2 {
			return violation
		}
		return nil
	})
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		m.Hooks().OnEnter(i, byte(vm.CALL), common.Address{}, common.Address{}, nil, 0, nil)
	}
	m.Hooks().OnExit(1, nil, 0, violation, true)
	m.Hooks().OnExit(0, nil, 0, nil, false)
	require.ErrorIs(t, m.ExecutionError(), violation)
	require.Len(t, collector.Observations(), 3)
	n, limited := rawManager(t, 1)
	n.Hooks().OnEnter(0, byte(vm.CALL), common.Address{}, common.Address{}, nil, 0, nil)
	n.Hooks().OnExit(0, nil, 0, nil, false)
	require.ErrorContains(t, limited.Error(), "budget")
	require.NoError(t, n.ExecutionError())
}

func TestObservationCodeAddress(t *testing.T) {
	m, c := rawManager(t, 100)
	sender, impl := common.HexToAddress("0xaa"), common.HexToAddress("0xbb")
	m.SetCodeReader(func(addr common.Address) []byte {
		if addr == sender {
			return ethtypes.AddressToDelegation(impl)
		}
		return nil
	})
	m.Hooks().OnEnter(0, byte(vm.CALL), common.Address{}, sender, nil, 100, nil)
	m.Hooks().OnEnter(1, byte(vm.DELEGATECALL), sender, impl, nil, 100, nil)
	for _, e := range c.Observations() {
		require.Equal(t, sender, e.ExecutionAddress)
		require.Equal(t, impl, e.CodeAddress)
		require.True(t, e.CodeAddressKnown)
		require.Nil(t, e.UserOperation)
	}
	m.Hooks().OnExit(1, nil, 0, nil, false)
	m.Hooks().OnExit(0, nil, 0, nil, false)
	require.NoError(t, c.Error())
}

func TestMalformedCallRemainsRaw(t *testing.T) {
	m, c := rawManager(t, 100)
	input := []byte{0xa9, 5, 0x9c, 0xbb}
	m.Hooks().OnEnter(0, byte(vm.CALL), common.Address{}, common.Address{}, input, 100, nil)
	m.Hooks().OnExit(0, nil, 0, nil, false)
	require.Len(t, c.Observations(), 3)
	require.Equal(t, input, c.Observations()[1].Input)
	require.Equal(t, vmtracer.ERC20Call, c.Observations()[1].Kind)
	require.Empty(t, nativeRecords(c.Observations()))
	require.NoError(t, c.Error())
	require.NoError(t, m.ExecutionError())
}

func TestManagerReferencesGlobalJournal(t *testing.T) {
	m, c := rawManager(t, 100)
	require.Same(t, c, m.Journal())
	m.Hooks().OnEnter(0, byte(vm.CALL), common.Address{}, common.Address{}, nil, 100, nil)
	require.Len(t, c.Observations(), 1)
	require.Len(t, m.Journal().Observations(), 1)
	m.Hooks().OnExit(0, nil, 0, nil, true)
	require.True(t, c.Observations()[0].Status.Reverted)
	m.InstallGlobal(NewCollector())
	require.ErrorContains(t, m.ExecutionError(), "once before execution")
}

func TestCodeReaderRestoresAfterNestedExecution(t *testing.T) {
	m, collector := rawManager(t, 100)
	target := common.HexToAddress("0xaa")
	impl := common.HexToAddress("0xbb")
	restore := m.SetCodeReader(func(common.Address) []byte { return ethtypes.AddressToDelegation(impl) })
	m.Hooks().OnEnter(0, byte(vm.CALL), common.Address{}, target, nil, 100, nil)
	require.Equal(t, impl, collector.Observations()[0].CodeAddress)
	nestedRestore := m.SetCodeReader(func(common.Address) []byte { return nil })
	m.NestedHooks().OnEnter(0, byte(vm.CALL), target, target, nil, 100, nil)
	require.Equal(t, target, collector.Observations()[1].CodeAddress)
	m.NestedHooks().OnExit(0, nil, 0, nil, false)
	nestedRestore()
	m.Hooks().OnEnter(1, byte(vm.CALL), target, target, nil, 100, nil)
	require.Equal(t, impl, collector.Observations()[3].CodeAddress)
	m.Hooks().OnExit(1, nil, 0, nil, false)
	m.Hooks().OnExit(0, nil, 0, nil, false)
	restore()
	require.NoError(t, m.ExecutionError())
}

func TestSessionRejectsJournalWithoutRejectingExecution(t *testing.T) {
	m, collector := rawManager(t, 100)
	m.Hooks().OnEnter(0, byte(vm.CALL), common.Address{}, common.Address{}, nil, 100, nil)
	second := NewCollector()
	cleanup, err := m.InstallSession(second)
	require.ErrorContains(t, err, "session cannot install an execution journal")
	cleanup()
	cleanup()
	require.Same(t, collector, m.Journal())
	require.NoError(t, m.ExecutionError())
	m.Hooks().OnExit(0, nil, 0, nil, false)
	require.Len(t, collector.Observations(), 2)
	require.Empty(t, second.Observations())
	require.True(t, collector.Observations()[0].Status.Success)
	require.NoError(t, m.ExecutionError())

	// A journal cannot be installed as a session even without a global journal.
	empty := vmtracer.New(nil)
	cleanup, err = empty.InstallSession(NewCollector())
	require.Error(t, err)
	cleanup()
	require.Nil(t, empty.Journal())
	require.NoError(t, empty.ExecutionError())
}

func TestSessionBeginRequiresActiveFrame(t *testing.T) {
	m, _ := rawManager(t, 100)
	ctx := vmtracer.WithManager(sdk.Context{}.WithContext(context.Background()), m)
	_, err := vmsession.Begin(ctx)
	require.ErrorContains(t, err, "active execution frame")
	m.Hooks().OnEnter(0, byte(vm.CALL), common.Address{}, common.Address{}, nil, 100, nil)
	view, err := vmsession.Begin(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(1), view.FrameID)
	m.Hooks().OnExit(0, nil, 0, nil, false)
	view.Close()
	_, err = vmsession.Begin(ctx)
	require.ErrorContains(t, err, "active execution frame")
}
