// Copyright (c) 2018 The VeChainThor developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package consensus

import (
	"crypto/ecdsa"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/vechain/thor/block"
	"github.com/vechain/thor/builtin"
	"github.com/vechain/thor/chain"
	"github.com/vechain/thor/genesis"
	"github.com/vechain/thor/lvldb"
	"github.com/vechain/thor/packer"
	"github.com/vechain/thor/state"
	"github.com/vechain/thor/thor"
	"github.com/vechain/thor/tx"
	"github.com/vechain/thor/vrf"
)

func TestConsensus(t *testing.T) {
	obValue := reflect.ValueOf(newTestConsensus(t))
	obType := obValue.Type()
	for i := 0; i < obValue.NumMethod(); i++ {
		if strings.HasPrefix(obType.Method(i).Name, "Test") {
			obValue.Method(i).Call(nil)
		}
	}
}

func txBuilder(tag byte) *tx.Builder {
	address := thor.BytesToAddress([]byte("addr"))
	return new(tx.Builder).
		GasPriceCoef(1).
		Gas(1000000).
		Expiration(100).
		Clause(tx.NewClause(&address).WithValue(big.NewInt(10)).WithData(nil)).
		Nonce(1).
		ChainTag(tag)
}

func txSign(builder *tx.Builder) *tx.Transaction {
	transaction := builder.Build()
	sig, _ := crypto.Sign(transaction.SigningHash().Bytes(), genesis.DevAccounts()[0].PrivateKey)
	return transaction.WithSignature(sig)
}

type testConsensus struct {
	t        *testing.T
	assert   *assert.Assertions
	con      *Consensus
	time     uint64
	pk       *ecdsa.PrivateKey
	parent   *block.Block
	original *block.Block
	tag      byte
}

func newTestConsensus(t *testing.T) *testConsensus {
	db, err := lvldb.NewMem()
	if err != nil {
		t.Fatal(err)
	}

	type account struct {
		ethsk *ecdsa.PrivateKey
		addr  thor.Address
		vrfsk *vrf.PrivateKey
		vrfpk *vrf.PublicKey
	}

	var accs []*account
	for i := uint64(0); i < thor.MaxBlockProposers; i++ {
		ethsk, _ := crypto.GenerateKey()
		addr := crypto.PubkeyToAddress(ethsk.PublicKey)
		vrfpk, vrfsk := vrf.GenKeyPair()
		accs = append(accs, &account{ethsk, thor.BytesToAddress(addr.Bytes()), vrfsk, vrfpk})
	}

	launchTime := uint64(1526400000)
	gen := new(genesis.Builder).
		GasLimit(thor.InitialGasLimit).
		Timestamp(launchTime).
		State(func(state *state.State) error {
			bal, _ := new(big.Int).SetString("1000000000000000000000000000", 10)
			state.SetCode(builtin.Authority.Address, builtin.Authority.RuntimeBytecodes())
			builtin.Params.Native(state).Set(thor.KeyExecutorAddress, new(big.Int).SetBytes(genesis.DevAccounts()[0].Address[:]))
			// for _, acc := range genesis.DevAccounts() {
			for _, acc := range accs {
				state.SetBalance(acc.addr, bal)
				state.SetEnergy(acc.addr, bal, launchTime)

				builtin.Authority.Native(state).Add(acc.addr, acc.addr, thor.Bytes32{}, acc.vrfpk.Bytes32())
				fmt.Printf("%x\n", acc.addr)
			}
			return nil
		})

	stateCreator := state.NewCreator(db)
	parent, _, err := gen.Build(stateCreator)
	if err != nil {
		t.Fatal(err)
	}

	c, err := chain.New(db, parent)
	if err != nil {
		t.Fatal(err)
	}

	forkConfig := thor.ForkConfig{
		VIP191:    math.MaxUint32,
		ETH_CONST: math.MaxUint32,
		BLOCKLIST: 0,
	}
	con := New(c, stateCreator, forkConfig)

	// proposer := genesis.DevAccounts()[0]

	var flow *packer.Flow
	var proposer *account
	now := launchTime + thor.BlockInterval
	for _, acc := range accs {
		p := packer.New(c, stateCreator, acc.addr, &acc.addr, thor.NoFork)
		flow, err = p.Schedule(parent.Header(), launchTime)
		if err != nil {
			continue
		}

		if flow.When() == now {
			proposer = acc
			break
		}
		flow = nil
	}
	if flow == nil {
		t.Fatal("No proposer found")
	}

	// block summary
	bs, _, err := flow.PackTxSetAndBlockSummary(proposer.ethsk)
	if err != nil {
		t.Fatal(err)
	}

	// endorsement
	for _, acc := range accs {
		if ok, proof, _ := con.IsCommittee(acc.vrfsk, now); ok {
			ed := block.NewEndorsement(bs, proof)
			sig, _ := crypto.Sign(ed.SigningHash().Bytes(), acc.ethsk)
			ed = ed.WithSignature(sig)
			flow.AddEndoresement(ed)

			// signer, _ := ed.Signer()
			fmt.Printf("orig: %x\n", *ed.VrfProof())
		}
		if uint64(flow.NumOfEndorsements()) >= thor.CommitteeSize {
			break
		}
	}
	if uint64(flow.NumOfEndorsements()) < thor.CommitteeSize {
		t.Errorf("Not enough endorsements added")
	}

	// block
	original, _, _, err := flow.Pack(proposer.ethsk)
	if err != nil {
		t.Fatal(t)
	}

	if _, _, err := con.Process(original, flow.When()); err != nil {
		t.Fatal(err)
	}

	return &testConsensus{
		t:        t,
		assert:   assert.New(t),
		con:      con,
		time:     flow.When(),
		pk:       proposer.ethsk,
		parent:   parent,
		original: original,
		tag:      c.Tag(),
	}
}

