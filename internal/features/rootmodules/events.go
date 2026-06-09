// Copyright IBM Corp. 2020, 2026
// SPDX-License-Identifier: MPL-2.0

package rootmodules

import (
	"context"
	"path/filepath"

	"github.com/hashicorp/terraform-ls/internal/document"
	"github.com/hashicorp/terraform-ls/internal/eventbus"
	"github.com/hashicorp/terraform-ls/internal/features/rootmodules/ast"
	"github.com/hashicorp/terraform-ls/internal/features/rootmodules/jobs"
	"github.com/hashicorp/terraform-ls/internal/job"
	"github.com/hashicorp/terraform-ls/internal/protocol"
	"github.com/hashicorp/terraform-ls/internal/terraform/datadir"
	"github.com/hashicorp/terraform-ls/internal/terraform/exec"
	op "github.com/hashicorp/terraform-ls/internal/terraform/module/operation"
	"github.com/hashicorp/terraform-ls/internal/uri"
)

func (f *RootModulesFeature) discover(ctx context.Context, path string, files []string) (job.IDs, error) {
	ids := make(job.IDs, 0)

	// The walker is expected to provide a context, but guard against a nil
	// parent so enqueuing jobs (which derives an OpenTelemetry span from it)
	// can never panic the server if a publisher omits it.
	if ctx == nil {
		ctx = context.Background()
	}

	rawUri := uri.FromPath(path)
	if uri, ok := datadir.ModuleUriFromDataDir(rawUri); ok {
		f.logger.Printf("discovered root module in %s", uri)
		dir := document.DirHandleFromURI(uri)
		err := f.Store.AddIfNotExists(dir.Path())
		if err != nil {
			return ids, err
		}

		return f.indexOpenRootModule(ctx, dir)
	}

	for _, file := range files {
		if ast.IsRootModuleFilename(file) {
			f.logger.Printf("discovered root module file in %s", path)

			dir := document.DirHandleFromPath(path)
			err := f.Store.AddIfNotExists(dir.Path())
			if err != nil {
				return ids, err
			}

			return f.indexOpenRootModule(ctx, dir)
		}
	}

	return ids, nil
}

// indexOpenRootModule schedules the indexing job chain for a root module
// directory, but only if it currently has open documents.
//
// This closes a race between didOpen and discover: there is no dedicated
// language id for root module related files, so the walker discovers root
// modules asynchronously. On large multi-root workspaces the walker may reach
// a directory *after* the client already sent didOpen for files in it. In that
// case didOpen returns early (no state entry yet) and only discover adds the
// entry. Without scheduling here, ObtainSchema would never run for already
// open root modules until a lockfile change or a subsequent didOpen.
//
// Whichever of {didOpen, discover} runs last schedules the jobs. Idempotency
// is guaranteed by the job state checks (e.g. ProviderSchemaState != OpStateUnknown,
// StateNotChangedErr), so the reverse order does not enqueue duplicate work.
func (f *RootModulesFeature) indexOpenRootModule(ctx context.Context, dir document.DirHandle) (job.IDs, error) {
	ids := make(job.IDs, 0)

	hasOpenDocs, err := f.stateStore.DocumentStore.HasOpenDocuments(dir)
	if err != nil {
		f.logger.Printf("error when checking for open documents in discovered root module %q: %s", dir.Path(), err)
	}
	if !hasOpenDocs {
		return ids, nil
	}

	return f.indexRootModule(ctx, dir)
}

func (f *RootModulesFeature) didOpen(ctx context.Context, dir document.DirHandle) (job.IDs, error) {
	ids := make(job.IDs, 0)
	path := dir.Path()

	// There is no dedicated language id for root module related files
	// so we rely on the walker to discover root modules and add them to the
	// store during walking.

	// Schedule jobs if state entry exists
	hasModuleRootRecord := f.Store.Exists(path)
	if !hasModuleRootRecord {
		return ids, nil
	}

	return f.indexRootModule(ctx, dir)
}

