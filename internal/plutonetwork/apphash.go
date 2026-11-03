package plutonetwork

import (
	"fmt"
	"strings"
)

type AppInfo struct {
	Node    string
	Height  int64
	AppHash string
}

// CompareAppHashes reports whether all supplied nodes are at one height and
// have an identical non-empty AppHash. Height skew is reported separately so
// a monitor can wait while nodes are still catching up.
func CompareAppHashes(nodes []AppInfo) (sameHeight, sameHash bool, err error) {
	if len(nodes) == 0 {
		return false, false, fmt.Errorf("no validator status was provided")
	}
	base := nodes[0]
	for _, node := range nodes[1:] {
		if node.Height != base.Height {
			return false, false, nil
		}
	}
	sameHeight = true
	if strings.TrimSpace(base.AppHash) == "" {
		return true, false, nil
	}
	baseHash := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(base.AppHash)), "0x")
	for _, node := range nodes[1:] {
		hash := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(node.AppHash)), "0x")
		if hash == "" || hash != baseHash {
			return true, false, nil
		}
	}
	return true, true, nil
}
