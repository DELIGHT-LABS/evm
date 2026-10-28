package tracer

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

// ExecutionInfo identifies one top-level EVM execution.
type ExecutionInfo struct {
	Message core.Message
	TxHash  common.Hash
	TxIndex uint64
	Commit  bool
}

// GlobalFactory creates a tracer for one EVM execution. Applications can also
// return a derived context to expose per-execution tracer state to their post-tx
// hooks. A new tracer is created for every execution.
type GlobalFactory func(ctx sdk.Context, execution ExecutionInfo) (sdk.Context, Tracer)

// InstallGlobal installs a tracer that observes the whole EVM execution.
// Global tracers normally live until the Manager is discarded.
func (m *Manager) InstallGlobal(tracer Tracer) func() {
	return m.install(tracer, false)
}
