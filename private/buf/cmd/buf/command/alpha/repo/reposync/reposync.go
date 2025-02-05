// Copyright 2020-2023 Buf Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package reposync

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bufbuild/buf/private/buf/bufcli"
	"github.com/bufbuild/buf/private/buf/bufsync"
	"github.com/bufbuild/buf/private/bufpkg/bufanalysis"
	"github.com/bufbuild/buf/private/bufpkg/bufmanifest"
	"github.com/bufbuild/buf/private/bufpkg/bufmodule/bufmoduleref"
	"github.com/bufbuild/buf/private/gen/proto/connect/buf/alpha/registry/v1alpha1/registryv1alpha1connect"
	registryv1alpha1 "github.com/bufbuild/buf/private/gen/proto/go/buf/alpha/registry/v1alpha1"
	"github.com/bufbuild/buf/private/pkg/app/appcmd"
	"github.com/bufbuild/buf/private/pkg/app/appflag"
	"github.com/bufbuild/buf/private/pkg/command"
	"github.com/bufbuild/buf/private/pkg/connectclient"
	"github.com/bufbuild/buf/private/pkg/git"
	"github.com/bufbuild/buf/private/pkg/manifest"
	"github.com/bufbuild/buf/private/pkg/normalpath"
	"github.com/bufbuild/buf/private/pkg/storage"
	"github.com/bufbuild/buf/private/pkg/storage/storagegit"
	"github.com/bufbuild/buf/private/pkg/stringutil"
	"github.com/bufbuild/connect-go"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	errorFormatFlagName      = "error-format"
	moduleFlagName           = "module"
	createFlagName           = "create"
	createVisibilityFlagName = "create-visibility"
	allBranchesFlagName      = "all-branches"
)

// NewCommand returns a new Command.
func NewCommand(
	name string,
	builder appflag.Builder,
) *appcmd.Command {
	flags := newFlags()
	return &appcmd.Command{
		Use:   name,
		Short: "Sync a Git repository to a registry",
		Long: "Sync a Git repository's commits to a registry in topological order. " +
			"Only commits in the current branch that are pushed to the 'origin' remote are processed. " +
			"Syncing all branches is possible using '--all-branches' flag." +
			// TODO rephrase in favor of a default module behavior.
			"Only modules specified via '--module' are synced.",
		Args: cobra.NoArgs,
		Run: builder.NewRunFunc(
			func(ctx context.Context, container appflag.Container) error {
				return run(ctx, container, flags)
			},
			// bufcli.NewErrorInterceptor(), // TODO re-enable
		),
		BindFlags: flags.Bind,
	}
}

type flags struct {
	ErrorFormat      string
	Modules          []string
	Create           bool
	CreateVisibility string
	AllBranches      bool
}

func newFlags() *flags {
	return &flags{}
}

func (f *flags) Bind(flagSet *pflag.FlagSet) {
	flagSet.StringVar(
		&f.ErrorFormat,
		errorFormatFlagName,
		"text",
		fmt.Sprintf(
			"The format for build errors printed to stderr. Must be one of %s",
			stringutil.SliceToString(bufanalysis.AllFormatStrings),
		),
	)
	// TODO: rework in favor of a default module behavior.
	flagSet.StringSliceVar(
		&f.Modules,
		moduleFlagName,
		nil,
		"The module(s) to sync to the BSR; this must be in the format <module-path>:<module-name>. "+
			"The <module-path> is the directory relative to the git repository, and the <module-name> "+
			"is the module's fully qualified name (FQN) as defined in "+
			"https://buf.build/docs/bsr/module/manage/#how-modules-are-defined",
	)
	bufcli.BindCreateVisibility(flagSet, &f.CreateVisibility, createVisibilityFlagName, createFlagName)
	flagSet.BoolVar(
		&f.Create,
		createFlagName,
		false,
		fmt.Sprintf("Create the repository if it does not exist. Must set a visibility using --%s", createVisibilityFlagName),
	)
	flagSet.BoolVar(
		&f.AllBranches,
		allBranchesFlagName,
		false,
		"Sync all git repository branches and not only the checked out one. "+
			"Only commits pushed to the 'origin' remote are processed. "+
			"Order of sync for git branches is as follows: First, it syncs the default branch read "+
			"from 'refs/remotes/origin/HEAD', and then all the rest of the branches present in "+
			"'refs/remotes/origin/*' in a lexicographical order.",
	)
}

