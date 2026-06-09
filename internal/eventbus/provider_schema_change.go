// Copyright IBM Corp. 2020, 2026
// SPDX-License-Identifier: MPL-2.0

package eventbus

import (
	"context"

	"github.com/hashicorp/terraform-ls/internal/document"
	"github.com/hashicorp/terraform-ls/internal/job"
)

// ProviderSchemaChangeEvent is an event that is fired whenever new provider
// schemas have been obtained for a root module (e.g. via
// `terraform providers schema -json` in the ObtainSchema job).
//
// Features that decode configuration against provider schemas subscribe to it
// so they can re-decode already open modules. This matters when the schema
// arrives *after* the initial decode: the decode jobs short-circuit on
// "state not changed" because their invalidation is tied to module content,
// not to provider schema availability. Without re-decoding, the reference
// graph is never rebuilt against the late schema and hover / go-to-definition
// stay broken until the file is edited.
type ProviderSchemaChangeEvent struct {
	Context context.Context

	Dir document.DirHandle
}

func (n *EventBus) OnProviderSchemaChange(identifier string, doneChannel DoneChannel) <-chan ProviderSchemaChangeEvent {
	n.logger.Printf("bus: %q subscribed to OnProviderSchemaChange", identifier)
	return n.providerSchemaChangeTopic.Subscribe(doneChannel)
}

// ProviderSchemaChange publishes the event to all subscribers and returns any
// job IDs they spawned, so the caller can track them for synchronization.
func (n *EventBus) ProviderSchemaChange(e ProviderSchemaChangeEvent) job.IDs {
	n.logger.Printf("bus: -> ProviderSchemaChange %s", e.Dir)
	return n.providerSchemaChangeTopic.Publish(e)
}