// indexRootModule schedules the full indexing job chain for a root module:
// Terraform version, module manifest, Terraform sources, provider versions and
// the provider schema (ObtainSchema). It is shared by didOpen and discover.
func (f *RootModulesFeature) indexRootModule(ctx context.Context, dir document.DirHandle) (job.IDs, error) {
	ids := make(job.IDs, 0)
	path := dir.Path()

	versionId, err := f.stateStore.JobStore.EnqueueJob(ctx, job.Job{
		Dir: dir,
		Func: func(ctx context.Context) error {
			ctx = exec.WithExecutorFactory(ctx, f.tfExecFactory)
			return jobs.GetTerraformVersion(ctx, f.Store, path)
		},
		Type: op.OpTypeGetTerraformVersion.String(),
	})
	if err != nil {
		return ids, nil
	}
	ids = append(ids, versionId)

	modManifestId, err := f.stateStore.JobStore.EnqueueJob(ctx, job.Job{
		Dir: dir,
		Func: func(ctx context.Context) error {
			return jobs.ParseModuleManifest(ctx, f.fs, f.Store, dir.Path())
		},
		Type: op.OpTypeParseModuleManifest.String(),
	})
	if err != nil {
		return ids, err
	}
	ids = append(ids, modManifestId)

	terraformSourcesId, err := f.stateStore.JobStore.EnqueueJob(ctx, job.Job{
		Dir: dir,
		Func: func(ctx context.Context) error {
			return jobs.ParseTerraformSources(ctx, f.fs, f.Store, dir.Path())
		},
		Type: op.OpTypeParseTerraformSources.String(),
	})
	if err != nil {
		return ids, err
	}
	ids = append(ids, terraformSourcesId)

	pSchemaVerId, err := f.stateStore.JobStore.EnqueueJob(ctx, job.Job{
		Dir: dir,
		Func: func(ctx context.Context) error {
			return jobs.ParseProviderVersions(ctx, f.fs, f.Store, path)
		},
		Type: op.OpTypeParseProviderVersions.String(),
	})
	if err != nil {
		return ids, err
	}
	ids = append(ids, pSchemaVerId)

	pSchemaId, err := f.stateStore.JobStore.EnqueueJob(ctx, job.Job{
		Dir: dir,
		Func: func(ctx context.Context) error {
			ctx = exec.WithExecutorFactory(ctx, f.tfExecFactory)
			return jobs.ObtainSchema(ctx, f.Store, f.stateStore.ProviderSchemas, path)
		},
		Type:      op.OpTypeObtainSchema.String(),
		DependsOn: job.IDs{pSchemaVerId},
		Defer:     f.notifyProviderSchemaChange(dir),
	})
	if err != nil {
		return ids, err
	}
	ids = append(ids, pSchemaId)

	return ids, nil
}

// notifyProviderSchemaChange returns a job.DeferFunc to attach to an
// ObtainSchema job. When the job obtained new schemas (jobErr == nil), it
// publishes a ProviderSchemaChangeEvent so that features which decode against
// provider schemas can re-decode any open modules. When the job
// short-circuited because the schema was already known
// (job.StateNotChangedErr) or failed, nothing is published.
func (f *RootModulesFeature) notifyProviderSchemaChange(dir document.DirHandle) job.DeferFunc {
	return func(ctx context.Context, jobErr error) (job.IDs, error) {
		if jobErr != nil {
			return nil, nil
		}

		spawnedIds := f.eventbus.ProviderSchemaChange(eventbus.ProviderSchemaChangeEvent{
			Context: ctx,
			Dir:     dir,
		})

		return spawnedIds, nil
	}
}