func run(
	ctx context.Context,
	container appflag.Container,
	flags *flags,
) (retErr error) {
	if err := bufcli.ValidateErrorFormatFlag(flags.ErrorFormat, errorFormatFlagName); err != nil {
		return err
	}
	if flags.CreateVisibility != "" {
		if !flags.Create {
			return appcmd.NewInvalidArgumentErrorf("Cannot set --%s without --%s.", createVisibilityFlagName, createFlagName)
		}
		// We re-parse below as needed, but do not return an appcmd.NewInvalidArgumentError below as
		// we expect validation to be handled here.
		if _, err := bufcli.VisibilityFlagToVisibility(flags.CreateVisibility); err != nil {
			return appcmd.NewInvalidArgumentError(err.Error())
		}
	} else if flags.Create {
		return appcmd.NewInvalidArgumentErrorf("--%s is required if --%s is set.", createVisibilityFlagName, createFlagName)
	}
	return sync(
		ctx,
		container,
		flags.Modules,
		// No need to pass `flags.Create`, this is not empty iff `flags.Create`
		flags.CreateVisibility,
		flags.AllBranches,
	)
}

func sync(
	ctx context.Context,
	container appflag.Container,
	modules []string,
	createWithVisibility string,
	allBranches bool,
) error {
	if len(modules) == 0 {
		container.Logger().Info("no modules to sync")
		return nil
	}
	// Assume that this command is run from the repository root. If not, `OpenRepository` will return
	// a dir not found error.
	repo, err := git.OpenRepository(ctx, git.DotGitDir, command.NewRunner())
	if err != nil {
		return fmt.Errorf("open repository: %w", err)
	}
	defer repo.Close()
	storageProvider := storagegit.NewProvider(
		repo.Objects(),
		storagegit.ProviderWithSymlinks(),
	)
	clientConfig, err := bufcli.NewConnectClientConfig(container)
	if err != nil {
		return fmt.Errorf("create connect client %w", err)
	}
	syncerOptions := []bufsync.SyncerOption{
		bufsync.SyncerWithResumption(syncPointResolver(clientConfig)),
		bufsync.SyncerWithGitCommitChecker(syncGitCommitChecker(clientConfig)),
		bufsync.SyncerWithModuleDefaultBranchGetter(defaultBranchGetter(clientConfig)),
	}
	if allBranches {
		syncerOptions = append(syncerOptions, bufsync.SyncerWithAllBranches())
	}
	for _, module := range modules {
		var moduleIdentityOverride bufmoduleref.ModuleIdentity
		colon := strings.IndexRune(module, ':')
		if colon == -1 {
			return appcmd.NewInvalidArgumentErrorf("module %q is missing an identity", module)
		}
		moduleIdentityOverride, err = bufmoduleref.ModuleIdentityForString(module[colon+1:])
		if err != nil {
			return fmt.Errorf("module identity: %w", err)
		}
		module = normalpath.Normalize(module[:colon])
		syncModule, err := bufsync.NewModule(module, moduleIdentityOverride)
		if err != nil {
			return fmt.Errorf("prepare module for sync: %w", err)
		}
		syncerOptions = append(syncerOptions, bufsync.SyncerWithModule(syncModule))
	}
	syncer, err := bufsync.NewSyncer(
		container.Logger(),
		repo,
		storageProvider,
		newErrorHandler(container.Logger()),
		syncerOptions...,
	)
	if err != nil {
		return fmt.Errorf("new syncer: %w", err)
	}
	return syncer.Sync(ctx, func(ctx context.Context, moduleCommit bufsync.ModuleCommit) error {
		syncPoint, err := pushOrCreate(
			ctx,
			clientConfig,
			repo,
			moduleCommit.Commit(),
			moduleCommit.Branch(),
			moduleCommit.Tags(),
			moduleCommit.Identity(),
			moduleCommit.Bucket(),
			createWithVisibility,
		)
		if err != nil {
			// We failed to push. We fail hard on this because the error may be recoverable
			// (i.e., the BSR may be down) and we should re-attempt this commit.
			return fmt.Errorf(
				"failed to push or create %s at %s: %w",
				moduleCommit.Identity().IdentityString(),
				moduleCommit.Commit().Hash(),
				err,
			)
		}
		_, err = container.Stderr().Write([]byte(
			// from local                     -> to remote
			// <git-branch>:<git-commit-hash> -> <module-identity>:<bsr-commit-name>
			fmt.Sprintf(
				"%s:%s -> %s:%s\n",
				moduleCommit.Branch(), moduleCommit.Commit().Hash().Hex(),
				moduleCommit.Identity().IdentityString(), syncPoint.BsrCommitName,
			)),
		)
		return err
	})
}

