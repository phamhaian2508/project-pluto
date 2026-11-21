// SPDX-License-Identifier: MIT
pragma solidity 0.8.24;

import "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import "@openzeppelin/contracts/token/ERC20/IERC20.sol";

contract SwapToken is ERC20 {
    constructor() ERC20("PlutoSwapToken", "PSWAP") {}
}

contract SwapPool {
    address public token0;
    address public token1;
    uint256 public reserve0;
    uint256 public reserve1;

    event Swap(
        address indexed caller,
        address indexed recipient,
        bool zeroForOne,
        uint256 amountIn,
        uint256 amountOut,
        uint256 reserve0After,
        uint256 reserve1After
    );

    constructor(address _token0, address _token1) {
        token0 = _token0;
        token1 = _token1;
    }

    function _calcAmountOut(
        uint256 amountIn,
        uint256 reserveIn,
        uint256 reserveOut
    ) internal pure returns (uint256) {
        uint256 amountInWithFee = amountIn * 997;
        return (amountInWithFee * reserveOut) / (reserveIn * 1000 + amountInWithFee);
    }

    function swap(
        bool zeroForOne,
        uint256 amountIn,
        uint256 minAmountOut,
        address recipient
    ) external returns (uint256 amountOut) {
        require(amountIn > 0, "SwapPool: zero amountIn");
        require(recipient != address(0), "SwapPool: zero recipient");

        uint256 reserveIn = zeroForOne ? reserve0 : reserve1;
        uint256 reserveOut = zeroForOne ? reserve1 : reserve0;
        require(reserveIn > 0 && reserveOut > 0, "SwapPool: zero reserve");

        {
            address tokIn = zeroForOne ? token0 : token1;
            require(
                IERC20(tokIn).balanceOf(address(this)) >= reserveIn + amountIn,
                "SwapPool: insufficient input"
            );
        }

        amountOut = _calcAmountOut(amountIn, reserveIn, reserveOut);
        require(amountOut > 0 && amountOut >= minAmountOut && amountOut < reserveOut, "SwapPool: insufficient output");

        {
            address tokOut = zeroForOne ? token1 : token0;
            require(IERC20(tokOut).transfer(recipient, amountOut), "SwapPool: transfer out failed");
        }

        if (zeroForOne) {
            reserve0 += amountIn;
            reserve1 -= amountOut;
        } else {
            reserve0 -= amountOut;
            reserve1 += amountIn;
        }

        emit Swap(msg.sender, recipient, zeroForOne, amountIn, amountOut, reserve0, reserve1);
    }
}

contract SwapRouter {
    function swapExactIn(
        address pool,
        bool zeroForOne,
        uint256 amountIn,
        uint256 minAmountOut,
        address recipient
    ) external returns (uint256 amountOut) {
        require(pool != address(0), "SwapRouter: zero pool");
        SwapPool p = SwapPool(pool);
        address tokIn = zeroForOne ? p.token0() : p.token1();
        require(IERC20(tokIn).transferFrom(msg.sender, pool, amountIn), "SwapRouter: transferFrom failed");
        return p.swap(zeroForOne, amountIn, minAmountOut, recipient);
    }
}