func (f *RootModulesFeature) pluginLockChange(ctx context.Context, dir document.DirHandle) (job.IDs, error) {
	ids := make(job.IDs, 0)
	path := dir.Path()

	// We might not have a record yet, so we add it
	err := f.Store.AddIfNotExists(path)
	if err != nil {
		return ids, err
	}

	pSchemaVerId, err := f.stateStore.JobStore.EnqueueJob(ctx, job.Job{
		Dir: dir,
		Func: func(ctx context.Context) error {
			return jobs.ParseProviderVersions(ctx, f.fs, f.Store, path)
		},
		IgnoreState: true,
		Type:        op.OpTypeParseProviderVersions.String(),
	})
	if err != nil {
		return ids, err
	}
	ids = append(ids, pSchemaVerId)

	pSchemaId, err := f.stateStore.JobStore.EnqueueJob(ctx, job.Job{
		Dir: dir,
		Func: func(ctx context.Context) error {
			ctx = exec.WithExecutorFactory(ctx, f.tfExecFactory)
			return jobs.ObtainSchema(ctx, f.Store, f.stateStore.ProviderSchemas, path)
		},
		IgnoreState: true,
		Type:        op.OpTypeObtainSchema.String(),
		DependsOn:   job.IDs{pSchemaVerId},
		Defer:       f.notifyProviderSchemaChange(dir),
	})
	if err != nil {
		return ids, err
	}
	ids = append(ids, pSchemaId)

	return ids, nil
}

func (f *RootModulesFeature) manifestChange(ctx context.Context, dir document.DirHandle, changeType protocol.FileChangeType) (job.IDs, error) {
	ids := make(job.IDs, 0)
	path := dir.Path()

	// We might not have a record yet, so we add it
	err := f.Store.AddIfNotExists(path)
	if err != nil {
		return ids, err
	}

	if changeType == protocol.Deleted {
		// Manifest is deleted, so we clear the manifest from the store
		f.Store.UpdateModManifest(path, nil, nil)
		// We also delete the Terraform Sources (if they exist), since delete changes can also happen if the
		// entire .terraform directory is deleted and there should only be either a manifest or terraform sources anyway
		f.Store.UpdateTerraformSources(path, nil, nil)
		return ids, nil
	}

	modManifestId, err := f.stateStore.JobStore.EnqueueJob(ctx, job.Job{
		Dir: dir,
		Func: func(ctx context.Context) error {
			return jobs.ParseModuleManifest(ctx, f.fs, f.Store, path)
		},
		Type: op.OpTypeParseModuleManifest.String(),
		Defer: func(ctx context.Context, jobErr error) (job.IDs, error) {
			return f.indexInstalledModuleCalls(ctx, dir)
		},
	})
	if err != nil {
		return ids, err
	}
	ids = append(ids, modManifestId)

	terraformSourcesId, err := f.stateStore.JobStore.EnqueueJob(ctx, job.Job{
		Dir: dir,
		Func: func(ctx context.Context) error {
			return jobs.ParseTerraformSources(ctx, f.fs, f.Store, dir.Path())
		},
		Type: op.OpTypeParseTerraformSources.String(),
		Defer: func(ctx context.Context, jobErr error) (job.IDs, error) {
			return f.indexTerraformSourcesDirs(ctx, dir)
		},
	})
	if err != nil {
		return ids, err
	}
	ids = append(ids, terraformSourcesId)

	return ids, nil
}

func (f *RootModulesFeature) indexInstalledModuleCalls(ctx context.Context, dir document.DirHandle) (job.IDs, error) {
	jobIds := make(job.IDs, 0)

	moduleCalls, err := f.Store.InstalledModuleCalls(dir.Path())
	if err != nil {
		return jobIds, err
	}

	for _, mc := range moduleCalls {
		mcHandle := document.DirHandleFromPath(mc.Path)
		f.stateStore.WalkerPaths.EnqueueDir(ctx, mcHandle)
	}

	return jobIds, nil
}

func (f *RootModulesFeature) indexTerraformSourcesDirs(ctx context.Context, dir document.DirHandle) (job.IDs, error) {
	jobIds := make(job.IDs, 0)

	for _, subDir := range f.Store.TerraformSourcesDirectories(dir.Path()) {
		dh := document.DirHandleFromPath(filepath.Join(dir.Path(), subDir))
		f.stateStore.WalkerPaths.EnqueueDir(ctx, dh)
	}

	return jobIds, nil
}