func syncPointResolver(clientConfig *connectclient.Config) bufsync.SyncPointResolver {
	return func(ctx context.Context, module bufmoduleref.ModuleIdentity, branch string) (git.Hash, error) {
		service := connectclient.Make(clientConfig, module.Remote(), registryv1alpha1connect.NewSyncServiceClient)
		syncPoint, err := service.GetGitSyncPoint(ctx, connect.NewRequest(&registryv1alpha1.GetGitSyncPointRequest{
			Owner:      module.Owner(),
			Repository: module.Repository(),
			Branch:     branch,
		}))
		if err != nil {
			if connect.CodeOf(err) == connect.CodeNotFound {
				// No syncpoint
				return nil, nil
			}
			return nil, fmt.Errorf("get git sync point: %w", err)
		}
		hash, err := git.NewHashFromHex(syncPoint.Msg.GetSyncPoint().GitCommitHash)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid sync point from BSR %q: %w",
				syncPoint.Msg.GetSyncPoint().GetGitCommitHash(),
				err,
			)
		}
		return hash, nil
	}
}

func syncGitCommitChecker(clientConfig *connectclient.Config) bufsync.SyncedGitCommitChecker {
	return func(ctx context.Context, module bufmoduleref.ModuleIdentity, commitHashes map[string]struct{}) (map[string]struct{}, error) {
		service := connectclient.Make(clientConfig, module.Remote(), registryv1alpha1connect.NewLabelServiceClient)
		res, err := service.GetLabelsInNamespace(ctx, connect.NewRequest(&registryv1alpha1.GetLabelsInNamespaceRequest{
			RepositoryOwner: module.Owner(),
			RepositoryName:  module.Repository(),
			LabelNamespace:  registryv1alpha1.LabelNamespace_LABEL_NAMESPACE_GIT_COMMIT,
			LabelNames:      stringutil.MapToSlice(commitHashes),
		}))
		if err != nil {
			if connect.CodeOf(err) == connect.CodeNotFound {
				// Repo is not created
				return nil, nil
			}
			return nil, fmt.Errorf("get labels in namespace: %w", err)
		}
		syncedHashes := make(map[string]struct{})
		for _, label := range res.Msg.Labels {
			syncedHash := label.LabelName.Name
			if _, expected := commitHashes[syncedHash]; !expected {
				return nil, fmt.Errorf("received unexpected synced hash %q, expected %v", syncedHash, commitHashes)
			}
			syncedHashes[syncedHash] = struct{}{}
		}
		return syncedHashes, nil
	}
}

func defaultBranchGetter(clientConfig *connectclient.Config) bufsync.ModuleDefaultBranchGetter {
	return func(ctx context.Context, module bufmoduleref.ModuleIdentity) (string, error) {
		service := connectclient.Make(clientConfig, module.Remote(), registryv1alpha1connect.NewRepositoryServiceClient)
		res, err := service.GetRepositoryByFullName(ctx, connect.NewRequest(&registryv1alpha1.GetRepositoryByFullNameRequest{
			FullName: module.Owner() + "/" + module.Repository(),
		}))
		if err != nil {
			if connect.CodeOf(err) == connect.CodeNotFound {
				// Repo is not created
				return "", bufsync.ErrModuleDoesNotExist
			}
			return "", fmt.Errorf("get repository by full name %q: %w", module.IdentityString(), err)
		}
		return res.Msg.Repository.DefaultBranch, nil
	}
}

type syncErrorHandler struct {
	logger *zap.Logger
}

func newErrorHandler(logger *zap.Logger) bufsync.ErrorHandler {
	return &syncErrorHandler{logger: logger}
}

func (s *syncErrorHandler) BuildFailure(module bufsync.Module, commit git.Commit, err error) error {
	// We failed to build the module. We can warn on this and carry on.
	// Note that because of resumption, Syncer will typically only come
	// across this commit once, we will not log this warning again.
	s.logger.Warn(
		"module build failure",
		zap.Stringer("commit", commit.Hash()),
		zap.Stringer("module", module),
		zap.Error(err),
	)
	return nil
}

func (s *syncErrorHandler) InvalidModuleConfig(module bufsync.Module, commit git.Commit, err error) error {
	// We found a module but the module config is invalid. We can warn on this
	// and carry on. Note that because of resumption, Syncer will typically only come
	// across this commit once, we will not log this warning again.
	s.logger.Warn(
		"invalid module config",
		zap.Stringer("commit", commit.Hash()),
		zap.Stringer("module", module),
		zap.Error(err),
	)
	return nil
}

