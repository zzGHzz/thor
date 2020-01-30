package consensus

import (
	"bytes"
	"crypto/rand"
	"math"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/vechain/thor/block"
	"github.com/vechain/thor/genesis"
	"github.com/vechain/thor/thor"
	"github.com/vechain/thor/vrf"
)

func TestThreshold(t *testing.T) {
	th := getCommitteeThreshold()
	// ratio = threhsold / (1 << 32 - 1) <= amp_factor * #committee / #node
	ratio := float64(th) / float64(math.MaxUint32)
	if ratio > float64(thor.CommitteeSize)/float64(thor.MaxBlockProposers)*float64(thor.CommitteeThresholdFactor) {
		t.Errorf("Invalid threshold")
	}
}

func TestIsCommitteeByPrivateKey(t *testing.T) {
	_, sk := vrf.GenKeyPair()

	// th := getCommitteeThreshold()

	var (
		msg       = make([]byte, 32)
		proof, pf *vrf.Proof
		err       error
		ok        bool
	)

	// Get a positive sample
	for {
		rand.Read(msg)
		proof, err = sk.Prove(msg)
		if err != nil {
			t.Error(err)
		}

		if isCommitteeByProof(proof) {
			break
		}
	}

	ok, pf, err = isCommitteeByPrivateKey(sk, thor.BytesToBytes32(msg))
	if err != nil || !ok || pf == nil || bytes.Compare(pf[:], proof[:]) != 0 {
		t.Errorf("Testing positive sample failed")
	}

	// Get a negative sample
	for {
		rand.Read(msg)
		proof, err = sk.Prove(msg)
		if err != nil {
			t.Error(err)
		}

		if !isCommitteeByProof(proof) {
			break
		}
	}

	ok, pf, err = isCommitteeByPrivateKey(sk, thor.BytesToBytes32(msg))
	if err != nil || ok || pf != nil {
		t.Errorf("Testing negative sample failed")
	}
}

func M(a ...interface{}) []interface{} {
	return a
}

func TestEpochNumber(t *testing.T) {
	_, cons, err := initConsensusTest()
	if err != nil {
		t.Fatal(err)
	}

	launchTime := cons.chain.GenesisBlock().Header().Timestamp()

	tests := []struct {
		expected interface{}
		returned interface{}
		msg      string
	}{
		{
			[]interface{}{uint32(0)},
			M(cons.EpochNumber(launchTime - 1)),
			"t < launch_time",
		},
		{
			[]interface{}{uint32(0)},
			M(cons.EpochNumber(launchTime + 1)),
			"t = launch_time + 1",
		},
		{
			[]interface{}{uint32(1)},
			M(cons.EpochNumber(launchTime + thor.BlockInterval)),
			"t = launch_time + block_interval",
		},
		{
			[]interface{}{uint32(1)},
			M(cons.EpochNumber(launchTime + thor.BlockInterval*thor.EpochInterval)),
			"t = launch_time + block_interval * epoch_interval",
		},
		{
			[]interface{}{uint32(1)},
			M(cons.EpochNumber(launchTime + thor.BlockInterval*thor.EpochInterval + 1)),
			"t = launch_time + block_interval * epoch_interval + 1",
		},
		{
			[]interface{}{uint32(2)},
			M(cons.EpochNumber(launchTime + thor.BlockInterval*(thor.EpochInterval+1))),
			"t = launch_time + block_interval * (epoch_interval + 1)",
		},
	}

	for _, test := range tests {
		assert.Equal(t, test.expected, test.returned, test.msg)
	}
}

func TestValidateBlockSummary(t *testing.T) {
	privateKey := genesis.DevAccounts()[0].PrivateKey

	packer, cons, err := initConsensusTest()
	if err != nil {
		t.Fatal(err)
	}

	nRound := uint32(1)
	addEmptyBlocks(packer, cons.chain, privateKey, nRound, make(map[uint32]interface{}))

	best := cons.chain.BestBlock()
	round := nRound + 1

	type testObj struct {
		ParentID              thor.Bytes32
		TxRoot                thor.Bytes32
		Timestamp, TotalScore uint64
	}

	tests := []struct {
		input testObj
		ret   error
		msg   string
	}{
		{
			testObj{best.Header().ID(), thor.Bytes32{}, cons.Timestamp(round), 2},
			nil,
			"clean case",
		},
		{
			testObj{best.Header().ParentID(), thor.Bytes32{}, cons.Timestamp(round), 2},
			consensusError("Inconsistent parent block ID"),
			"Invalid parent ID",
		},
		{
			testObj{best.Header().ID(), thor.Bytes32{}, cons.Timestamp(round) - 1, 2},
			consensusError("Invalid timestamp"),
			"Invalid timestamp",
		},
	}

	for _, test := range tests {
		bs := block.NewBlockSummary(test.input.ParentID, test.input.TxRoot, test.input.Timestamp, test.input.TotalScore)
		sig, _ := crypto.Sign(bs.SigningHash().Bytes(), privateKey)
		bs = bs.WithSignature(sig)
		assert.Equal(t, cons.ValidateBlockSummary(bs, best.Header(), test.input.Timestamp), test.ret, test.msg)
	}
}

