package keyper

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto/ecies"

	"github.com/brainbot-com/shutter/shuttermint/contract"
	"github.com/brainbot-com/shutter/shuttermint/keyper/epochkg"
	"github.com/brainbot-com/shutter/shuttermint/keyper/observe"
	"github.com/brainbot-com/shutter/shuttermint/keyper/puredkg"
	"github.com/brainbot-com/shutter/shuttermint/medley"
	"github.com/brainbot-com/shutter/shuttermint/shmsg"
)

type decryptfn func(encrypted []byte) ([]byte, error)

// IRunEnv is passed as a parameter to IAction's Run function.
type IRunEnv interface {
	MessageSender
	GetContractCaller(ctx context.Context) *ContractCaller
	WatchTransaction(tx *types.Transaction)
}

// IAction describes an action to run as determined by the Decider's Decide method.
type IAction interface {
	Run(ctx context.Context, runenv IRunEnv) error
}

var _ IAction = SendShuttermintMessage{}

// DKG is used to store local state about active DKG processes. Each DKG has a corresponding
// observe.Eon struct stored in observe.Shutter, which we can find with Shutter's FindEon method.
type DKG struct {
	Eon                  uint64
	Keypers              []common.Address
	Pure                 *puredkg.PureDKG
	CommitmentsIndex     int
	PolyEvalsIndex       int
	AccusationsIndex     int
	ApologiesIndex       int
	OutgoingPolyEvalMsgs []puredkg.PolyEvalMsg
}

// EKG is used to store local state about the epoch key generation process.
type EKG struct {
	Eon                       uint64
	Keypers                   []common.Address
	EpochKG                   *epochkg.EpochKG
	EpochSecretKeySharesIndex int
}

func (dkg *DKG) ShortInfo() string {
	return fmt.Sprintf("eon=%d, #keypers=%d, %s", dkg.Eon, len(dkg.Keypers), dkg.Pure.ShortInfo())
}

// newApology create a new shmsg apology message from the given puredkg apologies
func (dkg *DKG) newApology(apologies []puredkg.ApologyMsg) *shmsg.Message {
	var accusers []common.Address
	var polyEvals []*big.Int

	for _, a := range apologies {
		accusers = append(accusers, dkg.Keypers[a.Accuser])
		polyEvals = append(polyEvals, a.Eval)
	}
	return shmsg.NewApology(dkg.Eon, accusers, polyEvals)
}

// newAccusation creates a new shmsg accusation message from the given puredkg accusations
func (dkg *DKG) newAccusation(accusations []puredkg.AccusationMsg) *shmsg.Message {
	var accused []common.Address
	for _, a := range accusations {
		accused = append(accused, dkg.Keypers[a.Accused])
	}
	return shmsg.NewAccusation(dkg.Eon, accused)
}

func (dkg *DKG) syncCommitments(eon observe.Eon) {
	for i := dkg.CommitmentsIndex; i < len(eon.Commitments); i++ {
		comm := eon.Commitments[i]
		phase := phaseLength.getPhaseAtHeight(comm.Height, eon.StartHeight)
		if phase != puredkg.Dealing {
			log.Printf("Warning: received commitment in wrong phase %s: %+v", phase, comm)
			continue
		}

		sender, err := medley.FindAddressIndex(dkg.Keypers, comm.Sender)
		if err != nil {
			continue
		}

		err = dkg.Pure.HandlePolyCommitmentMsg(
			puredkg.PolyCommitmentMsg{Eon: comm.Eon, Gammas: comm.Gammas, Sender: uint64(sender)},
		)
		if err != nil {
			log.Printf("Error in syncCommitments: %s", err)
		}
	}
	dkg.CommitmentsIndex = len(eon.Commitments)
}

