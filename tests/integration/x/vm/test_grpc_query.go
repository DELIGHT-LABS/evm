package vm

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/tracing"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	ethlogger "github.com/ethereum/go-ethereum/eth/tracers/logger"
	ethparams "github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	rpctypes "github.com/cosmos/evm/rpc/types"
	"github.com/cosmos/evm/server/config"
	testconstants "github.com/cosmos/evm/testutil/constants"
	"github.com/cosmos/evm/testutil/integration/evm/factory"
	"github.com/cosmos/evm/testutil/integration/evm/network"
	"github.com/cosmos/evm/testutil/keyring"
	"github.com/cosmos/evm/testutil/tx"
	testutiltypes "github.com/cosmos/evm/testutil/types"
	feemarkettypes "github.com/cosmos/evm/x/feemarket/types"
	"github.com/cosmos/evm/x/vm/keeper"
	"github.com/cosmos/evm/x/vm/keeper/testdata"
	"github.com/cosmos/evm/x/vm/statedb"
	"github.com/cosmos/evm/x/vm/types"

	sdkmath "cosmossdk.io/math"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	vestingtypes "github.com/cosmos/cosmos-sdk/x/auth/vesting/types"
)

// Not valid Ethereum address
const invalidAddress = "0x0000"

func (s *KeeperTestSuite) TestQueryAccount() {
	baseDenom := types.GetEVMCoinDenom()
	testCases := []struct {
		msg         string
		getReq      func() *types.QueryAccountRequest
		expResponse *types.QueryAccountResponse
		expPass     bool
	}{
		{
			"invalid address",
			func() *types.QueryAccountRequest {
				return &types.QueryAccountRequest{
					Address: invalidAddress,
				}
			},
			nil,
			false,
		},
		{
			"success",
			func() *types.QueryAccountRequest {
				amt := sdk.Coins{sdk.NewInt64Coin(baseDenom, 100)}

				// Add new unfunded key
				index := s.Keyring.AddKey()
				addr := s.Keyring.GetAddr(index)

				err := s.Network.App.GetBankKeeper().MintCoins(
					s.Network.GetContext(),
					types.ModuleName,
					amt,
				)
				s.Require().NoError(err)

				err = s.Network.App.GetBankKeeper().SendCoinsFromModuleToAccount(
					s.Network.GetContext(),
					types.ModuleName,
					addr.Bytes(),
					amt,
				)
				s.Require().NoError(err)

				return &types.QueryAccountRequest{
					Address: addr.String(),
				}
			},
			&types.QueryAccountResponse{
				Balance:  "100",
				CodeHash: common.BytesToHash(crypto.Keccak256(nil)).Hex(),
				Nonce:    0,
			},
			true,
		},
	}

	for _, tc := range testCases {
		s.Run(fmt.Sprintf("Case %s", tc.msg), func() {
			req := tc.getReq()
			expectedResponse := tc.expResponse

			ctx := s.Network.GetContext()
			// Function under test
			res, err := s.Network.GetEvmClient().Account(ctx, req)

			s.Require().Equal(expectedResponse, res)

			if tc.expPass {
				s.Require().NoError(err)
			} else {
				s.Require().Error(err)
			}
		})
	}
}

func (s *KeeperTestSuite) TestQueryCosmosAccount() {
	testCases := []struct {
		msg           string
		getReqAndResp func() (*types.QueryCosmosAccountRequest, *types.QueryCosmosAccountResponse)
		expPass       bool
	}{
		{
			"invalid address",
			func() (*types.QueryCosmosAccountRequest, *types.QueryCosmosAccountResponse) {
				req := &types.QueryCosmosAccountRequest{
					Address: invalidAddress,
				}
				return req, nil
			},
			false,
		},
		{
			"success",
			func() (*types.QueryCosmosAccountRequest, *types.QueryCosmosAccountResponse) {
				key := s.Keyring.GetKey(0)
				expAccount := &types.QueryCosmosAccountResponse{
					CosmosAddress: key.AccAddr.String(),
					Sequence:      0,
					AccountNumber: 0,
				}
				req := &types.QueryCosmosAccountRequest{
					Address: key.Addr.String(),
				}

				return req, expAccount
			},
			true,
		},
		{
			"success with seq and account number",
			func() (*types.QueryCosmosAccountRequest, *types.QueryCosmosAccountResponse) {
				index := s.Keyring.AddKey()
				newKey := s.Keyring.GetKey(index)
				accountNumber := uint64(100)
				acc := s.Network.App.GetAccountKeeper().NewAccountWithAddress(
					s.Network.GetContext(),
					newKey.AccAddr,
				)

				s.Require().NoError(acc.SetSequence(10))
				s.Require().NoError(acc.SetAccountNumber(accountNumber))
				s.Network.App.GetAccountKeeper().SetAccount(s.Network.GetContext(), acc)

				expAccount := &types.QueryCosmosAccountResponse{
					CosmosAddress: newKey.AccAddr.String(),
					Sequence:      10,
					AccountNumber: accountNumber,
				}

				req := &types.QueryCosmosAccountRequest{
					Address: newKey.Addr.String(),
				}
				return req, expAccount
			},
			true,
		},
	}

	for _, tc := range testCases {
		s.Run(fmt.Sprintf("Case %s", tc.msg), func() {
			req, expectedResponse := tc.getReqAndResp()

			ctx := s.Network.GetContext()

			// Function under test
			res, err := s.Network.GetEvmClient().CosmosAccount(ctx, req)

			s.Require().Equal(expectedResponse, res)

			if tc.expPass {
				s.Require().NoError(err)
			} else {
				s.Require().Error(err)
			}
		})
	}
}

func (s *KeeperTestSuite) TestQueryBalance() {
	baseDenom := types.GetEVMCoinDenom()

	testCases := []struct {
		msg           string
		getReqAndResp func() (*types.QueryBalanceRequest, *types.QueryBalanceResponse)
		expPass       bool
	}{
		{
			"invalid address",
			func() (*types.QueryBalanceRequest, *types.QueryBalanceResponse) {
				req := &types.QueryBalanceRequest{
					Address: invalidAddress,
				}
				return req, nil
			},
			false,
		},
		{
			"success",
			func() (*types.QueryBalanceRequest, *types.QueryBalanceResponse) {
				newIndex := s.Keyring.AddKey()
				addr := s.Keyring.GetAddr(newIndex)

				balance := int64(100)
				amt := sdk.Coins{sdk.NewInt64Coin(baseDenom, balance)}

				err := s.Network.App.GetBankKeeper().MintCoins(s.Network.GetContext(), types.ModuleName, amt)
				s.Require().NoError(err)
				err = s.Network.App.GetBankKeeper().SendCoinsFromModuleToAccount(s.Network.GetContext(), types.ModuleName, addr.Bytes(), amt)
				s.Require().NoError(err)

				req := &types.QueryBalanceRequest{
					Address: addr.String(),
				}
				return req, &types.QueryBalanceResponse{
					Balance: fmt.Sprint(balance),
				}
			},
			true,
		},
	}

	for _, tc := range testCases {
		s.Run(fmt.Sprintf("Case %s", tc.msg), func() {
			req, resp := tc.getReqAndResp()

			ctx := s.Network.GetContext()
			res, err := s.Network.GetEvmClient().Balance(ctx, req)

			s.Require().Equal(resp, res)
			if tc.expPass {
				s.Require().NoError(err)
			} else {
				s.Require().Error(err)
			}
		})
	}
}

func (s *KeeperTestSuite) TestQueryStorage() {
	testCases := []struct {
		msg           string
		getReqAndResp func() (*types.QueryStorageRequest, *types.QueryStorageResponse)
		expPass       bool
	}{
		{
			"invalid address",
			func() (*types.QueryStorageRequest, *types.QueryStorageResponse) {
				req := &types.QueryStorageRequest{
					Address: invalidAddress,
				}
				return req, nil
			},
			false,
		},
		{
			"success",
			func() (*types.QueryStorageRequest, *types.QueryStorageResponse) {
				key := common.BytesToHash([]byte("key"))
				value := []byte("value")
				expValue := common.BytesToHash(value)

				newIndex := s.Keyring.AddKey()
				addr := s.Keyring.GetAddr(newIndex)

				s.Network.App.GetEVMKeeper().SetState(
					s.Network.GetContext(),
					addr,
					key,
					value,
				)

				req := &types.QueryStorageRequest{
					Address: addr.String(),
					Key:     key.String(),
				}
				return req, &types.QueryStorageResponse{
					Value: expValue.String(),
				}
			},
			true,
		},
	}

	for _, tc := range testCases {
		s.Run(fmt.Sprintf("Case %s", tc.msg), func() {
			req, expectedResp := tc.getReqAndResp()

			ctx := s.Network.GetContext()
			res, err := s.Network.GetEvmClient().Storage(ctx, req)

			s.Require().Equal(expectedResp, res)

			if tc.expPass {
				s.Require().NoError(err)
			} else {
				s.Require().Error(err)
			}
		})
	}
}

func (s *KeeperTestSuite) TestQueryCode() {
	var (
		req     *types.QueryCodeRequest
		expCode []byte
	)

	testCases := []struct {
		msg           string
		getReqAndResp func() (*types.QueryCodeRequest, *types.QueryCodeResponse)
		expPass       bool
	}{
		{
			"invalid address",
			func() (*types.QueryCodeRequest, *types.QueryCodeResponse) {
				req = &types.QueryCodeRequest{
					Address: invalidAddress,
				}
				return req, nil
			},
			false,
		},
		{
			"success",
			func() (*types.QueryCodeRequest, *types.QueryCodeResponse) {
				newIndex := s.Keyring.AddKey()
				addr := s.Keyring.GetAddr(newIndex)

				expCode = []byte("code")
				stateDB := s.Network.GetStateDB()
				stateDB.SetCode(addr, expCode, 0x0)
				s.Require().NoError(stateDB.Commit())

				req = &types.QueryCodeRequest{
					Address: addr.String(),
				}
				return req, &types.QueryCodeResponse{
					Code: hexutil.Bytes(expCode),
				}
			},
			true,
		},
	}

	for _, tc := range testCases {
		s.Run(fmt.Sprintf("Case %s", tc.msg), func() {
			req, expectedResponse := tc.getReqAndResp()

			ctx := s.Network.GetContext()
			res, err := s.Network.GetEvmClient().Code(ctx, req)

			s.Require().Equal(expectedResponse, res)
			if tc.expPass {
				s.Require().NoError(err)
			} else {
				s.Require().Error(err)
			}
		})
	}
}

// TODO: Fix this one
func (s *KeeperTestSuite) TestQueryTxLogs() {
	expLogs := []*types.Log{}
	txHash := common.BytesToHash([]byte("tx_hash"))
	txIndex := uint(1)
	logIndex := uint(0)

	testCases := []struct {
		msg      string
		malleate func(vm.StateDB)
	}{
		{
			"empty logs",
			func(vm.StateDB) {
				expLogs = nil
			},
		},
		{
			"success",
			func(vmdb vm.StateDB) {
				addr := s.Keyring.GetAddr(0)
				expLogs = []*types.Log{
					{
						Address:     addr.String(),
						Topics:      []string{common.BytesToHash([]byte("topic")).String()},
						Data:        []byte("data"),
						BlockNumber: 1,
						TxHash:      txHash.String(),
						TxIndex:     uint64(txIndex),
						BlockHash:   common.BytesToHash(s.Network.GetContext().HeaderHash()).Hex(),
						Index:       uint64(logIndex),
						Removed:     false,
					},
				}

				for _, log := range types.LogsToEthereum(expLogs) {
					vmdb.AddLog(log)
				}
			},
		},
	}

	for _, tc := range testCases {
		s.Run(fmt.Sprintf("Case %s", tc.msg), func() {
			txCfg := statedb.NewTxConfig(
				txHash,
				txIndex,
			)
			vmdb := statedb.New(
				s.Network.GetContext(),
				s.Network.App.GetEVMKeeper(),
				txCfg,
			)

			tc.malleate(vmdb)
			s.Require().NoError(vmdb.Commit())

			logs := vmdb.Logs()
			s.Require().Equal(expLogs, types.NewLogsFromEth(logs))
		})
	}
}

func (s *KeeperTestSuite) TestQueryParams() {
	ctx := s.Network.GetContext()
	expParams := types.DefaultParams()
	expParams.ActiveStaticPrecompiles = types.AvailableStaticPrecompiles
	expParams.ExtraEIPs = nil
	expParams.EvmDenom = testconstants.ExampleAttoDenom
	expParams.ExtendedDenomOptions = &types.ExtendedDenomOptions{ExtendedDenom: testconstants.ExampleAttoDenom}

	res, err := s.Network.GetEvmClient().Params(ctx, &types.QueryParamsRequest{})
	s.Require().NoError(err)
	s.Require().Equal(expParams, res.Params)
}

func (s *KeeperTestSuite) TestQueryValidatorAccount() {
	testCases := []struct {
		msg           string
		getReqAndResp func() (*types.QueryValidatorAccountRequest, *types.QueryValidatorAccountResponse)
		expPass       bool
	}{
		{
			"invalid address",
			func() (*types.QueryValidatorAccountRequest, *types.QueryValidatorAccountResponse) {
				req := &types.QueryValidatorAccountRequest{
					ConsAddress: "",
				}
				return req, nil
			},
			false,
		},
		{
			"success",
			func() (*types.QueryValidatorAccountRequest, *types.QueryValidatorAccountResponse) {
				val := s.Network.GetValidators()[0]
				consAddr, err := val.GetConsAddr()
				s.Require().NoError(err)

				req := &types.QueryValidatorAccountRequest{
					ConsAddress: sdk.ConsAddress(consAddr).String(),
				}

				addrBz, err := s.Network.App.GetStakingKeeper().ValidatorAddressCodec().StringToBytes(val.OperatorAddress)
				s.Require().NoError(err)

				resp := &types.QueryValidatorAccountResponse{
					AccountAddress: sdk.AccAddress(addrBz).String(),
					Sequence:       0,
					AccountNumber:  2,
				}

				return req, resp
			},
			true,
		},
		{
			"success with seq and account number",
			func() (*types.QueryValidatorAccountRequest, *types.QueryValidatorAccountResponse) {
				val := s.Network.GetValidators()[0]
				consAddr, err := val.GetConsAddr()
				s.Require().NoError(err)

				// create validator account and set sequence and account number
				accNumber := uint64(100)
				accSeq := uint64(10)

				addrBz, err := s.Network.App.GetStakingKeeper().ValidatorAddressCodec().StringToBytes(val.OperatorAddress)
				s.Require().NoError(err)

				accAddrStr := sdk.AccAddress(addrBz).String()

				baseAcc := &authtypes.BaseAccount{Address: accAddrStr}
				acc := s.Network.App.GetAccountKeeper().NewAccount(s.Network.GetContext(), baseAcc)
				s.Require().NoError(acc.SetSequence(accSeq))
				s.Require().NoError(acc.SetAccountNumber(accNumber))
				s.Network.App.GetAccountKeeper().SetAccount(s.Network.GetContext(), acc)

				resp := &types.QueryValidatorAccountResponse{
					AccountAddress: accAddrStr,
					Sequence:       accSeq,
					AccountNumber:  accNumber,
				}
				req := &types.QueryValidatorAccountRequest{
					ConsAddress: sdk.ConsAddress(consAddr).String(),
				}

				return req, resp
			},
			true,
		},
	}

	for _, tc := range testCases {
		s.Run(fmt.Sprintf("Case %s", tc.msg), func() {
			req, resp := tc.getReqAndResp()
			ctx := s.Network.GetContext()
			res, err := s.Network.GetEvmClient().ValidatorAccount(ctx, req)

			s.Require().Equal(resp, res)
			if tc.expPass {
				s.Require().NoError(err)
			} else {
				s.Require().Error(err)
			}
		})
	}
}

