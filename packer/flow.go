// Copyright (c) 2018 The VeChainThor developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package packer

import (
	"crypto/ecdsa"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/pkg/errors"
	"github.com/vechain/thor/block"
	"github.com/vechain/thor/runtime"
	"github.com/vechain/thor/state"
	"github.com/vechain/thor/thor"
	"github.com/vechain/thor/tx"
	"github.com/vechain/thor/vrf"
)

// Flow the flow of packing a new block.
type Flow struct {
	packer       *Packer
	parentHeader *block.Header
	runtime      *runtime.Runtime
	processedTxs map[thor.Bytes32]bool // txID -> reverted
	gasUsed      uint64
	txs          tx.Transactions
	receipts     tx.Receipts
	features     tx.Features

	blockSummary *block.Summary
	txSet        *block.TxSet
	endorsements block.Endorsements
	totalScore   uint64
}

// NewFlow ...
func NewFlow(
	packer *Packer,
	parentHeader *block.Header,
	runtime *runtime.Runtime,
	features tx.Features,
) *Flow {
	return &Flow{
		packer:       packer,
		parentHeader: parentHeader,
		runtime:      runtime,
		processedTxs: make(map[thor.Bytes32]bool),
		features:     features,
	}
}

// IncTotalScore ....
func (f *Flow) IncTotalScore(score uint64) {
	f.runtime.Context().TotalScore += score
}

// SetBlockSummary ...
func (f *Flow) SetBlockSummary(bs *block.Summary) {
	f.blockSummary = bs
}

// GetBlockSummary ...
func (f *Flow) GetBlockSummary() *block.Summary {
	return f.blockSummary
}

// IsEmpty ...
func (f *Flow) IsEmpty() bool {
	return f.packer == nil
}

// AddEndoresement stores an endorsement
func (f *Flow) AddEndoresement(ed *block.Endorsement) bool {
	return f.endorsements.Add(ed)
}

// NumEndorsement returns how many endorsements having been stored
func (f *Flow) NumEndorsement() int {
	return f.endorsements.Len()
}

// Txs ...
func (f *Flow) Txs() tx.Transactions {
	return f.txs
}

// PackTxSetAndBlockSummary packs the tx set and block summary
func (f *Flow) PackTxSetAndBlockSummary(sk *ecdsa.PrivateKey, totalScore uint64) error {
	var (
		sig []byte
		err error
	)

	if f.packer.nodeMaster != thor.Address(crypto.PubkeyToAddress(sk.PublicKey)) {
		return errors.New("private key mismatch")
	}

	// pack tx set
	ts := block.NewTxSet(f.txs)
	sig, err = crypto.Sign(ts.SigningHash().Bytes(), sk)
	if err != nil {
		return err
	}
	f.txSet = ts.WithSignature(sig)

	// pack block summary
	best := f.packer.chain.BestBlock()
	parent := best.Header().ID()
	root := f.txSet.RootHash()
	time := best.Header().Timestamp() + thor.BlockInterval
	bs := block.NewBlockSummary(parent, root, time, f.totalScore)
	sig, err = crypto.Sign(bs.SigningHash().Bytes(), sk)
	if err != nil {
		return err
	}
	f.blockSummary = bs.WithSignature(sig)

	return nil
}

// ParentHeader returns parent block header.
func (f *Flow) ParentHeader() *block.Header {
	return f.parentHeader
}

// When the target time to do packing.
func (f *Flow) When() uint64 {
	return f.runtime.Context().Time
}

// TotalScore returns total score of new block.
func (f *Flow) TotalScore() uint64 {
	return f.runtime.Context().TotalScore
}

func (f *Flow) findTx(txID thor.Bytes32) (found bool, reverted bool, err error) {
	if reverted, ok := f.processedTxs[txID]; ok {
		return true, reverted, nil
	}
	txMeta, err := f.runtime.Chain().GetTransactionMeta(txID)
	if err != nil {
		if f.packer.repo.IsNotFound(err) {
			return false, false, nil
		}
		return false, false, err
	}
	return true, txMeta.Reverted, nil
}