func (dkg *DKG) syncPolyEvals(eon observe.Eon, decrypt decryptfn) {
	keyperIndex := dkg.Pure.Keyper
	for i := dkg.PolyEvalsIndex; i < len(eon.PolyEvals); i++ {
		eval := eon.PolyEvals[i]
		phase := phaseLength.getPhaseAtHeight(eval.Height, eon.StartHeight)
		if phase != puredkg.Dealing {
			log.Printf("Warning: received polyeval in wrong phase %s: %+v", phase, eval)
			continue
		}

		sender, err := medley.FindAddressIndex(dkg.Keypers, eval.Sender)
		if err != nil {
			continue
		}
		if uint64(sender) == keyperIndex {
			continue
		}

		for j, receiver := range eval.Receivers {
			receiverIndex, err := medley.FindAddressIndex(dkg.Keypers, receiver)
			if err != nil {
				log.Printf("Error in syncPolyEvals: %s", err)
				continue
			}
			if uint64(receiverIndex) != keyperIndex {
				continue
			}
			encrypted := eval.EncryptedEvals[j]
			evalBytes, err := decrypt(encrypted)
			if err != nil {
				log.Printf("Error in syncPolyEvals: %s", err)
				continue
			}
			b := new(big.Int)
			b.SetBytes(evalBytes)
			err = dkg.Pure.HandlePolyEvalMsg(
				puredkg.PolyEvalMsg{
					Eon:      eval.Eon,
					Sender:   uint64(sender),
					Receiver: keyperIndex,
					Eval:     b,
				})
			if err != nil {
				log.Printf("Error in syncPolyEvals: %s", err)
			}
		}
	}
	dkg.PolyEvalsIndex = len(eon.PolyEvals)
}

func (dkg *DKG) syncAccusations(eon observe.Eon) {
	for i := dkg.AccusationsIndex; i < len(eon.Accusations); i++ {
		accusation := eon.Accusations[i]
		phase := phaseLength.getPhaseAtHeight(accusation.Height, eon.StartHeight)
		if phase != puredkg.Accusing {
			log.Printf("Warning: received accusation in wrong phase %s: %+v", phase, accusation)
			continue
		}

		sender, err := medley.FindAddressIndex(dkg.Keypers, accusation.Sender)
		if err != nil {
			log.Printf("Error: cannot handle accusation. bad sender: %s", accusation.Sender.Hex())
			continue
		}
		for _, accused := range accusation.Accused {
			accusedIndex, err := medley.FindAddressIndex(dkg.Keypers, accused)
			if err != nil {
				log.Printf("Error in syncAccusations: %s", err)
				continue
			}
			err = dkg.Pure.HandleAccusationMsg(
				puredkg.AccusationMsg{
					Eon:     dkg.Eon,
					Accuser: uint64(sender),
					Accused: uint64(accusedIndex),
				})
			if err != nil {
				log.Printf("Error: cannot handle accusation: %s", err)
			}
		}
	}
	dkg.AccusationsIndex = len(eon.Accusations)
}

func (dkg *DKG) syncApologies(eon observe.Eon) {
	for i := dkg.ApologiesIndex; i < len(eon.Apologies); i++ {
		apology := eon.Apologies[i]
		phase := phaseLength.getPhaseAtHeight(apology.Height, eon.StartHeight)
		if phase != puredkg.Apologizing {
			log.Printf("Warning: received apology in wrong phase %s: %+v", phase, apology)
			continue
		}

		sender, err := medley.FindAddressIndex(dkg.Keypers, apology.Sender)
		if err != nil {
			log.Printf("Error: cannot handle apology. bad sender: %s", apology.Sender.Hex())
			continue
		}
		for j, accuser := range apology.Accusers {
			accuserIndex, err := medley.FindAddressIndex(dkg.Keypers, accuser)
			if err != nil {
				log.Printf("Error in syncApologies: %s", err)
				continue
			}
			err = dkg.Pure.HandleApologyMsg(
				puredkg.ApologyMsg{
					Eon:     dkg.Eon,
					Accuser: uint64(accuserIndex),
					Accused: uint64(sender),
					Eval:    apology.PolyEval[j],
				})
			if err != nil {
				log.Printf("Error: cannot handle apology: %s", err)
			}
		}
	}
	dkg.ApologiesIndex = len(eon.Apologies)
}

// State is the keyper's internal state
type State struct {
	CheckinMessageSent       bool
	LastSentBatchConfigIndex uint64
	LastEonStarted           uint64
	DKGs                     []DKG
	EKGs                     []*EKG
	PendingHalfStep          *uint64
	PendingAppeals           map[uint64]struct{}
	LastEpochSecretShareSent uint64
}

