package runvm

import (
	"github.com/iotaledger/goshimmer/dapps/valuetransfers/packages/balance"
	"github.com/iotaledger/wasp/packages/kv"
	"github.com/iotaledger/wasp/packages/util"
	"github.com/iotaledger/wasp/packages/vm"
	"github.com/iotaledger/wasp/packages/vm/builtin"
	"github.com/iotaledger/wasp/packages/vm/processor"
	"github.com/iotaledger/wasp/packages/vm/sandbox"
	"github.com/iotaledger/wasp/packages/vm/vmconst"
)

// runTheRequest:
// - handles request token
// - processes reward logic
// - checks authorisations for protected requests
// - redirects reserved request codes (is supported) to hardcoded processing
// - redirects not reserved codes (is supported) to SC VM
// - in case of something not correct the whole operation is NOP, however
//   all the sent fees and other funds remains in the SC address (this may change).
func runTheRequest(ctx *vm.VMContext) {
	ctx.Log.Debugf("runTheRequest IN:\n%s\n", ctx.RequestRef.RequestBlock().String(ctx.RequestRef.RequestId()))

	if !handleRewards(ctx) {
		return
	}

	reqBlock := ctx.RequestRef.RequestBlock()
	if reqBlock.RequestCode().IsProtected() {
		// check authorisation
		if !ctx.RequestRef.IsAuthorised(&ctx.OwnerAddress) {
			// if protected call is not authorised by the containing transaction, do nothing
			// the result will be taking all iotas and no effect on state
			// Maybe it is nice to return back all iotas exceeding minimum reward ??? TODO

			ctx.Log.Warnf("protected request %s (code %s) is not authorised by %s",
				ctx.RequestRef.RequestId().String(), reqBlock.RequestCode(), ctx.OwnerAddress.String(),
			)
			ctx.Log.Debugw("protected request is not authorised",
				"req", ctx.RequestRef.RequestId().String(),
				"code", reqBlock.RequestCode(),
				"owner", ctx.OwnerAddress.String(),
				"inputs", util.InputsToStringByAddress(ctx.RequestRef.Tx.Inputs()),
			)
			return
		}
		if ctx.VirtualState.StateIndex() > 0 && !ctx.VirtualState.InitiatedBy(&ctx.OwnerAddress) {
			// for states after #0 it is required to have record about initiator's address in the solid state
			// to prevent attack when owner (initiator) address is overwritten in the quorum of bootup records
			// TODO protection may also be set at the lowest level of the solid state. i.e. some metadata that variable
			// is protected by some address and authorisation with that address is needed to modify the value

			ctx.Log.Errorf("inconsistent state: variable '%s' != owner record from bootup record '%s'",
				vmconst.VarNameOwnerAddress, ctx.OwnerAddress.String())

			return
		}
	}
	// authorisation check passed
	if reqBlock.RequestCode().IsReserved() {
		// finding and running builtin entry point
		entryPoint, ok := builtin.Processor.GetEntryPoint(reqBlock.RequestCode())
		if !ok {
			ctx.Log.Warnf("can't find entry point for request code %s in the builtin processor", reqBlock.RequestCode())
			return
		}
		entryPoint.Run(sandbox.NewSandbox(ctx))

		defer ctx.Log.Debugw("runTheRequest OUT BUILTIN",
			"reqId", ctx.RequestRef.RequestId().Short(),
			"programHash", ctx.ProgramHash.String(),
			"code", ctx.RequestRef.RequestBlock().RequestCode().String(),
			"state update", ctx.StateUpdate.String(),
		)
		return
	}

	// request requires user-defined program on VM
	proc, err := processor.Acquire(ctx.ProgramHash.String())
	if err != nil {
		ctx.Log.Warn(err)
		return
	}
	defer processor.Release(ctx.ProgramHash.String())

	entryPoint, ok := proc.GetEntryPoint(reqBlock.RequestCode())
	if !ok {
		ctx.Log.Warnf("can't find entry point for request code %s in the user-defined processor prog hash: %s",
			reqBlock.RequestCode(), ctx.ProgramHash.String())
		return
	}

	sandbox := sandbox.NewSandbox(ctx)
	func() {
		defer func() {
			if r := recover(); r != nil {
				ctx.Log.Errorf("Recovered from panic in SC: %v", r)
				if _, ok := r.(kv.DBError); ok {
					// There was an error accessing the DB
					// TODO invalidate the whole batch?
				}
				sandbox.Rollback()
			}
		}()
		entryPoint.Run(sandbox)
	}()

	defer ctx.Log.Debugw("runTheRequest OUT USER DEFINED",
		"reqId", ctx.RequestRef.RequestId().Short(),
		"programHash", ctx.ProgramHash.String(),
		"code", ctx.RequestRef.RequestBlock().RequestCode().String(),
		"state update", ctx.StateUpdate.String(),
	)
}

// handleRewards return true if to continue with request processing
func handleRewards(ctx *vm.VMContext) bool {
	if ctx.RewardAddress[0] == 0 {
		// first byte is never 0 for the correct address
		return true
	}
	if ctx.MinimumReward <= 0 {
		return true
	}
	if ctx.RequestRef.IsAuthorised(&ctx.Address) {
		// no need for rewards from itself
		return true
	}

	var err error

	reqTxId := ctx.RequestRef.Tx.ID()
	// determining how many iotas have been left in the request transaction
	availableIotas := ctx.TxBuilder.GetInputBalanceFromTransaction(balance.ColorIOTA, reqTxId)

	var proceed bool
	// taking into account 1 request token which will be recolored back to iota
	// and will remain in the smart contract address
	if availableIotas+1 >= ctx.MinimumReward {
		err = ctx.TxBuilder.MoveToAddressFromTransaction(ctx.RewardAddress, balance.ColorIOTA, ctx.MinimumReward, reqTxId)
		proceed = true
	} else {
		// if reward is not enough, the state update will be empty, i.e. NOP (the fee will be taken)
		err = ctx.TxBuilder.MoveToAddressFromTransaction(ctx.RewardAddress, balance.ColorIOTA, availableIotas, reqTxId)
		proceed = false
	}
	if err != nil {
		ctx.Log.Panicf("can't move reward tokens: %v", err)
	}
	return proceed
}
