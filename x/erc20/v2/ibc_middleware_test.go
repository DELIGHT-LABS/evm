package v2

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/cosmos/ibc-go/v10/modules/apps/transfer/types"
	channeltypes "github.com/cosmos/ibc-go/v10/modules/core/04-channel/types"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	"github.com/cosmos/ibc-go/v10/modules/core/exported"

	"cosmossdk.io/log"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

type stubIBCModule struct {
	recvResult channeltypesv2.RecvPacketResult
}

func (m stubIBCModule) OnSendPacket(
	sdk.Context,
	string,
	string,
	uint64,
	channeltypesv2.Payload,
	sdk.AccAddress,
) error {
	return nil
}

func (m stubIBCModule) OnRecvPacket(
	sdk.Context,
	string,
	string,
	uint64,
	channeltypesv2.Payload,
	sdk.AccAddress,
) channeltypesv2.RecvPacketResult {
	return m.recvResult
}

func (m stubIBCModule) OnTimeoutPacket(
	sdk.Context,
	string,
	string,
	uint64,
	channeltypesv2.Payload,
	sdk.AccAddress,
) error {
	return nil
}

func (m stubIBCModule) OnAcknowledgementPacket(
	sdk.Context,
	string,
	string,
	uint64,
	[]byte,
	channeltypesv2.Payload,
	sdk.AccAddress,
) error {
	return nil
}

type stubErc20Keeper struct {
	recvAck   exported.Acknowledgement
	recvCalls int
}

func (k *stubErc20Keeper) OnRecvPacket(
	sdk.Context,
	channeltypes.Packet,
	exported.Acknowledgement,
) exported.Acknowledgement {
	k.recvCalls++
	return k.recvAck
}

func (*stubErc20Keeper) OnAcknowledgementPacket(
	sdk.Context,
	channeltypes.Packet,
	types.FungibleTokenPacketData,
	channeltypes.Acknowledgement,
) error {
	return nil
}

func (*stubErc20Keeper) OnTimeoutPacket(
	sdk.Context,
	channeltypes.Packet,
	types.FungibleTokenPacketData,
) error {
	return nil
}

func (*stubErc20Keeper) Logger(sdk.Context) log.Logger {
	return log.NewNopLogger()
}

func TestOnRecvPacketPropagatesKeeperAcknowledgement(t *testing.T) {
	baseAck := channeltypes.NewResultAcknowledgement([]byte{1})
	modifiedAck := channeltypes.NewResultAcknowledgement([]byte{2})
	errorAck := channeltypes.NewErrorAcknowledgement(errors.New("conversion failed"))
	underlyingFailure := channeltypesv2.RecvPacketResult{
		Status:          channeltypesv2.PacketStatus_Failure,
		Acknowledgement: []byte("underlying failure"),
	}

	testCases := []struct {
		name           string
		appResult      channeltypesv2.RecvPacketResult
		keeperAck      exported.Acknowledgement
		expectedResult channeltypesv2.RecvPacketResult
		expectedCalls  int
	}{
		{
			name: "conversion failure becomes packet failure",
			appResult: channeltypesv2.RecvPacketResult{
				Status:          channeltypesv2.PacketStatus_Success,
				Acknowledgement: baseAck.Acknowledgement(),
			},
			keeperAck: errorAck,
			expectedResult: channeltypesv2.RecvPacketResult{
				Status: channeltypesv2.PacketStatus_Failure,
			},
			expectedCalls: 1,
		},
		{
			name: "modified success acknowledgement is propagated",
			appResult: channeltypesv2.RecvPacketResult{
				Status:          channeltypesv2.PacketStatus_Success,
				Acknowledgement: baseAck.Acknowledgement(),
			},
			keeperAck: modifiedAck,
			expectedResult: channeltypesv2.RecvPacketResult{
				Status:          channeltypesv2.PacketStatus_Success,
				Acknowledgement: modifiedAck.Acknowledgement(),
			},
			expectedCalls: 1,
		},
		{
			name: "unchanged success acknowledgement is preserved",
			appResult: channeltypesv2.RecvPacketResult{
				Status:          channeltypesv2.PacketStatus_Success,
				Acknowledgement: baseAck.Acknowledgement(),
			},
			keeperAck: baseAck,
			expectedResult: channeltypesv2.RecvPacketResult{
				Status:          channeltypesv2.PacketStatus_Success,
				Acknowledgement: baseAck.Acknowledgement(),
			},
			expectedCalls: 1,
		},
		{
			name:           "underlying application failure bypasses keeper",
			appResult:      underlyingFailure,
			keeperAck:      modifiedAck,
			expectedResult: underlyingFailure,
			expectedCalls:  0,
		},
	}

	packetData := types.NewFungibleTokenPacketData("uatom", "1", "sender", "receiver", "")
	payload := channeltypesv2.NewPayload(
		types.PortID,
		types.PortID,
		types.V1,
		types.EncodingJSON,
		packetData.GetBytes(),
	)
	ctx := sdk.Context{}.WithLogger(log.NewNopLogger())

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			keeper := &stubErc20Keeper{recvAck: tc.keeperAck}
			middleware := NewIBCMiddleware(stubIBCModule{recvResult: tc.appResult}, keeper)

			result := middleware.OnRecvPacket(ctx, "source-client", "destination-client", 1, payload, nil)

			require.Equal(t, tc.expectedResult, result)
			require.Equal(t, tc.expectedCalls, keeper.recvCalls)
		})
	}
}
