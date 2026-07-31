package filters

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/eth/filters"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/stretchr/testify/require"
)

func TestGetLogsDisabled(t *testing.T) {
	api := &PublicFilterAPI{}

	logs, err := api.GetLogs(context.Background(), filters.FilterCriteria{})

	require.Nil(t, logs)
	require.ErrorIs(t, err, errGetLogsDisabled)
	require.EqualError(t, err, "eth_getLogs is disabled")
}

func TestGetFilterLogsDisabled(t *testing.T) {
	api := &PublicFilterAPI{}

	logs, err := api.GetFilterLogs(context.Background(), rpc.ID("0x1"))

	require.Nil(t, logs)
	require.ErrorIs(t, err, errGetFilterLogsDisabled)
	require.EqualError(t, err, "eth_getFilterLogs is disabled")
}

func TestTimeoutLoop_PanicOnNilCancel(t *testing.T) {
	api := &PublicFilterAPI{
		filters:   make(map[rpc.ID]*filter),
		filtersMu: sync.Mutex{},
		deadline:  10 * time.Millisecond,
	}
	api.filters[rpc.NewID()] = &filter{
		typ:      filters.BlocksSubscription,
		deadline: time.NewTimer(0),
	}
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("cancel panic")
			}
			close(done)
		}()
		api.timeoutLoop()
	}()
	panicked := false
	select {
	case <-done:
		panicked = true
	case <-time.After(100 * time.Millisecond):
	}
	require.False(t, panicked)
}