func (s *KeeperTestSuite) TestEstimateGas() {
	gasHelper := hexutil.Uint64(20000)
	higherGas := hexutil.Uint64(25000)
	// Hardcode recipient address to avoid non determinism in tests
	hardcodedRecipient := common.HexToAddress("0xC6Fe5D33615a1C52c08018c47E8Bc53646A0E101")

	erc20Contract, err := testdata.LoadERC20Contract()
	s.Require().NoError(err)

	testCases := []struct {
		msg             string
		getArgs         func() types.TransactionArgs
		expPass         bool
		expGas          uint64
		EnableFeemarket bool
		gasCap          uint64
	}{
		// should success, because transfer value is zero
		{
			"success - default args - special case for ErrIntrinsicGas on contract creation, raise gas limit",
			func() types.TransactionArgs {
				return types.TransactionArgs{}
			},
			true,
			ethparams.TxGasContractCreation,
			false,
			config.DefaultGasCap,
		},
		// should success, because transfer value is zero
		{
			"success - default args with 'to' address",
			func() types.TransactionArgs {
				return types.TransactionArgs{To: &common.Address{}}
			},
			true,
			ethparams.TxGas,
			false,
			config.DefaultGasCap,
		},
		// should fail, because the default From address(zero address) don't have fund
		{
			"fail - not enough balance",
			func() types.TransactionArgs {
				return types.TransactionArgs{
					To:    &common.Address{},
					Value: (*hexutil.Big)(big.NewInt(100)),
				}
			},
			false,
			0,
			false,
			config.DefaultGasCap,
		},
		// should success, enough balance now
		{
			"success - enough balance",
			func() types.TransactionArgs {
				addr := s.Keyring.GetAddr(0)
				return types.TransactionArgs{
					To:    &common.Address{},
					From:  &addr,
					Value: (*hexutil.Big)(big.NewInt(100)),
				}
			},
			true,
			ethparams.TxGas,
			false,
			config.DefaultGasCap,
		},
		{
			"fail - not enough balance w/ gas fee cap",
			func() types.TransactionArgs {
				addr := s.Keyring.GetAddr(0)
				hexBigInt := hexutil.Big(*big.NewInt(1))
				balance := s.Network.App.GetBankKeeper().GetBalance(s.Network.GetContext(), sdk.AccAddress(addr.Bytes()), types.GetEVMCoinDenom())
				value := balance.Amount.Add(sdkmath.NewInt(1))
				return types.TransactionArgs{
					To:           &common.Address{},
					From:         &addr,
					Value:        (*hexutil.Big)(value.BigInt()),
					MaxFeePerGas: &hexBigInt,
				}
			},
			false,
			0,
			false,
			config.DefaultGasCap,
		},
		{
			"fail - insufficient funds for gas * price + value w/ gas fee cap",
			func() types.TransactionArgs {
				addr := s.Keyring.GetAddr(0)
				hexBigInt := hexutil.Big(*big.NewInt(1))
				balance := s.Network.App.GetBankKeeper().GetBalance(s.Network.GetContext(), sdk.AccAddress(addr.Bytes()), types.GetEVMCoinDenom())
				value := balance.Amount.Sub(sdkmath.NewInt(1))
				return types.TransactionArgs{
					To:           &common.Address{},
					From:         &addr,
					Value:        (*hexutil.Big)(value.BigInt()),
					MaxFeePerGas: &hexBigInt,
				}
			},
			false,
			0,
			false,
			config.DefaultGasCap,
		},
		// should success, because gas limit lower than 21000 is ignored
		{
			"gas exceed allowance",
			func() types.TransactionArgs {
				return types.TransactionArgs{To: &common.Address{}, Gas: &gasHelper}
			},
			true,
			ethparams.TxGas,
			false,
			config.DefaultGasCap,
		},
		// should fail, invalid gas cap
		{
			"gas exceed global allowance",
			func() types.TransactionArgs {
				return types.TransactionArgs{To: &common.Address{}}
			},
			false,
			0,
			false,
			20000,
		},
		// estimate gas of an erc20 contract deployment, the exact gas number is checked with geth
		{
			"contract deployment",
			func() types.TransactionArgs {
				ctorArgs, err := erc20Contract.ABI.Pack(
					"",
					&hardcodedRecipient,
					sdkmath.NewIntWithDecimal(1000, 18).BigInt(),
				)
				s.Require().NoError(err)
				data := erc20Contract.Bin
				data = append(data, ctorArgs...)

				addr := s.Keyring.GetAddr(0)
				return types.TransactionArgs{
					Data: (*hexutil.Bytes)(&data),
					From: &addr,
				}
			},
			true,
			1187108,
			false,
			config.DefaultGasCap,
		},
		// estimate gas of an erc20 transfer, the exact gas number is checked with geth
		{
			"erc20 transfer",
			func() types.TransactionArgs {
				key := s.Keyring.GetKey(0)
				contractAddr, err := deployErc20Contract(key, s.Factory)
				s.Require().NoError(err)

				err = s.Network.NextBlock()
				s.Require().NoError(err)

				transferData, err := erc20Contract.ABI.Pack(
					"transfer",
					hardcodedRecipient,
					big.NewInt(1000),
				)
				s.Require().NoError(err)
				return types.TransactionArgs{
					To:   &contractAddr,
					Data: (*hexutil.Bytes)(&transferData),
					From: &key.Addr,
				}
			},
			true,
			51880,
			false,
			config.DefaultGasCap,
		},
		// repeated tests with EnableFeemarket
		{
			"default args w/ EnableFeemarket",
			func() types.TransactionArgs {
				return types.TransactionArgs{To: &common.Address{}}
			},
			true,
			ethparams.TxGas,
			true,
			config.DefaultGasCap,
		},
		{
			"not enough balance w/ EnableFeemarket",
			func() types.TransactionArgs {
				return types.TransactionArgs{
					To:    &common.Address{},
					Value: (*hexutil.Big)(big.NewInt(100)),
				}
			},
			false,
			0,
			true,
			config.DefaultGasCap,
		},
		{
			"enough balance w/ EnableFeemarket",
			func() types.TransactionArgs {
				addr := s.Keyring.GetAddr(0)
				return types.TransactionArgs{
					To:    &common.Address{},
					From:  &addr,
					Value: (*hexutil.Big)(big.NewInt(100)),
				}
			},
			true,
			ethparams.TxGas,
			true,
			config.DefaultGasCap,
		},
		{
			"gas exceed allowance w/ EnableFeemarket",
			func() types.TransactionArgs {
				return types.TransactionArgs{To: &common.Address{}, Gas: &gasHelper}
			},
			true,
			ethparams.TxGas,
			true,
			config.DefaultGasCap,
		},
		{
			"gas exceed global allowance w/ EnableFeemarket",
			func() types.TransactionArgs {
				return types.TransactionArgs{To: &common.Address{}}
			},
			false,
			0,
			true,
			20000,
		},
		{
			"contract deployment w/ EnableFeemarket",
			func() types.TransactionArgs {
				ctorArgs, err := erc20Contract.ABI.Pack(
					"",
					&hardcodedRecipient,
					sdkmath.NewIntWithDecimal(1000, 18).BigInt(),
				)
				s.Require().NoError(err)
				data := erc20Contract.Bin
				data = append(data, ctorArgs...)

				sender := s.Keyring.GetAddr(0)
				return types.TransactionArgs{
					Data: (*hexutil.Bytes)(&data),
					From: &sender,
				}
			},
			true,
			1187108,
			true,
			config.DefaultGasCap,
		},
		{
			"erc20 transfer w/ EnableFeemarket",
			func() types.TransactionArgs {
				key := s.Keyring.GetKey(1)

				contractAddr, err := deployErc20Contract(key, s.Factory)
				s.Require().NoError(err)

				err = s.Network.NextBlock()
				s.Require().NoError(err)

				transferData, err := erc20Contract.ABI.Pack(
					"transfer",
					hardcodedRecipient,
					big.NewInt(1000),
				)
				s.Require().NoError(err)

				return types.TransactionArgs{
					To:   &contractAddr,
					From: &key.Addr,
					Data: (*hexutil.Bytes)(&transferData),
				}
			},
			true,
			51880,
			true,
			config.DefaultGasCap,
		},
		{
			"contract creation but 'create' param disabled",
			func() types.TransactionArgs {
				addr := s.Keyring.GetAddr(0)
				ctorArgs, err := erc20Contract.ABI.Pack(
					"",
					&addr,
					sdkmath.NewIntWithDecimal(1000, 18).BigInt(),
				)
				s.Require().NoError(err)

				data := erc20Contract.Bin
				data = append(data, ctorArgs...)

				args := types.TransactionArgs{
					From: &addr,
					Data: (*hexutil.Bytes)(&data),
				}
				params := s.Network.App.GetEVMKeeper().GetParams(s.Network.GetContext())
				params.AccessControl = types.AccessControl{
					Create: types.AccessControlType{
						AccessType: types.AccessTypeRestricted,
					},
				}
				err = s.Network.App.GetEVMKeeper().SetParams(
					s.Network.GetContext(),
					params,
				)
				s.Require().NoError(err)

				return args
			},
			false,
			0,
			false,
			config.DefaultGasCap,
		},
		{
			"specified gas in args higher than ethparams.TxGas (21,000)",
			func() types.TransactionArgs {
				return types.TransactionArgs{
					To:  &common.Address{},
					Gas: &higherGas,
				}
			},
			true,
			ethparams.TxGas,
			false,
			config.DefaultGasCap,
		},
		{
			"specified gas in args higher than request gasCap",
			func() types.TransactionArgs {
				return types.TransactionArgs{
					To:  &common.Address{},
					Gas: &higherGas,
				}
			},
			true,
			ethparams.TxGas,
			false,
			22_000,
		},
		{
			"invalid args - specified both gasPrice and maxFeePerGas",
			func() types.TransactionArgs {
				hexBigInt := hexutil.Big(*big.NewInt(1))

				return types.TransactionArgs{
					To:           &common.Address{},
					GasPrice:     &hexBigInt,
					MaxFeePerGas: &hexBigInt,
				}
			},
			false,
			0,
			false,
			config.DefaultGasCap,
		},
	}

	for _, tc := range testCases {
		s.Run(fmt.Sprintf("Case %s", tc.msg), func() {
			// Start from a clean state
			s.Require().NoError(s.Network.NextBlock())

			// Update feemarket params per test
			evmParams := feemarkettypes.DefaultParams()
			if !tc.EnableFeemarket {
				evmParams = s.Network.App.GetFeeMarketKeeper().GetParams(
					s.Network.GetContext(),
				)
				evmParams.NoBaseFee = true
			}

			err := s.Network.App.GetFeeMarketKeeper().SetParams(
				s.Network.GetContext(),
				evmParams,
			)
			s.Require().NoError(err)

			// Get call args
			args := tc.getArgs()
			marshalArgs, err := json.Marshal(args)
			s.Require().NoError(err)

			req := types.EthCallRequest{
				Args:            marshalArgs,
				GasCap:          tc.gasCap,
				ProposerAddress: s.Network.GetContext().BlockHeader().ProposerAddress,
			}

			// Function under test
			rsp, err := s.Network.GetEvmClient().EstimateGas(
				s.Network.GetContext(),
				&req,
			)
			if tc.expPass {
				s.Require().NoError(err)
				s.Require().Equal(int64(tc.expGas), int64(rsp.Gas)) //#nosec G115
			} else {
				s.Require().Error(err)
			}
		})
	}
}

func (s *KeeperTestSuite) TestEstimateGasIncludesDeterministicPostTxHookGas() {
	s.SetupTest()

	const hookGas = uint64(1_000)
	var lastReceipt *gethtypes.Receipt
	var lastPreliminaryGas uint64
	var lastPreliminaryCumulativeGas uint64
	firstHook := &testHooks{
		postProcessing: func(ctx sdk.Context, _ common.Address, _ core.Message, _ *gethtypes.Receipt) error {
			ctx.GasMeter().ConsumeGas(400, "first deterministic post tx hook")
			return nil
		},
	}
	secondHook := &testHooks{
		postProcessing: func(ctx sdk.Context, _ common.Address, _ core.Message, receipt *gethtypes.Receipt) error {
			s.Require().Equal(storetypes.KVGasConfig(), ctx.KVGasConfig())
			s.Require().Equal(storetypes.TransientGasConfig(), ctx.TransientKVGasConfig())
			lastReceipt = receipt
			lastPreliminaryGas = receipt.GasUsed
			lastPreliminaryCumulativeGas = receipt.CumulativeGasUsed
			ctx.GasMeter().ConsumeGas(600, "second deterministic post tx hook")
			return nil
		},
	}
	s.Network.App.GetEVMKeeper().SetHooks(keeper.NewMultiEvmHooks(firstHook, secondHook))

	sender := s.Keyring.GetAddr(0)
	recipient := s.Keyring.GetAddr(1)
	zeroGasPrice := hexutil.Big(*big.NewInt(0))
	args := types.TransactionArgs{
		From:     &sender,
		To:       &recipient,
		GasPrice: &zeroGasPrice,
	}
	argsBz, err := json.Marshal(args)
	s.Require().NoError(err)
	req := &types.EthCallRequest{
		Args:            argsBz,
		GasCap:          config.DefaultGasCap,
		ProposerAddress: s.Network.GetContext().BlockHeader().ProposerAddress,
	}

	estimate, err := s.Network.GetEvmClient().EstimateGas(s.Network.GetContext(), req)
	s.Require().NoError(err)
	s.Require().Equal(ethparams.TxGas+hookGas, estimate.Gas)
	s.Require().Equal(ethparams.TxGas, lastPreliminaryGas)
	s.Require().Equal(ethparams.TxGas, lastPreliminaryCumulativeGas)

	tx, err := s.Factory.GenerateSignedEthTx(s.Keyring.GetPrivKey(0), types.EvmTxArgs{
		To:       &recipient,
		GasLimit: estimate.Gas,
		GasPrice: big.NewInt(0),
	})
	s.Require().NoError(err)
	msg := tx.GetMsgs()[0].(*types.MsgEthereumTx)
	ctx := s.Network.GetContext().WithGasMeter(storetypes.NewGasMeter(estimate.Gas * 2))
	response, err := s.Network.App.GetEVMKeeper().ApplyTransaction(ctx, msg.AsTransaction())
	s.Require().NoError(err)
	s.Require().False(response.Failed())
	s.Require().Equal(estimate.Gas, response.GasUsed)
	s.Require().NotNil(lastReceipt)
	s.Require().Equal(estimate.Gas, lastReceipt.GasUsed)
	s.Require().Equal(estimate.Gas, lastReceipt.CumulativeGasUsed)
	s.Require().Equal(estimate.Gas, ctx.GasMeter().GasConsumed())
	s.Require().Equal(estimate.Gas, s.Network.App.GetEVMKeeper().GetTransientGasUsed(ctx))
}

