package plutonetwork

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	cometconfig "github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/crypto"
	"github.com/cometbft/cometbft/crypto/ed25519"
	"github.com/cometbft/cometbft/p2p"
	"github.com/cometbft/cometbft/privval"
	cmttypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
)

const (
	DefaultNodeCount    = 4
	DefaultAccountCount = 64
	ChainID             = "pluto-validator-1"
	GenesisAccount      = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	GenesisBalance      = "1000000000000000000000"
	P2PPortBase         = 27656
	CometRPCBase        = 27757
	EthereumRPCBase     = 18545
)

// NodeInfo is safe network metadata for scripts and dashboards. It deliberately
// excludes all validator and node private keys.
type NodeInfo struct {
	Name        string `json:"name"`
	Home        string `json:"home"`
	NodeID      string `json:"node_id"`
	P2PAddress  string `json:"p2p_address"`
	CometRPC    string `json:"comet_rpc"`
	EthereumRPC string `json:"ethereum_rpc"`
}

type NetworkInfo struct {
	ChainID    string     `json:"chain_id"`
	EVMChainID int64      `json:"evm_chain_id"`
	Nodes      []NodeInfo `json:"nodes"`
}

type TestAccount struct {
	Address    common.Address `json:"address"`
	PrivateKey string         `json:"private_key"`
}

type generatedNode struct {
	info    NodeInfo
	nodeKey *p2p.NodeKey
	pv      *privval.FilePV
	home    string
}

// Setup creates a fresh local four-validator test network. It refuses to write
// into a non-empty root so a repeated setup cannot overwrite node data or keys.
func Setup(root string) (*NetworkInfo, error) {
	return SetupWithPortOffset(root, 0)
}

