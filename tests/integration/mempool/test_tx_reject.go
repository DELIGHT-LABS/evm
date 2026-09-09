package mempool

import (
	"bytes"
	"encoding/json"
	"math/big"
	"net/http/httptest"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"

	evmmempool "github.com/cosmos/evm/mempool"
	precompilecommon "github.com/cosmos/evm/precompiles/common"
	rpcbackend "github.com/cosmos/evm/rpc/backend"
	evmtypes "github.com/cosmos/evm/x/vm/types"

	"cosmossdk.io/log/v2"

	"github.com/cosmos/cosmos-sdk/client"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

func (s *IntegrationTestSuite) TestTxRejectHTTPWithRealMempool() {
	realMempool, ok := s.network.App.GetMempool().(rpcbackend.Mempool)
	s.Require().True(ok)

	backend := &rpcbackend.Backend{
		ClientCtx:  client.Context{}.WithTxConfig(s.network.GetEncodingConfig().TxConfig),
		Logger:     log.NewNopLogger(),
		EvmChainID: s.network.GetEIP155ChainID(),
		Mempool:    realMempool,
	}
	server := rpc.NewServer()
	s.T().Cleanup(server.Stop)
	s.Require().NoError(server.RegisterName("eth", backend))

	unfundedIndex := s.keyring.AddKey()
	unfunded := s.createEVMValueTransferTx(s.keyring.GetKey(unfundedIndex), 0, big.NewInt(1_000_000_000))
	rejection := sendRawTransactionHTTP(s, server, rawEthereumTransaction(s, unfunded))
	s.Require().Contains(rejection, "error")

	var rpcErr struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    string `json:"data"`
	}
	s.Require().NoError(json.Unmarshal(rejection["error"], &rpcErr))
	s.Require().Equal(-32000, rpcErr.Code)
	s.Require().NotEmpty(rpcErr.Message)
	insufficientFunds := precompilecommon.SharedErrorABI.Errors[precompilecommon.SolidityErrSDKInsufficientFunds]
	s.Require().Equal(hexutil.Encode(insufficientFunds.ID[:4]), rpcErr.Data)

	fundedKey := s.keyring.GetKey(0)
	future := s.createEVMValueTransferTx(fundedKey, 2, big.NewInt(1_000_000_000))
	rawFuture := rawEthereumTransaction(s, future)
	accepted := sendRawTransactionHTTP(s, server, rawFuture)
	s.Require().NotContains(accepted, "error")

	futureMsg := future.GetMsgs()[0].(*evmtypes.MsgEthereumTx)
	var result string
	s.Require().NoError(json.Unmarshal(accepted["result"], &result))
	s.Require().Equal(futureMsg.AsTransaction().Hash().Hex(), result)

	evmMempool, ok := realMempool.(*evmmempool.Mempool)
	s.Require().True(ok)
	pool := evmMempool.GetTxPool()
	pending, queued := pool.ContentFrom(fundedKey.Addr)
	s.Require().Empty(pending)
	s.Require().Len(queued, 1)
	s.Require().Equal(uint64(2), queued[0].Nonce())
}

func rawEthereumTransaction(s *IntegrationTestSuite, tx sdk.Tx) []byte {
	s.T().Helper()
	msg, ok := tx.GetMsgs()[0].(*evmtypes.MsgEthereumTx)
	s.Require().True(ok)
	raw, err := msg.AsTransaction().MarshalBinary()
	s.Require().NoError(err)
	return raw
}

func sendRawTransactionHTTP(s *IntegrationTestSuite, server *rpc.Server, raw []byte) map[string]json.RawMessage {
	s.T().Helper()
	payload, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_sendRawTransaction",
		"params":  []interface{}{hexutil.Encode(raw)},
	})
	s.Require().NoError(err)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(recorder, request)
	s.Require().Equal(200, recorder.Code)

	var response map[string]json.RawMessage
	s.Require().NoError(json.Unmarshal(recorder.Body.Bytes(), &response))
	return response
}