func (s *KeeperTestSuite) TestEstimateGasPostTxHookReceiptTypeMatchesApply() {
	s.EnableFeemarket = true
	defer func() { s.EnableFeemarket = false }()
	s.SetupTest()

	const hookGas = uint64(1_000)
	var receiptType uint8
	s.Network.App.GetEVMKeeper().SetHooks(keeper.NewMultiEvmHooks(&testHooks{
		postProcessing: func(ctx sdk.Context, _ common.Address, _ core.Message, receipt *gethtypes.Receipt) error {
			receiptType = receipt.Type
			if receipt.Type == gethtypes.DynamicFeeTxType {
				ctx.GasMeter().ConsumeGas(hookGas, "dynamic fee receipt hook")
			}
			return nil
		},
	}))

	sender := s.Keyring.GetAddr(0)
	recipient := s.Keyring.GetAddr(1)
	maxFeePerGas := hexutil.Big(*big.NewInt(ethparams.InitialBaseFee))
	maxPriorityFeePerGas := hexutil.Big(*big.NewInt(0))
	argsBz, err := json.Marshal(types.TransactionArgs{
		From:                 &sender,
		To:                   &recipient,
		MaxFeePerGas:         &maxFeePerGas,
		MaxPriorityFeePerGas: &maxPriorityFeePerGas,
	})
	s.Require().NoError(err)

	estimate, err := s.Network.GetEvmClient().EstimateGas(s.Network.GetContext(), &types.EthCallRequest{
		Args:            argsBz,
		GasCap:          config.DefaultGasCap,
		ProposerAddress: s.Network.GetContext().BlockHeader().ProposerAddress,
	})
	s.Require().NoError(err)
	s.Require().Equal(uint8(gethtypes.DynamicFeeTxType), receiptType)
	s.Require().Equal(ethparams.TxGas+hookGas, estimate.Gas)

	tx, err := s.Factory.GenerateSignedEthTx(s.Keyring.GetPrivKey(0), types.EvmTxArgs{
		To:        &recipient,
		GasLimit:  estimate.Gas,
		GasFeeCap: big.NewInt(ethparams.InitialBaseFee),
		GasTipCap: big.NewInt(0),
	})
	s.Require().NoError(err)
	ethTx := tx.GetMsgs()[0].(*types.MsgEthereumTx).AsTransaction()
	s.Require().Equal(uint8(gethtypes.DynamicFeeTxType), ethTx.Type())

	response, err := s.Network.App.GetEVMKeeper().ApplyTransaction(
		s.Network.GetContext().WithGasMeter(storetypes.NewGasMeter(estimate.Gas*2)),
		ethTx,
	)
	s.Require().NoError(err)
	s.Require().False(response.Failed())
	s.Require().Equal(uint8(gethtypes.DynamicFeeTxType), receiptType)
	s.Require().Equal(estimate.Gas, response.GasUsed)
}

func (s *KeeperTestSuite) TestEstimateGasAccessListReceiptTypeMatchesApply() {
	s.EnableFeemarket = true
	defer func() { s.EnableFeemarket = false }()
	s.SetupTest()

	var estimateReceiptType, applyReceiptType uint8
	s.Network.App.GetEVMKeeper().SetHooks(keeper.NewMultiEvmHooks(&testHooks{
		postProcessing: func(_ sdk.Context, _ common.Address, _ core.Message, receipt *gethtypes.Receipt) error {
			applyReceiptType = receipt.Type
			return nil
		},
		estimateProcessing: func(_ sdk.Context, _ common.Address, _ core.Message, receipt *gethtypes.Receipt) error {
			estimateReceiptType = receipt.Type
			return nil
		},
	}))

	sender := s.Keyring.GetAddr(0)
	recipient := s.Keyring.GetAddr(1)
	gasPrice := big.NewInt(ethparams.InitialBaseFee)
	accessList := gethtypes.AccessList{}
	gasPriceArg := hexutil.Big(*gasPrice)
	argsBz, err := json.Marshal(types.TransactionArgs{
		From:       &sender,
		To:         &recipient,
		GasPrice:   &gasPriceArg,
		AccessList: &accessList,
	})
	s.Require().NoError(err)

	estimate, err := s.Network.GetEvmClient().EstimateGas(s.Network.GetContext(), &types.EthCallRequest{
		Args:            argsBz,
		GasCap:          config.DefaultGasCap,
		ProposerAddress: s.Network.GetContext().BlockHeader().ProposerAddress,
	})
	s.Require().NoError(err)
	s.Require().Equal(uint8(gethtypes.AccessListTxType), estimateReceiptType)

	ethSigner := gethtypes.LatestSignerForChainID(types.GetEthChainConfig().ChainID)
	msg, err := newSignedEthTx(
		&gethtypes.AccessListTx{
			ChainID:    types.GetEthChainConfig().ChainID,
			GasPrice:   gasPrice,
			Gas:        estimate.Gas,
			To:         &recipient,
			Value:      big.NewInt(0),
			Data:       nil,
			AccessList: accessList,
		},
		s.Network.App.GetEVMKeeper().GetNonce(s.Network.GetContext(), sender),
		sdk.AccAddress(sender.Bytes()),
		tx.NewSigner(s.Keyring.GetPrivKey(0)),
		ethSigner,
	)
	s.Require().NoError(err)
	s.Require().Equal(uint8(gethtypes.AccessListTxType), msg.AsTransaction().Type())

	response, err := s.Network.App.GetEVMKeeper().ApplyTransaction(
		s.Network.GetContext().WithGasMeter(storetypes.NewGasMeter(estimate.Gas*2)),
		msg.AsTransaction(),
	)
	s.Require().NoError(err)
	s.Require().False(response.Failed())
	s.Require().Equal(uint8(gethtypes.AccessListTxType), applyReceiptType)
}

func (s *KeeperTestSuite) TestEstimateGasTxTraceIndexMatchesApply() {
	s.SetupTest()

	evmKeeper := s.Network.App.GetEVMKeeper()
	hook := &txTraceByReceiptIndexHook{keeper: evmKeeper}
	evmKeeper.SetHooks(keeper.NewMultiEvmHooks(hook))

	sender := s.Keyring.GetAddr(0)
	recipient := s.Keyring.GetAddr(1)
	value := hexutil.Big(*big.NewInt(1))
	zeroGasPrice := hexutil.Big(*big.NewInt(0))
	argsBz, err := json.Marshal(types.TransactionArgs{
		From:     &sender,
		To:       &recipient,
		Value:    &value,
		GasPrice: &zeroGasPrice,
	})
	s.Require().NoError(err)

	queryCtx := s.Network.GetQueryContext()
	s.Require().Equal(-1, queryCtx.TxIndex())
	estimate, err := s.Network.GetEvmClient().EstimateGas(queryCtx, &types.EthCallRequest{
		Args:            argsBz,
		GasCap:          config.DefaultGasCap,
		ProposerAddress: queryCtx.BlockHeader().ProposerAddress,
	})
	s.Require().NoError(err)
	s.Require().Equal(statedb.NewEmptyTxConfig().TxIndex, hook.ReceiptIndex)
	s.Require().Greater(hook.Touches, 0)
	estimateTouches := hook.Touches
	estimateTransfers := hook.Transfers
	estimateHookGas := hook.HookGas

	const applyTxIndex = 3
	tx, err := s.Factory.GenerateSignedEthTx(s.Keyring.GetPrivKey(0), types.EvmTxArgs{
		To:       &recipient,
		Amount:   big.NewInt(1),
		GasLimit: estimate.Gas,
		GasPrice: big.NewInt(0),
	})
	s.Require().NoError(err)
	ethTx := tx.GetMsgs()[0].(*types.MsgEthereumTx).AsTransaction()
	applyCtx := s.Network.GetContext().
		WithTxIndex(applyTxIndex).
		WithGasMeter(storetypes.NewGasMeter(estimate.Gas * 2))
	txConfig := evmKeeper.TxConfig(applyCtx, ethTx.Hash())
	response, err := evmKeeper.ApplyTransaction(applyCtx, ethTx)
	s.Require().NoError(err)
	s.Require().False(response.Failed())
	s.Require().Equal(txConfig.TxIndex, hook.ReceiptIndex)
	s.Require().Equal(uint(applyTxIndex), hook.ReceiptIndex)
	s.Require().Equal(estimateTouches, hook.Touches)
	s.Require().Equal(estimateTransfers, hook.Transfers)
	s.Require().Equal(estimateHookGas, hook.HookGas)
	s.Require().Equal(estimate.Gas, response.GasUsed)
}

func (s *KeeperTestSuite) TestEstimateGasPlainTransferFinalReprobe() {
	s.SetupTest()

	var productionCalls, estimateCalls int
	s.Network.App.GetEVMKeeper().SetHooks(keeper.NewMultiEvmHooks(&testHooks{
		postProcessing: func(_ sdk.Context, _ common.Address, _ core.Message, _ *gethtypes.Receipt) error {
			productionCalls++
			return nil
		},
		estimateProcessing: func(_ sdk.Context, _ common.Address, _ core.Message, _ *gethtypes.Receipt) error {
			estimateCalls++
			return nil
		},
	}))

	sender := s.Keyring.GetAddr(0)
	recipient := s.Keyring.GetAddr(1)
	zeroGasPrice := hexutil.Big(*big.NewInt(0))
	argsBz, err := json.Marshal(types.TransactionArgs{
		From:     &sender,
		To:       &recipient,
		GasPrice: &zeroGasPrice,
	})
	s.Require().NoError(err)

	estimate, err := s.Network.GetEvmClient().EstimateGas(s.Network.GetContext(), &types.EthCallRequest{
		Args:            argsBz,
		GasCap:          config.DefaultGasCap,
		ProposerAddress: s.Network.GetContext().BlockHeader().ProposerAddress,
	})
	s.Require().NoError(err)
	s.Require().Equal(uint64(ethparams.TxGas), estimate.Gas)
	s.Require().Zero(productionCalls, "estimation must not call the production hook")
	s.Require().Equal(2, estimateCalls, "the shortcut result must be proved by a final candidate execution")
}

func (s *KeeperTestSuite) TestEstimateGasRejectsProductionOnlyHook() {
	s.SetupTest()

	hook := &productionOnlyHook{}
	s.Network.App.GetEVMKeeper().SetHooks(keeper.NewMultiEvmHooks(hook))
	sender := s.Keyring.GetAddr(0)
	recipient := s.Keyring.GetAddr(1)
	zeroGasPrice := hexutil.Big(*big.NewInt(0))
	argsBz, err := json.Marshal(types.TransactionArgs{
		From:     &sender,
		To:       &recipient,
		GasPrice: &zeroGasPrice,
	})
	s.Require().NoError(err)

	_, err = s.Network.GetEvmClient().EstimateGas(s.Network.GetContext(), &types.EthCallRequest{
		Args:            argsBz,
		GasCap:          config.DefaultGasCap,
		ProposerAddress: s.Network.GetContext().BlockHeader().ProposerAddress,
	})
	s.Require().ErrorContains(err, "does not support gas estimation")
	s.Require().Zero(hook.Calls, "estimation must not fall back to the production hook")
}

func (s *KeeperTestSuite) TestEstimateGasHookSeesUpfrontFeeDeduction() {
	s.EnableFeemarket = true
	defer func() { s.EnableFeemarket = false }()
	s.SetupTest()

	evmKeeper := s.Network.App.GetEVMKeeper()
	sender := s.Keyring.GetAddr(0)
	recipient := s.Keyring.GetAddr(1)
	startingBalance := evmKeeper.SpendableCoin(s.Network.GetContext(), sender).ToBig()
	checkBalance := func(ctx sdk.Context, _ common.Address, msg core.Message, _ *gethtypes.Receipt) error {
		fee := new(big.Int).Mul(new(big.Int).SetUint64(msg.GasLimit), msg.GasPrice)
		expected := new(big.Int).Sub(startingBalance, fee)
		actual := evmKeeper.SpendableCoin(ctx, sender).ToBig()
		if actual.Cmp(expected) != 0 {
			return fmt.Errorf("hook sender balance %s, expected post-fee balance %s", actual, expected)
		}
		return nil
	}
	evmKeeper.SetHooks(keeper.NewMultiEvmHooks(&testHooks{
		postProcessing:     checkBalance,
		estimateProcessing: checkBalance,
	}))

	gasPrice := hexutil.Big(*big.NewInt(ethparams.InitialBaseFee))
	argsBz, err := json.Marshal(types.TransactionArgs{
		From:     &sender,
		To:       &recipient,
		GasPrice: &gasPrice,
	})
	s.Require().NoError(err)

	_, err = s.Network.GetEvmClient().EstimateGas(s.Network.GetContext(), &types.EthCallRequest{
		Args:            argsBz,
		GasCap:          config.DefaultGasCap,
		ProposerAddress: s.Network.GetContext().BlockHeader().ProposerAddress,
	})
	s.Require().NoError(err)
	s.Require().Equal(startingBalance, evmKeeper.SpendableCoin(s.Network.GetContext(), sender).ToBig())
}

func (s *KeeperTestSuite) TestEstimateGasAppliesSenderOverrideBeforeFeeDeduction() {
	s.EnableFeemarket = true
	defer func() { s.EnableFeemarket = false }()
	s.SetupTest()

	evmKeeper := s.Network.App.GetEVMKeeper()
	sender := common.HexToAddress("0x0000000000000000000000000000000000000042")
	recipient := s.Keyring.GetAddr(1)
	overrideBalance := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	valueInt := big.NewInt(1_000)
	const overrideNonce uint64 = 7
	checkOverride := func(ctx sdk.Context, _ common.Address, msg core.Message, _ *gethtypes.Receipt) error {
		fee := new(big.Int).Mul(new(big.Int).SetUint64(msg.GasLimit), msg.GasPrice)
		expectedBalance := new(big.Int).Sub(new(big.Int).Set(overrideBalance), fee)
		expectedBalance.Sub(expectedBalance, valueInt)
		actualBalance := evmKeeper.SpendableCoin(ctx, sender).ToBig()
		if actualBalance.Cmp(expectedBalance) != 0 {
			return fmt.Errorf("hook sender balance %s, expected overridden post-fee balance %s", actualBalance, expectedBalance)
		}
		if nonce := evmKeeper.GetNonce(ctx, sender); nonce != overrideNonce+1 {
			return fmt.Errorf("hook sender nonce %d, expected %d", nonce, overrideNonce+1)
		}
		return nil
	}
	evmKeeper.SetHooks(keeper.NewMultiEvmHooks(&testHooks{
		postProcessing:     checkOverride,
		estimateProcessing: checkOverride,
	}))

	gasPrice := hexutil.Big(*big.NewInt(ethparams.InitialBaseFee))
	value := hexutil.Big(*valueInt)
	argsBz, err := json.Marshal(types.TransactionArgs{
		From:     &sender,
		To:       &recipient,
		Value:    &value,
		GasPrice: &gasPrice,
	})
	s.Require().NoError(err)
	overrides := []byte(fmt.Sprintf(
		`{"%s":{"balance":"%s","nonce":"0x%x"}}`,
		sender.Hex(), hexutil.EncodeBig(overrideBalance), overrideNonce,
	))

	_, err = s.Network.GetEvmClient().EstimateGas(s.Network.GetContext(), &types.EthCallRequest{
		Args:            argsBz,
		GasCap:          config.DefaultGasCap,
		ProposerAddress: s.Network.GetContext().BlockHeader().ProposerAddress,
		Overrides:       overrides,
	})
	s.Require().NoError(err)
	s.Require().Zero(evmKeeper.GetNonce(s.Network.GetContext(), sender))
	s.Require().True(evmKeeper.SpendableCoin(s.Network.GetContext(), sender).IsZero())
}

func (s *KeeperTestSuite) TestEstimateGasChargesNestedHookEVMGasExactlyOnce() {
	s.SetupTest()

	evmKeeper := s.Network.App.GetEVMKeeper()
	hook := &NestedEVMGasHook{
		keeper:   evmKeeper,
		contract: common.HexToAddress(testconstants.WEVMOSContractMainnet),
	}
	evmKeeper.SetHooks(keeper.NewMultiEvmHooks(hook))

	sender := s.Keyring.GetAddr(0)
	recipient := s.Keyring.GetAddr(1)
	zeroGasPrice := hexutil.Big(*big.NewInt(0))
	argsBz, err := json.Marshal(types.TransactionArgs{
		From:     &sender,
		To:       &recipient,
		GasPrice: &zeroGasPrice,
	})
	s.Require().NoError(err)

	estimate, err := s.Network.GetEvmClient().EstimateGas(s.Network.GetContext(), &types.EthCallRequest{
		Args:            argsBz,
		GasCap:          config.DefaultGasCap,
		ProposerAddress: s.Network.GetContext().BlockHeader().ProposerAddress,
	})
	s.Require().NoError(err)
	s.Require().Greater(hook.GasUsed, uint64(0))
	s.Require().Equal(ethparams.TxGas+hook.GasUsed, estimate.Gas)

	tx, err := s.Factory.GenerateSignedEthTx(s.Keyring.GetPrivKey(0), types.EvmTxArgs{
		To:       &recipient,
		GasLimit: estimate.Gas,
		GasPrice: big.NewInt(0),
	})
	s.Require().NoError(err)
	response, err := evmKeeper.ApplyTransaction(
		s.Network.GetContext().WithGasMeter(storetypes.NewGasMeter(estimate.Gas*2)),
		tx.GetMsgs()[0].(*types.MsgEthereumTx).AsTransaction(),
	)
	s.Require().NoError(err)
	s.Require().False(response.Failed())
	s.Require().Equal(estimate.Gas, response.GasUsed)
}