func (tc *testConsensus) sign(blk *block.Block) *block.Block {
	sig, err := crypto.Sign(blk.Header().SigningHash().Bytes(), tc.pk)
	if err != nil {
		tc.t.Fatal(err)
	}
	return blk.WithSignature(sig)
}

func (tc *testConsensus) originalBuilder() *block.Builder {
	header := tc.original.Header()
	return new(block.Builder).
		ParentID(header.ParentID()).
		Timestamp(header.Timestamp()).
		TotalScore(header.TotalScore()).
		GasLimit(header.GasLimit()).
		GasUsed(header.GasUsed()).
		Beneficiary(header.Beneficiary()).
		StateRoot(header.StateRoot()).
		ReceiptsRoot(header.ReceiptsRoot()).
		// vip193 features
		SigOnBlockSummary(header.SigOnBlockSummary()).
		SigsOnEndorsement(header.SigsOnEndoresment()).
		VrfProofs(header.VrfProofs())
}

func (tc *testConsensus) consent(blk *block.Block) error {
	_, _, err := tc.con.Process(blk, tc.time)
	return err
}

func (tc *testConsensus) TestValidateBlockHeader() {
	triggers := make(map[string]func())
	triggers["triggerErrTimestampBehindParent"] = func() {
		build := tc.originalBuilder()

		blk := tc.sign(build.Timestamp(tc.parent.Header().Timestamp()).Build())
		err := tc.consent(blk)
		expect := consensusError(
			fmt.Sprintf(
				"block timestamp behind parents: parent %v, current %v",
				tc.parent.Header().Timestamp(),
				blk.Header().Timestamp(),
			),
		)
		tc.assert.Equal(err, expect)

		blk = tc.sign(build.Timestamp(tc.parent.Header().Timestamp() - 1).Build())
		err = tc.consent(blk)
		expect = consensusError(
			fmt.Sprintf(
				"block timestamp behind parents: parent %v, current %v",
				tc.parent.Header().Timestamp(),
				blk.Header().Timestamp(),
			),
		)
		tc.assert.Equal(err, expect)
	}
	triggers["triggerErrInterval"] = func() {
		build := tc.originalBuilder()
		blk := tc.sign(build.Timestamp(tc.original.Header().Timestamp() + 1).Build())
		err := tc.consent(blk)
		expect := consensusError(
			fmt.Sprintf(
				"block interval not rounded: parent %v, current %v",
				tc.parent.Header().Timestamp(),
				blk.Header().Timestamp(),
			),
		)
		tc.assert.Equal(err, expect)
	}
	triggers["triggerErrFutureBlock"] = func() {
		build := tc.originalBuilder()
		blk := tc.sign(build.Timestamp(tc.time + thor.BlockInterval*2).Build())
		err := tc.consent(blk)
		tc.assert.Equal(err, errFutureBlock)
	}
	triggers["triggerInvalidGasLimit"] = func() {
		build := tc.originalBuilder()
		blk := tc.sign(build.GasLimit(tc.parent.Header().GasLimit() * 2).Build())
		err := tc.consent(blk)
		expect := consensusError(
			fmt.Sprintf(
				"block gas limit invalid: parent %v, current %v",
				tc.parent.Header().GasLimit(),
				blk.Header().GasLimit(),
			),
		)
		tc.assert.Equal(err, expect)
	}
	triggers["triggerExceedGaUsed"] = func() {
		build := tc.originalBuilder()
		blk := tc.sign(build.GasUsed(tc.original.Header().GasLimit() + 1).Build())
		err := tc.consent(blk)
		expect := consensusError(
			fmt.Sprintf(
				"block gas used exceeds limit: limit %v, used %v",
				tc.parent.Header().GasLimit(),
				blk.Header().GasUsed(),
			),
		)
		tc.assert.Equal(err, expect)
	}
	triggers["triggerInvalidTotalScore"] = func() {
		build := tc.originalBuilder()
		blk := tc.sign(build.TotalScore(tc.parent.Header().TotalScore()).Build())
		err := tc.consent(blk)
		expect := consensusError(
			fmt.Sprintf(
				"block total score invalid: parent %v, current %v",
				tc.parent.Header().TotalScore(),
				blk.Header().TotalScore(),
			),
		)
		tc.assert.Equal(err, expect)
	}

	for _, trigger := range triggers {
		trigger()
	}
}