// Decider decides on the next actions to take based on our internal State and the current Shutter
// and MainChain state for a single step. For each step the keyper creates a new Decider. The
// actions to run are stored inside the Actions field.
type Decider struct {
	Config    KeyperConfig
	State     *State
	Shutter   *observe.Shutter
	MainChain *observe.MainChain
	Actions   []IAction
}

// SendShuttermintMessage is an Action that sends a message to shuttermint
type SendShuttermintMessage struct {
	description string
	msg         *shmsg.Message
}

func (a SendShuttermintMessage) Run(ctx context.Context, runenv IRunEnv) error {
	log.Printf("Run: %s", a)
	return runenv.SendMessage(ctx, a.msg)
}

func (a SendShuttermintMessage) String() string {
	return fmt.Sprintf("-> shuttermint: %s", a.description)
}

// ExecuteCipherBatch is an Action that instructs the executor contract to execute a cipher batch.
type ExecuteCipherBatch struct {
	halfStep        uint64
	cipherBatchHash [32]byte
	transactions    [][]byte
	keyperIndex     uint64
}

func (a ExecuteCipherBatch) Run(ctx context.Context, runenv IRunEnv) error {
	log.Printf("Run: %s", a)

	cc := runenv.GetContractCaller(ctx)
	auth, err := cc.Auth()
	if err != nil {
		return err
	}

	tx, err := cc.ExecutorContract.ExecuteCipherBatch(auth, a.cipherBatchHash, a.transactions, a.keyperIndex)
	if err != nil {
		// XXX consider handling the error somehow
		log.Printf("Error creating cipher batch execution tx: %s", err)
		return nil
	}
	runenv.WatchTransaction(tx)

	return nil
}

func (a ExecuteCipherBatch) String() string {
	return fmt.Sprintf("-> executor contract: execute cipher half step %d", a.halfStep)
}

// ExecutePlainBatch is an Action that instructs the executor contract to execute a plain batch.
type ExecutePlainBatch struct {
	halfStep     uint64
	transactions [][]byte
}

func (a ExecutePlainBatch) Run(ctx context.Context, runenv IRunEnv) error {
	log.Printf("Run: %s", a)

	cc := runenv.GetContractCaller(ctx)
	auth, err := cc.Auth()
	if err != nil {
		return err
	}

	tx, err := cc.ExecutorContract.ExecutePlainBatch(auth, a.transactions)
	if err != nil {
		// XXX consider handling the error somehow
		log.Printf("Error creating plain batch execution tx: %s", err)
		return nil
	}
	runenv.WatchTransaction(tx)

	return nil
}

func (a ExecutePlainBatch) String() string {
	return fmt.Sprintf("-> executor contract: execute plain half step %d", a.halfStep)
}

// SkipCipherBatch is an Action that instructs the executor contract to skip a cipher batch
type SkipCipherBatch struct {
	halfStep uint64
}

func (a SkipCipherBatch) Run(ctx context.Context, runenv IRunEnv) error {
	log.Printf("Run: %s", a)

	cc := runenv.GetContractCaller(ctx)
	auth, err := cc.Auth()
	if err != nil {
		return err
	}

	tx, err := cc.ExecutorContract.SkipCipherExecution(auth)
	if err != nil {
		// XXX consider handling the error somehow
		log.Printf("Error creating skip cipher execution tx: %s", err)
		return nil
	}
	runenv.WatchTransaction(tx)

	return nil
}

func (a SkipCipherBatch) String() string {
	return fmt.Sprintf("-> executor contract: skip plain half step %d", a.halfStep)
}

// Accuse is an action accusing the executor of a given half step at the keyper slasher.
type Accuse struct {
	halfStep    uint64
	keyperIndex uint64 // index of the accuser, not the executor
}

func (a Accuse) Run(ctx context.Context, runenv IRunEnv) error {
	log.Printf("Run: %s", a)

	cc := runenv.GetContractCaller(ctx)
	auth, err := cc.Auth()
	if err != nil {
		return err
	}

	tx, err := cc.KeyperSlasher.Accuse(auth, a.halfStep, a.keyperIndex)
	if err != nil {
		return err
	}
	runenv.WatchTransaction(tx)

	return nil
}

