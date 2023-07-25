package service

import (
	"encoding/hex"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/babylonchain/btc-validator/proto"
)

// jurySigSubmissionLoop is the reactor to submit Jury signature for pending BTC delegations
func (app *ValidatorApp) jurySigSubmissionLoop() {
	defer app.wg.Done()

	interval := app.config.JuryModeConfig.QueryInterval
	jurySigTicker := time.NewTicker(interval)

	for {
		select {
		case <-jurySigTicker.C:
			dels, err := app.getPendingDelegationsForAll()
			if err != nil {
				app.logger.WithFields(logrus.Fields{
					"err": err,
				}).Error("failed to get pending delegations")
				continue
			}

			for _, d := range dels {
				_, err := app.AddJurySignature(d)
				if err != nil {
					app.logger.WithFields(logrus.Fields{
						"err":        err,
						"del_btc_pk": d.BtcPk,
					}).Error("failed to submit Jury sig to the Bitcoin delegation")
				}
			}

		case <-app.quit:
			return
		}
	}

}

// validatorSubmissionLoop is the reactor to submit finality signature and public randomness
func (app *ValidatorApp) validatorSubmissionLoop() {
	defer app.wg.Done()

	commitRandTicker := time.NewTicker(app.config.RandomnessCommitInterval)

	for {
		select {
		case b := <-app.poller.GetBlockInfoChan():
			_, err := app.SubmitFinalitySignaturesForAll(b)
			if err != nil {
				app.logger.WithFields(logrus.Fields{
					"err":        err,
					"bbn_height": b.Height,
				}).Error("failed to submit finality signature to Babylon")
			}
		case <-commitRandTicker.C:
			lastBlock, err := app.GetCurrentBbnBlock()
			if err != nil {
				app.logger.WithFields(logrus.Fields{
					"err": err,
				}).Fatal("failed to get the current Babylon block")
			}
			_, err = app.CommitPubRandForAll(lastBlock)
			if err != nil {
				app.logger.WithFields(logrus.Fields{
					"block_height": lastBlock.Height,
					"err":          err,
				}).Error("failed to commit public randomness")
				continue
			}
		case <-app.quit:
			return
		}
	}
}

// main event loop for the validator app
func (app *ValidatorApp) eventLoop() {
	defer app.wg.Done()

	for {
		select {
		case req := <-app.createValidatorRequestChan:
			resp, err := app.handleCreateValidatorRequest(req)

			if err != nil {
				req.errResponse <- err
				continue
			}

			req.successResponse <- resp

		case ev := <-app.finalitySigAddedEventChan:
			val, err := app.vs.GetValidator(ev.bbnPubKey.Key)

			if err != nil {
				// we always check if the validator is in the DB before sending the registration request
				app.logger.WithFields(logrus.Fields{
					"bbn_pk": ev.bbnPubKey,
				}).Fatal("finality signature added validator not found in DB")
			}

			// update the last_voted_height
			err = app.vs.SetValidatorLastVotedHeight(val, ev.height)
			if err != nil {
				app.logger.WithFields(logrus.Fields{
					"bbn_pk":            ev.bbnPubKey,
					"block_height":      ev.height,
					"last_voted_height": val.LastVotedHeight,
				}).Fatal("err while updating the validator last voted height to DB")
			}

			// return to the caller
			ev.successResponse <- &addFinalitySigResponse{
				txHash: ev.txHash,
			}

		case ev := <-app.validatorRegisteredEventChan:
			val, err := app.vs.GetValidator(ev.bbnPubKey.Key)

			if err != nil {
				// we always check if the validator is in the DB before sending the registration request
				app.logger.WithFields(logrus.Fields{
					"bbn_pk": ev.bbnPubKey,
				}).Fatal("Registred validator not found in DB")
			}

			// change the status of the validator to registered
			err = app.vs.SetValidatorStatus(val, proto.ValidatorStatus_REGISTERED)

			if err != nil {
				app.logger.WithFields(logrus.Fields{
					"bbn_pk": ev.bbnPubKey,
				}).Fatal("err while saving validator to DB")
			}

			// return to the caller
			ev.successResponse <- &registerValidatorResponse{
				txHash: ev.txHash,
			}

		case ev := <-app.pubRandCommittedEventChan:
			val, err := app.vs.GetValidator(ev.bbnPubKey.Key)
			if err != nil {
				// we always check if the validator is in the DB before sending the registration request
				app.logger.WithFields(logrus.Fields{
					"bbn_pk": ev.bbnPubKey,
				}).Fatal("Public randomness committed validator not found in DB")
			}

			val.LastCommittedHeight = ev.startingHeight + uint64(len(ev.pubRandList)-1)

			// save the updated validator object to DB
			err = app.vs.SaveValidator(val)

			if err != nil {
				app.logger.WithFields(logrus.Fields{
					"bbn_pk": ev.bbnPubKey,
				}).Fatal("err while saving validator to DB")
			}

			// save the committed random list to DB
			// TODO 1: Optimize the db interface to batch the saving operations
			// TODO 2: Consider safety after recovery
			for i, pr := range ev.privRandList {
				height := ev.startingHeight + uint64(i)
				privRand := pr.Bytes()
				randPair := &proto.SchnorrRandPair{
					SecRand: privRand[:],
					PubRand: ev.pubRandList[i].MustMarshal(),
				}
				err = app.vs.SaveRandPair(ev.bbnPubKey.Key, height, randPair)
				if err != nil {
					app.logger.WithFields(logrus.Fields{
						"bbn_pk": ev.bbnPubKey,
					}).Fatal("err while saving committed random pair to DB")
				}
			}

			// return to the caller
			ev.successResponse <- &commitPubRandResponse{
				txHash: ev.txHash,
			}
		case ev := <-app.jurySigAddedEventChan:
			// TODO do we assume the delegator is also a BTC validator?
			// if so, do we want to change its status to ACTIVE here?
			// if not, maybe we can remove the handler of this event

			// return to the caller
			ev.successResponse <- &addJurySigResponse{
				txHash: ev.txHash,
			}

		case <-app.quit:
			return
		}
	}
}

