package client

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/avast/retry-go"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/tendermint/tendermint/mempool"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
	tmtypes "github.com/tendermint/tendermint/types"
)

// BroadcastTx broadcasts a transactions either synchronously or asynchronously
// based on the context parameters. The result of the broadcast is parsed into
// an intermediate structure which is logged if the context has a logger
// defined.
func (cc *ChainClient) BroadcastTx(ctx context.Context, tx []byte) (res *sdk.TxResponse, err error) {
	// broadcast tx sync waits for check tx to pass
	res, err = cc.BroadcastTxSync(ctx, tx)
	if err != nil || res.Code != 0 {
		return
	}

	// once check tx passes, we wait for the tx to leave the mempool of the node
mempool:
	for {
		select {
		case <-time.After(time.Millisecond * 50):
			if !cc.TxInMempool(ctx, res.TxHash) {
				break mempool
			}
		case <-ctx.Done():
			return
		}
	}

	txid, err := hex.DecodeString(res.TxHash)
	if err != nil {
		return
	}

	var resTx *ctypes.ResultTx
	if err = retry.Do(func() error {
		resTx, err = cc.RPCClient.Tx(ctx, txid, false)
		return err
	}, retry.Context(ctx), retry.MaxDelay(100*time.Millisecond)); err != nil {
		return
	}
	return cc.mkTxResult(resTx)
}

func (cc *ChainClient) mkTxResult(resTx *ctypes.ResultTx) (*sdk.TxResponse, error) {
	txb, err := cc.Codec.TxConfig.TxDecoder()(resTx.Tx)
	if err != nil {
		return nil, err
	}
	p, ok := txb.(intoAny)
	if !ok {
		return nil, fmt.Errorf("expecting a type implementing intoAny, got: %T", txb)
	}
	any := p.AsAny()
	// TODO: maybe don't make up the time here?
	// we can fetch the block for the block time buts thats
	// more round trips
	return sdk.NewResponseResultTx(resTx, any, time.Now().Format(time.RFC3339)), nil
}

// Deprecated: this interface is used only internally for scenario we are
// deprecating (StdTxConfig support)
type intoAny interface {
	AsAny() *codectypes.Any
}

func (cc *ChainClient) TxInMempool(ctx context.Context, txHash string) bool {
	limit := 1000
	// TODO: maybe retry this on error?
	// would do this in the case of inconsistent errors from TM
	res, err := cc.RPCClient.UnconfirmedTxs(ctx, &limit)
	if err != nil {
		return false
	}
	for _, txbz := range res.Txs {
		if strings.EqualFold(txHash, fmt.Sprintf("%X", txbz.Hash())) {
			return true
		}
	}
	return false
}

func (cc *ChainClient) BroadcastTxSync(ctx context.Context, tx []byte) (*sdk.TxResponse, error) {
	res, err := cc.RPCClient.BroadcastTxSync(ctx, tx)
	if errRes := CheckTendermintError(err, tx); errRes != nil {
		return errRes, nil
	}

	return sdk.NewResponseFormatBroadcastTx(res), err

}

// CheckTendermintError checks if the error returned from BroadcastTx is a
// Tendermint error that is returned before the tx is submitted due to
// precondition checks that failed. If an Tendermint error is detected, this
// function returns the correct code back in TxResponse.
//
// TODO: Avoid brittle string matching in favor of error matching. This requires
// a change to Tendermint's RPCError type to allow retrieval or matching against
// a concrete error type.
func CheckTendermintError(err error, tx tmtypes.Tx) *sdk.TxResponse {
	if err == nil {
		return nil
	}

	errStr := strings.ToLower(err.Error())
	txHash := fmt.Sprintf("%X", tx.Hash())

	switch {
	case strings.Contains(errStr, strings.ToLower(mempool.ErrTxInCache.Error())):
		return &sdk.TxResponse{
			Code:      sdkerrors.ErrTxInMempoolCache.ABCICode(),
			Codespace: sdkerrors.ErrTxInMempoolCache.Codespace(),
			TxHash:    txHash,
		}

	case strings.Contains(errStr, "mempool is full"):
		return &sdk.TxResponse{
			Code:      sdkerrors.ErrMempoolIsFull.ABCICode(),
			Codespace: sdkerrors.ErrMempoolIsFull.Codespace(),
			TxHash:    txHash,
		}

	case strings.Contains(errStr, "tx too large"):
		return &sdk.TxResponse{
			Code:      sdkerrors.ErrTxTooLarge.ABCICode(),
			Codespace: sdkerrors.ErrTxTooLarge.Codespace(),
			TxHash:    txHash,
		}
	default:
		// More error debugging here!!
		return nil
	}
}