func (a Accuse) String() string {
	return fmt.Sprintf("-> keyper slasher: accuse for half step %d", a.halfStep)
}

// Appeal is an action countering an earlier invalid accusation.
type Appeal struct {
	authorization contract.Authorization
}

func (a Appeal) Run(ctx context.Context, runenv IRunEnv) error {
	log.Printf("Run: %s", a)

	cc := runenv.GetContractCaller(ctx)
	auth, err := cc.Auth()
	if err != nil {
		return err
	}

	tx, err := cc.KeyperSlasher.Appeal(auth, a.authorization)
	if err != nil {
		return err
	}
	runenv.WatchTransaction(tx)

	return nil
}

func (a Appeal) String() string {
	return fmt.Sprintf("-> keyper slasher: appeal for half step %d", a.authorization.HalfStep)
}

var errEKGNotFound = errors.New("EKG not found")

func (st *State) FindEKGByEon(eon uint64) (*EKG, error) {
	for _, epochkg := range st.EKGs {
		if epochkg.Eon == eon {
			return epochkg, nil
		}
	}
	return nil, errEKGNotFound
}

// addAction stores the given IAction to be run later
func (dcdr *Decider) addAction(a IAction) {
	dcdr.Actions = append(dcdr.Actions, a)
}

func (dcdr *Decider) sendShuttermintMessage(description string, msg *shmsg.Message) {
	dcdr.addAction(SendShuttermintMessage{
		description: description,
		msg:         msg,
	})
}

// shouldSendCheckin returns true if we should send the CheckIn message
func (dcdr *Decider) shouldSendCheckin() bool {
	if dcdr.State.CheckinMessageSent {
		return false
	}
	if dcdr.Shutter.IsCheckedIn(dcdr.Config.Address()) {
		return false
	}
	return dcdr.Shutter.IsKeyper(dcdr.Config.Address())
}

func (dcdr *Decider) sendCheckIn() {
	validatorPublicKey := dcdr.Config.ValidatorKey.Public().(ed25519.PublicKey)
	msg := shmsg.NewCheckIn([]byte(validatorPublicKey), &dcdr.Config.EncryptionKey.PublicKey)
	dcdr.sendShuttermintMessage("checkin", msg)
}

func (dcdr *Decider) maybeSendCheckIn() {
	if dcdr.shouldSendCheckin() {
		dcdr.sendCheckIn()
		dcdr.State.CheckinMessageSent = true
	}
}

func (dcdr *Decider) sendBatchConfig(configIndex uint64, config contract.BatchConfig) {
	msg := shmsg.NewBatchConfig(
		config.StartBatchIndex,
		config.Keypers,
		config.Threshold,
		dcdr.Config.ConfigContractAddress,
		configIndex,
		false,
		false,
	)
	dcdr.sendShuttermintMessage(fmt.Sprintf("batch config, index=%d", configIndex), msg)
}

func (dcdr *Decider) maybeSendBatchConfig() {
	if len(dcdr.Shutter.BatchConfigs) == 0 {
		log.Printf("shutter is not bootstrapped")
		return
	}
	configIndex := 1 + dcdr.Shutter.BatchConfigs[len(dcdr.Shutter.BatchConfigs)-1].ConfigIndex

	if configIndex <= dcdr.State.LastSentBatchConfigIndex {
		return // already sent this one out
	}

	if configIndex < uint64(len(dcdr.MainChain.BatchConfigs)) {
		dcdr.sendBatchConfig(configIndex, dcdr.MainChain.BatchConfigs[configIndex])
		dcdr.State.LastSentBatchConfigIndex = configIndex
	}
}

func (dcdr *Decider) sendEonStartVoting(startBatchIndex uint64) {
	msg := shmsg.NewEonStartVote(startBatchIndex)
	dcdr.sendShuttermintMessage(fmt.Sprintf("eon start voting, startBatchIndex=%d", startBatchIndex), msg)
}