// Adopt try to execute the given transaction.
// If the tx is valid and can be executed on current state (regardless of VM error),
// it will be adopted by the new block.
func (f *Flow) Adopt(tx *tx.Transaction) error {
	origin, _ := tx.Origin()
	if f.runtime.Context().Number >= f.packer.forkConfig.BLOCKLIST && thor.IsOriginBlocked(origin) {
		return badTxError{"tx origin blocked"}
	}

	if err := tx.TestFeatures(f.features); err != nil {
		return badTxError{err.Error()}
	}

	switch {
	case tx.ChainTag() != f.packer.repo.ChainTag():
		return badTxError{"chain tag mismatch"}
	case f.runtime.Context().Number < tx.BlockRef().Number():
		return errTxNotAdoptableNow
	case tx.IsExpired(f.runtime.Context().Number):
		return badTxError{"expired"}
	case f.gasUsed+tx.Gas() > f.runtime.Context().GasLimit:
		// has enough space to adopt minimum tx
		if f.gasUsed+thor.TxGas+thor.ClauseGas <= f.runtime.Context().GasLimit {
			// try to find a lower gas tx
			return errTxNotAdoptableNow
		}
		return errGasLimitReached
	}

	// check if tx already there
	if found, _, err := f.findTx(tx.ID()); err != nil {
		return err
	} else if found {
		return errKnownTx
	}

	if dependsOn := tx.DependsOn(); dependsOn != nil {
		// check if deps exists
		found, reverted, err := f.findTx(*dependsOn)
		if err != nil {
			return err
		}
		if !found {
			return errTxNotAdoptableNow
		}
		if reverted {
			return errTxNotAdoptableForever
		}
	}

	checkpoint := f.runtime.State().NewCheckpoint()
	receipt, err := f.runtime.ExecuteTransaction(tx)
	if err != nil {
		// skip and revert state
		f.runtime.State().RevertTo(checkpoint)
		return badTxError{err.Error()}
	}
	f.processedTxs[tx.ID()] = receipt.Reverted
	f.gasUsed += receipt.GasUsed
	f.receipts = append(f.receipts, receipt)
	f.txs = append(f.txs, tx)
	return nil
}

// Pack build and sign the new block.
func (f *Flow) Pack(privateKey *ecdsa.PrivateKey) (*block.Block, *state.Stage, tx.Receipts, error) {
	if f.packer.nodeMaster != thor.Address(crypto.PubkeyToAddress(privateKey.PublicKey)) {
		return nil, nil, nil, errors.New("private key mismatch")
	}

	stage, err := f.runtime.State().Stage()
	if err != nil {
		return nil, nil, nil, err
	}
	stateRoot := stage.Hash()

	builder := new(block.Builder).
		Beneficiary(f.runtime.Context().Beneficiary).
		GasLimit(f.runtime.Context().GasLimit).
		ParentID(f.parentHeader.ID()).
		Timestamp(f.runtime.Context().Time).
		TotalScore(f.runtime.Context().TotalScore).
		GasUsed(f.gasUsed).
		ReceiptsRoot(f.receipts.RootHash()).
		StateRoot(stateRoot).
		TransactionFeatures(f.features)

	for _, tx := range f.txs {
		builder.Transaction(tx)
	}
	newBlock := builder.Build()

	sig, err := crypto.Sign(newBlock.Header().SigningHash().Bytes(), privateKey)
	if err != nil {
		return nil, nil, nil, err
	}
	return newBlock.WithSignature(sig), stage, f.receipts, nil
}

// PackHeader build the new block header.
func (f *Flow) PackHeader(sk *ecdsa.PrivateKey, p []*vrf.Proof, s1 []byte, s2 [][]byte) (*block.Header, *state.Stage, tx.Receipts, error) {
	if f.packer.nodeMaster != thor.Address(crypto.PubkeyToAddress(sk.PublicKey)) {
		return nil, nil, nil, errors.New("private key mismatch")
	}

	if err := f.runtime.Seeker().Err(); err != nil {
		return nil, nil, nil, err
	}

	stage := f.runtime.State().Stage()
	stateRoot, err := stage.Hash()
	if err != nil {
		return nil, nil, nil, err
	}

	builder := new(block.HeaderBuilder).
		Beneficiary(f.runtime.Context().Beneficiary).
		GasLimit(f.runtime.Context().GasLimit).
		ParentID(f.parentHeader.ID()).
		Timestamp(f.runtime.Context().Time).
		TotalScore(f.runtime.Context().TotalScore).
		GasUsed(f.gasUsed).
		ReceiptsRoot(f.receipts.RootHash()).
		StateRoot(stateRoot).
		TransactionFeatures(f.features).
		// Committee(c).
		VrfProofs(p).SigOnBlockSummary(s1).SigOnEndorsement(s2)

	header := builder.Build()

	sig, err := crypto.Sign(header.SigningHash().Bytes(), sk)
	if err != nil {
		return nil, nil, nil, err
	}
	return header.WithSignature(sig), stage, f.receipts, nil
}