func (s *KeeperTestSuite) TestCallEVMWithDataPreservesPostTxHookStateDB() {
	s.SetupTest()

	evmKeeper := s.Network.App.GetEVMKeeper()
	var ret []byte
	evmKeeper.SetHooks(keeper.NewMultiEvmHooks(&testHooks{
		postProcessing: func(ctx sdk.Context, from common.Address, _ core.Message, _ *gethtypes.Receipt) error {
			contract := common.HexToAddress("0x0000000000000000000000000000000000000042")
			stateDB := statedb.New(ctx, evmKeeper, statedb.NewEmptyTxConfig())
			stateDB.SetCode(
				contract,
				[]byte{0x60, 0x2a, 0x60, 0x00, 0x52, 0x60, 0x20, 0x60, 0x00, 0xf3},
				tracing.CodeChangeUnspecified,
			)
			response, err := evmKeeper.CallEVMWithData(
				ctx,
				stateDB,
				from,
				&contract,
				nil,
				false,
				false,
				new(big.Int).SetUint64(ctx.GasMeter().GasRemaining()),
			)
			if err == nil {
				ret = response.Ret
			}
			return err
		},
	}))

	recipient := s.Keyring.GetAddr(1)
	tx, err := s.Factory.GenerateSignedEthTx(s.Keyring.GetPrivKey(0), types.EvmTxArgs{
		To:       &recipient,
		GasLimit: 500_000,
		GasPrice: big.NewInt(0),
	})
	s.Require().NoError(err)

	response, err := evmKeeper.ApplyTransaction(
		s.Network.GetContext().WithGasMeter(storetypes.NewGasMeter(1_000_000)),
		tx.GetMsgs()[0].(*types.MsgEthereumTx).AsTransaction(),
	)
	s.Require().NoError(err)
	s.Require().False(response.Failed())
	s.Require().Len(ret, common.HashLength)
	s.Require().Equal(byte(0x2a), ret[common.HashLength-1])
}

func (s *KeeperTestSuite) TestCallEVMViewWithDataDiscardsStateChanges() {
	s.SetupTest()

	evmKeeper := s.Network.App.GetEVMKeeper()
	contract := common.HexToAddress("0x0000000000000000000000000000000000000043")
	stateDB := statedb.New(s.Network.GetContext(), evmKeeper, statedb.NewEmptyTxConfig())
	stateDB.SetCode(contract, []byte{0x60, 0x01, 0x60, 0x00, 0x55, 0x00}, tracing.CodeChangeUnspecified)
	s.Require().NoError(stateDB.Commit())

	evmKeeper.SetHooks(keeper.NewMultiEvmHooks(&testHooks{
		postProcessing: func(ctx sdk.Context, from common.Address, _ core.Message, _ *gethtypes.Receipt) error {
			_, err := evmKeeper.CallEVMViewWithData(
				ctx,
				from,
				&contract,
				nil,
				new(big.Int).SetUint64(ctx.GasMeter().GasRemaining()),
			)
			return err
		},
	}))

	recipient := s.Keyring.GetAddr(1)
	tx, err := s.Factory.GenerateSignedEthTx(s.Keyring.GetPrivKey(0), types.EvmTxArgs{
		To:       &recipient,
		GasLimit: 500_000,
		GasPrice: big.NewInt(0),
	})
	s.Require().NoError(err)

	response, err := evmKeeper.ApplyTransaction(
		s.Network.GetContext().WithGasMeter(storetypes.NewGasMeter(1_000_000)),
		tx.GetMsgs()[0].(*types.MsgEthereumTx).AsTransaction(),
	)
	s.Require().NoError(err)
	s.Require().False(response.Failed())
	s.Require().Equal(common.Hash{}, evmKeeper.GetState(s.Network.GetContext(), contract, common.Hash{}))
}

func (s *KeeperTestSuite) TestCallEVMViewWithDataClampsGasToParentRemaining() {
	s.SetupTest()

	const parentGasLimit = uint64(30_000)

	evmKeeper := s.Network.App.GetEVMKeeper()
	contract := common.HexToAddress("0x0000000000000000000000000000000000000044")
	stateDB := statedb.New(s.Network.GetContext(), evmKeeper, statedb.NewEmptyTxConfig())
	stateDB.SetCode(contract, []byte{0x5b, 0x60, 0x00, 0x56}, tracing.CodeChangeUnspecified)
	s.Require().NoError(stateDB.Commit())

	ctx := s.Network.GetContext().WithGasMeter(storetypes.NewGasMeter(parentGasLimit))
	response, err := evmKeeper.CallEVMViewWithData(
		ctx,
		s.Keyring.GetAddr(0),
		&contract,
		nil,
		new(big.Int).SetUint64(100_000),
	)
	s.Require().Error(err)
	s.Require().NotNil(response)
	s.Require().True(response.Failed())
	s.Require().LessOrEqual(response.GasUsed, parentGasLimit)
	s.Require().Equal(parentGasLimit, ctx.GasMeter().GasConsumed())
}

func (s *KeeperTestSuite) TestCallEVMViewWithDataPreservesExplicitGasCapOOG() {
	s.SetupTest()

	const (
		parentGasLimit = uint64(100_000)
		gasCap         = uint64(30_000)
	)

	evmKeeper := s.Network.App.GetEVMKeeper()
	contract := common.HexToAddress("0x0000000000000000000000000000000000000046")
	stateDB := statedb.New(s.Network.GetContext(), evmKeeper, statedb.NewEmptyTxConfig())
	stateDB.SetCode(contract, []byte{0x5b, 0x60, 0x00, 0x56}, tracing.CodeChangeUnspecified)
	s.Require().NoError(stateDB.Commit())

	ctx := s.Network.GetContext().WithGasMeter(storetypes.NewGasMeter(parentGasLimit))
	response, err := evmKeeper.CallEVMViewWithData(
		ctx,
		s.Keyring.GetAddr(0),
		&contract,
		nil,
		new(big.Int).SetUint64(gasCap),
	)
	s.Require().Error(err)
	s.Require().NotErrorIs(err, sdkerrors.ErrOutOfGas)
	s.Require().NotNil(response)
	s.Require().Equal(vm.ErrOutOfGas.Error(), response.VmError)
	s.Require().Equal(gasCap, response.GasUsed)
	s.Require().Equal(gasCap, ctx.GasMeter().GasConsumed())
}

func (s *KeeperTestSuite) TestCallEVMViewWithDataChargesActualGasOnRevert() {
	s.SetupTest()

	const parentGasLimit = uint64(100_000)

	evmKeeper := s.Network.App.GetEVMKeeper()
	contract := common.HexToAddress("0x0000000000000000000000000000000000000045")
	stateDB := statedb.New(s.Network.GetContext(), evmKeeper, statedb.NewEmptyTxConfig())
	stateDB.SetCode(contract, []byte{0x60, 0x00, 0x60, 0x00, 0xfd}, tracing.CodeChangeUnspecified)
	s.Require().NoError(stateDB.Commit())

	ctx := s.Network.GetContext().WithGasMeter(storetypes.NewGasMeter(parentGasLimit))
	response, err := evmKeeper.CallEVMViewWithData(
		ctx,
		s.Keyring.GetAddr(0),
		&contract,
		nil,
		new(big.Int).SetUint64(parentGasLimit),
	)
	s.Require().Error(err)
	s.Require().NotNil(response)
	s.Require().Equal(vm.ErrExecutionReverted.Error(), response.VmError)
	s.Require().Less(response.GasUsed, parentGasLimit)
	s.Require().Equal(response.GasUsed, ctx.GasMeter().GasConsumed())
}

func (s *KeeperTestSuite) TestApplyTransactionNestedViewIntrinsicGasShortfallIsOutOfGas() {
	s.SetupTest()

	const gasLimit = uint64(30_000)

	evmKeeper := s.Network.App.GetEVMKeeper()
	recipient := s.Keyring.GetAddr(1)
	evmKeeper.SetHooks(keeper.NewMultiEvmHooks(&testHooks{
		postProcessing: func(ctx sdk.Context, from common.Address, _ core.Message, _ *gethtypes.Receipt) error {
			_, err := evmKeeper.CallEVMViewWithData(
				ctx,
				from,
				&recipient,
				nil,
				new(big.Int).SetUint64(ctx.GasMeter().GasRemaining()),
			)
			return err
		},
	}))

	tx, err := s.Factory.GenerateSignedEthTx(s.Keyring.GetPrivKey(0), types.EvmTxArgs{
		To:       &recipient,
		GasLimit: gasLimit,
		GasPrice: big.NewInt(0),
	})
	s.Require().NoError(err)
	ctx := s.Network.GetContext().WithGasMeter(storetypes.NewGasMeter(gasLimit * 2))
	response, err := evmKeeper.ApplyTransaction(ctx, tx.GetMsgs()[0].(*types.MsgEthereumTx).AsTransaction())
	s.Require().NoError(err)
	s.Require().True(response.Failed())
	s.Require().Equal(vm.ErrOutOfGas.Error(), response.VmError)
	s.Require().Equal(gasLimit, response.GasUsed)
	s.Require().Equal(gasLimit, ctx.GasMeter().GasConsumed())
}

func (s *KeeperTestSuite) TestEstimateGasPreservesPostTxHookRevertData() {
	s.SetupTest()

	revertData := []byte{0xde, 0xad, 0xbe, 0xef}
	s.Network.App.GetEVMKeeper().SetHooks(keeper.NewMultiEvmHooks(RevertHook{Data: revertData}))
	sender := s.Keyring.GetAddr(0)
	recipient := s.Keyring.GetAddr(1)
	zeroGasPrice := hexutil.Big(*big.NewInt(0))
	argsBz, err := json.Marshal(types.TransactionArgs{
		From:     &sender,
		To:       &recipient,
		GasPrice: &zeroGasPrice,
	})
	s.Require().NoError(err)

	estimate, err := s.Network.GetEvmClient().EstimateGas(s.Network.GetContext(), &types.EthCallRequest{
		Args:            argsBz,
		GasCap:          100_000,
		ProposerAddress: s.Network.GetContext().BlockHeader().ProposerAddress,
	})
	s.Require().NoError(err)
	s.Require().Equal(vm.ErrExecutionReverted.Error(), estimate.VmError)
	s.Require().Equal(revertData, estimate.Ret)

	tx, err := s.Factory.GenerateSignedEthTx(s.Keyring.GetPrivKey(0), types.EvmTxArgs{
		To:       &recipient,
		GasLimit: 100_000,
		GasPrice: big.NewInt(0),
	})
	s.Require().NoError(err)
	response, err := s.Network.App.GetEVMKeeper().ApplyTransaction(
		s.Network.GetContext().WithGasMeter(storetypes.NewGasMeter(200_000)),
		tx.GetMsgs()[0].(*types.MsgEthereumTx).AsTransaction(),
	)
	s.Require().NoError(err)
	s.Require().True(response.Failed())
	s.Require().Equal(vm.ErrExecutionReverted.Error(), response.VmError)
	s.Require().Equal(revertData, response.Ret)
}

func (s *KeeperTestSuite) TestEstimateGasPostTxHookFailureClasses() {
	for _, tc := range []struct {
		name        string
		hook        types.EvmHooks
		errContains string
	}{
		{
			name:        "ordinary error is stable",
			hook:        &FailureHook{},
			errContains: "failed to execute post transaction processing",
		},
		{
			name: "hook out of gas reports allowance",
			hook: &testHooks{postProcessing: func(ctx sdk.Context, _ common.Address, _ core.Message, _ *gethtypes.Receipt) error {
				ctx.GasMeter().ConsumeGas(ctx.GasMeter().GasRemaining()+1, "force estimate hook out of gas")
				return nil
			}},
			errContains: "gas required exceeds allowance",
		},
	} {
		s.Run(tc.name, func() {
			s.SetupTest()
			s.Network.App.GetEVMKeeper().SetHooks(keeper.NewMultiEvmHooks(tc.hook))
			sender := s.Keyring.GetAddr(0)
			recipient := s.Keyring.GetAddr(1)
			zeroGasPrice := hexutil.Big(*big.NewInt(0))
			argsBz, err := json.Marshal(types.TransactionArgs{
				From:     &sender,
				To:       &recipient,
				GasPrice: &zeroGasPrice,
			})
			s.Require().NoError(err)
			req := &types.EthCallRequest{
				Args:            argsBz,
				GasCap:          100_000,
				ProposerAddress: s.Network.GetContext().BlockHeader().ProposerAddress,
			}

			_, firstErr := s.Network.GetEvmClient().EstimateGas(s.Network.GetContext(), req)
			_, secondErr := s.Network.GetEvmClient().EstimateGas(s.Network.GetContext(), req)
			s.Require().Error(firstErr)
			s.Require().Error(secondErr)
			s.Require().Contains(firstErr.Error(), tc.errContains)
			s.Require().Equal(firstErr.Error(), secondErr.Error())
		})
	}
}

func (s *KeeperTestSuite) TestEstimateGasRecoversWhenOptimisticHookCandidateFails() {
	s.SetupTest()

	const hookRemainingThreshold = uint64(50_000)
	var (
		sawHighCandidate     bool
		sawOptimisticFailure bool
	)
	hook := &testHooks{postProcessing: func(ctx sdk.Context, _ common.Address, _ core.Message, _ *gethtypes.Receipt) error {
		if ctx.GasMeter().Limit() > hookRemainingThreshold {
			sawHighCandidate = true
			ctx.GasMeter().ConsumeGas(1_000, "small high-limit hook work")
			return nil
		}
		sawOptimisticFailure = true
		ctx.GasMeter().ConsumeGas(ctx.GasMeter().GasRemaining()+1, "gas-sensitive hook work")
		return nil
	}}
	s.Network.App.GetEVMKeeper().SetHooks(keeper.NewMultiEvmHooks(hook))

	sender := s.Keyring.GetAddr(0)
	recipient := s.Keyring.GetAddr(1)
	zeroGasPrice := hexutil.Big(*big.NewInt(0))
	argsBz, err := json.Marshal(types.TransactionArgs{
		From:     &sender,
		To:       &recipient,
		GasPrice: &zeroGasPrice,
	})
	s.Require().NoError(err)
	estimate, err := s.Network.GetEvmClient().EstimateGas(s.Network.GetContext(), &types.EthCallRequest{
		Args:            argsBz,
		GasCap:          500_000,
		ProposerAddress: s.Network.GetContext().BlockHeader().ProposerAddress,
	})
	s.Require().NoError(err)
	s.Require().True(sawHighCandidate)
	s.Require().True(sawOptimisticFailure)
	s.Require().Greater(estimate.Gas, ethparams.TxGas+hookRemainingThreshold)

	tx, err := s.Factory.GenerateSignedEthTx(s.Keyring.GetPrivKey(0), types.EvmTxArgs{
		To:       &recipient,
		GasLimit: estimate.Gas,
		GasPrice: big.NewInt(0),
	})
	s.Require().NoError(err)
	response, err := s.Network.App.GetEVMKeeper().ApplyTransaction(
		s.Network.GetContext().WithGasMeter(storetypes.NewGasMeter(estimate.Gas*2)),
		tx.GetMsgs()[0].(*types.MsgEthereumTx).AsTransaction(),
	)
	s.Require().NoError(err)
	s.Require().False(response.Failed())
	s.Require().LessOrEqual(response.GasUsed, estimate.Gas)
}