func (dcdr *Decider) startDKG(eon observe.Eon) {
	batchConfig := dcdr.Shutter.FindBatchConfigByBatchIndex(eon.StartEvent.BatchIndex)
	keyperIndex, err := medley.FindAddressIndex(batchConfig.Keypers, dcdr.Config.Address())
	if err != nil {
		return
	}

	pure := puredkg.NewPureDKG(eon.Eon, uint64(len(batchConfig.Keypers)), batchConfig.Threshold, uint64(keyperIndex))
	dkg := DKG{Eon: eon.Eon, Pure: &pure, Keypers: batchConfig.Keypers}
	dcdr.State.DKGs = append(dcdr.State.DKGs, dkg)
}

func (dcdr *Decider) maybeStartDKG() {
	for _, eon := range dcdr.Shutter.Eons {
		if eon.Eon > dcdr.State.LastEonStarted {
			// TODO we should check that we do not start eons that are in the past
			dcdr.startDKG(eon)
			dcdr.State.LastEonStarted = eon.Eon
		}
	}
}

// PhaseLength is used to store the accumulated lengths of the DKG phases
type PhaseLength struct {
	Off         int64
	Dealing     int64
	Accusing    int64
	Apologizing int64
}

func (plen *PhaseLength) getPhaseAtHeight(height int64, eonStartHeight int64) puredkg.Phase {
	if height < eonStartHeight+plen.Off {
		return puredkg.Off
	}
	if height < eonStartHeight+plen.Dealing {
		return puredkg.Dealing
	}
	if height < eonStartHeight+plen.Accusing {
		return puredkg.Accusing
	}
	if height < eonStartHeight+plen.Apologizing {
		return puredkg.Apologizing
	}
	return puredkg.Finalized
}

var phaseLength = PhaseLength{
	Off:         0,
	Dealing:     30,
	Accusing:    60,
	Apologizing: 90,
}

// sendPolyEvals sends the outgoing PolyEvalMsg stored in dkg that can be sent. A PolyEvalMessage
// can only be sent, when we do have the receiver's public encryption key. If we're beyond the
// 'Dealing' phase, it's too late to send these messages. In that case we clear the
// OutgoingPolyEvalMsgs field and log a message.
func (dcdr *Decider) sendPolyEvals(dkg *DKG) {
	if len(dkg.OutgoingPolyEvalMsgs) == 0 {
		return
	}

	if dkg.Pure.Phase > puredkg.Dealing {
		log.Printf(
			"Warning: could not send %d poly eval messages for eon %d, because the dealing phase is already over",
			len(dkg.OutgoingPolyEvalMsgs),
			dkg.Eon)
		dkg.OutgoingPolyEvalMsgs = nil
		return
	}

	var newOutgoing []puredkg.PolyEvalMsg
	var receivers []common.Address
	var encryptedEvals [][]byte

	for _, p := range dkg.OutgoingPolyEvalMsgs {
		receiver := dkg.Keypers[p.Receiver]
		encryptionKey, ok := dcdr.Shutter.KeyperEncryptionKeys[receiver]
		if ok {
			encrypted, err := ecies.Encrypt(rand.Reader, encryptionKey, p.Eval.Bytes(), nil, nil)
			if err != nil {
				panic(err)
			}
			encryptedEvals = append(encryptedEvals, encrypted)
			receivers = append(receivers, receiver)
		} else {
			newOutgoing = append(newOutgoing, p)
		}
	}
	if len(receivers) > 0 {
		dcdr.sendShuttermintMessage(
			fmt.Sprintf("poly eval, eon=%d, %d receivers, %d still outgoing", dkg.Eon, len(receivers), len(newOutgoing)),
			shmsg.NewPolyEval(dkg.Eon, receivers, encryptedEvals))
		dkg.OutgoingPolyEvalMsgs = newOutgoing
		if len(dkg.OutgoingPolyEvalMsgs) == 0 {
			log.Printf("Sent all poly eval messages for eon %d", dkg.Eon)
		}
	}
}

func (dcdr *Decider) startPhase1Dealing(dkg *DKG) {
	commitment, polyEvals, err := dkg.Pure.StartPhase1Dealing()
	if err != nil {
		panic(err) // XXX fix error handling
	}

	dkg.OutgoingPolyEvalMsgs = polyEvals

	dcdr.sendShuttermintMessage(
		fmt.Sprintf("poly commitment, eon=%d", dkg.Eon),
		shmsg.NewPolyCommitment(dkg.Eon, commitment.Gammas))
}

