// Copyright (c) 2018 The VeChainThor developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package consensus

import (
	"github.com/hashicorp/golang-lru/simplelru"

	"github.com/vechain/thor/block"
	"github.com/vechain/thor/builtin"
	"github.com/vechain/thor/chain"
	"github.com/vechain/thor/runtime"
	"github.com/vechain/thor/state"
	"github.com/vechain/thor/thor"
	"github.com/vechain/thor/tx"
	"github.com/vechain/thor/xenv"
)

// Consensus check whether the block is verified,
// and predicate which trunk it belong to.
type Consensus struct {
	repo                 *chain.Repository
	stater               *state.Stater
	forkConfig           thor.ForkConfig
	correctReceiptsRoots map[string]string
	candidatesCache      *simplelru.LRU
	beaconCache          *simplelru.LRU
}

// New create a Consensus instance.
func New(repo *chain.Repository, stater *state.Stater, forkConfig thor.ForkConfig) *Consensus {
	candidatesCache, _ := simplelru.NewLRU(16, nil)
	beaconCache, _ := simplelru.NewLRU(16, nil)

	return &Consensus{
		repo:                 repo,
		stater:               stater,
		forkConfig:           forkConfig,
		correctReceiptsRoots: thor.LoadCorrectReceiptsRoots(),
		candidatesCache:      candidatesCache,
		beaconCache:          beaconCache,
	}
}

// Process process a block.
func (c *Consensus) Process(blk *block.Block, nowTimestamp uint64) (*state.Stage, tx.Receipts, error) {
	header := blk.Header()

	if _, err := c.repo.GetBlockSummary(header.ID()); err != nil {
		if !c.repo.IsNotFound(err) {
			return nil, nil, err
		}
	} else {
		return nil, nil, errKnownBlock
	}

	parentSummary, err := c.repo.GetBlockSummary(header.ParentID())
	if err != nil {
		if !c.repo.IsNotFound(err) {
			return nil, nil, err
		}
		return nil, nil, errParentMissing
	}

	state := c.stater.NewState(parentSummary.Header.StateRoot())

	vip191 := c.forkConfig.VIP191
	if vip191 == 0 {
		vip191 = 1
	}
	// Before process hook of VIP-191, update builtin extension contract's code to V2
	if header.Number() == vip191 {
		if err := state.SetCode(builtin.Extension.Address, builtin.Extension.V2.RuntimeBytecodes()); err != nil {
			return nil, nil, err
		}
	}

	var features tx.Features
	if header.Number() >= vip191 {
		features |= tx.DelegationFeature
	}

	if header.TxsFeatures() != features {
		return nil, nil, newConsensusError("Process", strErrTxFeatures,
			[]string{strDataExpected, strDataCurr},
			[]interface{}{features, header.TxsFeatures()}, "")
	}

	vip193 := c.forkConfig.VIP193
	if vip193 == 0 {
		vip193 = 1
	}
	if header.Number() == vip193 {
		if err := state.SetCode(builtin.Authority.Address, builtin.Authority.V2.RuntimeBytecodes()); err != nil {
			return nil, nil, newConsensusError("Process", "failed to add authority v2 bytecode", nil, nil, err.Error())
		}
		if err := c.UpdateConsensusNodesForVip193(header); err != nil {
			return nil, nil, err.(consensusError).AddTraceInfo("Process")
		}
	}

	stage, receipts, err := c.validate(state, blk, parentSummary.Header, nowTimestamp)
	if err != nil {
		return nil, nil, err
	}

	return stage, receipts, nil
}

// NewRuntimeForReplay ...
func (c *Consensus) NewRuntimeForReplay(header *block.Header, skipPoA bool) (*runtime.Runtime, error) {
	signer, err := header.Signer()
	if err != nil {
		return nil, err
	}
	parentSummary, err := c.repo.GetBlockSummary(header.ParentID())
	if err != nil {
		if !c.repo.IsNotFound(err) {
			return nil, err
		}
		return nil, errParentMissing
	}
	state := c.stater.NewState(parentSummary.Header.StateRoot())
	if !skipPoA {
		if _, err := c.validateProposer(header, parentSummary.Header, state); err != nil {
			return nil, err
		}
	}

	return runtime.New(
		c.repo.NewChain(header.ParentID()),
		state,
		&xenv.BlockContext{
			Beneficiary: header.Beneficiary(),
			Signer:      signer,
			Number:      header.Number(),
			Time:        header.Timestamp(),
			GasLimit:    header.GasLimit(),
			TotalScore:  header.TotalScore(),
		},
		c.forkConfig), nil
}

// UpdateConsensusNodesForVip193 adds vrf public key for each existing consensus node
func (c *Consensus) UpdateConsensusNodesForVip193(header *block.Header) error {
	if c.forkConfig.VIP193 != header.Number() {
		return newConsensusError("UpdateNode", "block number inconsistent with vip193",
			[]string{strDataExp, strDataCurr},
			[]interface{}{c.forkConfig.VIP193, header.Number()}, "")
	}

	st := c.stater.NewState(header.ID())
	aut := builtin.Authority.Native(st)
	candidates, err := aut.AllCandidates()
	if err != nil {
		return err
	}

	for i, candidate := range candidates {
		vrfpk := thor.GetVrfPuiblicKey(candidate.NodeMaster)
		if vrfpk.IsZero() {
			return newConsensusError("UpdateNode", "vrf public key not found",
				[]string{"node"}, []interface{}{candidate.NodeMaster}, "")
		}

		candidate.VrfPublicKey = vrfpk

		ok, err := aut.Add2(candidate.NodeMaster, candidate.Endorsor, candidate.Identity, candidate.VrfPublicKey)
		var causeMsg string
		if err != nil {
			causeMsg = err.Error()
		}
		if !ok || err != nil {
			return newConsensusError("UpdateNode", "failed to add node",
				[]string{"node"}, []interface{}{candidate.NodeMaster}, causeMsg)
		}

		if !candidate.Active {
			ok, err := aut.Update2(candidate.NodeMaster, false)
			var causeMsg string
			if err != nil {
				causeMsg = err.Error()
			}
			if (!ok && i > 0) || err != nil {
				return newConsensusError("UpdateNode", "failed to update node status",
					[]string{"node"}, []interface{}{candidate.NodeMaster}, causeMsg)
			}
		}
	}

	// Update again the status of the first node because when it is added to the state
	// it is unlinked and therefore, its status is not updated
	if !candidates[0].Active {
		ok, err := aut.Update2(candidates[0].NodeMaster, false)
		var causeMsg string
		if err != nil {
			causeMsg = err.Error()
		}
		if !ok || err != nil {
			return newConsensusError("UpdateNode", "failed to update node status",
				[]string{"node"}, []interface{}{candidates[0].NodeMaster}, causeMsg)
		}
	}

	return nil
}
