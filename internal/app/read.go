package app

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/phamhaian2508/project-pluto/internal/evm"
)

// CurrentHeight trả về chiều cao của khối đã chốt mới nhất.
func (app *App) CurrentHeight() uint64 {
	if app.currentHeight < 0 {
		return 0
	}
	return uint64(app.currentHeight)
}

func (app *App) CurrentAppHash() common.Hash {
	return common.BytesToHash(app.appHash)
}

// ExecutionMetricsAtHeight returns execution metrics embedded in a committed block.
func (app *App) ExecutionMetricsAtHeight(height uint64) (*BlockExecutionMetrics, bool, error) {
	block, found, err := app.EthereumBlockByNumber(height)
	if err != nil || !found {
		return nil, found, err
	}
	metrics := block.Execution
	return &metrics, true, nil
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
