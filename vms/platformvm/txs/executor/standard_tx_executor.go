// Copyright (C) 2019-2024, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/MetalBlockchain/metalgo/chains/atomic"
	"github.com/MetalBlockchain/metalgo/ids"
	"github.com/MetalBlockchain/metalgo/utils/constants"
	"github.com/MetalBlockchain/metalgo/utils/set"
	"github.com/MetalBlockchain/metalgo/vms/components/avax"
	"github.com/MetalBlockchain/metalgo/vms/components/verify"
	"github.com/MetalBlockchain/metalgo/vms/platformvm/state"
	"github.com/MetalBlockchain/metalgo/vms/platformvm/txs"
)

var (
	_ txs.Visitor = (*StandardTxExecutor)(nil)

	errEmptyNodeID                = errors.New("validator nodeID cannot be empty")
	errMaxStakeDurationTooLarge   = errors.New("max stake duration must be less than or equal to the global max stake duration")
	errMissingStartTimePreDurango = errors.New("staker transactions must have a StartTime pre-Durango")
)

type StandardTxExecutor struct {
	// inputs, to be filled before visitor methods are called
	*Backend
	State state.Diff // state is expected to be modified
	Tx    *txs.Tx

	// outputs of visitor execution
	OnAccept       func() // may be nil
	Inputs         set.Set[ids.ID]
	AtomicRequests map[ids.ID]*atomic.Requests // may be nil
}

func (*StandardTxExecutor) AdvanceTimeTx(*txs.AdvanceTimeTx) error {
	return ErrWrongTxType
}

func (*StandardTxExecutor) RewardValidatorTx(*txs.RewardValidatorTx) error {
	return ErrWrongTxType
}

func (e *StandardTxExecutor) CreateChainTx(tx *txs.CreateChainTx) error {
	if err := e.Tx.SyntacticVerify(e.Ctx); err != nil {
		return err
	}

	var (
		currentTimestamp = e.State.GetTimestamp()
		isDurangoActive  = e.Config.IsDurangoActivated(currentTimestamp)
	)
	if err := avax.VerifyMemoFieldLength(tx.Memo, isDurangoActive); err != nil {
		return err
	}

	baseTxCreds, err := verifyPoASubnetAuthorization(e.Backend, e.State, e.Tx, tx.SubnetID, tx.SubnetAuth)
	if err != nil {
		return err
	}

	// Verify the flowcheck
	createBlockchainTxFee := e.Config.GetCreateBlockchainTxFee(currentTimestamp)
	if err := e.FlowChecker.VerifySpend(
		tx,
		e.State,
		tx.Ins,
		tx.Outs,
		baseTxCreds,
		map[ids.ID]uint64{
			e.Ctx.AVAXAssetID: createBlockchainTxFee,
		},
	); err != nil {
		return err
	}

	txID := e.Tx.ID()

	// Consume the UTXOS
	avax.Consume(e.State, tx.Ins)
	// Produce the UTXOS
	avax.Produce(e.State, txID, tx.Outs)
	// Add the new chain to the database
	e.State.AddChain(e.Tx)

	// If this proposal is committed and this node is a member of the subnet
	// that validates the blockchain, create the blockchain
	e.OnAccept = func() {
		e.Config.CreateChain(txID, tx)
	}
	return nil
}

func (e *StandardTxExecutor) CreateSubnetTx(tx *txs.CreateSubnetTx) error {
	// Make sure this transaction is well formed.
	if err := e.Tx.SyntacticVerify(e.Ctx); err != nil {
		return err
	}

	var (
		currentTimestamp = e.State.GetTimestamp()
		isDurangoActive  = e.Config.IsDurangoActivated(currentTimestamp)
	)
	if err := avax.VerifyMemoFieldLength(tx.Memo, isDurangoActive); err != nil {
		return err
	}

	// Verify the flowcheck
	createSubnetTxFee := e.Config.GetCreateSubnetTxFee(currentTimestamp)
	if err := e.FlowChecker.VerifySpend(
		tx,
		e.State,
		tx.Ins,
		tx.Outs,
		e.Tx.Creds,
		map[ids.ID]uint64{
			e.Ctx.AVAXAssetID: createSubnetTxFee,
		},
	); err != nil {
		return err
	}

	txID := e.Tx.ID()

	// Consume the UTXOS
	avax.Consume(e.State, tx.Ins)
	// Produce the UTXOS
	avax.Produce(e.State, txID, tx.Outs)
	// Add the new subnet to the database
	e.State.AddSubnet(e.Tx)
	e.State.SetSubnetOwner(txID, tx.Owner)
	return nil
}

