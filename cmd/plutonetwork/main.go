package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/phamhaian2508/project-pluto/internal/plutonetwork"
)

func main() {
	root := flag.String("root", ".pluto-network", "directory for the Pluto network with four validators")
	portOffset := flag.Int("port-offset", 0, "offset for all network ports; use a unique value for another test network")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/plutonetwork [-root .pluto-network] [-port-offset 0]")
		os.Exit(2)
	}
	info, err := plutonetwork.SetupWithPortOffset(*root, *portOffset)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoded, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
	fmt.Printf("\nPluto validator network files created under %s\n", *root)
}