func getValidCommittee(seed thor.Bytes32) (*vrf.Proof, *vrf.PublicKey) {
	maxIter := 1000
	for i := 0; i < maxIter; i++ {
		pk, sk := vrf.GenKeyPair()
		proof, _ := sk.Prove(seed.Bytes())
		if isCommitteeByProof(proof) {
			return proof, pk
		}
	}
	return nil, nil
}

func getInvalidCommittee(seed thor.Bytes32) (*vrf.Proof, *vrf.PublicKey) {
	maxIter := 1000
	for i := 0; i < maxIter; i++ {
		pk, sk := vrf.GenKeyPair()
		proof, _ := sk.Prove(seed.Bytes())
		if !isCommitteeByProof(proof) {
			return proof, pk
		}
	}
	return nil, nil
}

func TestValidateEndorsement(t *testing.T) {
	var (
		proof *vrf.Proof
		err   error
		ed    *block.Endorsement
		sig   []byte
	)

	// ethsk, _ := crypto.GenerateKey()
	ethsk := genesis.DevAccounts()[0].PrivateKey
	_, vrfsk := vrf.GenKeyPairFromSeed(ethsk.D.Bytes())
	if *vrfsk != *genesis.DevAccounts()[0].VrfPrivateKey {
		t.Errorf("Invalid vrf private key")
	}

	_, cons, err := initConsensusTest()
	if err != nil {
		t.Fatal(err)
	}
	genHeader := cons.chain.GenesisBlock().Header()

	// Create a valid block summary at round 1
	bs := block.NewBlockSummary(genHeader.ID(), thor.Bytes32{}, genHeader.Timestamp()+thor.BlockInterval, 1)
	sig, _ = crypto.Sign(bs.SigningHash().Bytes(), ethsk)
	bs = bs.WithSignature(sig)

	// Get the committee keys and proof
	beacon := getBeaconFromHeader(cons.chain.GenesisBlock().Header())
	seed := seed(beacon, 1)

	// Invalid signer
	proof, _ = vrfsk.Prove(seed.Bytes())
	ed = block.NewEndorsement(bs, proof)
	sk, _ := crypto.GenerateKey()
	sig, _ = crypto.Sign(ed.SigningHash().Bytes(), sk)
	ed = ed.WithSignature(sig)
	if err = cons.ValidateEndorsement(ed, genHeader, bs.Timestamp()); err != consensusError("Signer not allowed to participate in consensus") {
		t.Errorf("Failed to test invalid signer")
	}

	// Invalid proof
	rand.Read(proof[:])
	ed = block.NewEndorsement(bs, proof)
	sig, _ = crypto.Sign(ed.SigningHash().Bytes(), ethsk)
	ed = ed.WithSignature(sig)
	if err = cons.ValidateEndorsement(ed, genHeader, bs.Timestamp()); err != consensusError("Invalid vrf proof") {
		t.Errorf("Failed to test invalid proof")
	}

	// Invalid committee
	proof, _ = vrfsk.Prove(seed.Bytes())
	ed = block.NewEndorsement(bs, proof)
	sig, _ = crypto.Sign(ed.SigningHash().Bytes(), ethsk)
	ed = ed.WithSignature(sig)
	err = cons.ValidateEndorsement(ed, genHeader, bs.Timestamp())
	if ok := IsCommitteeByProof(proof); !ok {
		if err != consensusError("Not a committee member") {
			t.Errorf("Failed to test invalid proof")
		}
	} else {
		if err != nil {
			t.Errorf("Failed to test valid endorsement")
		}
	}
}

func BenchmarkTestEthSig(b *testing.B) {
	sk, _ := crypto.GenerateKey()

	msg := make([]byte, 32)

	for i := 0; i < b.N; i++ {
		rand.Read(msg)
		crypto.Sign(msg, sk)
	}
}

func BenchmarkBeacon(b *testing.B) {
	_, cons, err := initConsensusTest()
	if err != nil {
		b.Fatal(err)
	}

	for i := 0; i < b.N; i++ {
		cons.beacon(uint32(i + 1))
	}
}
