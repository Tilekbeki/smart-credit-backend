package pipeline

import (
	"context"
	"encoding/json"
)

type RecoveryCommand struct {
	ActorID, ApplicationID, Key, Reason string
	Version                             int64
}
type RecoveryResponse struct {
	Status int
	Body   json.RawMessage
}
type RecoveryRepository interface {
	RecoverJob(context.Context, RecoveryCommand) (RecoveryResponse, error)
}
