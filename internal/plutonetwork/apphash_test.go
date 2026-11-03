package plutonetwork

import "testing"

func TestCompareAppHashes(t *testing.T) {
	tests := []struct {
		name       string
		nodes      []AppInfo
		sameHeight bool
		sameHash   bool
	}{
		{
			name:       "matching validators",
			nodes:      []AppInfo{{Node: "v1", Height: 12, AppHash: "0xABCD"}, {Node: "v2", Height: 12, AppHash: "abcd"}},
			sameHeight: true, sameHash: true,
		},
		{
			name:       "validator catching up",
			nodes:      []AppInfo{{Node: "v1", Height: 12, AppHash: "abcd"}, {Node: "v2", Height: 11, AppHash: "abcd"}},
			sameHeight: false, sameHash: false,
		},
		{
			name:       "same height divergent state",
			nodes:      []AppInfo{{Node: "v1", Height: 12, AppHash: "abcd"}, {Node: "v2", Height: 12, AppHash: "abce"}},
			sameHeight: true, sameHash: false,
		},
		{
			name:       "empty genesis hash is not a verified hash",
			nodes:      []AppInfo{{Node: "v1", Height: 0}, {Node: "v2", Height: 0}},
			sameHeight: true, sameHash: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sameHeight, sameHash, err := CompareAppHashes(test.nodes)
			if err != nil {
				t.Fatal(err)
			}
			if sameHeight != test.sameHeight || sameHash != test.sameHash {
				t.Fatalf("got sameHeight=%t sameHash=%t, want %t/%t", sameHeight, sameHash, test.sameHeight, test.sameHash)
			}
		})
	}
}

func TestCompareAppHashesRejectsEmptySet(t *testing.T) {
	if _, _, err := CompareAppHashes(nil); err == nil {
		t.Fatal("empty validator set was accepted")
	}
}
