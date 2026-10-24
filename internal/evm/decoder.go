package evm

import (

	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Các hàm chỉ thực hiện tính toán trên CPU.

type TxProcessor struct {
	chainID *big.Int
}

func NewTxProcessor(chainID int64) *TxProcessor {
	return &TxProcessor{
		chainID: big.NewInt(chainID),
	}
}


// Giải mã các byte RLP thành giao dịch.
func (tp *TxProcessor) DecodeTx(txBytes []byte) (*types.Transaction, error) {
	tx := &types.Transaction{}
	if err := tx.UnmarshalBinary(txBytes); err != nil {
		return nil, fmt.Errorf("failed to decode rlp: %v", err)
	}
	return tx, nil
}


// Lấy địa chỉ người gửi.
func (tp *TxProcessor) RecoverSender(tx *types.Transaction) (common.Address,error){

	// Dùng EIP-155 Signer hoặc HomesteadSigner
	signer := types.LatestSignerForChainID(tp.chainID)

	sender, err := types.Sender(signer, tx)

	if err != nil {
		return common.Address{}, fmt.Errorf("Invalid signature: %w", err)
	}

	return sender, nil
}
