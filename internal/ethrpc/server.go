package ethrpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	cmttypes "github.com/cometbft/cometbft/types"
	gethrpc "github.com/ethereum/go-ethereum/rpc"
	"github.com/huyCuong73/pluto/internal/app"
)

type TxBroadcaster interface {
	BroadcastTxSync(context.Context, cmttypes.Tx) (*coretypes.ResultBroadcastTx, error)
}

type Server struct {
	rpc      *gethrpc.Server
	http     *http.Server
	listener net.Listener
}

func New(application *app.App, broadcasters ...TxBroadcaster) (*Server, error) {
	var broadcaster TxBroadcaster
	if len(broadcasters) > 0 {
		broadcaster = broadcasters[0]
	}
	rpcServer := gethrpc.NewServer()
	services := []struct {
		name string
		api  any
	}{
		{"eth", &EthAPI{app: application, broadcaster: broadcaster}},
		{"net", &NetAPI{}},
		{"web3", &Web3API{}},
	}
	for _, service := range services {
		if err := rpcServer.RegisterName(service.name, service.api); err != nil {
			rpcServer.Stop()
			return nil, fmt.Errorf("register %s JSON-RPC service: %w", service.name, err)
		}
	}
	return &Server{rpc: rpcServer}, nil
}

func (s *Server) Handler() http.Handler { return s.HTTPHandler() }

func (s *Server) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Access-Control-Allow-Origin", "*")
		response.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		response.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if request.Header.Get("Access-Control-Request-Private-Network") == "true" {
			response.Header().Set("Access-Control-Allow-Private-Network", "true")
		}
		if request.Method == http.MethodOptions {
			response.WriteHeader(http.StatusNoContent)
			return
		}
		s.rpc.ServeHTTP(response, request)
	})
}

func (s *Server) Start(address string) error {
	if s.listener != nil {
		return errors.New("Ethereum JSON-RPC server already started")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	s.listener = listener
	s.http = &http.Server{Handler: s.Handler()}
	go func() {
		_ = s.http.Serve(listener)
	}()
	return nil
}

func (s *Server) Close(ctx context.Context) error {
	s.rpc.Stop()
	if s.http == nil {
		return nil
	}
	return s.http.Shutdown(ctx)
}