// Loop for handling requests to send stuff to babylon. It is necessart to properly
// serialize bayblon sends as otherwise we would keep hitting sequence mismatch errors.
// This could be done either by send loop or by lock. We choose send loop as it is
// more flexible.
// TODO: This could be probably separate component responsible for queuing stuff
// and sending it to babylon.
func (app *ValidatorApp) handleSentToBabylonLoop() {
	defer app.wg.Done()
	for {
		select {
		case req := <-app.addFinalitySigRequestChan:
			// TODO: need to start passing context here to be able to cancel the request in case of app quiting
			txHash, err := app.bc.SubmitFinalitySig(req.valBtcPk, req.blockHeight, req.blockLastCommitHash, req.sig)

			if err != nil {
				app.logger.WithFields(logrus.Fields{
					"err":       err,
					"btcPubKey": req.valBtcPk.MarshalHex(),
					"height":    req.blockHeight,
				}).Error("failed to submit finality signature")
				req.errResponse <- err
				continue
			}

			app.logger.WithFields(logrus.Fields{
				"btcPubKey": req.valBtcPk.MarshalHex(),
				"height":    req.blockHeight,
				"txHash":    hex.EncodeToString(txHash),
			}).Info("successfully submitted a finality signature to babylon")

			app.finalitySigAddedEventChan <- &finalitySigAddedEvent{
				bbnPubKey: req.bbnPubKey,
				height:    req.blockHeight,
				txHash:    txHash,
				// pass the channel to the event so that we can send the response to the user which requested
				// the registration
				successResponse: req.successResponse,
			}
		case req := <-app.registerValidatorRequestChan:
			// we won't do any retries here to not block the loop for more important messages.
			// Most probably it fails due so some user error so we just return the error to the user.
			// TODO: need to start passing context here to be able to cancel the request in case of app quiting
			txHash, err := app.bc.RegisterValidator(req.bbnPubKey, req.btcPubKey, req.pop)

			if err != nil {
				app.logger.WithFields(logrus.Fields{
					"err":       err,
					"bbnPubKey": hex.EncodeToString(req.bbnPubKey.Key),
					"btcPubKey": req.btcPubKey.MarshalHex(),
				}).Error("failed to register validator")
				req.errResponse <- err
				continue
			}

			app.logger.WithFields(logrus.Fields{
				"bbnPk":  req.bbnPubKey,
				"txHash": hex.EncodeToString(txHash),
			}).Info("successfully registered validator on babylon")

			app.validatorRegisteredEventChan <- &validatorRegisteredEvent{
				bbnPubKey: req.bbnPubKey,
				txHash:    txHash,
				// pass the channel to the event so that we can send the response to the user which requested
				// the registration
				successResponse: req.successResponse,
			}
		case req := <-app.commitPubRandRequestChan:
			// TODO: need to start passing context here to be able to cancel the request in case of app quiting
			txHash, err := app.bc.CommitPubRandList(req.valBtcPk, req.startingHeight, req.pubRandList, req.sig)
			if err != nil {
				app.logger.WithFields(logrus.Fields{
					"err":         err,
					"btcPubKey":   req.valBtcPk,
					"startHeight": req.startingHeight,
				}).Error("failed to commit public randomness")
				req.errResponse <- err
				continue
			}

			app.logger.WithFields(logrus.Fields{
				"btcPk":  req.valBtcPk.MarshalHex(),
				"txHash": hex.EncodeToString(txHash),
			}).Info("successfully committed public rand list on babylon")

			app.pubRandCommittedEventChan <- &pubRandCommittedEvent{
				startingHeight: req.startingHeight,
				bbnPubKey:      req.bbnPubKey,
				valBtcPk:       req.valBtcPk,
				privRandList:   req.privRandList,
				pubRandList:    req.pubRandList,
				txHash:         txHash,
				// pass the channel to the event so that we can send the response to the user which requested
				// the commit
				successResponse: req.successResponse,
			}
		case req := <-app.addJurySigRequestChan:
			// TODO: we should add some retry mechanism or we can have a health checker to check the connection periodically
			txHash, err := app.bc.SubmitJurySig(req.valBtcPk, req.delBtcPk, req.sig)
			if err != nil {
				app.logger.WithFields(logrus.Fields{
					"err":          err,
					"valBtcPubKey": req.valBtcPk.MarshalHex(),
					"delBtcPubKey": req.delBtcPk.MarshalHex(),
				}).Error("failed to submit Jury signature")
				req.errResponse <- err
				continue
			}

			app.logger.WithFields(logrus.Fields{
				"delBtcPk":     req.delBtcPk.MarshalHex(),
				"valBtcPubKey": req.valBtcPk.MarshalHex(),
				"txHash":       hex.EncodeToString(txHash),
			}).Info("successfully submit Jury sig over Bitcoin delegation to Babylon")

			app.jurySigAddedEventChan <- &jurySigAddedEvent{
				bbnPubKey: req.bbnPubKey,
				txHash:    txHash,
				// pass the channel to the event so that we can send the response to the user which requested
				// the registration
				successResponse: req.successResponse,
			}

		case <-app.quit:
			return
		}
	}
}