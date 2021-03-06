package consensus

import (
	"fmt"
	"github.com/iotaledger/goshimmer/dapps/valuetransfers/packages/address"
	"github.com/iotaledger/goshimmer/dapps/valuetransfers/packages/address/signaturescheme"
	"github.com/iotaledger/goshimmer/dapps/valuetransfers/packages/balance"
	valuetransaction "github.com/iotaledger/goshimmer/dapps/valuetransfers/packages/transaction"
	"github.com/iotaledger/wasp/packages/committee"
	"github.com/iotaledger/wasp/packages/hashing"
	"github.com/iotaledger/wasp/packages/sctransaction"
	"github.com/iotaledger/wasp/packages/util"
	"github.com/iotaledger/wasp/packages/vm"
	"github.com/iotaledger/wasp/plugins/runvm"
)

type runCalculationsParams struct {
	requests        []*request
	leaderPeerIndex uint16
	balances        map[valuetransaction.ID][]*balance.Balance
	rewardAddress   address.Address
	timestamp       int64
}

// runs the VM for requests and posts result to committee's queue
func (op *operator) runCalculationsAsync(par runCalculationsParams) {
	if op.currentState == nil {
		op.log.Debugf("runCalculationsAsync: variable currentState is not known")
		return
	}
	numReqs := len(par.requests)
	if len(op.filterNotReadyYet(par.requests)) != numReqs {
		op.log.Errorf("runCalculationsAsync: inconsistency: some requests not ready yet")
		return
	}
	var progHash hashing.HashValue
	if ph, ok := op.getProgramHash(); ok {
		// may not be needed if ready requests are only built-in
		progHash = *ph
	}
	ctx := &vm.VMTask{
		LeaderPeerIndex: par.leaderPeerIndex,
		ProgramHash:     progHash,
		Address:         *op.committee.Address(),
		Color:           *op.committee.Color(),
		Entropy:         (hashing.HashValue)(op.stateTx.ID()),
		Balances:        par.balances,
		OwnerAddress:    *op.committee.OwnerAddress(),
		RewardAddress:   par.rewardAddress,
		MinimumReward:   op.getMinimumReward(),
		Requests:        takeRefs(par.requests),
		Timestamp:       par.timestamp,
		VirtualState:    op.currentState,
		Log:             op.log,
	}
	ctx.OnFinish = func(err error) {
		if err != nil {
			op.log.Errorf("VM task failed: %v", err)
			return
		}
		op.committee.ReceiveMessage(ctx)
	}
	if err := runvm.RunComputationsAsync(ctx); err != nil {
		op.log.Errorf("RunComputationsAsync: %v", err)
	}
}

func (op *operator) sendResultToTheLeader(result *vm.VMTask) {
	op.log.Debugw("sendResultToTheLeader")

	sigShare, err := op.dkshare.SignShare(result.ResultTransaction.EssenceBytes())
	if err != nil {
		op.log.Errorf("error while signing transaction %v", err)
		return
	}

	reqids := make([]sctransaction.RequestId, len(result.Requests))
	for i := range reqids {
		reqids[i] = *result.Requests[i].RequestId()
	}

	essenceHash := hashing.HashData(result.ResultTransaction.EssenceBytes())
	batchHash := vm.BatchHash(reqids, result.Timestamp, result.LeaderPeerIndex)

	op.log.Debugw("sendResultToTheLeader",
		"leader", result.LeaderPeerIndex,
		"batchHash", batchHash.String(),
		"essenceHash", essenceHash.String(),
		"ts", result.Timestamp,
	)

	msgData := util.MustBytes(&committee.SignedHashMsg{
		PeerMsgHeader: committee.PeerMsgHeader{
			StateIndex: op.mustStateIndex(),
		},
		BatchHash:     batchHash,
		OrigTimestamp: result.Timestamp,
		EssenceHash:   *essenceHash,
		SigShare:      sigShare,
	})

	if err := op.committee.SendMsg(result.LeaderPeerIndex, committee.MsgSignedHash, msgData); err != nil {
		op.log.Error(err)
	}
	// remember all sent transactions for this state index
	op.sentResultsToLeader[result.LeaderPeerIndex] = result.ResultTransaction
}

func (op *operator) saveOwnResult(result *vm.VMTask) {
	sigShare, err := op.dkshare.SignShare(result.ResultTransaction.EssenceBytes())
	if err != nil {
		op.log.Errorf("error while signing transaction %v", err)
		return
	}

	reqids := make([]sctransaction.RequestId, len(result.Requests))
	for i := range reqids {
		reqids[i] = *result.Requests[i].RequestId()
	}

	bh := vm.BatchHash(reqids, result.Timestamp, result.LeaderPeerIndex)
	if bh != op.leaderStatus.batchHash {
		panic("bh != op.leaderStatus.batchHash")
	}
	if len(result.Requests) != int(result.ResultBatch.Size()) {
		panic("len(result.Requests) != int(result.ResultBatch.Size())")
	}

	essenceHash := hashing.HashData(result.ResultTransaction.EssenceBytes())
	op.log.Debugw("saveOwnResult",
		"batchHash", bh.String(),
		"ts", result.Timestamp,
		"essenceHash", essenceHash.String(),
	)

	op.leaderStatus.resultTx = result.ResultTransaction
	op.leaderStatus.batch = result.ResultBatch
	op.leaderStatus.signedResults[op.committee.OwnPeerIndex()] = &signedResult{
		essenceHash: *essenceHash,
		sigShare:    sigShare,
	}
}

func (op *operator) aggregateSigShares(sigShares [][]byte) (signaturescheme.Signature, error) {
	resTx := op.leaderStatus.resultTx

	finalSignature, err := op.dkshare.RecoverFullSignature(sigShares, resTx.EssenceBytes())
	if err != nil {
		return nil, err
	}
	if err := resTx.PutSignature(finalSignature); err != nil {
		return nil, fmt.Errorf("something wrong while aggregating final signature: %v", err)
	}
	return finalSignature, nil
}
