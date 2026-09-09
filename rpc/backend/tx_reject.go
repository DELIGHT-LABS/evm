package backend

import (
	"github.com/cosmos/evm/mempool"
	"github.com/cosmos/evm/mempool/txpool"
	common "github.com/cosmos/evm/precompiles/common"
	rpctypes "github.com/cosmos/evm/rpc/types"
)

// mempoolErrors holds backend-specific mappings. Shared Geth and Cosmos error
// conversion lives in rpc/types so query handlers can use it independently.
var mempoolErrors = common.MustNewErrorRegistry(common.SharedErrorABI,
	common.NewErrorMapping(mempool.ErrNonceLow, common.SolidityErrNonceTooLow),
	common.NewErrorMapping(mempool.ErrNonceGap, common.SolidityErrNonceGap),
	common.NewErrorMapping(txpool.ErrTxGasPriceTooLow, common.SolidityErrGasPriceTooLow),
	common.NewErrorMapping(txpool.ErrGasLimit, common.SolidityErrGasLimitExceeded),
	common.NewErrorMapping(txpool.ErrInvalidSender, common.SolidityErrInvalidSender),
)

// resolveTxRejectError normalizes failures at the backend's final RPC boundary,
// adding mempool mappings to the shared resolver. Validation and acceptance
// decisions remain in the existing transaction execution paths.
func resolveTxRejectError(original error) error {
	return rpctypes.ResolveTxRejectError(original, mempoolErrors.Translate)
}
