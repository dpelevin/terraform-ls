// Copyright IBM Corp. 2020, 2026
// SPDX-License-Identifier: MPL-2.0

package rootmodules

import (
	"context"
	"path/filepath"
	"testing"

	lsctx "github.com/hashicorp/terraform-ls/internal/context"
	"github.com/hashicorp/terraform-ls/internal/document"
	"github.com/hashicorp/terraform-ls/internal/eventbus"
	"github.com/hashicorp/terraform-ls/internal/filesystem"
	"github.com/hashicorp/terraform-ls/internal/job"
	globalState "github.com/hashicorp/terraform-ls/internal/state"
	"github.com/hashicorp/terraform-ls/internal/terraform/exec"
)

// expectedIndexJobCount is the number of jobs indexRootModule schedules:
// GetTerraformVersion, ParseModuleManifest, ParseTerraformSources,
// ParseProviderVersions and ObtainSchema.
const expectedIndexJobCount = 5

func newTestFeature(t *testing.T) (*RootModulesFeature, *globalState.StateStore) {
	t.Helper()

	ss, err := globalState.NewStateStore()
	if err != nil {
		t.Fatal(err)
	}
	eventBus := eventbus.NewEventBus()
	fs := filesystem.NewFilesystem(ss.DocumentStore)

	feature, err := NewRootModulesFeature(eventBus, ss, fs, exec.NewMockExecutor(nil))
	if err != nil {
		t.Fatal(err)
	}

	return feature, ss
}

// openDocument marks a document as open in the given directory so that
// HasOpenDocuments(dir) returns true.
func openDocument(t *testing.T, ss *globalState.StateStore, dirPath string) {
	t.Helper()

	docHandle := document.HandleFromPath(filepath.Join(dirPath, "main.tf"))
	err := ss.DocumentStore.OpenDocument(docHandle, "terraform", 0, []byte("# test"))
	if err != nil {
		t.Fatal(err)
	}
}

// TestRootModulesFeature_discover_racesAfterDidOpen verifies that when the
// walker discovers a root module *after* the client already opened a document
// in that directory, discover() schedules the indexing jobs (including
// ObtainSchema). This is the race the fix closes: previously discover() only
// added the state entry and never scheduled jobs, so the provider schema was
// never obtained for already open root modules.
func TestRootModulesFeature_discover_racesAfterDidOpen(t *testing.T) {
	feature, ss := newTestFeature(t)
	ctx := lsctx.WithDocumentContext(context.Background(), lsctx.Document{})

	dirPath := t.TempDir()

	// Client opens a document before the walker reaches the directory.
	openDocument(t, ss, dirPath)

	// didOpen arrives first: no state entry yet, so nothing is scheduled.
	dir := document.DirHandleFromPath(dirPath)
	ids, err := feature.didOpen(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected didOpen to schedule no jobs before discovery, got %d", len(ids))
	}

	// Walker discovers the root module afterwards. Because the directory has
	// open documents, discover() must now schedule the full indexing chain.
	ids, err = feature.discover(ctx, dirPath, []string{".terraform.lock.hcl"})
	if err != nil {
		t.Fatal(err)
	}

	assertIndexChainScheduled(t, ss, dir, ids)
}

// TestRootModulesFeature_discover_noOpenDocs verifies that discover() does not
// schedule any jobs when there are no open documents in the directory (the
// common walker-first case, where didOpen will schedule them later).
func TestRootModulesFeature_discover_noOpenDocs(t *testing.T) {
	feature, _ := newTestFeature(t)
	ctx := lsctx.WithDocumentContext(context.Background(), lsctx.Document{})

	dirPath := t.TempDir()

	ids, err := feature.discover(ctx, dirPath, []string{".terraform.lock.hcl"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected discover to schedule no jobs without open documents, got %d", len(ids))
	}
	if !feature.Store.Exists(document.DirHandleFromPath(dirPath).Path()) {
		t.Fatal("expected discover to add a root module state entry")
	}
}

// TestRootModulesFeature_discover_thenDidOpen verifies the reverse order:
// discover() runs first (no open docs yet) and schedules nothing, then
// didOpen() schedules the full chain once the entry exists.
func TestRootModulesFeature_discover_thenDidOpen(t *testing.T) {
	feature, ss := newTestFeature(t)
	ctx := lsctx.WithDocumentContext(context.Background(), lsctx.Document{})

	dirPath := t.TempDir()

	// Walker discovers first, no open documents -> nothing scheduled, but the
	// state entry is created.
	ids, err := feature.discover(ctx, dirPath, []string{".terraform.lock.hcl"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected discover to schedule no jobs without open documents, got %d", len(ids))
	}

	// Client opens a document, didOpen schedules the chain.
	openDocument(t, ss, dirPath)
	dir := document.DirHandleFromPath(dirPath)
	ids, err = feature.didOpen(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	assertIndexChainScheduled(t, ss, dir, ids)
}

// TestRootModulesFeature_discover_datadir verifies the datadir branch of
// discover() also schedules the indexing chain when the discovered root module
// directory has open documents.
func TestRootModulesFeature_discover_datadir(t *testing.T) {
	feature, ss := newTestFeature(t)
	ctx := lsctx.WithDocumentContext(context.Background(), lsctx.Document{})

	dirPath := t.TempDir()
	openDocument(t, ss, dirPath)

	// The walker reports the .terraform data directory itself, which
	// ModuleUriFromDataDir resolves back to dirPath.
	dataDirPath := filepath.Join(dirPath, ".terraform")
	ids, err := feature.discover(ctx, dataDirPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	dir := document.DirHandleFromPath(dirPath)
	assertIndexChainScheduled(t, ss, dir, ids)
}

func assertIndexChainScheduled(t *testing.T, ss *globalState.StateStore, dir document.DirHandle, ids job.IDs) {
	t.Helper()

	if len(ids) != expectedIndexJobCount {
		t.Fatalf("expected %d jobs to be scheduled, got %d", expectedIndexJobCount, len(ids))
	}

	queued, err := ss.JobStore.ListIncompleteJobsForDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != expectedIndexJobCount {
		t.Fatalf("expected %d incomplete jobs for %q, got %d", expectedIndexJobCount, dir.Path(), len(queued))
	}
}
