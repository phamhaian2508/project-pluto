package app

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"strings"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func encodeCheckTx(t *testing.T, transaction *types.Transaction) []byte {
	t.Helper()
	encoded, err := transaction.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func signCheckTx(
	t *testing.T,
	key *ecdsa.PrivateKey,
	signingChainID *big.Int,
	nonce uint64,
	gas uint64,
	to *common.Address,
	value *big.Int,
	data []byte,
) *types.Transaction {
	t.Helper()
	transaction := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		GasPrice: new(big.Int),
		Gas:      gas,
		To:       to,
		Value:    value,
		Data:     data,
	})
	signed, err := types.SignTx(transaction, types.LatestSignerForChainID(signingChainID), key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func checkTransaction(t *testing.T, app *App, encoded []byte) *abci.CheckTxResponse {
	t.Helper()
	response, err := app.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: encoded})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestCheckTxAcceptsValidTransaction(t *testing.T) {
	app, _, key := testApp(t)
	to := common.HexToAddress("0x0000000000000000000000000000000000001234")
	transaction := signCheckTx(t, key, chainID, 0, 21000, &to, big.NewInt(1), nil)

	response := checkTransaction(t, app, encodeCheckTx(t, transaction))
	if response.Code != 0 {
		t.Fatalf("valid transaction rejected: %s", response.Log)
	}
	if response.GasWanted != 21000 {
		t.Fatalf("GasWanted = %d, want 21000", response.GasWanted)
	}
}

func TestCheckTxRejectsInvalidTransactions(t *testing.T) {
	app, _, key := testApp(t)
	to := common.HexToAddress("0x0000000000000000000000000000000000001234")

	wrongChain := signCheckTx(t, key, big.NewInt(1), 0, 21000, &to, big.NewInt(1), nil)
	wrongNonce := signCheckTx(t, key, chainID, 1, 21000, &to, big.NewInt(1), nil)
	lowIntrinsicGas := signCheckTx(t, key, chainID, 0, 21000, &to, new(big.Int), []byte{1})
	overBlockGas := signCheckTx(t, key, chainID, 0, blockGasLimit+1, &to, new(big.Int), nil)
	oversized := signCheckTx(t, key, chainID, 0, 600000, &to, new(big.Int), make([]byte, maxTransactionSize))

	invalidV := new(big.Int).Add(new(big.Int).Mul(chainID, big.NewInt(2)), big.NewInt(35))
	invalidSignature := types.NewTx(&types.LegacyTx{
		Nonce:    0,
		GasPrice: new(big.Int),
		Gas:      21000,
		To:       &to,
		Value:    new(big.Int),
		V:        invalidV,
		R:        new(big.Int),
		S:        new(big.Int),
	})

	poorKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	insufficientBalance := signCheckTx(t, poorKey, chainID, 0, 21000, &to, big.NewInt(1), nil)

	tests := []struct {
		name    string
		encoded []byte
		wantLog string
	}{
		{name: "size", encoded: encodeCheckTx(t, oversized), wantLog: "transaction too large"},
		{name: "encoding", encoded: []byte{0xff, 0xff}, wantLog: "invalid tx encoding"},
		{name: "chain ID", encoded: encodeCheckTx(t, wrongChain), wantLog: "wrong chain ID"},
		{name: "signature", encoded: encodeCheckTx(t, invalidSignature), wantLog: "invalid signature"},
		{name: "nonce", encoded: encodeCheckTx(t, wrongNonce), wantLog: "invalid nonce"},
		{name: "balance", encoded: encodeCheckTx(t, insufficientBalance), wantLog: "insufficient balance"},
		{name: "intrinsic gas", encoded: encodeCheckTx(t, lowIntrinsicGas), wantLog: "intrinsic gas too low"},
		{name: "block gas limit", encoded: encodeCheckTx(t, overBlockGas), wantLog: "exceeds block gas limit"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := checkTransaction(t, app, test.encoded)
			if response.Code == 0 {
				t.Fatal("invalid transaction was accepted")
			}
			if !strings.Contains(response.Log, test.wantLog) {
				t.Fatalf("log = %q, want substring %q", response.Log, test.wantLog)
			}
		})
	}
}
