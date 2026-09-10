package types_test

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/evm/x/vm/types"
)

func TestTransactionArgsPreserveAccessListWithGasPrice(t *testing.T) {
	to := common.HexToAddress("0x0000000000000000000000000000000000000042")
	chainID := hexutil.Big(*big.NewInt(1))
	nonce := hexutil.Uint64(1)
	gas := hexutil.Uint64(50_000)
	gasPrice := hexutil.Big(*big.NewInt(1))
	value := hexutil.Big(*big.NewInt(0))
	accessList := ethtypes.AccessList{{
		Address:     to,
		StorageKeys: []common.Hash{common.HexToHash("0x01")},
	}}
	args := types.TransactionArgs{
		To:         &to,
		Gas:        &gas,
		GasPrice:   &gasPrice,
		Value:      &value,
		Nonce:      &nonce,
		AccessList: &accessList,
		ChainID:    &chainID,
	}

	tx := args.ToTransaction(ethtypes.LegacyTxType)

	require.Equal(t, uint8(ethtypes.AccessListTxType), tx.Type())
	require.Equal(t, accessList, tx.AccessList())

	args.AccessList = nil
	require.Equal(t, uint8(ethtypes.LegacyTxType), args.ToTransaction(ethtypes.LegacyTxType).Type())
}