func (dcdr *Decider) startPhase2Accusing(dkg *DKG) {
	accusations := dkg.Pure.StartPhase2Accusing()
	dcdr.sendShuttermintMessage(
		fmt.Sprintf("accusations, eon=%d, count=%d", dkg.Eon, len(accusations)),
		dkg.newAccusation(accusations))
}

func (dcdr *Decider) startPhase3Apologizing(dkg *DKG) {
	apologies := dkg.Pure.StartPhase3Apologizing()
	dcdr.sendShuttermintMessage(
		fmt.Sprintf("apologies, eon=%d, count=%d", dkg.Eon, len(apologies)),
		dkg.newApology(apologies))
}

func (dcdr *Decider) dkgFinalize(dkg *DKG) {
	dkg.Pure.Finalize()
	dkgresult, err := dkg.Pure.ComputeResult()
	if err != nil {
		log.Printf("Error: DKG process failed for %s: %s", dkg.ShortInfo(), err)
		return
	}
	log.Printf("Success: DKG process succeeced for %s", dkg.ShortInfo())
	ekg := &EKG{
		Eon:     dkg.Eon,
		Keypers: dkg.Keypers,
		EpochKG: epochkg.NewEpochKG(&dkgresult),
	}
	dcdr.State.EKGs = append(dcdr.State.EKGs, ekg)
}

func (dcdr *Decider) syncDKGWithEon(dkg *DKG, eon observe.Eon) {
	decrypt := func(encrypted []byte) ([]byte, error) {
		return dcdr.Config.EncryptionKey.Decrypt(encrypted, []byte(""), []byte(""))
	}

	phaseAtCurrentHeight := phaseLength.getPhaseAtHeight(dcdr.Shutter.CurrentBlock, eon.StartHeight)

	if dkg.Pure.Phase == puredkg.Off && phaseAtCurrentHeight >= puredkg.Dealing {
		dcdr.startPhase1Dealing(dkg)
	}
	dkg.syncCommitments(eon)
	dkg.syncPolyEvals(eon, decrypt)

	if dkg.Pure.Phase == puredkg.Dealing && phaseAtCurrentHeight >= puredkg.Accusing {
		dcdr.startPhase2Accusing(dkg)
	}
	dkg.syncAccusations(eon)

	if dkg.Pure.Phase == puredkg.Accusing && phaseAtCurrentHeight >= puredkg.Apologizing {
		dcdr.startPhase3Apologizing(dkg)
	}
	dkg.syncApologies(eon)

	if dkg.Pure.Phase == puredkg.Apologizing && phaseAtCurrentHeight >= puredkg.Finalized {
		dcdr.dkgFinalize(dkg)
	}
}

func (dcdr *Decider) handleDKGs() {
	for i := range dcdr.State.DKGs {
		dkg := &dcdr.State.DKGs[i]
		eon, err := dcdr.Shutter.FindEon(dkg.Eon)
		if err != nil {
			panic(err)
		}
		dcdr.syncDKGWithEon(dkg, *eon)
		dcdr.sendPolyEvals(dkg)
	}
}

func (dcdr *Decider) publishEpochSecretKeyShare(batchIndex uint64) {
	epoch := batchIndex + 1
	eon, err := dcdr.Shutter.FindEonByBatchIndex(batchIndex)
	if err != nil {
		return
	}

	ekg, err := dcdr.State.FindEKGByEon(eon.Eon)
	if err != nil {
		log.Printf("Cannot find EpochKG for eon=%d", eon.Eon)
		return
	}
	dcdr.sendEpochSecretKeyShare(ekg.EpochKG, epoch)
}

