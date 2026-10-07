package app

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/huyCuong73/pluto/internal/evm"
)

// CurrentHeight returns the latest committed block height.
func (app *App) CurrentHeight() uint64 {
	if app.currentHeight < 0 {
		return 0
	}
	return uint64(app.currentHeight)
}

func (app *App) CurrentAppHash() common.Hash {
	return common.BytesToHash(app.appHash)
}

func (app *App) Balance(address common.Address) *big.Int {
	return evm.NewPebbleStateDB(app.db).GetBalance(address).ToBig()
}

func (app *App) Nonce(address common.Address) uint64 {
	return evm.NewPebbleStateDB(app.db).GetNonce(address)
}

func (app *App) Code(address common.Address) []byte {
	return common.CopyBytes(evm.NewPebbleStateDB(app.db).GetCode(address))
}

func (app *App) Storage(address common.Address, slot common.Hash) common.Hash {
	return evm.NewPebbleStateDB(app.db).GetState(address, slot)
}
