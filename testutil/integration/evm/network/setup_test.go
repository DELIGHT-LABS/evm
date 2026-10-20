package network

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/cosmos/evm/testutil"
	erc20types "github.com/cosmos/evm/x/erc20/types"

	minttypes "github.com/cosmos/cosmos-sdk/x/mint/types"
)

func TestCustomizeGenesisWithoutOptionalModules(t *testing.T) {
	genesis := testutil.GenesisState{"existing": json.RawMessage(`{"preserved":true}`)}
	custom := CustomGenesisState{
		minttypes.ModuleName:  minttypes.DefaultGenesisState(),
		erc20types.ModuleName: erc20types.DefaultGenesisState(),
	}

	// An absent optional module must not require its keeper or codec, even when
	// a reusable suite includes module-specific genesis overrides.
	got, err := customizeGenesis(nil, custom, genesis)
	require.NoError(t, err)
	require.Equal(t, testutil.GenesisState{"existing": json.RawMessage(`{"preserved":true}`)}, got)
}