func (e *StandardTxExecutor) ImportTx(tx *txs.ImportTx) error {
	if err := e.Tx.SyntacticVerify(e.Ctx); err != nil {
		return err
	}

	var (
		currentTimestamp = e.State.GetTimestamp()
		isDurangoActive  = e.Config.IsDurangoActivated(currentTimestamp)
	)
	if err := avax.VerifyMemoFieldLength(tx.Memo, isDurangoActive); err != nil {
		return err
	}

	e.Inputs = set.NewSet[ids.ID](len(tx.ImportedInputs))
	utxoIDs := make([][]byte, len(tx.ImportedInputs))
	for i, in := range tx.ImportedInputs {
		utxoID := in.UTXOID.InputID()

		e.Inputs.Add(utxoID)
		utxoIDs[i] = utxoID[:]
	}

	// Skip verification of the shared memory inputs if the other primary
	// network chains are not guaranteed to be up-to-date.
	if e.Bootstrapped.Get() && !e.Config.PartialSyncPrimaryNetwork {
		if err := verify.SameSubnet(context.TODO(), e.Ctx, tx.SourceChain); err != nil {
			return err
		}

		allUTXOBytes, err := e.Ctx.SharedMemory.Get(tx.SourceChain, utxoIDs)
		if err != nil {
			return fmt.Errorf("failed to get shared memory: %w", err)
		}

		utxos := make([]*avax.UTXO, len(tx.Ins)+len(tx.ImportedInputs))
		for index, input := range tx.Ins {
			utxo, err := e.State.GetUTXO(input.InputID())
			if err != nil {
				return fmt.Errorf("failed to get UTXO %s: %w", &input.UTXOID, err)
			}
			utxos[index] = utxo
		}
		for i, utxoBytes := range allUTXOBytes {
			utxo := &avax.UTXO{}
			if _, err := txs.Codec.Unmarshal(utxoBytes, utxo); err != nil {
				return fmt.Errorf("failed to unmarshal UTXO: %w", err)
			}
			utxos[i+len(tx.Ins)] = utxo
		}

		ins := make([]*avax.TransferableInput, len(tx.Ins)+len(tx.ImportedInputs))
		copy(ins, tx.Ins)
		copy(ins[len(tx.Ins):], tx.ImportedInputs)

		if err := e.FlowChecker.VerifySpendUTXOs(
			tx,
			utxos,
			ins,
			tx.Outs,
			e.Tx.Creds,
			map[ids.ID]uint64{
				e.Ctx.AVAXAssetID: e.Config.TxFee,
			},
		); err != nil {
			return err
		}
	}

	txID := e.Tx.ID()

	// Consume the UTXOS
	avax.Consume(e.State, tx.Ins)
	// Produce the UTXOS
	avax.Produce(e.State, txID, tx.Outs)

	// Note: We apply atomic requests even if we are not verifying atomic
	// requests to ensure the shared state will be correct if we later start
	// verifying the requests.
	e.AtomicRequests = map[ids.ID]*atomic.Requests{
		tx.SourceChain: {
			RemoveRequests: utxoIDs,
		},
	}
	return nil
}

