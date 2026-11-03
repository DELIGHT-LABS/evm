package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http/httptest"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/stretchr/testify/mock"

	"github.com/cosmos/evm/crypto/ethsecp256k1"
	precompilecommon "github.com/cosmos/evm/precompiles/common"
	"github.com/cosmos/evm/rpc/backend/mocks"
	ethapi "github.com/cosmos/evm/rpc/namespaces/ethereum/eth"
	rpctypes "github.com/cosmos/evm/rpc/types"
	evmtypes "github.com/cosmos/evm/x/vm/types"

	"cosmossdk.io/log/v2"
	"cosmossdk.io/math"

	"github.com/cosmos/cosmos-sdk/crypto"
)

func (s *TestSuite) TestSendTransactionRejectionHTTP() {
	priv, err := ethsecp256k1.GenerateKey()
	s.Require().NoError(err)
	from := common.BytesToAddress(priv.PubKey().Address())
	to := common.HexToAddress("0x1234")
	gas, nonce := hexutil.Uint64(100000), hexutil.Uint64(1)
	args := evmtypes.TransactionArgs{
		From: &from, To: &to, Gas: &gas, Nonce: &nonce,
		GasPrice: new(hexutil.Big),
	}
	buildBroadcastTx(s, priv, math.NewInt(1), args)
	cause := fmt.Errorf("pool rejected: %w", core.ErrInsufficientFunds)
	RegisterMempoolInsert(s.T(), s.Mempool(), nil, cause)

	_, goErr := s.backend.SendTransaction(s.Ctx(), args)
	s.Require().ErrorIs(goErr, cause)
	var rejected *rpctypes.TxRejectError
	s.Require().ErrorAs(goErr, &rejected)
	s.Require().Same(cause, rejected.Unwrap())

	response := s.transactionRPCResponse("eth_sendTransaction", args)
	s.Require().NotNil(response.Error)
	definition := precompilecommon.SharedErrorABI.Errors[precompilecommon.SolidityErrSDKInsufficientFunds]
	s.Require().Equal(-32000, response.Error.Code)
	s.Require().Equal(cause.Error(), response.Error.Message)
	s.Require().Equal(hexutil.Encode(definition.ID[:4]), response.Error.Data)
}

func (s *TestSuite) TestSendTransactionChainIDRejectionHTTP() {
	priv, err := ethsecp256k1.GenerateKey()
	s.Require().NoError(err)
	from := common.BytesToAddress(priv.PubKey().Address())
	to := common.HexToAddress("0x1234")
	gas, nonce := hexutil.Uint64(100000), hexutil.Uint64(1)
	args := evmtypes.TransactionArgs{
		From: &from, To: &to, Gas: &gas, Nonce: &nonce,
		GasPrice: new(hexutil.Big),
	}
	armor := crypto.EncryptArmorPrivKey(priv, "", "eth_secp256k1")
	s.Require().NoError(s.backend.ClientCtx.Keyring.ImportPrivKey("test_key", armor, ""))
	expected := new(big.Int).Set(s.backend.EvmChainID)
	actual := new(big.Int).Add(expected, big.NewInt(1))
	args.ChainID = (*hexutil.Big)(actual)

	_, goErr := s.backend.SendTransaction(s.Ctx(), args)
	var cause *evmtypes.ChainIDMismatchError
	s.Require().ErrorAs(goErr, &cause)
	s.Require().Equal(expected, cause.Expected)
	s.Require().Equal(actual, cause.Actual)
	s.Require().EqualError(goErr, cause.Error())

	response := s.transactionRPCResponse("eth_sendTransaction", args)
	s.Require().NotNil(response.Error)
	s.Require().Equal(-32000, response.Error.Code)
	s.Require().Equal(cause.Error(), response.Error.Message)
	definition := precompilecommon.SharedErrorABI.Errors[precompilecommon.SolidityErrChainIdMismatch]
	data, err := hexutil.Decode(response.Error.Data)
	s.Require().NoError(err)
	s.Require().GreaterOrEqual(len(data), 4)
	s.Require().Equal(definition.ID[:4], data[:4])
	decoded, err := definition.Inputs.Unpack(data[4:])
	s.Require().NoError(err)
	s.Require().Equal([]interface{}{expected, actual}, decoded)
	s.Mempool().AssertNotCalled(s.T(), "Insert", mock.Anything, mock.Anything)
}

func (s *TestSuite) TestFillTransactionRejectionHTTP() {
	priv, err := ethsecp256k1.GenerateKey()
	s.Require().NoError(err)
	from := common.BytesToAddress(priv.PubKey().Address())
	to := common.HexToAddress("0x1234")
	gas, nonce := hexutil.Uint64(100000), hexutil.Uint64(999)
	args := evmtypes.TransactionArgs{
		From: &from, To: &to, Gas: &gas, Nonce: &nonce,
		GasPrice: (*hexutil.Big)(big.NewInt(1)),
	}
	cometClient, _ := buildBroadcastTx(s, priv, math.NewInt(1), args)
	height := int64(1)
	RegisterHeader(cometClient, &height, nil)
	cause := fmt.Errorf("estimate rejected: %w", core.ErrInsufficientFundsForTransfer)
	queryClient := s.backend.QueryClient.QueryClient.(*mocks.EVMQueryClient)
	queryClient.On("EstimateGas", mock.Anything, mock.Anything).Return(nil, cause).Once()

	args.Gas = nil
	response := s.transactionRPCResponse("eth_fillTransaction", args)
	s.Require().NotNil(response.Error)
	definition := precompilecommon.SharedErrorABI.Errors[precompilecommon.SolidityErrSDKInsufficientFunds]
	s.Require().Equal(-32000, response.Error.Code)
	s.Require().Equal(cause.Error(), response.Error.Message)
	s.Require().Equal(hexutil.Encode(definition.ID[:4]), response.Error.Data)

	// Supplying gas still bypasses estimation and does not perform nonce admission.
	args.Gas = &gas
	response = s.transactionRPCResponse("eth_fillTransaction", args)
	s.Require().Nil(response.Error)
	var filled rpctypes.SignTransactionResult
	s.Require().NoError(json.Unmarshal(response.Result, &filled))
	s.Require().Equal(uint64(nonce), filled.Tx.Nonce())
	s.Require().Equal(uint64(gas), filled.Tx.Gas())
	queryClient.AssertNumberOfCalls(s.T(), "EstimateGas", 1)
	s.Mempool().AssertNotCalled(s.T(), "Insert", mock.Anything, mock.Anything)
}

type transactionRPCResult struct {
	Result json.RawMessage
	Error  *struct {
		Code    int
		Message string
		Data    string
	}
}

func (s *TestSuite) transactionRPCResponse(method string, args evmtypes.TransactionArgs) transactionRPCResult {
	s.T().Helper()
	server := rpc.NewServer()
	defer server.Stop()
	s.Require().NoError(server.RegisterName("eth", ethapi.NewPublicAPI(log.NewNopLogger(), s.backend)))
	payload, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": []interface{}{args},
	})
	s.Require().NoError(err)
	req := httptest.NewRequest("POST", "/", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	s.Require().Equal(200, recorder.Code)
	var response transactionRPCResult
	s.Require().NoError(json.Unmarshal(recorder.Body.Bytes(), &response))
	return response
}
