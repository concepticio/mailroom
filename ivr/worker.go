package ivr

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/jmoiron/sqlx"
	"github.com/nyaruka/goflow/flows"
	"github.com/nyaruka/mailroom"
	"github.com/nyaruka/mailroom/config"
	"github.com/nyaruka/mailroom/models"
	"github.com/nyaruka/mailroom/queue"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

func init() {
	mailroom.AddTaskFunction(mailroom.StartIVRFlowBatchType, handleFlowStartTask)
}

func handleFlowStartTask(ctx context.Context, mr *mailroom.Mailroom, task *queue.Task) error {
	// decode our task body
	if task.Type != mailroom.StartIVRFlowBatchType {
		return errors.Errorf("unknown event type passed to ivr worker: %s", task.Type)
	}
	batch := &models.FlowStartBatch{}
	err := json.Unmarshal(task.Task, batch)
	if err != nil {
		return errors.Wrapf(err, "error unmarshalling flow start batch: %s", string(task.Task))
	}

	return HandleFlowStartBatch(ctx, mr.Config, mr.DB, mr.RP, batch)
}

// HandleFlowStartBatch starts a batch of contacts in an IVR flow
func HandleFlowStartBatch(ctx context.Context, config *config.Config, db *sqlx.DB, rp *redis.Pool, batch *models.FlowStartBatch) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute*5)
	defer cancel()

	// contacts we will exclude either because they are in a flow or have already been in this one
	exclude := make(map[flows.ContactID]bool, 5)

	// filter out anybody who has has a flow run in this flow if appropriate
	if !batch.RestartParticipants() {
		// find all participants that have been in this flow
		started, err := models.FindFlowStartedOverlap(ctx, db, batch.FlowID(), batch.ContactIDs())
		if err != nil {
			return errors.Wrapf(err, "error finding others started flow: %d", batch.FlowID())
		}
		for _, c := range started {
			exclude[c] = true
		}
	}

	// filter out our list of contacts to only include those that should be started
	if !batch.IncludeActive() {
		// find all participants active in any flow
		active, err := models.FindActiveRunOverlap(ctx, db, batch.ContactIDs())
		if err != nil {
			return errors.Wrapf(err, "error finding other active flow: %d", batch.FlowID())
		}
		for _, c := range active {
			exclude[c] = true
		}
	}

	// filter into our final list of contacts
	contactIDs := make([]flows.ContactID, 0, len(batch.ContactIDs()))
	for _, c := range batch.ContactIDs() {
		if !exclude[c] {
			contactIDs = append(contactIDs, c)
		}
	}

	// load our org assets
	org, err := models.GetOrgAssets(ctx, db, batch.OrgID())
	if err != nil {
		return errors.Wrapf(err, "error loading org assets for org: %d", batch.OrgID())
	}

	// ok, we can initiate calls for the remaining contacts
	contacts, err := models.LoadContacts(ctx, db, org, contactIDs)
	if err != nil {
		return errors.Wrapf(err, "error loading contacts")
	}

	// for each contacts, request a call start
	for _, contact := range contacts {
		start := time.Now()
		session, err := RequestCallStart(ctx, config, db, org, batch, contact)
		if err != nil {
			logrus.WithError(err).Errorf("error starting ivr flow for contact: %d and flow: %d", contact.ID(), batch.FlowID())
			continue
		}
		logrus.WithFields(logrus.Fields{
			"elapsed":     time.Since(start),
			"contact_id":  contact.ID(),
			"status":      session.Status(),
			"start_id":    batch.StartID().Int64,
			"external_id": session.ExternalID(),
		}).Debug("requested call for contact")
	}

	// if this is a last batch, mark our start as started
	if batch.IsLast() {
		err := models.MarkStartComplete(ctx, db, batch.StartID())
		if err != nil {
			return errors.Wrapf(err, "error trying to set batch as complete")
		}
	}

	return nil
}