func (s *KeeperTestSuite) TestEstimateGasCandidateStateIsDiscardedAndHooksSeePostState() {
	s.SetupTest()

	erc20Contract, err := testdata.LoadERC20Contract()
	s.Require().NoError(err)
	contractAddr, err := deployErc20Contract(s.Keyring.GetKey(0), s.Factory)
	s.Require().NoError(err)
	s.Require().NoError(s.Network.NextBlock())

	const mintedDenom = "aestimatehook"
	transferAmount := big.NewInt(123)
	sender := s.Keyring.GetAddr(0)
	recipient := s.Keyring.GetAddr(1)
	evmKeeper := s.Network.App.GetEVMKeeper()
	recipientBefore := new(uint256.Int).Set(evmKeeper.GetBalance(s.Network.GetContext(), recipient))
	moduleAddr := s.Network.App.GetAccountKeeper().GetModuleAddress("mint")

	var sawPostState, sawTrace, sawCleanCandidate bool
	sawCleanCandidate = true
	hook := &testHooks{
		postProcessing: func(ctx sdk.Context, _ common.Address, msg core.Message, _ *gethtypes.Receipt) error {
			if msg.Value.Sign() > 0 {
				sawPostState = sawPostState || evmKeeper.GetBalance(ctx, recipient).Cmp(recipientBefore) > 0
			}
			if len(msg.Data) > 0 {
				touches, _ := evmKeeper.GetTxTrace(ctx, 0)
				sawTrace = sawTrace || len(touches) > 0
			}
			beforeMint := s.Network.App.GetBankKeeper().GetBalance(ctx, moduleAddr, mintedDenom)
			sawCleanCandidate = sawCleanCandidate && beforeMint.IsZero()
			return s.Network.App.GetMintKeeper().MintCoins(
				ctx,
				sdk.NewCoins(sdk.NewInt64Coin(mintedDenom, 1)),
			)
		},
	}
	evmKeeper.SetHooks(keeper.NewMultiEvmHooks(hook))

	zeroGasPrice := hexutil.Big(*big.NewInt(0))
	value := hexutil.Big(*transferAmount)
	args := types.TransactionArgs{
		From:     &sender,
		To:       &recipient,
		Value:    &value,
		GasPrice: &zeroGasPrice,
	}
	argsBz, err := json.Marshal(args)
	s.Require().NoError(err)
	req := &types.EthCallRequest{
		Args:            argsBz,
		GasCap:          config.DefaultGasCap,
		ProposerAddress: s.Network.GetContext().BlockHeader().ProposerAddress,
	}

	first, err := s.Network.GetEvmClient().EstimateGas(s.Network.GetContext(), req)
	s.Require().NoError(err)
	second, err := s.Network.GetEvmClient().EstimateGas(s.Network.GetContext(), req)
	s.Require().NoError(err)

	transferData, err := erc20Contract.ABI.Pack("transfer", recipient, big.NewInt(1))
	s.Require().NoError(err)
	contractArgs := types.TransactionArgs{
		From:     &sender,
		To:       &contractAddr,
		Data:     (*hexutil.Bytes)(&transferData),
		GasPrice: &zeroGasPrice,
	}
	contractArgsBz, err := json.Marshal(contractArgs)
	s.Require().NoError(err)
	_, err = s.Network.GetEvmClient().EstimateGas(s.Network.GetContext(), &types.EthCallRequest{
		Args:            contractArgsBz,
		GasCap:          config.DefaultGasCap,
		ProposerAddress: s.Network.GetContext().BlockHeader().ProposerAddress,
	})
	s.Require().NoError(err)

	s.Require().Equal(first.Gas, second.Gas)
	s.Require().Greater(first.Gas, ethparams.TxGas)
	s.Require().True(sawPostState, "hook must see EVM post-state")
	s.Require().True(sawTrace, "hook must see the candidate tx trace")
	s.Require().True(sawCleanCandidate, "each probe must start without prior hook state")
	s.Require().True(s.Network.App.GetBankKeeper().GetBalance(s.Network.GetContext(), moduleAddr, mintedDenom).IsZero())
	touches, transfers := evmKeeper.GetTxTrace(s.Network.GetContext(), 0)
	s.Require().Empty(touches)
	s.Require().Empty(transfers)
	s.Require().Equal(recipientBefore, evmKeeper.GetBalance(s.Network.GetContext(), recipient))
}

func (s *KeeperTestSuite) TestEstimateGasHookSeesStateOverrideWithoutLeakingIt() {
	s.SetupTest()

	sender := s.Keyring.GetAddr(0)
	recipient := s.Keyring.GetAddr(1)
	evmKeeper := s.Network.App.GetEVMKeeper()
	recipientBefore := new(uint256.Int).Set(evmKeeper.GetBalance(s.Network.GetContext(), recipient))
	var sawOverriddenPostState bool
	hook := &testHooks{
		postProcessing: func(ctx sdk.Context, _ common.Address, _ core.Message, _ *gethtypes.Receipt) error {
			balance := evmKeeper.GetBalance(ctx, recipient)
			sawOverriddenPostState = balance.Cmp(uint256.NewInt(100)) == 0
			return nil
		},
	}
	evmKeeper.SetHooks(keeper.NewMultiEvmHooks(hook))

	zeroGasPrice := hexutil.Big(*big.NewInt(0))
	value := hexutil.Big(*big.NewInt(100))
	argsBz, err := json.Marshal(types.TransactionArgs{
		From:     &sender,
		To:       &recipient,
		Value:    &value,
		GasPrice: &zeroGasPrice,
	})
	s.Require().NoError(err)
	overrides := []byte(fmt.Sprintf(`{"%s":{"balance":"0x0"}}`, recipient.Hex()))

	estimate, err := s.Network.GetEvmClient().EstimateGas(s.Network.GetContext(), &types.EthCallRequest{
		Args:            argsBz,
		GasCap:          config.DefaultGasCap,
		ProposerAddress: s.Network.GetContext().BlockHeader().ProposerAddress,
		Overrides:       overrides,
	})
	s.Require().NoError(err)
	s.Require().GreaterOrEqual(estimate.Gas, ethparams.TxGas)
	s.Require().True(sawOverriddenPostState)
	s.Require().Equal(recipientBefore, evmKeeper.GetBalance(s.Network.GetContext(), recipient))
}

func (s *KeeperTestSuite) TestEstimateGasWithStateOverrides() {
	// Hardcode recipient address to avoid non determinism in tests
	hardcodedRecipient := common.HexToAddress("0xC6Fe5D33615a1C52c08018c47E8Bc53646A0E101")

	erc20Contract, err := testdata.LoadERC20Contract()
	s.Require().NoError(err)

	testCases := []struct {
		msg             string
		getArgs         func() types.TransactionArgs
		getOverrides    func() string
		expPass         bool
		expGas          uint64
		EnableFeemarket bool
		gasCap          uint64
	}{
		{
			"success - native transfer with balance override",
			func() types.TransactionArgs {
				addr := s.Keyring.GetAddr(0)
				recipient := common.HexToAddress("0x963EBDf2e1f8DB8707D05FC75bfeFFBa1B5BaC17")
				return types.TransactionArgs{
					From:  &addr,
					To:    &recipient,
					Value: (*hexutil.Big)(big.NewInt(10000000000000000)), // 0.01 ether
				}
			},
			func() string {
				// Override recipient's balance to 0
				return `{
					"0x963EBDf2e1f8DB8707D05FC75bfeFFBa1B5BaC17": {
						"balance": "0x0"
					}
				}`
			},
			true,
			ethparams.TxGas,
			false,
			config.DefaultGasCap,
		},
		{
			"success - erc20 transfer with code and storage override",
			func() types.TransactionArgs {
				addr := s.Keyring.GetAddr(0)
				contractAddr := common.HexToAddress("0x5555555555555555555555555555555555555555")

				// Prepare transfer(address,uint256) call data
				// 100 TOKEN with 18 decimals
				amount := new(big.Int)
				amount.SetString("100000000000000000000", 10)
				transferData, err := erc20Contract.ABI.Pack(
					"transfer",
					hardcodedRecipient,
					amount,
				)
				s.Require().NoError(err)

				return types.TransactionArgs{
					From:  &addr,
					To:    &contractAddr,
					Input: (*hexutil.Bytes)(&transferData),
				}
			},
			func() string {
				// Override contract code and sender's balance in ERC20 contract
				// Storage slot for balances[sender] - simplified for testing
				erc20Contract, err := testdata.LoadERC20Contract()
				s.Require().NoError(err)

				sender := s.Keyring.GetAddr(0)
				slot := crypto.Keccak256Hash(
					common.LeftPadBytes(sender.Bytes(), common.HashLength),
					make([]byte, common.HashLength),
				)

				amount := new(big.Int)
				amount.SetString("100000000000000000000", 10)

				contractHex := hex.EncodeToString(erc20Contract.Bin)
				runtimeIdx := strings.Index(contractHex, "f3fe")
				s.Require().Greater(runtimeIdx, -1)
				runtimeHex := contractHex[runtimeIdx+4:]

				overrides := map[string]map[string]interface{}{
					"0x5555555555555555555555555555555555555555": {
						"code": "0x" + runtimeHex,
						"stateDiff": map[string]string{
							slot.Hex(): fmt.Sprintf("0x%064x", amount),
						},
					},
				}

				bz, err := json.Marshal(overrides)
				s.Require().NoError(err)

				return string(bz)
			},
			true,
			49140,
			false,
			config.DefaultGasCap,
		},
		{
			"success - override account nonce",
			func() types.TransactionArgs {
				addr := s.Keyring.GetAddr(0)
				return types.TransactionArgs{
					From:  &addr,
					To:    &common.Address{},
					Value: (*hexutil.Big)(big.NewInt(100)),
				}
			},
			func() string {
				addr := s.Keyring.GetAddr(0)
				return fmt.Sprintf(`{
					"%s": {
						"nonce": "0x10"
					}
				}`, addr.Hex())
			},
			true,
			ethparams.TxGas,
			false,
			config.DefaultGasCap,
		},
	}

	for _, tc := range testCases {
		s.Run(fmt.Sprintf("Case %s", tc.msg), func() {
			// Start from a clean state
			s.Require().NoError(s.Network.NextBlock())

			// Update feemarket params per test
			evmParams := feemarkettypes.DefaultParams()
			if !tc.EnableFeemarket {
				evmParams = s.Network.App.GetFeeMarketKeeper().GetParams(
					s.Network.GetContext(),
				)
				evmParams.NoBaseFee = true
			}

			err := s.Network.App.GetFeeMarketKeeper().SetParams(
				s.Network.GetContext(),
				evmParams,
			)
			s.Require().NoError(err)

			// Get call args
			args := tc.getArgs()
			marshalArgs, err := json.Marshal(args)
			s.Require().NoError(err)

			// Get overrides
			overrides := json.RawMessage(tc.getOverrides())

			req := types.EthCallRequest{
				Args:            marshalArgs,
				GasCap:          tc.gasCap,
				ProposerAddress: s.Network.GetContext().BlockHeader().ProposerAddress,
				Overrides:       overrides,
			}

			// Function under test
			rsp, err := s.Network.GetEvmClient().EstimateGas(
				s.Network.GetContext(),
				&req,
			)
			if tc.expPass {
				s.Require().NoError(err)
				s.Require().Equal(int64(tc.expGas), int64(rsp.Gas)) //#nosec G115
			} else {
				s.Require().Error(err)
			}
		})
	}
}

func getDefaultTraceTxRequest(unitNetwork network.Network) *types.QueryTraceTxRequest {
	ctx := unitNetwork.GetContext()
	chainID := unitNetwork.GetEIP155ChainID().Int64()
	return &types.QueryTraceTxRequest{
		BlockMaxGas: ctx.ConsensusParams().Block.MaxGas,
		ChainId:     chainID,
		BlockTime:   ctx.BlockTime(),
		TraceConfig: &types.TraceConfig{},
	}
}

