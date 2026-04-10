package main

import (
	"context"
	"fmt"

	sdkerrors "github.com/sei-protocol/sei-chain/sei-cosmos/types/errors"
	typestx "github.com/sei-protocol/sei-chain/sei-cosmos/types/tx"
)

func SendTx(
	ctx context.Context,
	txBytes []byte,
	mode typestx.BroadcastMode,
	loadtestClient LoadTestClient,
) (ok bool, wrongSequence bool) {
	grpcRes, err := loadtestClient.GetTxClient().BroadcastTx(
		ctx,
		&typestx.BroadcastTxRequest{
			Mode:    mode,
			TxBytes: txBytes,
		},
	)
	if grpcRes != nil {
		if grpcRes.TxResponse.Code == 0 {
			return true, false
		}
		fmt.Printf("Failed to broadcast tx with response: %v \n", grpcRes)
		if grpcRes.TxResponse.Codespace == sdkerrors.RootCodespace && grpcRes.TxResponse.Code == sdkerrors.ErrWrongSequence.ABCICode() {
			return false, true
		}
	} else if err != nil && ctx.Err() == nil {
		fmt.Printf("Failed to broadcast tx: %v \n", err)
	}
	return false, false
}
