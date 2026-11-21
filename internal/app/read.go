package app

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/phamhaian2508/project-pluto/internal/evm"
	"github.com/phamhaian2508/project-pluto/internal/store"
)

// CurrentHeight trả về chiều cao của khối đã chốt mới nhất.
func (app *App) CurrentHeight() uint64 {
	app.metaMu.RLock()
	h := app.currentHeight
	app.metaMu.RUnlock()
	if h < 0 {
		return 0
	}
	return uint64(h)
}

func (app *App) CurrentAppHash() common.Hash {
	app.metaMu.RLock()
	hash := common.BytesToHash(app.appHash)
	app.metaMu.RUnlock()
	return hash
}

// GenesisAppHash trả về AppHash cố định ở khối 0 (không đổi sau khi tăng chiều cao).
func (app *App) GenesisAppHash() common.Hash {
	app.metaMu.RLock()
	defer app.metaMu.RUnlock()
	if len(app.genesisAppHash) > 0 {
		return common.BytesToHash(app.genesisAppHash)
	}
	if app.currentHeight == 0 {
		return common.BytesToHash(app.appHash)
	}
	return common.Hash{}
}

// CommittedHead trả nguyên tử cặp (height, appHash) bền vững mới nhất dưới cùng 1 khóa đọc.
func (app *App) CommittedHead() (uint64, common.Hash) {
	app.metaMu.RLock()
	defer app.metaMu.RUnlock()
	var h uint64
	if app.currentHeight > 0 {
		h = uint64(app.currentHeight)
	}
	return h, common.BytesToHash(app.appHash)
}

// ChainID trả bản sao EVM chain ID đang cấu hình trên App.
func (app *App) ChainID() *big.Int {
	return app.effectiveChainID()
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

// committedSnapshot trả nguyên tử (height, appHash, snap) tại cùng ranh giới Commit.
// Bên gọi bắt buộc gọi snap.Close() sau khi đọc xong.
func (app *App) committedSnapshot() (uint64, common.Hash, *store.PebbleSnapshot) {
	app.metaMu.RLock()
	defer app.metaMu.RUnlock()
	var h uint64
	if app.currentHeight > 0 {
		h = uint64(app.currentHeight)
	}
	var snap *store.PebbleSnapshot
	if app.db != nil {
		snap = app.db.NewSnapshot()
	}
	return h, common.BytesToHash(app.appHash), snap
}

func (app *App) Balance(address common.Address) *big.Int {
	_, _, snap := app.committedSnapshot()
	defer snap.Close()
	return evm.NewPebbleStateDBFromKV(snap).GetBalance(address).ToBig()
}

func (app *App) Nonce(address common.Address) uint64 {
	_, _, snap := app.committedSnapshot()
	defer snap.Close()
	return evm.NewPebbleStateDBFromKV(snap).GetNonce(address)
}

func (app *App) Code(address common.Address) []byte {
	_, _, snap := app.committedSnapshot()
	defer snap.Close()
	return common.CopyBytes(evm.NewPebbleStateDBFromKV(snap).GetCode(address))
}

func (app *App) Storage(address common.Address, slot common.Hash) common.Hash {
	_, _, snap := app.committedSnapshot()
	defer snap.Close()
	return evm.NewPebbleStateDBFromKV(snap).GetState(address, slot)
}