func (tc *testConsensus) TestTxDepBroken() {
	txID := txSign(txBuilder(tc.tag)).ID()
	tx := txSign(txBuilder(tc.tag).DependsOn(&txID))
	err := tc.consent(
		tc.sign(
			tc.originalBuilder().Transaction(tx).Build(),
		),
	)
	tc.assert.Equal(err, consensusError("tx dep broken"))
}

func (tc *testConsensus) TestKnownBlock() {
	err := tc.consent(tc.parent)
	tc.assert.Equal(err, errKnownBlock)
}

func (tc *testConsensus) TestTxAlreadyExists() {
	tx := txSign(txBuilder(tc.tag))
	err := tc.consent(
		tc.sign(
			tc.originalBuilder().Transaction(tx).Transaction(tx).Build(),
		),
	)
	tc.assert.Equal(err, consensusError("tx already exists"))
}

func (tc *testConsensus) TestParentMissing() {
	build := tc.originalBuilder()
	blk := tc.sign(build.ParentID(tc.original.Header().ID()).Build())
	err := tc.consent(blk)
	tc.assert.Equal(err, errParentMissing)
}

func (tc *testConsensus) TestValidateBlockBody() {
	triggers := make(map[string]func())
	triggers["triggerErrTxSignerUnavailable"] = func() {
		blk := tc.sign(tc.originalBuilder().Transaction(txBuilder(tc.tag).Build()).Build())
		err := tc.consent(blk)
		expect := consensusError("tx signer unavailable: invalid signature length")
		tc.assert.Equal(err, expect)
	}

	triggers["triggerErrTxsRootMismatch"] = func() {
		transaction := txSign(txBuilder(tc.tag))
		transactions := tx.Transactions{transaction}
		blk := tc.sign(block.Compose(tc.original.Header(), transactions))
		err := tc.consent(blk)
		expect := consensusError(
			fmt.Sprintf(
				"block txs root mismatch: want %v, have %v",
				tc.original.Header().TxsRoot(),
				transactions.RootHash(),
			),
		)
		tc.assert.Equal(err, expect)
	}
	triggers["triggerErrChainTagMismatch"] = func() {
		err := tc.consent(
			tc.sign(
				tc.originalBuilder().Transaction(
					txSign(txBuilder(tc.tag + 1)),
				).Build(),
			),
		)
		expect := consensusError(
			fmt.Sprintf(
				"tx chain tag mismatch: want %v, have %v",
				tc.tag,
				tc.tag+1,
			),
		)
		tc.assert.Equal(err, expect)
	}
	triggers["triggerErrRefFutureBlock"] = func() {
		err := tc.consent(
			tc.sign(
				tc.originalBuilder().Transaction(
					txSign(txBuilder(tc.tag).BlockRef(tx.NewBlockRef(100))),
				).Build(),
			),
		)
		expect := consensusError("tx ref future block: ref 100, current 1")
		tc.assert.Equal(err, expect)
	}
	triggers["triggerTxOriginBlocked"] = func() {
		thor.MockBlocklist([]string{genesis.DevAccounts()[9].Address.String()})
		t := txBuilder(tc.tag).Build()
		sig, _ := crypto.Sign(t.SigningHash().Bytes(), genesis.DevAccounts()[9].PrivateKey)
		t = t.WithSignature(sig)

		blk := tc.sign(
			tc.originalBuilder().Transaction(t).Build(),
		)
		err := tc.consent(blk)
		expect := consensusError(
			fmt.Sprintf("tx origin blocked got packed: %v", genesis.DevAccounts()[9].Address),
		)
		tc.assert.Equal(err, expect)
	}

	for _, trigger := range triggers {
		trigger()
	}
}