func (s *KeeperTestSuite) TestTraceTx() {
	s.EnableFeemarket = true
	defer func() { s.EnableFeemarket = false }()
	s.SetupTest()

	// Hardcode recipient address to avoid non determinism in tests
	hardcodedRecipient := common.HexToAddress("0xC6Fe5D33615a1C52c08018c47E8Bc53646A0E101")

	erc20Contract, err := testdata.LoadERC20Contract()
	s.Require().NoError(err)

	testCases := []struct {
		msg             string
		malleate        func()
		getRequest      func() *types.QueryTraceTxRequest
		getPredecessors func() []*types.MsgEthereumTx
		expPass         bool
		expPanics       bool
		expectedTrace   string
	}{
		{
			msg: "default trace",
			getRequest: func() *types.QueryTraceTxRequest {
				return getDefaultTraceTxRequest(s.Network)
			},
			getPredecessors: func() []*types.MsgEthereumTx {
				return nil
			},
			expPass: true,
			expectedTrace: "{\"gas\":34780,\"failed\":false," +
				"\"returnValue\":\"0x0000000000000000000000000000000000000000000000000000000000000001\"," +
				"\"structLogs\":[{\"pc\":0,\"op\":\"PUSH1\",\"gas",
		},
		{
			msg: "default trace with filtered response",
			getRequest: func() *types.QueryTraceTxRequest {
				defaultRequest := getDefaultTraceTxRequest(s.Network)
				defaultRequest.TraceConfig = &types.TraceConfig{
					DisableStack:   true,
					DisableStorage: true,
					EnableMemory:   false,
				}
				return defaultRequest
			},
			getPredecessors: func() []*types.MsgEthereumTx {
				return nil
			},
			expPass: true,
			expectedTrace: "{\"gas\":34780,\"failed\":false," +
				"\"returnValue\":\"0x0000000000000000000000000000000000000000000000000000000000000001\"," +
				"\"structLogs\":[{\"pc\":0,\"op\":\"PUSH1\",\"gas",
		},
		{
			msg: "javascript tracer",
			getRequest: func() *types.QueryTraceTxRequest {
				traceConfig := &types.TraceConfig{
					Tracer: "{data: [], fault: function(log) {}, step: function(log) { if(log.op.toString() == \"CALL\") this.data.push(log.stack.peek(0)); }, result: function() { return this.data; }}",
				}
				defaultRequest := getDefaultTraceTxRequest(s.Network)
				defaultRequest.TraceConfig = traceConfig
				return defaultRequest
			},
			getPredecessors: func() []*types.MsgEthereumTx {
				return nil
			},
			expPass:       true,
			expectedTrace: "[]",
		},
		{
			msg: "default tracer with predecessors",
			getRequest: func() *types.QueryTraceTxRequest {
				return getDefaultTraceTxRequest(s.Network)
			},
			getPredecessors: func() []*types.MsgEthereumTx {
				// create predecessor tx
				// Use different address to avoid nonce collision
				senderKey := s.Keyring.GetKey(1)
				contractAddr, err := deployErc20Contract(senderKey, s.Factory)
				s.Require().NoError(err)

				err = s.Network.NextBlock()
				s.Require().NoError(err)

				txMsg, err := executeTransferCall(
					transferParams{
						senderKey:     senderKey,
						contractAddr:  contractAddr,
						recipientAddr: hardcodedRecipient,
					},
					s.Factory,
				)
				s.Require().NoError(err)

				return []*types.MsgEthereumTx{txMsg}
			},
			expPass: true,
			expectedTrace: "{\"gas\":34780,\"failed\":false," +
				"" + "\"returnValue\":\"0x0000000000000000000000000000000000000000000000000000000000000001\"," +
				"" + "\"structLogs\":[{\"pc\":0,\"op\":\"PUSH1\",\"gas",
		},
		{
			msg: "invalid too many predecessors",
			getRequest: func() *types.QueryTraceTxRequest {
				return getDefaultTraceTxRequest(s.Network)
			},
			getPredecessors: func() []*types.MsgEthereumTx {
				pred := make([]*types.MsgEthereumTx, 10001)
				for i := 0; i < 10001; i++ {
					pred[i] = &types.MsgEthereumTx{}
				}

				return pred
			},
			expPass: false,
		},
		{
			msg: "no panic when gas limit exceeded for predecessors",
			getRequest: func() *types.QueryTraceTxRequest {
				return getDefaultTraceTxRequest(s.Network)
			},
			getPredecessors: func() []*types.MsgEthereumTx {
				// Create predecessor tx
				// Use different address to avoid nonce collision
				senderKey := s.Keyring.GetKey(1)
				contractAddr, err := deployErc20Contract(senderKey, s.Factory)
				s.Require().NoError(err)
				s.Require().NoError(s.Network.NextBlock())
				numTxs := 1500
				txs := make([]*types.MsgEthereumTx, 0, numTxs)
				for range numTxs {
					txMsg := buildTransferTx(
						s.T(),
						transferParams{
							senderKey:     senderKey,
							contractAddr:  contractAddr,
							recipientAddr: hardcodedRecipient,
						},
						s.Factory,
					)
					txs = append(txs, txMsg)
				}
				return txs
			},
			expPanics: false,
			expPass:   true,
			expectedTrace: "{\"gas\":34780,\"failed\":false," +
				"\"returnValue\":\"0x0000000000000000000000000000000000000000000000000000000000000001\"," +
				"\"structLogs\":[{\"pc\":0,\"op\":\"PUSH1\",\"gas",
		},
		{
			msg: "error when requested block num greater than chain height",
			getRequest: func() *types.QueryTraceTxRequest {
				req := getDefaultTraceTxRequest(s.Network)
				req.BlockNumber = math.MaxInt64
				return req
			},
			getPredecessors: func() []*types.MsgEthereumTx {
				return nil
			},
			expPass: false,
		},
		{
			msg: "invalid trace config - Negative Limit",
			getRequest: func() *types.QueryTraceTxRequest {
				defaultRequest := getDefaultTraceTxRequest(s.Network)
				defaultRequest.TraceConfig = &types.TraceConfig{
					DisableStack:   true,
					DisableStorage: true,
					EnableMemory:   false,
					Limit:          -1,
				}
				return defaultRequest
			},
			getPredecessors: func() []*types.MsgEthereumTx {
				return nil
			},
			expPass: false,
		},
		{
			msg: "invalid trace config - Invalid Tracer",
			getRequest: func() *types.QueryTraceTxRequest {
				defaultRequest := getDefaultTraceTxRequest(s.Network)
				defaultRequest.TraceConfig = &types.TraceConfig{
					Tracer: "invalid_tracer",
				}
				return defaultRequest
			},
			getPredecessors: func() []*types.MsgEthereumTx {
				return nil
			},
			expPass: false,
		},
		{
			msg: "invalid trace config - Invalid Timeout",
			getRequest: func() *types.QueryTraceTxRequest {
				defaultRequest := getDefaultTraceTxRequest(s.Network)
				defaultRequest.TraceConfig = &types.TraceConfig{
					DisableStack:   true,
					DisableStorage: true,
					EnableMemory:   false,
					Timeout:        "wrong_time",
				}
				return defaultRequest
			},
			getPredecessors: func() []*types.MsgEthereumTx {
				return nil
			},
			expPass: false,
		},
		{
			msg: "default tracer with contract creation tx as predecessor but 'create' param disabled",
			getRequest: func() *types.QueryTraceTxRequest {
				return getDefaultTraceTxRequest(s.Network)
			},
			getPredecessors: func() []*types.MsgEthereumTx {
				// use different address to avoid nonce collision
				senderKey := s.Keyring.GetKey(1)

				constructorArgs := []interface{}{
					senderKey.Addr,
					sdkmath.NewIntWithDecimal(1000, 18).BigInt(),
				}
				compiledContract := erc20Contract
				deploymentData := testutiltypes.ContractDeploymentData{
					Contract:        compiledContract,
					ConstructorArgs: constructorArgs,
				}

				txArgs, err := s.Factory.GenerateDeployContractArgs(senderKey.Addr, types.EvmTxArgs{}, deploymentData)
				s.Require().NoError(err)

				txMsg, err := s.Factory.GenerateMsgEthereumTx(senderKey.Priv, txArgs)
				s.Require().NoError(err)

				_, err = s.Factory.ExecuteEthTx(
					senderKey.Priv,
					txArgs, // Default values
				)
				s.Require().NoError(err)

				params := s.Network.App.GetEVMKeeper().GetParams(s.Network.GetContext())
				params.AccessControl = types.AccessControl{
					Create: types.AccessControlType{
						AccessType: types.AccessTypeRestricted,
					},
				}
				err = s.Network.App.GetEVMKeeper().SetParams(s.Network.GetContext(), params)
				s.Require().NoError(err)
				return []*types.MsgEthereumTx{&txMsg}
			},
			expPass: true,
			expectedTrace: "{\"gas\":34780,\"failed\":false," +
				"" + "\"returnValue\":\"0x0000000000000000000000000000000000000000000000000000000000000001\"," +
				"" + "\"structLogs\":[{\"pc\":0,\"op\":\"PUSH1\",\"gas",
			// expFinalGas:   26744, // gas consumed in traceTx setup (GetProposerAddr + CalculateBaseFee) + gas consumed in malleate func
		},
		{
			msg: "Empty request",
			getRequest: func() *types.QueryTraceTxRequest {
				return nil
			},
			getPredecessors: func() []*types.MsgEthereumTx {
				return nil
			},
			expPass: false,
		},
	}

	for _, tc := range testCases {
		s.Run(fmt.Sprintf("Case %s", tc.msg), func() {
			// Clean up per test
			defaultEvmParams := types.DefaultParams()
			err := s.Network.App.GetEVMKeeper().SetParams(s.Network.GetContext(), defaultEvmParams)
			s.Require().NoError(err)

			err = s.Network.NextBlock()
			s.Require().NoError(err)

			// ----- Contract Deployment -----
			senderKey := s.Keyring.GetKey(0)
			contractAddr, err := deployErc20Contract(senderKey, s.Factory)
			s.Require().NoError(err)

			err = s.Network.NextBlock()
			s.Require().NoError(err)

			// --- Add predecessor ---
			predecessors := tc.getPredecessors()

			// Get the message to trace
			msgToTrace, err := executeTransferCall(
				transferParams{
					senderKey:     senderKey,
					contractAddr:  contractAddr,
					recipientAddr: hardcodedRecipient,
				},
				s.Factory,
			)
			s.Require().NoError(err)

			s.Require().NoError(s.Network.NextBlock())

			// Get the trace request
			traceReq := tc.getRequest()
			if traceReq != nil {
				// Add predecessor to trace request
				traceReq.Predecessors = predecessors
				traceReq.Msg = msgToTrace
			}

			if tc.expPanics {
				s.Require().Panics(func() {
					//nolint:errcheck // we just want this to panic.
					s.Network.GetEvmClient().TraceTx(
						s.Network.GetContext(),
						traceReq,
					)
				})
				return
			}

			// Function under test
			res, err := s.Network.GetEvmClient().TraceTx(
				s.Network.GetContext(),
				traceReq,
			)
			if tc.expPass {
				s.Require().NoError(err)

				// if data is to big, slice the result
				if len(res.Data) > 150 {
					s.Require().Equal(tc.expectedTrace, string(res.Data[:150]))
				} else {
					s.Require().Equal(tc.expectedTrace, string(res.Data))
				}
				if traceReq.TraceConfig == nil || traceReq.TraceConfig.Tracer == "" {
					var result ethlogger.ExecutionResult
					s.Require().NoError(json.Unmarshal(res.Data, &result))
					s.Require().Positive(result.Gas)
				}
			} else {
				s.Require().Error(err)
			}
		})
	}
}

func (s *KeeperTestSuite) TestTraceBlock() {
	s.EnableFeemarket = true
	defer func() { s.EnableFeemarket = false }()
	s.SetupTest()

	// Hardcode recipient to make gas estimation deterministic
	hardcodedTransferRecipient := common.HexToAddress("0xC6Fe5D33615a1C52c08018c47E8Bc53646A0E101")

	testCases := []struct {
		msg              string
		getRequest       func() types.QueryTraceBlockRequest
		getAdditionalTxs func() []*types.MsgEthereumTx
		expPass          bool
		traceResponse    string
	}{
		{
			msg: "default trace",
			getRequest: func() types.QueryTraceBlockRequest {
				return getDefaultTraceBlockRequest(s.Network)
			},
			getAdditionalTxs: func() []*types.MsgEthereumTx {
				return nil
			},
			expPass: true,
			traceResponse: "[{\"result\":{\"gas\":34780,\"failed\":false," +
				"\"returnValue\":\"0x0000000000000000000000000000000000000000000000000000000000000001\"," +
				"\"structLogs\":[{\"pc\":0,\"op\":\"PU",
		},
		{
			msg: "filtered trace",
			getRequest: func() types.QueryTraceBlockRequest {
				defaultReq := getDefaultTraceBlockRequest(s.Network)
				defaultReq.TraceConfig = &types.TraceConfig{
					DisableStack:   true,
					DisableStorage: true,
					EnableMemory:   false,
				}
				return defaultReq
			},
			getAdditionalTxs: func() []*types.MsgEthereumTx {
				return nil
			},
			expPass: true,
			traceResponse: "[{\"result\":{\"gas\":34780,\"failed\":false," +
				"\"returnValue\":\"0x0000000000000000000000000000000000000000000000000000000000000001\"," +
				"\"structLogs\":[{\"pc\":0,\"op\":\"PU",
		},
		{
			msg: "javascript tracer",
			getRequest: func() types.QueryTraceBlockRequest {
				defaultReq := getDefaultTraceBlockRequest(s.Network)
				defaultReq.TraceConfig = &types.TraceConfig{
					Tracer: "{data: [], fault: function(log) {}, step: function(log) { if(log.op.toString() == \"CALL\") this.data.push(log.stack.peek(0)); }, result: function() { return this.data; }}",
				}
				return defaultReq
			},
			getAdditionalTxs: func() []*types.MsgEthereumTx {
				return nil
			},
			expPass:       true,
			traceResponse: "[{\"result\":[]}]",
		},
		{
			msg: "tracer with multiple transactions",
			getRequest: func() types.QueryTraceBlockRequest {
				return getDefaultTraceBlockRequest(s.Network)
			},
			getAdditionalTxs: func() []*types.MsgEthereumTx {
				// create predecessor tx
				// Use different address to avoid nonce collision
				senderKey := s.Keyring.GetKey(1)
				contractAddr, err := deployErc20Contract(senderKey, s.Factory)
				s.Require().NoError(err)

				err = s.Network.NextBlock()
				s.Require().NoError(err)

				firstTransferMessage, err := executeTransferCall(
					transferParams{
						senderKey:     s.Keyring.GetKey(1),
						contractAddr:  contractAddr,
						recipientAddr: hardcodedTransferRecipient,
					},
					s.Factory,
				)
				s.Require().NoError(err)
				return []*types.MsgEthereumTx{firstTransferMessage}
			},
			expPass: true,
			traceResponse: "[{\"result\":{\"gas\":34780,\"failed\":false," +
				"\"returnValue\":\"0x0000000000000000000000000000000000000000000000000000000000000001\"," +
				"\"structLogs\":[{\"pc\":0,\"op\":\"PU",
		},
		{
			msg: "invalid trace config - Negative Limit",
			getRequest: func() types.QueryTraceBlockRequest {
				defaultReq := getDefaultTraceBlockRequest(s.Network)
				defaultReq.TraceConfig = &types.TraceConfig{
					Limit: -1,
				}
				return defaultReq
			},
			getAdditionalTxs: func() []*types.MsgEthereumTx {
				return nil
			},
			expPass: false,
		},
		{
			msg: "invalid trace config - Invalid Tracer",
			getRequest: func() types.QueryTraceBlockRequest {
				defaultReq := getDefaultTraceBlockRequest(s.Network)
				defaultReq.TraceConfig = &types.TraceConfig{
					Tracer: "invalid_tracer",
				}
				return defaultReq
			},
			getAdditionalTxs: func() []*types.MsgEthereumTx {
				return nil
			},
			expPass: true,
			traceResponse: "[{\"error\":\"rpc error: code = Internal desc = ReferenceError: invalid_tracer is not" +
				" defined",
		},
	}

	for _, tc := range testCases {
		s.Run(fmt.Sprintf("Case %s", tc.msg), func() {
			// Start from fresh block
			s.Require().NoError(s.Network.NextBlock())

			// ----- Contract Deployment -----
			senderKey := s.Keyring.GetKey(0)
			contractAddr, err := deployErc20Contract(senderKey, s.Factory)
			s.Require().NoError(err)

			err = s.Network.NextBlock()
			s.Require().NoError(err)

			// --- Add predecessor ---
			txs := tc.getAdditionalTxs()

			// --- Contract Call ---
			msgToTrace, err := executeTransferCall(
				transferParams{
					senderKey:     senderKey,
					contractAddr:  contractAddr,
					recipientAddr: hardcodedTransferRecipient,
				},
				s.Factory,
			)
			s.Require().NoError(err)
			txs = append(txs, msgToTrace)

			s.Require().NoError(s.Network.NextBlock())

			// Get the trace request
			traceReq := tc.getRequest()
			// Add txs to trace request
			traceReq.Txs = txs

			res, err := s.Network.GetEvmClient().TraceBlock(s.Network.GetContext(), &traceReq)

			if tc.expPass {
				s.Require().NoError(err)
				// if data is too big, slice the result
				if len(res.Data) > 200 {
					s.Require().Contains(string(res.Data[:200]), tc.traceResponse)
				} else {
					s.Require().Contains(string(res.Data), tc.traceResponse)
				}
			} else {
				s.Require().Error(err)
			}
		})
	}
}

