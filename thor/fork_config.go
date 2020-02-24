// Copyright (c) 2019 The VeChainThor developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package thor

import (
	"fmt"
	"math"
	"strings"
)

// ForkConfig config for a fork.
type ForkConfig struct {
	VIP191    uint32
	ETH_CONST uint32
	BLOCKLIST uint32
	VIP193    uint32
}

func (fc ForkConfig) String() string {
	var strs []string
	push := func(name string, blockNum uint32) {
		if blockNum != math.MaxUint32 {
			strs = append(strs, fmt.Sprintf("%v: #%v", name, blockNum))
		}
	}

	push("VIP191", fc.VIP191)
	push("ETH_CONST", fc.ETH_CONST)
	push("BLOCKLIST", fc.BLOCKLIST)
	push("VIP193", fc.VIP193)

	return strings.Join(strs, ", ")
}

// NoFork a special config without any forks.
var NoFork = ForkConfig{
	VIP191:    math.MaxUint32,
	ETH_CONST: math.MaxUint32,
	BLOCKLIST: math.MaxUint32,
	VIP193:    math.MaxInt32,
}

// for well-known networks
var forkConfigs = map[Bytes32]ForkConfig{
	// mainnet
	MustParseBytes32("0x00000000851caf3cfdb6e899cf5958bfb1ac3413d346d43539627e6be7ec1b4a"): {
		VIP191:    3337300,
		ETH_CONST: 3337300,
		BLOCKLIST: 4817300,
	},
	// testnet
	MustParseBytes32("0x000000000b2bce3c70bc649a02749e8687721b09ed2e15997f466536b20bb127"): {
		VIP191:    2898800,
		ETH_CONST: 3192500,
		BLOCKLIST: math.MaxUint32,
	},
}

// GetForkConfig get fork config for given genesis ID.
func GetForkConfig(genesisID Bytes32) ForkConfig {
	return forkConfigs[genesisID]
}

// list of vrf public keys for existing masternodes
var vrfPublicKeyMap = map[Address]Bytes32{
	MustParseAddress("0x2a02604a8b7aaa84991c21d7de1c3238046c5275"): MustParseBytes32("0x96893d6f2d785dbdf75d635d74ee53b85a3e7837150d321c4965de3def134182"),
	MustParseAddress("0x86fd9eb1cf082d7d6b0c6033fc89ccfcbf648549"): MustParseBytes32("0x97b182c4d88435c3781bf5f29a59c169a91564acbf193c9ba95a4db3fa703f26"),
	MustParseAddress("0x8f53d18bb03c84ed92abe0b6a9a8c277dbbf719f"): MustParseBytes32("0x2ab534b885f45e7e628e3bea8bb1a7e914f0009d077a44ac2d4461e7731fcb2c"),
}

// GetVrfPuiblicKey returns the vrf public key for a given node
func GetVrfPuiblicKey(addr Address) Bytes32 {
	return vrfPublicKeyMap[addr]
}