func (dcdr *Decider) syncEKGWithEon(ekg *EKG, eon *observe.Eon) {
	for i := ekg.EpochSecretKeySharesIndex; i < len(eon.EpochSecretKeyShares); i++ {
		share := eon.EpochSecretKeyShares[i]
		sender, err := medley.FindAddressIndex(ekg.Keypers, share.Sender)
		if err != nil {
			continue
		}
		if _, ok := ekg.EpochKG.SecretKeys[share.Epoch]; ok {
			continue
		}
		err = ekg.EpochKG.HandleEpochSecretKeyShare(
			&epochkg.EpochSecretKeyShare{
				Eon:    share.Eon,
				Epoch:  share.Epoch,
				Sender: uint64(sender),
				Share:  share.Share,
			},
		)
		if err != nil {
			log.Printf("Error while handling epoch secret key share: %s", err)
			continue
		}
		if key, ok := ekg.EpochKG.SecretKeys[share.Epoch]; ok {
			log.Printf("Epoch secret key generated for epoch %d", share.Epoch)
			_ = key
		}
	}
	ekg.EpochSecretKeySharesIndex = len(eon.EpochSecretKeyShares)
}

func (dcdr *Decider) syncEKGs() {
	for _, ekg := range dcdr.State.EKGs {
		eon, err := dcdr.Shutter.FindEon(ekg.Eon)
		if err != nil {
			panic(err)
		}
		dcdr.syncEKGWithEon(ekg, eon)
	}
}

func (dcdr *Decider) publishEpochSecretKeyShares() {
	blockNum := dcdr.MainChain.CurrentBlock

	// Find the active config for the given block on the main chain
	activeCFGIdx := dcdr.MainChain.ActiveConfigIndex(blockNum)
	bc := dcdr.MainChain.BatchConfigs[activeCFGIdx]

	// find the corresponding config on shutter
	_, err := dcdr.Shutter.FindBatchConfigByConfigIndex(uint64(activeCFGIdx))
	if err != nil {
		return
	}

	currentBatchIndex := bc.BatchIndex(blockNum)
	// publish the private epoch key share for batch indexes < currentBatchIndex
	// TODO Limit the number of messages we sent. It doesn't make sense to publish the secret
	// key share, if the batch already finished/failed.
	for batchIndex := dcdr.State.LastEpochSecretShareSent; batchIndex < currentBatchIndex; batchIndex++ {
		dcdr.publishEpochSecretKeyShare(batchIndex)
	}
}

func (dcdr *Decider) handleEpochKG() {
	dcdr.publishEpochSecretKeyShares()
	dcdr.syncEKGs()
}

func (dcdr *Decider) sendEpochSecretKeyShare(epochKG *epochkg.EpochKG, epoch uint64) {
	if epoch == 0 {
		panic("epoch must be positive")
	}
	epochSecretKeyShare := epochKG.ComputeEpochSecretKeyShare(epoch)
	dcdr.sendShuttermintMessage(
		fmt.Sprintf("epoch secret key share, epoch=%d in eon=%d", epoch, epochKG.Eon),
		shmsg.NewEpochSecretKeyShare(epochKG.Eon, epoch, epochSecretKeyShare),
	)
	dcdr.State.LastEpochSecretShareSent = epoch
}

func (dcdr *Decider) maybeExecuteBatch() {
	config := dcdr.MainChain.CurrentConfig()
	if !config.IsActive() {
		return // nothing to execute if config is inactive
	}

	batchIndex := config.BatchIndex(dcdr.MainChain.CurrentBlock)

	nextHalfStep := dcdr.MainChain.NumExecutionHalfSteps
	if dcdr.State.PendingHalfStep != nil && nextHalfStep > *dcdr.State.PendingHalfStep {
		// Reset the pending half step if the current one is greater.
		// XXX There's a chance that another keyper has executed the previous half step and our tx
		// is still pending. In that case we should probably wait until it fails before sending
		// another one.
		dcdr.State.PendingHalfStep = nil
	}
	if nextHalfStep >= batchIndex*2 {
		return // everything has been executed already
	}

	dcdr.maybeExecuteHalfStep(nextHalfStep)
}

