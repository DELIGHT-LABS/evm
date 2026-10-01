package common

import (
	"testing"

	"github.com/stretchr/testify/suite"

	evm "github.com/cosmos/evm"
	"github.com/cosmos/evm/evmd/tests/integration"
	"github.com/cosmos/evm/tests/integration/precompiles/common"
	testapp "github.com/cosmos/evm/testutil/app"
)

func TestStaticCallTestSuite(t *testing.T) {
	create := testapp.ToEvmAppCreator[evm.IntegrationNetworkApp](integration.CreateEvmd, "evm.IntegrationNetworkApp")
	s := common.NewStaticCallTestSuite(create)
	suite.Run(t, s)
}