// SetupWithPortOffset assigns another non-overlapping port range while keeping
// the same relative layout. It is useful for isolated sequential/parallel workloads.
func SetupWithPortOffset(root string, portOffset int) (*NetworkInfo, error) {
	if root == "" {
		return nil, fmt.Errorf("network root is required")
	}
	if portOffset < 0 || P2PPortBase+portOffset+DefaultNodeCount-1 > 65535 ||
		CometRPCBase+portOffset+DefaultNodeCount-1 > 65535 ||
		EthereumRPCBase+portOffset+DefaultNodeCount-1 > 65535 {
		return nil, fmt.Errorf("port offset %d is invalid", portOffset)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve network root: %w", err)
	}
	if entries, err := os.ReadDir(root); err == nil && len(entries) != 0 {
		return nil, fmt.Errorf("network root %s is not empty; choose a new directory to preserve existing files", root)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect network root %s: %w", root, err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create network root: %w", err)
	}

	nodes := make([]generatedNode, DefaultNodeCount)
	validators := make([]cmttypes.GenesisValidator, DefaultNodeCount)
	for i := range nodes {
		name := fmt.Sprintf("validator%d", i+1)
		home := filepath.Join(root, name)
		cometconfig.EnsureRoot(home)
		base := cometconfig.DefaultConfig().SetRoot(home)
		nodeKey, err := p2p.LoadOrGenNodeKey(base.NodeKeyFile())
		if err != nil {
			return nil, fmt.Errorf("create %s node key: %w", name, err)
		}
		pv, err := privval.GenFilePV(base.PrivValidatorKeyFile(), base.PrivValidatorStateFile(), func() (crypto.PrivKey, error) {
			return ed25519.GenPrivKey(), nil
		})
		if err != nil {
			return nil, fmt.Errorf("create %s validator key: %w", name, err)
		}
		pv.Save()
		validatorPubKey, err := pv.GetPubKey()
		if err != nil {
			return nil, fmt.Errorf("read %s validator public key: %w", name, err)
		}
		validators[i] = cmttypes.GenesisValidator{
			Address: pv.GetAddress(), PubKey: validatorPubKey, Power: 10, Name: name,
		}
		p2pPort := P2PPortBase + portOffset + i
		cometRPCPort := CometRPCBase + portOffset + i
		ethRPCPort := EthereumRPCBase + portOffset + i
		nodes[i] = generatedNode{
			info: NodeInfo{
				Name:        name,
				Home:        home,
				NodeID:      string(nodeKey.ID()),
				P2PAddress:  fmt.Sprintf("127.0.0.1:%d", p2pPort),
				CometRPC:    fmt.Sprintf("http://127.0.0.1:%d", cometRPCPort),
				EthereumRPC: fmt.Sprintf("http://127.0.0.1:%d", ethRPCPort),
			},
			nodeKey: nodeKey, pv: pv, home: home,
		}
	}

	accounts := make([]TestAccount, DefaultAccountCount)
	alloc := map[string]map[string]string{GenesisAccount: {"balance": GenesisBalance}}
	for i := range accounts {
		privateKey, err := gethcrypto.GenerateKey()
		if err != nil {
			return nil, fmt.Errorf("create local test account %d: %w", i+1, err)
		}
		address := gethcrypto.PubkeyToAddress(privateKey.PublicKey)
		accounts[i] = TestAccount{Address: address, PrivateKey: "0x" + common.Bytes2Hex(gethcrypto.FromECDSA(privateKey))}
		alloc[address.Hex()] = map[string]string{"balance": GenesisBalance}
	}
	appState, err := json.Marshal(struct {
		Alloc map[string]map[string]string `json:"alloc"`
	}{Alloc: alloc})
	if err != nil {
		return nil, fmt.Errorf("encode shared application genesis: %w", err)
	}
	genesis := &cmttypes.GenesisDoc{
		GenesisTime:     time.Now().UTC().Add(-time.Second),
		ChainID:         ChainID,
		InitialHeight:   1,
		ConsensusParams: cmttypes.DefaultConsensusParams(),
		Validators:      validators,
		AppState:        appState,
	}
	if err := genesis.ValidateAndComplete(); err != nil {
		return nil, fmt.Errorf("validate shared genesis: %w", err)
	}

	info := &NetworkInfo{ChainID: ChainID, EVMChainID: 31337, Nodes: make([]NodeInfo, len(nodes))}
	for i := range nodes {
		node := &nodes[i]
		config := cometconfig.DefaultConfig().SetRoot(node.home)
		config.Moniker = node.info.Name
		config.LogLevel = "info"
		config.RPC.ListenAddress = fmt.Sprintf("tcp://127.0.0.1:%d", CometRPCBase+portOffset+i)
		config.P2P.ListenAddress = fmt.Sprintf("tcp://127.0.0.1:%d", P2PPortBase+portOffset+i)
		config.P2P.AllowDuplicateIP = true
		config.P2P.PersistentPeers = persistentPeers(nodes, i)
		config.Consensus.TimeoutCommit = time.Second
		configFile := filepath.Join(node.home, cometconfig.DefaultConfigDir, cometconfig.DefaultConfigFileName)
		cometconfig.WriteConfigFile(configFile, config)
		genesisFile := filepath.Join(node.home, cometconfig.DefaultConfigDir, cometconfig.DefaultGenesisJSONName)
		if err := genesis.SaveAs(genesisFile); err != nil {
			return nil, fmt.Errorf("write %s shared genesis: %w", node.info.Name, err)
		}
		info.Nodes[i] = node.info
	}
	metadata, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode network metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, "network.json"), append(metadata, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("write network metadata: %w", err)
	}
	accountData, err := json.MarshalIndent(accounts, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode funded test accounts: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, "accounts.json"), append(accountData, '\n'), 0o600); err != nil {
		return nil, fmt.Errorf("write funded test accounts: %w", err)
	}
	return info, nil
}

func persistentPeers(nodes []generatedNode, self int) string {
	peers := make([]string, 0, len(nodes)-1)
	for i, node := range nodes {
		if i == self {
			continue
		}
		peers = append(peers, fmt.Sprintf("%s@%s", node.info.NodeID, node.info.P2PAddress))
	}
	return strings.Join(peers, ",")
}