func (tc *testConsensus) TestValidateProposer() {
	triggers := make(map[string]func())
	triggers["triggerErrSignerUnavailable"] = func() {
		blk := tc.originalBuilder().Build()
		err := tc.consent(blk)
		expect := consensusError("block signer unavailable: invalid signature length")
		tc.assert.Equal(err, expect)
	}
	triggers["triggerErrSignerInvalid"] = func() {
		blk := tc.originalBuilder().Build()
		pk, _ := crypto.GenerateKey()
		sig, _ := crypto.Sign(blk.Header().SigningHash().Bytes(), pk)
		blk = blk.WithSignature(sig)
		err := tc.consent(blk)
		expect := consensusError(
			fmt.Sprintf(
				"block signer invalid: %v unauthorized block proposer",
				thor.Address(crypto.PubkeyToAddress(pk.PublicKey)),
			),
		)
		tc.assert.Equal(err, expect)
	}
	triggers["triggerErrTimestampUnscheduled"] = func() {
		blk := tc.originalBuilder().Build()
		sig, _ := crypto.Sign(blk.Header().SigningHash().Bytes(), genesis.DevAccounts()[1].PrivateKey)
		blk = blk.WithSignature(sig)
		err := tc.consent(blk)
		expect := consensusError(
			fmt.Sprintf(
				"block timestamp unscheduled: t %v, s %v",
				blk.Header().Timestamp(),
				thor.Address(crypto.PubkeyToAddress(genesis.DevAccounts()[1].PrivateKey.PublicKey)),
			),
		)
		tc.assert.Equal(err, expect)
	}
	triggers["triggerTotalScoreInvalid"] = func() {
		build := tc.originalBuilder()
		blk := tc.sign(build.TotalScore(tc.original.Header().TotalScore() + 100).Build())
		err := tc.consent(blk)
		expect := consensusError("block total score invalid: want 1, have 101")
		tc.assert.Equal(err, expect)
	}

	for _, trigger := range triggers {
		trigger()
	}
}
