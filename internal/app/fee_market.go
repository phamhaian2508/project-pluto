package app

import "math/big"

const (
	baseFeeElasticityMultiplier = uint64(2)
	baseFeeChangeDenominator    = uint64(8)
)

var initialBaseFeePerGas = new(big.Int).SetUint64(1_000_000_000) // 1 gwei

// BaseFeePerGasForBlock returns the deterministic EIP-1559 base fee for a block.
// Blocks written before fee-market activation have no base fee; the first block
// after those records uses the configured initial base fee.
func (app *App) BaseFeePerGasForBlock(height uint64) (*big.Int, error) {
	if height <= 1 {
		return new(big.Int).Set(initialBaseFeePerGas), nil
	}
	parent, found, err := app.EthereumBlockByNumber(height - 1)
	if err != nil {
		return nil, err
	}
	if !found || parent.BaseFeePerGas == nil {
		return new(big.Int).Set(initialBaseFeePerGas), nil
	}
	return nextBaseFee(parent.BaseFeePerGas, parent.GasUsed, parent.GasLimit), nil
}

// NextBaseFeePerGas returns the base fee expected for the next block.
func (app *App) NextBaseFeePerGas() (*big.Int, error) {
	return app.BaseFeePerGasForBlock(uint64(app.currentHeight + 1))
}

// CurrentBaseFeePerGas returns the base fee recorded in the latest committed block.
func (app *App) CurrentBaseFeePerGas() (*big.Int, error) {
	if app.currentHeight <= 0 {
		return new(big.Int), nil
	}
	block, found, err := app.EthereumBlockByNumber(uint64(app.currentHeight))
	if err != nil {
		return nil, err
	}
	if !found || block.BaseFeePerGas == nil {
		return new(big.Int), nil
	}
	return new(big.Int).Set(block.BaseFeePerGas), nil
}

func nextBaseFee(parentBaseFee *big.Int, parentGasUsed, parentGasLimit uint64) *big.Int {
	if parentBaseFee == nil || parentGasLimit == 0 {
		return new(big.Int).Set(initialBaseFeePerGas)
	}
	target := parentGasLimit / baseFeeElasticityMultiplier
	if target == 0 || parentGasUsed == target {
		return new(big.Int).Set(parentBaseFee)
	}
	if parentGasUsed > target {
		gasDelta := parentGasUsed - target
		delta := new(big.Int).Mul(parentBaseFee, new(big.Int).SetUint64(gasDelta))
		delta.Div(delta, new(big.Int).SetUint64(target))
		delta.Div(delta, new(big.Int).SetUint64(baseFeeChangeDenominator))
		if delta.Sign() == 0 {
			delta.SetInt64(1)
		}
		return new(big.Int).Add(parentBaseFee, delta)
	}
	gasDelta := target - parentGasUsed
	delta := new(big.Int).Mul(parentBaseFee, new(big.Int).SetUint64(gasDelta))
	delta.Div(delta, new(big.Int).SetUint64(target))
	delta.Div(delta, new(big.Int).SetUint64(baseFeeChangeDenominator))
	if delta.Cmp(parentBaseFee) > 0 {
		return new(big.Int)
	}
	return new(big.Int).Sub(parentBaseFee, delta)
}
