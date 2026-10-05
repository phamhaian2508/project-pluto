// plutotx: công cụ dòng lệnh nhỏ để thử chain Pluto.
//
//	go run ./cmd/plutotx balance 0xĐịaChỉ
//	go run ./cmd/plutotx send -key <privkey-hex> -to 0xĐịaChỉ -eth 10
package main

import (
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// Phải khớp với chainID trong internal/app/app.go
const chainID = 1

const rpcURL = "http://localhost:26657"

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
		Data    string `json:"data"`
	} `json:"error"`
}

// rpcGet gọi RPC dạng URI của CometBFT
func rpcGet(method string, params url.Values) (json.RawMessage, error) {
	resp, err := http.Get(rpcURL + "/" + method + "?" + params.Encode())
	if err != nil {
		return nil, fmt.Errorf("không kết nối được node (node đã chạy chưa?): %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var r rpcResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("phản hồi không phải JSON: %s", string(body))
	}
	if r.Error != nil {
		return nil, fmt.Errorf("RPC lỗi: %s %s", r.Error.Message, r.Error.Data)
	}
	return r.Result, nil
}

// query gọi ABCI Query của app (path: "balance" hoặc "nonce")
func query(path string, data []byte) (string, error) {
	res, err := rpcGet("abci_query", url.Values{
		"path": {`"` + path + `"`},
		"data": {"0x" + hex.EncodeToString(data)},
	})
	if err != nil {
		return "", err
	}

	var out struct {
		Response struct {
			Code  uint32 `json:"code"`
			Log   string `json:"log"`
			Value string `json:"value"`
		} `json:"response"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	if out.Response.Code != 0 {
		return "", fmt.Errorf("query lỗi: %s", out.Response.Log)
	}
	val, err := base64.StdEncoding.DecodeString(out.Response.Value)
	if err != nil {
		return "", err
	}
	return string(val), nil
}

func cmdBalance(args []string) {
	if len(args) != 1 {
		fmt.Println("cách dùng: plutotx balance 0xĐịaChỉ")
		os.Exit(1)
	}
	addr := common.HexToAddress(args[0])

	bal, err := query("balance", addr.Bytes())
	if err != nil {
		fmt.Println("Lỗi:", err)
		os.Exit(1)
	}
	nonce, err := query("nonce", addr.Bytes())
	if err != nil {
		fmt.Println("Lỗi:", err)
		os.Exit(1)
	}
	fmt.Printf("Địa chỉ : %s\nSố dư   : %s wei\nNonce   : %s\n", addr.Hex(), bal, nonce)
}

func cmdSend(args []string) {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	keyHex := fs.String("key", "", "private key (hex) của người gửi")
	toHex := fs.String("to", "", "địa chỉ người nhận")
	eth := fs.Int64("eth", 0, "số ETH (số nguyên) cần chuyển")
	fs.Parse(args)

	if *keyHex == "" || *toHex == "" || *eth <= 0 {
		fmt.Println("cách dùng: plutotx send -key <hex> -to 0xĐịaChỉ -eth 10")
		os.Exit(1)
	}

	key, err := crypto.HexToECDSA(strings.TrimPrefix(*keyHex, "0x"))
	if err != nil {
		fmt.Println("Private key không hợp lệ:", err)
		os.Exit(1)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	to := common.HexToAddress(*toHex)

	// Lấy nonce hiện tại từ node
	nonceStr, err := query("nonce", from.Bytes())
	if err != nil {
		fmt.Println("Lỗi:", err)
		os.Exit(1)
	}
	nonce, err := strconv.ParseUint(nonceStr, 10, 64)
	if err != nil {
		fmt.Println("Nonce không hợp lệ:", nonceStr)
		os.Exit(1)
	}

	// 1 ETH = 1e18 wei
	value := new(big.Int).Mul(big.NewInt(*eth), big.NewInt(1e18))

	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		GasPrice: big.NewInt(0), // Phase 1: gas miễn phí
		Gas:      21000,
		To:       &to,
		Value:    value,
	})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(big.NewInt(chainID)), key)
	if err != nil {
		fmt.Println("Ký tx lỗi:", err)
		os.Exit(1)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		fmt.Println("Encode tx lỗi:", err)
		os.Exit(1)
	}

	res, err := rpcGet("broadcast_tx_sync", url.Values{"tx": {"0x" + hex.EncodeToString(raw)}})
	if err != nil {
		fmt.Println("Lỗi:", err)
		os.Exit(1)
	}

	var out struct {
		Code uint32 `json:"code"`
		Log  string `json:"log"`
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		fmt.Println("Không đọc được phản hồi:", string(res))
		os.Exit(1)
	}
	if out.Code != 0 {
		fmt.Printf("Mempool từ chối tx (code %d): %s\n", out.Code, out.Log)
		os.Exit(1)
	}
	fmt.Printf("Đã gửi tx từ %s -> %s (%d ETH)\nTx hash: %s\nĐợi ~2 giây để vào block, rồi kiểm tra lại bằng lệnh balance.\n",
		from.Hex(), to.Hex(), *eth, out.Hash)
}

func cmdDeploy(args []string) {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	keyHex := fs.String("key", "", "private key hex of deployer")
	bytecodeHex := fs.String("bytecode", "", "compiled contract creation bytecode hex")
	gas := fs.Uint64("gas", 3000000, "gas limit")
	_ = fs.Parse(args)
	if *keyHex == "" || *bytecodeHex == "" || *gas == 0 {
		fatalUsage("deploy -key <hex> -bytecode <creation-bytecode-hex> [-gas limit]")
	}
	data, err := decodeHex(*bytecodeHex)
	if err != nil {
		fatal("invalid bytecode", err)
	}
	key, err := crypto.HexToECDSA(strings.TrimPrefix(*keyHex, "0x"))
	if err != nil {
		fatal("invalid private key", err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	nonce := fetchNonce(from)
	tx := types.NewTx(&types.LegacyTx{Nonce: nonce, GasPrice: new(big.Int), Gas: *gas, Data: data})
	submitSignedTx(tx, key, func() {
		fmt.Printf("Contract address: %s\n", crypto.CreateAddress(from, nonce).Hex())
	})
}

func cmdCall(args []string) {
	fs := flag.NewFlagSet("call", flag.ExitOnError)
	keyHex := fs.String("key", "", "private key hex of caller")
	toHex := fs.String("to", "", "contract address")
	dataHex := fs.String("data", "", "ABI encoded function selector and arguments")
	gas := fs.Uint64("gas", 300000, "gas limit")
	_ = fs.Parse(args)
	if *keyHex == "" || !common.IsHexAddress(*toHex) || *dataHex == "" || *gas == 0 {
		fatalUsage("call -key <hex> -to <contract> -data <calldata-hex> [-gas limit]")
	}
	data, err := decodeHex(*dataHex)
	if err != nil {
		fatal("invalid calldata", err)
	}
	key, err := crypto.HexToECDSA(strings.TrimPrefix(*keyHex, "0x"))
	if err != nil {
		fatal("invalid private key", err)
	}
	to := common.HexToAddress(*toHex)
	nonce := fetchNonce(crypto.PubkeyToAddress(key.PublicKey))
	tx := types.NewTx(&types.LegacyTx{Nonce: nonce, GasPrice: new(big.Int), Gas: *gas, To: &to, Data: data})
	submitSignedTx(tx, key, nil)
}

func cmdStorage(args []string) {
	if len(args) != 2 || !common.IsHexAddress(args[0]) {
		fatalUsage("storage <contract-address> <slot-hex>")
	}
	addr, slot := common.HexToAddress(args[0]), common.HexToHash(args[1])
	data := append(addr.Bytes(), slot.Bytes()...)
	value, err := query("storage", data)
	if err != nil {
		fatal("storage query failed", err)
	}
	fmt.Printf("0x%s\n", hex.EncodeToString([]byte(value)))
}

func fetchNonce(from common.Address) uint64 {
	nonceStr, err := query("nonce", from.Bytes())
	if err != nil {
		fatal("nonce query failed", err)
	}
	nonce, err := strconv.ParseUint(nonceStr, 10, 64)
	if err != nil {
		fatal("invalid nonce from node", err)
	}
	return nonce
}

func submitSignedTx(tx *types.Transaction, key *ecdsa.PrivateKey, after func()) {
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(big.NewInt(chainID)), key)
	if err != nil {
		fatal("transaction signing failed", err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		fatal("transaction encoding failed", err)
	}
	res, err := rpcGet("broadcast_tx_sync", url.Values{"tx": {"0x" + hex.EncodeToString(raw)}})
	if err != nil {
		fatal("transaction broadcast failed", err)
	}
	var out struct {
		Code uint32 `json:"code"`
		Log  string `json:"log"`
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		fatal("invalid broadcast response", err)
	}
	if out.Code != 0 {
		fatal("mempool rejected transaction", fmt.Errorf("code %d: %s", out.Code, out.Log))
	}
	fmt.Printf("Tx hash: %s\n", out.Hash)
	if after != nil {
		after()
	}
}

func decodeHex(value string) ([]byte, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "0x")
	if len(value)%2 != 0 {
		value = "0" + value
	}
	return hex.DecodeString(value)
}

func fatalUsage(usage string)         { fmt.Fprintln(os.Stderr, "usage:", usage); os.Exit(2) }
func fatal(message string, err error) { fmt.Fprintln(os.Stderr, message+":", err); os.Exit(1) }

func main() {
	if len(os.Args) < 2 {
		fmt.Println("lệnh: balance | send")
		os.Exit(1)
	}
	switch os.Args[1] {
	case "balance":
		cmdBalance(os.Args[2:])
	case "send":
		cmdSend(os.Args[2:])
	case "deploy":
		cmdDeploy(os.Args[2:])
	case "call":
		cmdCall(os.Args[2:])
	case "storage":
		cmdStorage(os.Args[2:])
	default:
		fmt.Println("lệnh không hợp lệ:", os.Args[1])
		os.Exit(1)
	}
}