func (e *StandardTxExecutor) ExportTx(tx *txs.ExportTx) error {
	if err := e.Tx.SyntacticVerify(e.Ctx); err != nil {
		return err
	}

	var (
		currentTimestamp = e.State.GetTimestamp()
		isDurangoActive  = e.Config.IsDurangoActivated(currentTimestamp)
	)
	if err := avax.VerifyMemoFieldLength(tx.Memo, isDurangoActive); err != nil {
		return err
	}

	outs := make([]*avax.TransferableOutput, len(tx.Outs)+len(tx.ExportedOutputs))
	copy(outs, tx.Outs)
	copy(outs[len(tx.Outs):], tx.ExportedOutputs)

	if e.Bootstrapped.Get() {
		if err := verify.SameSubnet(context.TODO(), e.Ctx, tx.DestinationChain); err != nil {
			return err
		}
	}

	// Verify the flowcheck
	if err := e.FlowChecker.VerifySpend(
		tx,
		e.State,
		tx.Ins,
		outs,
		e.Tx.Creds,
		map[ids.ID]uint64{
			e.Ctx.AVAXAssetID: e.Config.TxFee,
		},
	); err != nil {
		return fmt.Errorf("failed verifySpend: %w", err)
	}

	txID := e.Tx.ID()

	// Consume the UTXOS
	avax.Consume(e.State, tx.Ins)
	// Produce the UTXOS
	avax.Produce(e.State, txID, tx.Outs)

	// Note: We apply atomic requests even if we are not verifying atomic
	// requests to ensure the shared state will be correct if we later start
	// verifying the requests.
	elems := make([]*atomic.Element, len(tx.ExportedOutputs))
	for i, out := range tx.ExportedOutputs {
		utxo := &avax.UTXO{
			UTXOID: avax.UTXOID{
				TxID:        txID,
				OutputIndex: uint32(len(tx.Outs) + i),
			},
			Asset: avax.Asset{ID: out.AssetID()},
			Out:   out.Out,
		}

		utxoBytes, err := txs.Codec.Marshal(txs.CodecVersion, utxo)
		if err != nil {
			return fmt.Errorf("failed to marshal UTXO: %w", err)
		}
		utxoID := utxo.InputID()
		elem := &atomic.Element{
			Key:   utxoID[:],
			Value: utxoBytes,
		}
		if out, ok := utxo.Out.(avax.Addressable); ok {
			elem.Traits = out.Addresses()
		}

		elems[i] = elem
	}
	e.AtomicRequests = map[ids.ID]*atomic.Requests{
		tx.DestinationChain: {
			PutRequests: elems,
		},
	}
	return nil
}

func (e *StandardTxExecutor) AddValidatorTx(tx *txs.AddValidatorTx) error {
	if tx.Validator.NodeID == ids.EmptyNodeID {
		return errEmptyNodeID
	}

	if _, err := verifyAddValidatorTx(
		e.Backend,
		e.State,
		e.Tx,
		tx,
	); err != nil {
		return err
	}

	if err := e.putStaker(tx); err != nil {
		return err
	}

	txID := e.Tx.ID()
	avax.Consume(e.State, tx.Ins)
	avax.Produce(e.State, txID, tx.Outs)

	if e.Config.PartialSyncPrimaryNetwork && tx.Validator.NodeID == e.Ctx.NodeID {
		e.Ctx.Log.Warn("verified transaction that would cause this node to become unhealthy",
			zap.String("reason", "primary network is not being fully synced"),
			zap.Stringer("txID", txID),
			zap.String("txType", "addValidator"),
			zap.Stringer("nodeID", tx.Validator.NodeID),
		)
	}
	return nil
}

func (e *StandardTxExecutor) AddSubnetValidatorTx(tx *txs.AddSubnetValidatorTx) error {
	if err := verifyAddSubnetValidatorTx(
		e.Backend,
		e.State,
		e.Tx,
		tx,
	); err != nil {
		return err
	}

	if err := e.putStaker(tx); err != nil {
		return err
	}

	txID := e.Tx.ID()
	avax.Consume(e.State, tx.Ins)
	avax.Produce(e.State, txID, tx.Outs)
	return nil
}

func (e *StandardTxExecutor) AddDelegatorTx(tx *txs.AddDelegatorTx) error {
	if _, err := verifyAddDelegatorTx(
		e.Backend,
		e.State,
		e.Tx,
		tx,
	); err != nil {
		return err
	}

	if err := e.putStaker(tx); err != nil {
		return err
	}

	txID := e.Tx.ID()
	avax.Consume(e.State, tx.Ins)
	avax.Produce(e.State, txID, tx.Outs)
	return nil
}

