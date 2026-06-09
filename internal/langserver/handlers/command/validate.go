// Copyright IBM Corp. 2020, 2026
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"fmt"

	"github.com/creachadair/jrpc2"
	"github.com/hashicorp/terraform-ls/internal/document"
	"github.com/hashicorp/terraform-ls/internal/features/modules/jobs"
	"github.com/hashicorp/terraform-ls/internal/job"
	"github.com/hashicorp/terraform-ls/internal/langserver/cmd"
	"github.com/hashicorp/terraform-ls/internal/langserver/progress"
	op "github.com/hashicorp/terraform-ls/internal/terraform/module/operation"
	"github.com/hashicorp/terraform-ls/internal/uri"
)

func (h *CmdHandler) TerraformValidateHandler(ctx context.Context, args cmd.CommandArgs) (interface{}, error) {
	dirUri, ok := args.GetString("uri")
	if !ok || dirUri == "" {
		return nil, fmt.Errorf("%w: expected module uri argument to be set", jrpc2.InvalidParams.Err())
	}

	if !uri.IsURIValid(dirUri) {
		return nil, fmt.Errorf("URI %q is not valid", dirUri)
	}

	if h.ModulesFeature == nil {
		return nil, fmt.Errorf("modules feature is not available")
	}

	dirHandle := document.DirHandleFromURI(dirUri)

	progress.Begin(ctx, "Validating")
	defer func() {
		progress.End(ctx, "Finished")
	}()

	progress.Report(ctx, "Running terraform validate ...")
	id, err := h.StateStore.JobStore.EnqueueJob(ctx, job.Job{
		Dir: dirHandle,
		Func: func(ctx context.Context) error {
			return jobs.TerraformValidate(ctx, h.ModulesFeature.Store, dirHandle.Path())
		},
		Type:        op.OpTypeTerraformValidate.String(),
		IgnoreState: true,
	})
	if err != nil {
		return nil, err
	}

	return nil, h.StateStore.JobStore.WaitForJobs(ctx, id)
}
