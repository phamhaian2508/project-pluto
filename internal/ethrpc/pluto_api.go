package ethrpc

import (
	"errors"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/phamhaian2508/project-pluto/internal/app"
)

// PlutoAPI exposes Pluto-specific diagnostics that are not part of Ethereum JSON-RPC.
type PlutoAPI struct {
	app *app.App
}

// GetBlockMetrics returns the executor metrics persisted for a committed block.
func (api *PlutoAPI) GetBlockMetrics(height hexutil.Uint64) (*app.BlockExecutionMetrics, error) {
	metrics, found, err := api.app.ExecutionMetricsAtHeight(uint64(height))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("execution metrics not found for block")
	}
	return metrics, nil
}