// Verifies a [*txs.RemoveSubnetValidatorTx] and, if it passes, executes it on
// [e.State]. For verification rules, see [verifyRemoveSubnetValidatorTx]. This
// transaction will result in [tx.NodeID] being removed as a validator of
// [tx.SubnetID].
// Note: [tx.NodeID] may be either a current or pending validator.
func (e *StandardTxExecutor) RemoveSubnetValidatorTx(tx *txs.RemoveSubnetValidatorTx) error {
	staker, isCurrentValidator, err := verifyRemoveSubnetValidatorTx(
		e.Backend,
		e.State,
		e.Tx,
		tx,
	)
	if err != nil {
		return err
	}

	if isCurrentValidator {
		e.State.DeleteCurrentValidator(staker)
	} else {
		e.State.DeletePendingValidator(staker)
	}

	// Invariant: There are no permissioned subnet delegators to remove.

	txID := e.Tx.ID()
	avax.Consume(e.State, tx.Ins)
	avax.Produce(e.State, txID, tx.Outs)

	return nil
}

func (e *StandardTxExecutor) TransformSubnetTx(tx *txs.TransformSubnetTx) error {
	if err := e.Tx.SyntacticVerify(e.Ctx); err != nil {
		return err
	}

	var (
		currentTimestamp = e.State.GetTimestamp()
		isDurangoActive  = e.Config.IsDurangoActivated(currentTimestamp)
	)
	if err := avax.VerifyMemoFieldLength(tx.Memo, isDurangoActive); err != nil {
		return err
	}

	// Note: math.MaxInt32 * time.Second < math.MaxInt64 - so this can never
	// overflow.
	if time.Duration(tx.MaxStakeDuration)*time.Second > e.Backend.Config.MaxStakeDuration {
		return errMaxStakeDurationTooLarge
	}

	baseTxCreds, err := verifyPoASubnetAuthorization(e.Backend, e.State, e.Tx, tx.Subnet, tx.SubnetAuth)
	if err != nil {
		return err
	}

	totalRewardAmount := tx.MaximumSupply - tx.InitialSupply
	if err := e.Backend.FlowChecker.VerifySpend(
		tx,
		e.State,
		tx.Ins,
		tx.Outs,
		baseTxCreds,
		// Invariant: [tx.AssetID != e.Ctx.AVAXAssetID]. This prevents the first
		//            entry in this map literal from being overwritten by the
		//            second entry.
		map[ids.ID]uint64{
			e.Ctx.AVAXAssetID: e.Config.TransformSubnetTxFee,
			tx.AssetID:        totalRewardAmount,
		},
	); err != nil {
		return err
	}

	txID := e.Tx.ID()

	// Consume the UTXOS
	avax.Consume(e.State, tx.Ins)
	// Produce the UTXOS
	avax.Produce(e.State, txID, tx.Outs)
	// Transform the new subnet in the database
	e.State.AddSubnetTransformation(e.Tx)
	e.State.SetCurrentSupply(tx.Subnet, tx.InitialSupply)
	return nil
}

func (e *StandardTxExecutor) AddPermissionlessValidatorTx(tx *txs.AddPermissionlessValidatorTx) error {
	if err := verifyAddPermissionlessValidatorTx(
		e.Backend,
		e.State,
		e.Tx,
		tx,
	); err != nil {
		return err
	}

	if err := e.putStaker(tx); err != nil {
		return err
	}

	txID := e.Tx.ID()
	avax.Consume(e.State, tx.Ins)
	avax.Produce(e.State, txID, tx.Outs)

	if e.Config.PartialSyncPrimaryNetwork &&
		tx.Subnet == constants.PrimaryNetworkID &&
		tx.Validator.NodeID == e.Ctx.NodeID {
		e.Ctx.Log.Warn("verified transaction that would cause this node to become unhealthy",
			zap.String("reason", "primary network is not being fully synced"),
			zap.Stringer("txID", txID),
			zap.String("txType", "addPermissionlessValidator"),
			zap.Stringer("nodeID", tx.Validator.NodeID),
		)
	}

	return nil
}

func (e *StandardTxExecutor) AddPermissionlessDelegatorTx(tx *txs.AddPermissionlessDelegatorTx) error {
	if err := verifyAddPermissionlessDelegatorTx(
		e.Backend,
		e.State,
		e.Tx,
		tx,
	); err != nil {
		return err
	}

	if err := e.putStaker(tx); err != nil {
		return err
	}

	txID := e.Tx.ID()
	avax.Consume(e.State, tx.Ins)
	avax.Produce(e.State, txID, tx.Outs)
	return nil
}