func (s *syncErrorHandler) InvalidSyncPoint(
	module bufsync.Module,
	branch string,
	syncPoint git.Hash,
	err error,
) error {
	// The most likely culprit for an invalid sync point is a rebase, where the last known
	// commit has been garbage collected. In this case, let's present a better error message.
	//
	// We may want to provide a flag for sync to continue despite this, accumulating the error,
	// and error at the end, so that other branches can continue to sync, but this branch is
	// out of date. This is not trivial if the branch that's been rebased is a long-lived
	// branch (like main) whose artifacts are consumed by other branches, as we may fail to
	// sync those commits if we continue. So we now we simply error.
	if errors.Is(err, git.ErrObjectNotFound) {
		return fmt.Errorf(
			"last synced commit %s was not found for module %s; did you rebase?",
			syncPoint,
			module,
		)
	}
	// Otherwise, we still want this to fail sync, let's bubble this up.
	return err
}

func pushOrCreate(
	ctx context.Context,
	clientConfig *connectclient.Config,
	repo git.Repository,
	commit git.Commit,
	branch string,
	tags []string,
	moduleIdentity bufmoduleref.ModuleIdentity,
	moduleBucket storage.ReadBucket,
	createWithVisibility string,
) (*registryv1alpha1.GitSyncPoint, error) {
	modulePin, err := push(
		ctx,
		clientConfig,
		repo,
		commit,
		branch,
		tags,
		moduleIdentity,
		moduleBucket,
	)
	if err != nil {
		// We rely on Push* returning a NotFound error to denote the repository is not created.
		// This technically could be a NotFound error for some other entity than the repository
		// in question, however if it is, then this Create call will just fail as the repository
		// is already created, and there is no side effect. The 99% case is that a NotFound
		// error is because the repository does not exist, and we want to avoid having to do
		// a GetRepository RPC call for every call to push --create.
		if createWithVisibility != "" && connect.CodeOf(err) == connect.CodeNotFound {
			if err := create(ctx, clientConfig, moduleIdentity, createWithVisibility); err != nil {
				return nil, fmt.Errorf("create repo: %w", err)
			}
			return push(
				ctx,
				clientConfig,
				repo,
				commit,
				branch,
				tags,
				moduleIdentity,
				moduleBucket,
			)
		}
		return nil, fmt.Errorf("push: %w", err)
	}
	return modulePin, nil
}

func push(
	ctx context.Context,
	clientConfig *connectclient.Config,
	repo git.Repository,
	commit git.Commit,
	branch string,
	tags []string,
	moduleIdentity bufmoduleref.ModuleIdentity,
	moduleBucket storage.ReadBucket,
) (*registryv1alpha1.GitSyncPoint, error) {
	service := connectclient.Make(clientConfig, moduleIdentity.Remote(), registryv1alpha1connect.NewSyncServiceClient)
	m, blobSet, err := manifest.NewFromBucket(ctx, moduleBucket)
	if err != nil {
		return nil, err
	}
	bucketManifest, blobs, err := bufmanifest.ToProtoManifestAndBlobs(ctx, m, blobSet)
	if err != nil {
		return nil, err
	}
	resp, err := service.SyncGitCommit(ctx, connect.NewRequest(&registryv1alpha1.SyncGitCommitRequest{
		Owner:      moduleIdentity.Owner(),
		Repository: moduleIdentity.Repository(),
		Manifest:   bucketManifest,
		Blobs:      blobs,
		Hash:       commit.Hash().Hex(),
		Branch:     branch,
		Tags:       tags,
		Author: &registryv1alpha1.GitIdentity{
			Name:  commit.Author().Name(),
			Email: commit.Author().Email(),
			Time:  timestamppb.New(commit.Author().Timestamp()),
		},
		Commiter: &registryv1alpha1.GitIdentity{
			Name:  commit.Committer().Name(),
			Email: commit.Committer().Email(),
			Time:  timestamppb.New(commit.Committer().Timestamp()),
		},
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.SyncPoint, nil
}

func create(
	ctx context.Context,
	clientConfig *connectclient.Config,
	moduleIdentity bufmoduleref.ModuleIdentity,
	visibility string,
) error {
	service := connectclient.Make(clientConfig, moduleIdentity.Remote(), registryv1alpha1connect.NewRepositoryServiceClient)
	visiblity, err := bufcli.VisibilityFlagToVisibility(visibility)
	if err != nil {
		return err
	}
	fullName := moduleIdentity.Owner() + "/" + moduleIdentity.Repository()
	_, err = service.CreateRepositoryByFullName(
		ctx,
		connect.NewRequest(&registryv1alpha1.CreateRepositoryByFullNameRequest{
			FullName:   fullName,
			Visibility: visiblity,
		}),
	)
	if err != nil && connect.CodeOf(err) == connect.CodeAlreadyExists {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("expected repository %s to be missing but found the repository to already exist", fullName))
	}
	return err
}
