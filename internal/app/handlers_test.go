package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/huyCuong73/pluto/internal/evm"
)

func TestEthereumRPCHandlerSupportsRemixReadMethods(t *testing.T) {
	app, from, _ := testApp(t)
	contract := common.HexToAddress("0x000000000000000000000000000000000000ca11")
	state := evm.NewPebbleStateDB(app.db)
	state.CreateAccount(contract)
	state.SetCode(contract, common.FromHex("0x6001600055600160005260206000f3"), 0)
	state.SetState(contract, common.Hash{}, common.BigToHash(common.Big2))
	if err := state.Commit(); err != nil {
		t.Fatal(err)
	}
	handler := NewEthereumRPCHandler(app, "http://127.0.0.1:26657")

	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "chain id", body: `{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`, want: `"result":"0x7a69"`},
		{name: "balance", body: `{"jsonrpc":"2.0","id":2,"method":"eth_getBalance","params":["` + from.Hex() + `","latest"]}`, want: `"result":"0x3635c9adc5dea00000"`},
		{name: "code", body: `{"jsonrpc":"2.0","id":3,"method":"eth_getCode","params":["` + contract.Hex() + `","latest"]}`, want: `"result":"0x6001600055600160005260206000f3"`},
		{name: "storage", body: `{"jsonrpc":"2.0","id":4,"method":"eth_getStorageAt","params":["` + contract.Hex() + `","0x0","latest"]}`, want: common.BigToHash(common.Big2).Hex()},
		{name: "call", body: `{"jsonrpc":"2.0","id":5,"method":"eth_call","params":[{"to":"` + contract.Hex() + `","gas":"0x186a0"},"latest"]}`, want: common.BigToHash(common.Big1).Hex()},
		{name: "estimate gas", body: `{"jsonrpc":"2.0","id":6,"method":"eth_estimateGas","params":[{"to":"` + contract.Hex() + `"},"latest"]}`, want: `"result":"0x`},
		{name: "fee history", body: `{"jsonrpc":"2.0","id":7,"method":"eth_feeHistory","params":["0x1","latest",[25,50,75]]}`, want: `"baseFeePerGas":["0x0","0x0"]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.body))
			req.Header.Set("Origin", "https://remix.ethereum.org")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)

			if response.Code != http.StatusOK {
				t.Fatalf("HTTP status = %d, body: %s", response.Code, response.Body.String())
			}
			if got := response.Header().Get("Access-Control-Allow-Origin"); got != "https://remix.ethereum.org" {
				t.Fatalf("CORS origin = %q", got)
			}
			var rpc rpcResponse
			if err := json.Unmarshal(response.Body.Bytes(), &rpc); err != nil {
				t.Fatalf("invalid JSON-RPC response: %v", err)
			}
			if rpc.Error != nil {
				t.Fatalf("RPC error: %+v", rpc.Error)
			}
			body := response.Body.String()
			if !strings.Contains(body, test.want) {
				t.Fatalf("response %s does not contain %s", body, test.want)
			}
		})
	}
}

func TestEthereumRPCHandlerRejectsMalformedRequest(t *testing.T) {
	app, _, _ := testApp(t)
	handler := NewEthereumRPCHandler(app, "http://127.0.0.1:26657")
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"1.0","id":1,"method":"eth_chainId"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)

	var rpc rpcResponse
	if err := json.Unmarshal(response.Body.Bytes(), &rpc); err != nil {
		t.Fatal(err)
	}
	if rpc.Error == nil || rpc.Error.Code != -32600 {
		t.Fatalf("expected invalid request error, got %+v", rpc)
	}
}