func (s *KeeperTestSuite) TestTraceCall() {
	s.EnableFeemarket = true
	defer func() { s.EnableFeemarket = false }()
	s.SetupTest()

	// Load ERC20 contract
	erc20Contract, err := testdata.LoadERC20Contract()
	s.Require().NoError(err)

	// Deploy ERC20 contract for testing
	senderKey := s.Keyring.GetKey(0)
	contractAddr, err := deployErc20Contract(senderKey, s.Factory)
	s.Require().NoError(err)
	s.Require().NoError(s.Network.NextBlock())

	balanceSlot := crypto.Keccak256Hash(
		common.LeftPadBytes(senderKey.Addr.Bytes(), 32),
		common.LeftPadBytes(big.NewInt(0).Bytes(), 32),
	)

	testCases := []struct {
		msg               string
		getCallArgs       func() []byte
		traceCallConfig   *rpctypes.TraceCallConfig
		expPass           bool
		traceResponse     string
		postTraceResponse string
	}{
		{
			msg: "default trace with contract call",
			getCallArgs: func() []byte {
				// Prepare transfer call data
				callArgs := testutiltypes.CallArgs{
					ContractABI: erc20Contract.ABI,
					MethodName:  "transfer",
					Args:        []interface{}{common.HexToAddress("0xC6Fe5D33615a1C52c08018c47E8Bc53646A0E101"), big.NewInt(1000)},
				}
				input, err := factory.GenerateContractCallArgs(callArgs)
				s.Require().NoError(err)
				return input
			},
			expPass:       true,
			traceResponse: "\"gas\":",
		},
		{
			msg: "callTracer with contract call",
			getCallArgs: func() []byte {
				callArgs := testutiltypes.CallArgs{
					ContractABI: erc20Contract.ABI,
					MethodName:  "balanceOf",
					Args:        []interface{}{senderKey.Addr},
				}
				input, err := factory.GenerateContractCallArgs(callArgs)
				s.Require().NoError(err)
				return input
			},
			traceCallConfig: &rpctypes.TraceCallConfig{
				TraceConfig: rpctypes.TraceConfig{
					TraceConfig: types.TraceConfig{
						Tracer: "callTracer",
					},
				},
			},
			expPass:       true,
			traceResponse: "\"type\":\"CALL\"",
		},
		{
			msg: "prestateTracer with contract call",
			getCallArgs: func() []byte {
				callArgs := testutiltypes.CallArgs{
					ContractABI: erc20Contract.ABI,
					MethodName:  "balanceOf",
					Args:        []interface{}{senderKey.Addr},
				}
				input, err := factory.GenerateContractCallArgs(callArgs)
				s.Require().NoError(err)
				return input
			},
			traceCallConfig: &rpctypes.TraceCallConfig{
				TraceConfig: rpctypes.TraceConfig{
					TraceConfig: types.TraceConfig{
						Tracer: "prestateTracer",
					},
				},
			},
			expPass:       true,
			traceResponse: "\"balance\":",
		},
		{
			msg: "trace with filtered options",
			getCallArgs: func() []byte {
				callArgs := testutiltypes.CallArgs{
					ContractABI: erc20Contract.ABI,
					MethodName:  "balanceOf",
					Args:        []interface{}{senderKey.Addr},
				}
				input, err := factory.GenerateContractCallArgs(callArgs)
				s.Require().NoError(err)
				return input
			},
			traceCallConfig: &rpctypes.TraceCallConfig{
				TraceConfig: rpctypes.TraceConfig{
					TraceConfig: types.TraceConfig{
						DisableStack:     true,
						DisableStorage:   true,
						EnableMemory:     false,
						EnableReturnData: true,
					},
				},
			},
			expPass:       true,
			traceResponse: "\"returnValue\":",
		},
		{
			msg: "javascript tracer",
			getCallArgs: func() []byte {
				callArgs := testutiltypes.CallArgs{
					ContractABI: erc20Contract.ABI,
					MethodName:  "balanceOf",
					Args:        []interface{}{senderKey.Addr},
				}
				input, err := factory.GenerateContractCallArgs(callArgs)
				s.Require().NoError(err)
				return input
			},
			traceCallConfig: &rpctypes.TraceCallConfig{
				TraceConfig: rpctypes.TraceConfig{
					TraceConfig: types.TraceConfig{
						Tracer: "{data: [], fault: function(log) {}, step: function(log) { if(log.op.toString() == \"CALL\") this.data.push(log.stack.peek(0)); }, result: function() { return this.data; }}",
					},
				},
			},
			expPass:       true,
			traceResponse: "[",
		},
		{
			msg: "trace simple value transfer",
			getCallArgs: func() []byte {
				return nil // Simple value transfer, no data
			},
			traceCallConfig: &rpctypes.TraceCallConfig{
				TraceConfig: rpctypes.TraceConfig{
					TraceConfig: types.TraceConfig{
						Tracer: "callTracer",
					},
				},
			},
			expPass:       true,
			traceResponse: "\"type\":\"CALL\"",
		},
		{
			msg: "invalid trace config - Negative Limit",
			getCallArgs: func() []byte {
				return nil
			},
			traceCallConfig: &rpctypes.TraceCallConfig{
				TraceConfig: rpctypes.TraceConfig{
					TraceConfig: types.TraceConfig{
						Limit: -1,
					},
				},
			},
			expPass: false,
		},
		{
			msg: "invalid trace config - Invalid Tracer",
			getCallArgs: func() []byte {
				return nil
			},
			traceCallConfig: &rpctypes.TraceCallConfig{
				TraceConfig: rpctypes.TraceConfig{
					TraceConfig: types.TraceConfig{
						Tracer: "invalid_tracer",
					},
				},
			},
			expPass: false,
		},
		{
			msg: "invalid trace config - Invalid Timeout",
			getCallArgs: func() []byte {
				return nil
			},
			traceCallConfig: &rpctypes.TraceCallConfig{
				TraceConfig: rpctypes.TraceConfig{
					TraceConfig: types.TraceConfig{
						Timeout: "wrong_time",
					},
				},
			},
			expPass: false,
		},
		{
			msg: "trace call without state override",
			getCallArgs: func() []byte {
				callArgs := testutiltypes.CallArgs{
					ContractABI: erc20Contract.ABI,
					MethodName:  "balanceOf",
					Args:        []interface{}{senderKey.Addr},
				}
				input, err := factory.GenerateContractCallArgs(callArgs)
				s.Require().NoError(err)
				return input
			},
			traceCallConfig: &rpctypes.TraceCallConfig{
				TraceConfig: rpctypes.TraceConfig{
					TraceConfig: types.TraceConfig{
						Tracer: "callTracer",
					},
				},
			},
			expPass:       true,
			traceResponse: `"output":"0x00000000000000000000000000000000000000000000003635c9adc5dea00000"`,
		},
		{
			msg: "trace call with balance override",
			getCallArgs: func() []byte {
				callArgs := testutiltypes.CallArgs{
					ContractABI: erc20Contract.ABI,
					MethodName:  "balanceOf",
					Args:        []interface{}{senderKey.Addr},
				}
				input, err := factory.GenerateContractCallArgs(callArgs)
				s.Require().NoError(err)
				return input
			},
			traceCallConfig: &rpctypes.TraceCallConfig{
				TraceConfig: rpctypes.TraceConfig{
					TraceConfig: types.TraceConfig{
						Tracer: "callTracer",
					},
				},
				StateOverrides: []byte(fmt.Sprintf(
					`{"%s":{"stateDiff":{"%s":"%s"}}}`,
					contractAddr.Hex(),
					balanceSlot.Hex(),
					common.BigToHash(big.NewInt(1)).Hex(),
				)),
			},
			expPass:           true,
			traceResponse:     `"output":"0x0000000000000000000000000000000000000000000000000000000000000001"`,
			postTraceResponse: `"output":"0x00000000000000000000000000000000000000000000003635c9adc5dea00000"`,
		},
		{
			msg: "invalid state overrides",
			getCallArgs: func() []byte {
				return []byte{}
			},
			traceCallConfig: &rpctypes.TraceCallConfig{
				TraceConfig: rpctypes.TraceConfig{
					TraceConfig: types.TraceConfig{
						Tracer: "callTracer",
					},
				},
				StateOverrides: []byte(fmt.Sprintf(`{"%s":{"code":`, contractAddr.Hex())),
			},
			expPass: false,
		},
	}

	for _, tc := range testCases {
		s.Run(fmt.Sprintf("Case %s", tc.msg), func() {
			// Get current block for tracing
			currentBlock := s.Network.GetContext().BlockHeight()

			// Build transaction args for the call
			callData := tc.getCallArgs()
			var to *common.Address
			if callData != nil {
				to = &contractAddr
			} else {
				// For simple transfers, use a different recipient
				recipient := common.HexToAddress("0xC6Fe5D33615a1C52c08018c47E8Bc53646A0E101")
				to = &recipient
			}

			// Marshal transaction args with default values
			gasLimit := hexutil.Uint64(100000)
			gasPrice := hexutil.Big(*big.NewInt(1000000000)) // 1 gwei
			defaultValue := hexutil.Big(*big.NewInt(0))

			txArgs := types.TransactionArgs{
				From:     &senderKey.Addr,
				To:       to,
				Gas:      &gasLimit,
				GasPrice: &gasPrice,
				Value:    &defaultValue,
				Input:    (*hexutil.Bytes)(&callData),
			}

			// If it's a value transfer test, add value
			if tc.msg == "trace simple value transfer" {
				value := hexutil.Big(*big.NewInt(1000))
				txArgs.Value = &value
			}

			argsBytes, err := json.Marshal(txArgs)
			s.Require().NoError(err)

			// Build trace request
			ctx := s.Network.GetContext()

			traceCallConfig := tc.traceCallConfig
			if traceCallConfig == nil {
				traceCallConfig = &rpctypes.TraceCallConfig{}
			}

			traceReq := &types.QueryTraceCallRequest{
				Args:            argsBytes,
				TraceConfig:     &traceCallConfig.TraceConfig.TraceConfig,
				BlockNumber:     currentBlock,
				BlockTime:       ctx.BlockTime(),
				BlockHash:       common.BytesToHash(ctx.HeaderHash()).Hex(),
				ProposerAddress: sdk.ConsAddress(ctx.BlockHeader().ProposerAddress),
				ChainId:         s.Network.GetEIP155ChainID().Int64(),
				Overrides:       traceCallConfig.StateOverrides,
			}

			// Execute trace call
			res, err := s.Network.GetEvmClient().TraceCall(s.Network.GetContext(), traceReq)

			if tc.expPass {
				s.Require().NoError(err)
				s.Require().NotNil(res)

				// Verify response contains expected trace data
				if tc.traceResponse != "" {
					s.Require().Contains(string(res.Data), tc.traceResponse)
				}

				if tc.postTraceResponse != "" {
					traceReq.Overrides = nil
					res, err = s.Network.GetEvmClient().TraceCall(s.Network.GetContext(), traceReq)
					s.Require().NoError(err)
					s.Require().Contains(string(res.Data), tc.postTraceResponse)
				}

				// For non-custom tracers, verify the result structure
				if traceCallConfig.Tracer == "" {
					var result ethlogger.ExecutionResult
					s.Require().NoError(json.Unmarshal(res.Data, &result))
					s.Require().NotNil(result.Gas)
				}
			} else {
				s.Require().Error(err)
			}
		})
	}
}

func (s *KeeperTestSuite) TestNonceInQuery() {
	s.EnableFeemarket = true
	defer func() { s.EnableFeemarket = false }()
	s.SetupTest()

	senderKey := s.Keyring.GetKey(0)
	nonce := s.Network.App.GetEVMKeeper().GetNonce(
		s.Network.GetContext(),
		senderKey.Addr,
	)
	s.Require().Equal(uint64(0), nonce)

	// accupy nonce 0
	contractAddr, err := deployErc20Contract(s.Keyring.GetKey(0), s.Factory)
	s.Require().NoError(err)

	erc20Contract, err := testdata.LoadERC20Contract()
	s.Require().NoError(err, "failed to load erc20 contract")

	// do an EthCall/EstimateGas with nonce 0
	ctorArgs, err := erc20Contract.ABI.Pack("", senderKey.Addr, big.NewInt(1000))
	s.Require().NoError(err)

	data := erc20Contract.Bin
	data = append(data, ctorArgs...)
	args, err := json.Marshal(&types.TransactionArgs{
		From: &senderKey.Addr,
		To:   &contractAddr,
		Data: (*hexutil.Bytes)(&data),
	})
	s.Require().NoError(err)

	proposerAddress := s.Network.GetContext().BlockHeader().ProposerAddress
	_, err = s.Network.GetEvmClient().EstimateGas(
		s.Network.GetContext(),
		&types.EthCallRequest{
			Args:            args,
			GasCap:          config.DefaultGasCap,
			ProposerAddress: proposerAddress,
		},
	)
	s.Require().NoError(err)

	_, err = s.Network.GetEvmClient().EthCall(
		s.Network.GetContext(),
		&types.EthCallRequest{
			Args:            args,
			GasCap:          config.DefaultGasCap,
			ProposerAddress: proposerAddress,
		},
	)
	s.Require().NoError(err)
}

func (s *KeeperTestSuite) TestQueryBaseFee() {
	s.EnableFeemarket = true
	defer func() { s.EnableFeemarket = false }()
	s.SetupTest()

	testCases := []struct {
		name       string
		getExpResp func() *types.QueryBaseFeeResponse
		setParams  func()
		expPass    bool
	}{
		{
			"pass - default Base Fee",
			func() *types.QueryBaseFeeResponse {
				initialBaseFee := sdkmath.NewInt(ethparams.InitialBaseFee)
				return &types.QueryBaseFeeResponse{BaseFee: &initialBaseFee}
			},
			func() {
				feemarketDefault := feemarkettypes.DefaultParams()
				s.Require().NoError(s.Network.App.GetFeeMarketKeeper().SetParams(s.Network.GetContext(), feemarketDefault))

				evmDefault := types.DefaultParams()
				s.Require().NoError(s.Network.App.GetEVMKeeper().SetParams(s.Network.GetContext(), evmDefault))
			},

			true,
		},
		{
			"pass - nil Base Fee when london hardfork not activated",
			func() *types.QueryBaseFeeResponse {
				return &types.QueryBaseFeeResponse{}
			},
			func() {
				feemarketDefault := feemarkettypes.DefaultParams()
				s.Require().NoError(s.Network.App.GetFeeMarketKeeper().SetParams(s.Network.GetContext(), feemarketDefault))

				chainConfig := types.DefaultChainConfig(s.Network.GetEIP155ChainID().Uint64())
				maxInt := sdkmath.NewInt(math.MaxInt64)
				chainConfig.LondonBlock = &maxInt
				chainConfig.ArrowGlacierBlock = &maxInt
				chainConfig.GrayGlacierBlock = &maxInt
				chainConfig.MergeNetsplitBlock = &maxInt
				chainConfig.ShanghaiTime = &maxInt
				chainConfig.CancunTime = &maxInt
				chainConfig.PragueTime = &maxInt

				configurator := types.NewEVMConfigurator()
				configurator.ResetTestConfig()
				err := types.SetChainConfig(chainConfig)
				s.Require().NoError(err)
				err = configurator.
					WithEVMCoinInfo(testconstants.ExampleChainCoinInfo[testconstants.ExampleChainID]).
					Configure()
				s.Require().NoError(err)
			},
			true,
		},
		{
			"pass - zero Base Fee when feemarket not activated",
			func() *types.QueryBaseFeeResponse {
				baseFee := sdkmath.ZeroInt()
				return &types.QueryBaseFeeResponse{BaseFee: &baseFee}
			},
			func() {
				feemarketDefault := feemarkettypes.DefaultParams()
				feemarketDefault.NoBaseFee = true
				s.Require().NoError(s.Network.App.GetFeeMarketKeeper().SetParams(s.Network.GetContext(), feemarketDefault))

				evmDefault := types.DefaultParams()
				s.Require().NoError(s.Network.App.GetEVMKeeper().SetParams(s.Network.GetContext(), evmDefault))
			},
			true,
		},
	}

	// Save initial configure to restore it between tests
	coinInfo := types.EvmCoinInfo{
		Denom:         types.GetEVMCoinDenom(),
		ExtendedDenom: types.GetEVMCoinExtendedDenom(),
		DisplayDenom:  types.GetEVMCoinDisplayDenom(),
		Decimals:      types.GetEVMCoinDecimals().Uint32(),
	}
	chainConfig := types.DefaultChainConfig(s.Network.GetEIP155ChainID().Uint64())

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			// Set necessary params
			tc.setParams()
			// Get the expected response
			expResp := tc.getExpResp()
			// Function under test
			res, err := s.Network.GetEvmClient().BaseFee(
				s.Network.GetContext(),
				&types.QueryBaseFeeRequest{},
			)
			if tc.expPass {
				s.Require().NotNil(res)
				s.Require().Equal(expResp, res, tc.name)
				s.Require().NoError(err)
			} else {
				s.Require().Error(err)
			}
			s.Require().NoError(s.Network.NextBlock())
			configurator := types.NewEVMConfigurator()
			configurator.ResetTestConfig()
			err = types.SetChainConfig(chainConfig)
			s.Require().NoError(err)
			err = configurator.
				WithEVMCoinInfo(coinInfo).
				Configure()
			s.Require().NoError(err)
		})
	}
}

