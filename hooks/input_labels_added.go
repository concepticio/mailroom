package hooks

import (
	"context"
	"fmt"

	"github.com/gomodule/redigo/redis"
	"github.com/jmoiron/sqlx"
	"github.com/nyaruka/goflow/flows"
	"github.com/nyaruka/goflow/flows/events"
	"github.com/nyaruka/mailroom/models"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

func init() {
	models.RegisterEventHook(events.TypeInputLabelsAdded, handleInputLabelsAdded)
}

// CommitAddedLabelsHook is our hook for input labels being added
type CommitAddedLabelsHook struct{}

var commitAddedLabelsHook = &CommitAddedLabelsHook{}

// Apply applies our input labels added, committing them in a single batch
func (h *CommitAddedLabelsHook) Apply(ctx context.Context, tx *sqlx.Tx, rp *redis.Pool, org *models.OrgAssets, scene map[*models.Scene][]interface{}) error {
	// build our list of msg label adds, we dedupe these so we never double add in the same transaction
	seen := make(map[string]bool)
	adds := make([]*models.MsgLabelAdd, 0, len(scene))

	for _, as := range scene {
		for _, a := range as {
			add := a.(*models.MsgLabelAdd)
			key := fmt.Sprintf("%d:%d", add.LabelID, add.MsgID)
			if !seen[key] {
				adds = append(adds, add)
				seen[key] = true
			}
		}
	}

	// insert our adds
	return models.AddMsgLabels(ctx, tx, adds)
}

// handleInputLabelsAdded is called for each input labels added event in a scene
func handleInputLabelsAdded(ctx context.Context, tx *sqlx.Tx, rp *redis.Pool, org *models.OrgAssets, scene *models.Scene, e flows.Event) error {
	event := e.(*events.InputLabelsAddedEvent)
	logrus.WithFields(logrus.Fields{
		"contact_uuid": scene.ContactUUID(),
		"session_id":   scene.ID(),
		"labels":       event.Labels,
	}).Debug("input labels added")

	// for each label add an insertion
	for _, l := range event.Labels {
		label := org.LabelByUUID(l.UUID)
		if label == nil {
			return errors.Errorf("unable to find label with UUID: %s", l.UUID)
		}

		if scene.Session() == nil {
			return errors.Errorf("cannot add label, not in a session")
		}

		if scene.Session().IncomingMsgID() == models.NilMsgID {
			return errors.Errorf("cannot add label, no incoming message for scene: %d", scene.ID())
		}

		scene.AddPreCommitEvent(commitAddedLabelsHook, &models.MsgLabelAdd{
			MsgID:   scene.Session().IncomingMsgID(),
			LabelID: label.ID(),
		})
	}

	return nil
}