func (dcdr *Decider) maybeExecuteHalfStep(nextHalfStep uint64) {
	if dcdr.State.PendingHalfStep != nil {
		// Don't try to execute anything if there's already a pending transaction executing the
		// current or another half step. Rather, wait for that tx to confirm first.
		return
	}

	batchIndex := nextHalfStep / 2
	batch, ok := dcdr.MainChain.Batches[batchIndex]
	if !ok {
		batch = &observe.Batch{BatchIndex: batchIndex}
	}

	config, ok := dcdr.MainChain.ConfigForBatchIndex(batchIndex)
	if !ok {
		return // nothing to do if config is inactive
	}
	keyperIndex, ok := config.KeyperIndex(dcdr.Config.Address())
	if !ok {
		return // only keypers can execute
	}

	delay, err := dcdr.executionDelay(nextHalfStep)
	if err != nil {
		log.Printf("unexpected error: %w", err)
		return // shouldn't happen
	}
	batchParams, err := contract.MakeBatchParams(&config, batchIndex)
	if err != nil {
		log.Printf("unexpected error: %w", err)
		return // shouldn't happen
	}
	executionBlock := batchParams.EndBlock + delay
	if dcdr.MainChain.CurrentBlock < executionBlock {
		return // wait for other keypers first
	}

	var action IAction
	if nextHalfStep%2 == 0 {
		// XXX: use transactions from voting here and make sure there are enough votes
		decryptedTransactions := [][]byte{}
		action = ExecuteCipherBatch{
			halfStep:        nextHalfStep,
			cipherBatchHash: batch.EncryptedBatchHash,
			transactions:    decryptedTransactions,
			keyperIndex:     keyperIndex,
		}
	} else {
		action = ExecutePlainBatch{
			halfStep:     nextHalfStep,
			transactions: batch.PlainTransactions,
		}
	}
	dcdr.State.PendingHalfStep = &nextHalfStep
	dcdr.addAction(action)
}

// maybeAppeal checks if there are any accusations against us and if so sends an appeal if possible.
func (dcdr *Decider) maybeAppeal() {
	dcdr.syncPendingAppeals()

	accusations := dcdr.MainChain.AccusationsAgainst(dcdr.Config.Address())
	for _, accusation := range accusations {
		if accusation.Appealed {
			continue
		}
		if _, ok := dcdr.State.PendingAppeals[accusation.HalfStep]; ok {
			continue // don't send appeal if we've already done so and the tx is still pending
		}

		// XXX: we have to create a contract.Authorization here

		// action := Appeal{
		// 	authorization: authorization,
		// }
		// dcdr.State.PendingAppeals[accusation.HalfStep] = struct{}{}
		// dcdr.addAction(action)
	}
}

// syncPendingAppeals removes any pending appeals that have been successfully handled by the main
// chain.
// XXX: It's possible that someone else appeals, in which case our tx would still be pending.
// Also, we don't notice if our tx fails (but this shouldn't happen if we prepare it properly).
func (dcdr *Decider) syncPendingAppeals() {
	for halfStep := range dcdr.State.PendingAppeals {
		for _, accusation := range dcdr.MainChain.Accusations {
			if halfStep == accusation.HalfStep && accusation.Appealed {
				delete(dcdr.State.PendingAppeals, halfStep)
			}
		}
	}
}

// ExecutionDelay returns the number of main chain blocks to wait before sending an execution tx.
// This makes sure not all keypers try to send the same tx at the same time.
func (dcdr *Decider) executionDelay(halfStep uint64) (uint64, error) {
	batchIndex := halfStep / 2
	config, ok := dcdr.MainChain.ConfigForBatchIndex(batchIndex)
	if !ok {
		return 0, fmt.Errorf("config is not active")
	}

	keyperIndex, ok := config.KeyperIndex(dcdr.Config.Address())
	if !ok {
		return 0, fmt.Errorf("not a keyper")
	}

	place := (halfStep + keyperIndex) % uint64(len(config.Keypers))
	return place * dcdr.Config.ExecutionStaggering, nil
}

// Decide determines the next actions to run.
func (dcdr *Decider) Decide() {
	// We can't go on unless we're registered as keyper in shuttermint
	if !dcdr.Shutter.IsKeyper(dcdr.Config.Address()) {
		log.Printf("not registered as keyper in shuttermint, nothing to do")
		return
	}
	dcdr.maybeSendCheckIn()
	dcdr.maybeSendBatchConfig()
	dcdr.maybeStartDKG()
	dcdr.handleDKGs()
	dcdr.handleEpochKG()
	dcdr.maybeExecuteBatch()
	dcdr.maybeAppeal()
}