func (s *KeeperTestSuite) TestEthCall() {
	s.SetupTest()

	erc20Contract, err := testdata.LoadERC20Contract()
	s.Require().NoError(err)

	// Generate common data for requests
	sender := s.Keyring.GetAddr(0)
	supply := sdkmath.NewIntWithDecimal(1000, 18).BigInt()
	ctorArgs, err := erc20Contract.ABI.Pack("", sender, supply)
	s.Require().NoError(err)
	data := erc20Contract.Bin
	data = append(data, ctorArgs...)

	testCases := []struct {
		name       string
		getReq     func() *types.EthCallRequest
		expVMError bool
	}{
		{
			"invalid args",
			func() *types.EthCallRequest {
				return &types.EthCallRequest{Args: []byte("invalid args"), GasCap: config.DefaultGasCap}
			},
			false,
		},
		{
			"invalid args - specified both gasPrice and maxFeePerGas",
			func() *types.EthCallRequest {
				hexBigInt := hexutil.Big(*big.NewInt(1))
				args, err := json.Marshal(&types.TransactionArgs{
					From:         &sender,
					Data:         (*hexutil.Bytes)(&data),
					GasPrice:     &hexBigInt,
					MaxFeePerGas: &hexBigInt,
				})
				s.Require().NoError(err)

				return &types.EthCallRequest{Args: args, GasCap: config.DefaultGasCap}
			},
			false,
		},
		{
			"set param AccessControl - no Access",
			func() *types.EthCallRequest {
				args, err := json.Marshal(&types.TransactionArgs{
					From: &sender,
					Data: (*hexutil.Bytes)(&data),
				})

				s.Require().NoError(err)
				req := &types.EthCallRequest{Args: args, GasCap: config.DefaultGasCap}

				params := s.Network.App.GetEVMKeeper().GetParams(s.Network.GetContext())
				params.AccessControl = types.AccessControl{
					Create: types.AccessControlType{
						AccessType: types.AccessTypeRestricted,
					},
				}
				err = s.Network.App.GetEVMKeeper().SetParams(s.Network.GetContext(), params)
				s.Require().NoError(err)
				return req
			},
			true,
		},
		{
			"set param AccessControl = non whitelist",
			func() *types.EthCallRequest {
				args, err := json.Marshal(&types.TransactionArgs{
					From: &sender,
					Data: (*hexutil.Bytes)(&data),
				})

				s.Require().NoError(err)
				req := &types.EthCallRequest{Args: args, GasCap: config.DefaultGasCap}

				params := s.Network.App.GetEVMKeeper().GetParams(s.Network.GetContext())
				params.AccessControl = types.AccessControl{
					Create: types.AccessControlType{
						AccessType: types.AccessTypePermissioned,
					},
				}
				err = s.Network.App.GetEVMKeeper().SetParams(s.Network.GetContext(), params)
				s.Require().NoError(err)
				return req
			},
			true,
		},
	}
	for _, tc := range testCases {
		s.Run(tc.name, func() {
			req := tc.getReq()

			res, err := s.Network.GetEvmClient().EthCall(s.Network.GetContext(), req)
			if tc.expVMError {
				s.Require().NotNil(res)
				s.Require().Contains(res.VmError, "does not have permission to deploy contracts")
			} else {
				s.Require().Error(err)
			}

			// Reset params
			defaultEvmParams := types.DefaultParams()
			err = s.Network.App.GetEVMKeeper().SetParams(s.Network.GetContext(), defaultEvmParams)
			s.Require().NoError(err)
		})
	}
}

func (s *KeeperTestSuite) TestBalance() {
	testCases := []struct {
		name        string
		returnedBal func() *uint256.Int
		expBalance  *uint256.Int
	}{
		{
			"Account method, vesting account (0 spendable, large locked balance)",
			func() *uint256.Int {
				addr := tx.GenerateAddress()
				accAddr := sdk.AccAddress(addr.Bytes())
				err := s.Network.App.GetBankKeeper().MintCoins(s.Network.GetContext(), "mint", sdk.NewCoins(sdk.NewCoin(s.Network.GetBaseDenom(), sdkmath.NewInt(100))))
				s.Require().NoError(err)
				err = s.Network.App.GetBankKeeper().SendCoinsFromModuleToAccount(s.Network.GetContext(), "mint", addr.Bytes(), sdk.NewCoins(sdk.NewCoin(s.Network.GetBaseDenom(), sdkmath.NewInt(100))))
				s.Require().NoError(err)

				// Make tx cost greater than balance
				balanceResp, err := s.Handler.GetBalanceFromEVM(accAddr)
				s.Require().NoError(err)

				balance, ok := sdkmath.NewIntFromString(balanceResp.Balance)
				s.Require().True(ok)
				s.Require().NotEqual(balance.String(), "0")

				// replace with vesting account
				ctx := s.Network.GetContext()
				baseAccount := s.Network.App.GetAccountKeeper().GetAccount(ctx, accAddr).(*authtypes.BaseAccount)
				baseDenom := s.Network.GetBaseDenom()
				currTime := s.Network.GetContext().BlockTime().Unix()
				acc, err := vestingtypes.NewContinuousVestingAccount(baseAccount, sdk.NewCoins(sdk.NewCoin(baseDenom, balance)), s.Network.GetContext().BlockTime().Unix(), currTime+100)
				s.Require().NoError(err)
				s.Network.App.GetAccountKeeper().SetAccount(ctx, acc)

				spendable := s.Network.App.GetBankKeeper().SpendableCoin(ctx, accAddr, baseDenom).Amount
				s.Require().Equal(spendable.String(), "0")

				evmBalanceRes, err := s.Handler.GetBalanceFromEVM(accAddr)
				s.Require().NoError(err)
				evmBalance := evmBalanceRes.Balance
				s.Require().Equal(evmBalance, "0")

				totalBalance := s.Network.App.GetBankKeeper().GetBalance(ctx, accAddr, baseDenom)
				s.Require().Equal(totalBalance.Amount, balance)

				res, err := s.Network.App.GetEVMKeeper().Account(s.Network.GetContext(), &types.QueryAccountRequest{Address: addr.String()})
				s.Require().NoError(err)
				bal, err := uint256.FromDecimal(res.Balance)
				s.Require().NoError(err)
				return bal
			},
			&uint256.Int{0},
		},
		{
			"Balance method, vesting account (0 spendable, large locked balance)",
			func() *uint256.Int {
				addr := tx.GenerateAddress()
				accAddr := sdk.AccAddress(addr.Bytes())
				err := s.Network.App.GetBankKeeper().MintCoins(s.Network.GetContext(), "mint", sdk.NewCoins(sdk.NewCoin(s.Network.GetBaseDenom(), sdkmath.NewInt(100))))
				s.Require().NoError(err)
				err = s.Network.App.GetBankKeeper().SendCoinsFromModuleToAccount(s.Network.GetContext(), "mint", addr.Bytes(), sdk.NewCoins(sdk.NewCoin(s.Network.GetBaseDenom(), sdkmath.NewInt(100))))
				s.Require().NoError(err)

				// Make tx cost greater than balance
				balanceResp, err := s.Handler.GetBalanceFromEVM(accAddr)
				s.Require().NoError(err)

				balance, ok := sdkmath.NewIntFromString(balanceResp.Balance)
				s.Require().True(ok)
				s.Require().NotEqual(balance.String(), "0")

				// replace with vesting account
				ctx := s.Network.GetContext()
				baseAccount := s.Network.App.GetAccountKeeper().GetAccount(ctx, accAddr).(*authtypes.BaseAccount)
				baseDenom := s.Network.GetBaseDenom()
				currTime := s.Network.GetContext().BlockTime().Unix()
				acc, err := vestingtypes.NewContinuousVestingAccount(baseAccount, sdk.NewCoins(sdk.NewCoin(baseDenom, balance)), s.Network.GetContext().BlockTime().Unix(), currTime+100)
				s.Require().NoError(err)
				s.Network.App.GetAccountKeeper().SetAccount(ctx, acc)

				spendable := s.Network.App.GetBankKeeper().SpendableCoin(ctx, accAddr, baseDenom).Amount
				s.Require().Equal(spendable.String(), "0")

				evmBalanceRes, err := s.Handler.GetBalanceFromEVM(accAddr)
				s.Require().NoError(err)
				evmBalance := evmBalanceRes.Balance
				s.Require().Equal(evmBalance, "0")

				totalBalance := s.Network.App.GetBankKeeper().GetBalance(ctx, accAddr, baseDenom)
				s.Require().Equal(totalBalance.Amount, balance)

				res, err := s.Network.App.GetEVMKeeper().Balance(s.Network.GetContext(), &types.QueryBalanceRequest{Address: addr.String()})
				s.Require().NoError(err)
				bal, err := uint256.FromDecimal(res.Balance)
				s.Require().NoError(err)
				return bal
			},
			&uint256.Int{0},
		},
		{
			"Account method, regular account",
			func() *uint256.Int {
				addr := tx.GenerateAddress()
				err := s.Network.App.GetBankKeeper().MintCoins(s.Network.GetContext(), "mint", sdk.NewCoins(sdk.NewCoin(s.Network.GetBaseDenom(), sdkmath.NewInt(100))))
				s.Require().NoError(err)
				err = s.Network.App.GetBankKeeper().SendCoinsFromModuleToAccount(s.Network.GetContext(), "mint", addr.Bytes(), sdk.NewCoins(sdk.NewCoin(s.Network.GetBaseDenom(), sdkmath.NewInt(100))))
				s.Require().NoError(err)
				res, err := s.Network.App.GetEVMKeeper().Account(s.Network.GetContext(), &types.QueryAccountRequest{Address: addr.String()})
				s.Require().NoError(err)
				bal, err := uint256.FromDecimal(res.Balance)
				s.Require().NoError(err)
				return bal
			},
			&uint256.Int{100},
		},
		{
			"Balance method, regular account",
			func() *uint256.Int {
				addr := tx.GenerateAddress()
				err := s.Network.App.GetBankKeeper().MintCoins(s.Network.GetContext(), "mint", sdk.NewCoins(sdk.NewCoin(s.Network.GetBaseDenom(), sdkmath.NewInt(100))))
				s.Require().NoError(err)
				err = s.Network.App.GetBankKeeper().SendCoinsFromModuleToAccount(s.Network.GetContext(), "mint", addr.Bytes(), sdk.NewCoins(sdk.NewCoin(s.Network.GetBaseDenom(), sdkmath.NewInt(100))))
				s.Require().NoError(err)
				res, err := s.Network.App.GetEVMKeeper().Balance(s.Network.GetContext(), &types.QueryBalanceRequest{Address: addr.String()})
				s.Require().NoError(err)
				bal, err := uint256.FromDecimal(res.Balance)
				s.Require().NoError(err)
				return bal
			},
			&uint256.Int{100},
		},
	}
	for _, tc := range testCases {
		s.Run(fmt.Sprintf("Case %s", tc.name), func() {
			s.SetupTest()
			s.Require().Equal(tc.returnedBal(), tc.expBalance)
		})
	}
}

func (s *KeeperTestSuite) TestEmptyRequest() {
	s.SetupTest()
	k := s.Network.App.GetEVMKeeper()

	testCases := []struct {
		name      string
		queryFunc func() (interface{}, error)
	}{
		{
			"Account method",
			func() (interface{}, error) {
				return k.Account(s.Network.GetContext(), nil)
			},
		},
		{
			"CosmosAccount method",
			func() (interface{}, error) {
				return k.CosmosAccount(s.Network.GetContext(), nil)
			},
		},
		{
			"ValidatorAccount method",
			func() (interface{}, error) {
				return k.ValidatorAccount(s.Network.GetContext(), nil)
			},
		},
		{
			"Balance method",
			func() (interface{}, error) {
				return k.Balance(s.Network.GetContext(), nil)
			},
		},
		{
			"Storage method",
			func() (interface{}, error) {
				return k.Storage(s.Network.GetContext(), nil)
			},
		},
		{
			"Code method",
			func() (interface{}, error) {
				return k.Code(s.Network.GetContext(), nil)
			},
		},
		{
			"EthCall method",
			func() (interface{}, error) {
				return k.EthCall(s.Network.GetContext(), nil)
			},
		},
		{
			"EstimateGas method",
			func() (interface{}, error) {
				return k.EstimateGas(s.Network.GetContext(), nil)
			},
		},
		{
			"TraceTx method",
			func() (interface{}, error) {
				return k.TraceTx(s.Network.GetContext(), nil)
			},
		},
		{
			"TraceBlock method",
			func() (interface{}, error) {
				return k.TraceBlock(s.Network.GetContext(), nil)
			},
		},
	}

	for _, tc := range testCases {
		s.Run(fmt.Sprintf("Case %s", tc.name), func() {
			_, err := tc.queryFunc()
			s.Require().Error(err)
		})
	}
}

func getDefaultTraceBlockRequest(unitNetwork network.Network) types.QueryTraceBlockRequest {
	ctx := unitNetwork.GetContext()
	chainID := unitNetwork.GetEIP155ChainID().Int64()
	return types.QueryTraceBlockRequest{
		BlockMaxGas: ctx.ConsensusParams().Block.MaxGas,
		ChainId:     chainID,
		BlockTime:   ctx.BlockTime(),
	}
}

func deployErc20Contract(from keyring.Key, txFactory factory.TxFactory) (common.Address, error) {
	erc20Contract, err := testdata.LoadERC20Contract()
	if err != nil {
		return common.Address{}, err
	}

	constructorArgs := []interface{}{
		from.Addr,
		sdkmath.NewIntWithDecimal(1000, 18).BigInt(),
	}
	compiledContract := erc20Contract
	contractAddr, err := txFactory.DeployContract(
		from.Priv,
		types.EvmTxArgs{}, // Default values
		testutiltypes.ContractDeploymentData{
			Contract:        compiledContract,
			ConstructorArgs: constructorArgs,
		},
	)
	if err != nil {
		return common.Address{}, err
	}
	return contractAddr, nil
}

type transferParams struct {
	senderKey     keyring.Key
	contractAddr  common.Address
	recipientAddr common.Address
}

func executeTransferCall(
	transferParams transferParams,
	txFactory factory.TxFactory,
) (msgEthereumTx *types.MsgEthereumTx, err error) {
	erc20Contract, err := testdata.LoadERC20Contract()
	if err != nil {
		return nil, err
	}

	transferArgs := types.EvmTxArgs{
		To: &transferParams.contractAddr,
	}
	callArgs := testutiltypes.CallArgs{
		ContractABI: erc20Contract.ABI,
		MethodName:  "transfer",
		Args:        []interface{}{transferParams.recipientAddr, big.NewInt(1000)},
	}

	input, err := factory.GenerateContractCallArgs(callArgs)
	if err != nil {
		return nil, err
	}
	transferArgs.Input = input

	// We need to get access to the message
	firstSignedTX, err := txFactory.GenerateSignedEthTx(transferParams.senderKey.Priv, transferArgs)
	if err != nil {
		return nil, err
	}
	txMsg, ok := firstSignedTX.GetMsgs()[0].(*types.MsgEthereumTx)
	if !ok {
		return nil, fmt.Errorf("invalid type")
	}

	result, err := txFactory.ExecuteContractCall(transferParams.senderKey.Priv, transferArgs, callArgs)
	if err != nil || !result.IsOK() {
		return nil, err
	}
	return txMsg, nil
}

func buildTransferTx(
	t *testing.T,
	transferParams transferParams,
	txFactory factory.TxFactory,
) (msgEthereumTx *types.MsgEthereumTx) {
	t.Helper()
	erc20Contract, err := testdata.LoadERC20Contract()
	require.NoError(t, err)

	transferArgs := types.EvmTxArgs{
		To: &transferParams.contractAddr,
	}
	callArgs := testutiltypes.CallArgs{
		ContractABI: erc20Contract.ABI,
		MethodName:  "transfer",
		Args:        []interface{}{transferParams.recipientAddr, big.NewInt(1000)},
	}

	input, err := factory.GenerateContractCallArgs(callArgs)
	require.NoError(t, err)
	transferArgs.Input = input

	// We need to get access to the message
	firstSignedTX, err := txFactory.GenerateSignedEthTx(transferParams.senderKey.Priv, transferArgs)
	require.NoError(t, err)
	txMsg, ok := firstSignedTX.GetMsgs()[0].(*types.MsgEthereumTx)
	require.True(t, ok, "expected MsgEthereumTx type, got type: %T", firstSignedTX.GetMsgs()[0])
	return txMsg
}
