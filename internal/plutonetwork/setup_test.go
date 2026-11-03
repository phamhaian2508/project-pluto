package plutonetwork

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	cometconfig "github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/p2p"
	cmttypes "github.com/cometbft/cometbft/types"
	"github.com/phamhaian2508/project-pluto/internal/app"
)

func TestSetupCreatesFourNodesWithOneSharedGenesis(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pluto-validator-network")
	info, err := Setup(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Nodes) != 4 || info.ChainID != ChainID || info.EVMChainID != 31337 {
		t.Fatalf("unexpected network metadata: %+v", info)
	}
	accountData, err := os.ReadFile(filepath.Join(root, "accounts.json"))
	if err != nil {
		t.Fatalf("read funded test accounts: %v", err)
	}
	var accounts []TestAccount
	if err := json.Unmarshal(accountData, &accounts); err != nil {
		t.Fatalf("decode funded test accounts: %v", err)
	}
	if len(accounts) != DefaultAccountCount {
		t.Fatalf("got %d funded test accounts, want %d", len(accounts), DefaultAccountCount)
	}
	networkData, err := os.ReadFile(filepath.Join(root, "network.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(networkData), accounts[0].PrivateKey) {
		t.Fatal("network metadata unexpectedly contains a private key")
	}

	var expected *cmttypes.GenesisDoc
	nodeIDs := make(map[string]bool)
	for i, node := range info.Nodes {
		if nodeIDs[node.NodeID] {
			t.Fatalf("duplicate node ID %q", node.NodeID)
		}
		nodeIDs[node.NodeID] = true
		genesisPath := filepath.Join(node.Home, cometconfig.DefaultConfigDir, cometconfig.DefaultGenesisJSONName)
		genesis, err := cmttypes.GenesisDocFromFile(genesisPath)
		if err != nil {
			t.Fatalf("load %s genesis: %v", node.Name, err)
		}
		if expected == nil {
			expected = genesis
			validatorAddresses := make(map[string]bool)
			for _, validator := range genesis.Validators {
				address := string(validator.Address)
				if validatorAddresses[address] {
					t.Fatalf("duplicate validator address %q", address)
				}
				validatorAddresses[address] = true
			}
		} else if genesis.ValidatorHash() == nil || string(genesis.ValidatorHash()) != string(expected.ValidatorHash()) {
			t.Fatalf("%s does not use the shared validator set", node.Name)
		}
		if genesis.ChainID != ChainID || len(genesis.Validators) != 4 || string(genesis.AppState) != string(expected.AppState) {
			t.Fatalf("%s has different shared genesis settings", node.Name)
		}
		configPath := filepath.Join(node.Home, cometconfig.DefaultConfigDir, cometconfig.DefaultConfigFileName)
		configBytes, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		configText := string(configBytes)
		if !strings.Contains(configText, "persistent_peers =") || !strings.Contains(configText, "timeout_commit = \"1s\"") {
			t.Fatalf("%s config is missing peer or timeout settings", node.Name)
		}
		if node.P2PAddress != "127.0.0.1:"+itoa(P2PPortBase+i) || node.CometRPC != "http://127.0.0.1:"+itoa(CometRPCBase+i) {
			t.Fatalf("unexpected port assignment for %s: %+v", node.Name, node)
		}
		loadedNodeKey, err := p2p.LoadNodeKey(filepath.Join(node.Home, "config", "node_key.json"))
		if err != nil || string(loadedNodeKey.ID()) != node.NodeID {
			t.Fatalf("node key mismatch for %s: err=%v", node.Name, err)
		}
	}
}

func TestSharedGenesisProducesIdenticalInitialAppHash(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pluto-validator-network")
	info, err := Setup(root)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var expected []byte
	for _, node := range info.Nodes {
		genesis, err := cmttypes.GenesisDocFromFile(filepath.Join(node.Home, "config", "genesis.json"))
		if err != nil {
			t.Fatal(err)
		}
		application, err := app.NewApp(filepath.Join(node.Home, "data"), logger)
		if err != nil {
			t.Fatalf("create %s app: %v", node.Name, err)
		}
		response, err := application.InitChain(context.Background(), &abci.InitChainRequest{AppStateBytes: genesis.AppState})
		if err != nil {
			application.Close()
			t.Fatalf("initialize %s: %v", node.Name, err)
		}
		if expected == nil {
			expected = append([]byte(nil), response.AppHash...)
		} else if string(response.AppHash) != string(expected) {
			application.Close()
			t.Fatalf("%s initial AppHash = %X; want %X", node.Name, response.AppHash, expected)
		}
		if err := application.Close(); err != nil {
			t.Fatalf("close %s app: %v", node.Name, err)
		}
	}
}

func TestSetupRefusesToOverwriteExistingFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pluto-validator-network")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "keep.txt")
	if err := os.WriteFile(marker, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Setup(root); err == nil {
		t.Fatal("setup overwrote a non-empty directory")
	}
	contents, err := os.ReadFile(marker)
	if err != nil || string(contents) != "preserve" {
		t.Fatalf("existing file was changed: contents=%q err=%v", contents, err)
	}
}

func TestSetupWithPortOffsetCreatesNonOverlappingPorts(t *testing.T) {
	info, err := SetupWithPortOffset(filepath.Join(t.TempDir(), "second"), 20)
	if err != nil {
		t.Fatal(err)
	}
	for i, node := range info.Nodes {
		if node.P2PAddress != "127.0.0.1:"+itoa(P2PPortBase+20+i) ||
			node.CometRPC != "http://127.0.0.1:"+itoa(CometRPCBase+20+i) ||
			node.EthereumRPC != "http://127.0.0.1:"+itoa(EthereumRPCBase+20+i) {
			t.Fatalf("unexpected offset ports for %s: %+v", node.Name, node)
		}
	}
}

func itoa(value int) string {
	return strconv.Itoa(value)
}