// Verifies a [*txs.TransferSubnetOwnershipTx] and, if it passes, executes it on
// [e.State]. For verification rules, see [verifyTransferSubnetOwnershipTx].
// This transaction will result in the ownership of [tx.Subnet] being transferred
// to [tx.Owner].
func (e *StandardTxExecutor) TransferSubnetOwnershipTx(tx *txs.TransferSubnetOwnershipTx) error {
	err := verifyTransferSubnetOwnershipTx(
		e.Backend,
		e.State,
		e.Tx,
		tx,
	)
	if err != nil {
		return err
	}

	e.State.SetSubnetOwner(tx.Subnet, tx.Owner)

	txID := e.Tx.ID()
	avax.Consume(e.State, tx.Ins)
	avax.Produce(e.State, txID, tx.Outs)
	return nil
}

func (e *StandardTxExecutor) BaseTx(tx *txs.BaseTx) error {
	if !e.Backend.Config.IsDurangoActivated(e.State.GetTimestamp()) {
		return ErrDurangoUpgradeNotActive
	}

	// Verify the tx is well-formed
	if err := e.Tx.SyntacticVerify(e.Ctx); err != nil {
		return err
	}

	if err := avax.VerifyMemoFieldLength(tx.Memo, true /*=isDurangoActive*/); err != nil {
		return err
	}

	// Verify the flowcheck
	if err := e.FlowChecker.VerifySpend(
		tx,
		e.State,
		tx.Ins,
		tx.Outs,
		e.Tx.Creds,
		map[ids.ID]uint64{
			e.Ctx.AVAXAssetID: e.Config.TxFee,
		},
	); err != nil {
		return err
	}

	txID := e.Tx.ID()
	// Consume the UTXOS
	avax.Consume(e.State, tx.Ins)
	// Produce the UTXOS
	avax.Produce(e.State, txID, tx.Outs)
	return nil
}

// Creates the staker as defined in [stakerTx] and adds it to [e.State].
func (e *StandardTxExecutor) putStaker(stakerTx txs.Staker) error {
	var (
		chainTime = e.State.GetTimestamp()
		txID      = e.Tx.ID()
		staker    *state.Staker
		err       error
	)

	if !e.Config.IsDurangoActivated(chainTime) {
		// Pre-Durango, stakers set a future [StartTime] and are added to the
		// pending staker set. They are promoted to the current staker set once
		// the chain time reaches [StartTime].
		scheduledStakerTx, ok := stakerTx.(txs.ScheduledStaker)
		if !ok {
			return fmt.Errorf("%w: %T", errMissingStartTimePreDurango, stakerTx)
		}
		staker, err = state.NewPendingStaker(txID, scheduledStakerTx)
	} else {
		// Only calculate the potentialReward for permissionless stakers.
		// Recall that we only need to check if this is a permissioned
		// validator as there are no permissioned delegators
		var potentialReward uint64
		if !stakerTx.CurrentPriority().IsPermissionedValidator() {
			subnetID := stakerTx.SubnetID()
			currentSupply, err := e.State.GetCurrentSupply(subnetID)
			if err != nil {
				return err
			}

			rewards, err := GetRewardsCalculator(e.Backend, e.State, subnetID)
			if err != nil {
				return err
			}

			// Post-Durango, stakers are immediately added to the current staker
			// set. Their [StartTime] is the current chain time.
			stakeDuration := stakerTx.EndTime().Sub(chainTime)
			potentialReward = rewards.Calculate(
				stakeDuration,
				stakerTx.Weight(),
				currentSupply,
			)

			e.State.SetCurrentSupply(subnetID, currentSupply+potentialReward)
		}

		staker, err = state.NewCurrentStaker(txID, stakerTx, chainTime, potentialReward)
	}
	if err != nil {
		return err
	}

	switch priority := staker.Priority; {
	case priority.IsCurrentValidator():
		e.State.PutCurrentValidator(staker)
	case priority.IsCurrentDelegator():
		e.State.PutCurrentDelegator(staker)
	case priority.IsPendingValidator():
		e.State.PutPendingValidator(staker)
	case priority.IsPendingDelegator():
		e.State.PutPendingDelegator(staker)
	default:
		return fmt.Errorf("staker %s, unexpected priority %d", staker.TxID, priority)
	}
	return nil
}