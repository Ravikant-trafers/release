/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package release

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"cloud.google.com/go/storage"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"k8s.io/release/pkg/gcp/gcs"
	"k8s.io/release/pkg/util"
	"k8s.io/utils/pointer"
)

// PushBuild is the main structure for pushing builds.
type PushBuild struct {
	opts *PushBuildOptions
}

// PushBuildOptions are the main options to pass to `PushBuild`.
type PushBuildOptions struct {
	// Specify an alternate bucket for pushes (normally 'devel' or 'ci').
	Bucket string

	// Specify an alternate build directory (defaults to `release.BuildDir`).
	BuildDir string

	// If set, push docker images to specified registry/project.
	DockerRegistry string

	// Comma separated list which can be used to upload additional version
	// files to GCS. The path is relative and is append to a GCS path. (--ci
	// only).
	ExtraVersionMarkers string

	// Specify a suffix to append to the upload destination on GCS.
	GCSSuffix string

	// Append suffix to version name if set.
	VersionSuffix string

	// Do not exit error if the build already exists on the gcs path.
	AllowDup bool

	// Used when called from Jenkins (for ci runs).
	CI bool

	// Do not update the latest file.
	NoUpdateLatest bool

	// Do not mark published bits on GCS as publicly readable.
	PrivateBucket bool

	// Specifies a fast build (linux amd64 only).
	Fast bool

	// Validate that the remove image digests exists, needs `skopeo` in
	// `$PATH`.
	ValidateRemoteImageDigests bool
}

type stageFile struct {
	srcPath  string
	dstPath  string
	required bool
}

var gcpStageFiles = []stageFile{
	{
		srcPath:  filepath.Join(GCEPath, "configure-vm.sh"),
		dstPath:  "extra/gce/configure-vm.sh",
		required: false,
	},
	{
		srcPath:  filepath.Join(GCIPath, "node.yaml"),
		dstPath:  "extra/gce/node.yaml",
		required: true,
	},
	{
		srcPath:  filepath.Join(GCIPath, "master.yaml"),
		dstPath:  "extra/gce/master.yaml",
		required: true,
	},
	{
		srcPath:  filepath.Join(GCIPath, "configure.sh"),
		dstPath:  "extra/gce/configure.sh",
		required: true,
	},
	{
		srcPath:  filepath.Join(GCIPath, "shutdown.sh"),
		dstPath:  "extra/gce/shutdown.sh",
		required: false,
	},
}

var windowsStageFiles = []stageFile{
	{
		srcPath:  filepath.Join(WindowsLocalPath, "configure.ps1"),
		dstPath:  "extra/gce/windows/configure.ps1",
		required: true,
	},
	{
		srcPath:  filepath.Join(WindowsLocalPath, "common.psm1"),
		dstPath:  "extra/gce/windows/common.psm1",
		required: true,
	},
	{
		srcPath:  filepath.Join(WindowsLocalPath, "k8s-node-setup.psm1"),
		dstPath:  "extra/gce/windows/k8s-node-setup.psm1",
		required: true,
	},
	{
		srcPath:  filepath.Join(WindowsLocalPath, "testonly/install-ssh.psm1"),
		dstPath:  "extra/gce/windows/install-ssh.psm1",
		required: true,
	},
	{
		srcPath:  filepath.Join(WindowsLocalPath, "testonly/user-profile.psm1"),
		dstPath:  "extra/gce/windows/user-profile.psm1",
		required: true,
	},
}

// NewPushBuild can be used to create a new PushBuild instnace.
func NewPushBuild(opts *PushBuildOptions) *PushBuild {
	return &PushBuild{opts}
}

// Push pushes the build by taking the internal options into account.
func (p *PushBuild) Push() error {
	latest, err := p.findLatestVersion()
	if err != nil {
		return errors.Wrap(err, "find latest version")
	}
	logrus.Infof("Latest version is %s", latest)

	client, err := storage.NewClient(context.Background())
	if err != nil {
		return errors.Wrap(err, "fetching gcloud credentials, try running \"gcloud auth application-default login\"")
	}

	bucket := client.Bucket(p.opts.Bucket)
	if bucket == nil {
		return errors.Errorf(
			"identify specified bucket for artifacts: %s", p.opts.Bucket,
		)
	}

	// Check if bucket exists and user has permissions
	requiredGCSPerms := []string{"storage.objects.create"}
	perms, err := bucket.IAM().TestPermissions(
		context.Background(), requiredGCSPerms,
	)
	if err != nil {
		return errors.Wrap(err, "find release artifact bucket")
	}
	if len(perms) != 1 {
		return errors.Errorf(
			"GCP user must have at least %s permissions on bucket %s",
			requiredGCSPerms, p.opts.Bucket,
		)
	}

	if err := p.StageLocalArtifacts(latest); err != nil {
		return errors.Wrap(err, "staging local artifacts")
	}

	// Publish container images
	gcsDest := "devel"
	if p.opts.CI {
		gcsDest = "ci"
	}
	gcsDest += p.opts.GCSSuffix

	if p.opts.Fast {
		gcsDest = filepath.Join(gcsDest, "fast")
	}
	gcsDest = filepath.Join(gcsDest, latest)
	logrus.Infof("GCS destination is %s", gcsDest)

	if err := p.PushReleaseArtifacts(
		filepath.Join(p.opts.BuildDir, GCSStagePath, latest),
		gcsDest,
	); err != nil {
		return errors.Wrap(err, "push release artifacts")
	}

	if p.opts.DockerRegistry != "" {
		images := NewImages()
		normalizedVersion := strings.ReplaceAll(latest, "+", "_")
		if err := images.Publish(
			p.opts.DockerRegistry, normalizedVersion, p.opts.BuildDir,
		); err != nil {
			return errors.Wrap(err, "publish container images")
		}

		if p.opts.ValidateRemoteImageDigests {
			if err := images.Validate(
				p.opts.DockerRegistry, normalizedVersion, p.opts.BuildDir,
			); err != nil {
				return errors.Wrap(err, "validate container images")
			}
		}
	}

	if !p.opts.CI {
		logrus.Info("No CI flag set, we're done")
		return nil
	}

	// Publish release to GCS
	versionMarkers := strings.Split(p.opts.ExtraVersionMarkers, ",")
	if err := NewPublisher().PublishVersion(
		gcsDest, latest, p.opts.BuildDir, p.opts.Bucket, versionMarkers,
		p.opts.PrivateBucket, p.opts.Fast,
	); err != nil {
		return errors.Wrap(err, "publish release")
	}

	return nil
}

func (p *PushBuild) findLatestVersion() (latestVersion string, err error) {
	// Check if latest build uses bazel
	dir, err := os.Getwd()
	if err != nil {
		return "", errors.Wrap(err, "get working directory")
	}

	isBazel, err := BuiltWithBazel(dir)
	if err != nil {
		return "", errors.Wrap(err, "identify if release built with Bazel")
	}

	if isBazel {
		logrus.Info("Using Bazel build version")
		version, err := ReadBazelVersion(dir)
		if err != nil {
			return "", errors.Wrap(err, "read Bazel build version")
		}
		latestVersion = version
	} else {
		logrus.Info("Using Dockerized build version")
		version, err := ReadDockerizedVersion(dir)
		if err != nil {
			return "", errors.Wrap(err, "read Dockerized build version")
		}
		latestVersion = version
	}

	logrus.Infof("Found build version: %s", latestVersion)

	valid, err := IsValidReleaseBuild(latestVersion)
	if err != nil {
		return "", errors.Wrap(
			err, "determine if release build version is valid",
		)
	}
	if !valid {
		return "", errors.Errorf(
			"build version %s is not valid for release", latestVersion,
		)
	}

	if p.opts.CI && IsDirtyBuild(latestVersion) {
		return "", errors.Errorf(
			"refusing to push dirty build %s with --ci flag given",
			latestVersion,
		)
	}

	if p.opts.VersionSuffix != "" {
		latestVersion += "-" + p.opts.VersionSuffix
	}

	return latestVersion, nil
}

// StageLocalArtifacts locally stages the release artifacts
// was releaselib.sh: release::gcs::locally_stage_release_artifacts
func (p *PushBuild) StageLocalArtifacts(version string) error {
	logrus.Info("Staging local artifacts")
	stageDir := filepath.Join(p.opts.BuildDir, GCSStagePath, version)

	logrus.Infof("Cleaning staging dir %s", stageDir)
	if err := util.RemoveAndReplaceDir(stageDir); err != nil {
		return errors.Wrap(err, "remove and replace GCS staging directory")
	}

	// Copy release tarballs to local GCS staging directory for push
	logrus.Info("Copying release tarballs")
	if err := util.CopyDirContentsLocal(
		filepath.Join(p.opts.BuildDir, ReleaseTarsPath), stageDir,
	); err != nil {
		return errors.Wrap(err, "copy source directory into destination")
	}

	// Copy helpful GCP scripts to local GCS staging directory for push
	logrus.Info("Copying GCP stage files")
	if err := p.copyStageFiles(stageDir, gcpStageFiles); err != nil {
		return errors.Wrapf(err, "copy GCP stage files")
	}

	// Copy helpful Windows scripts to local GCS staging directory for push
	logrus.Info("Copying Windows stage files")
	if err := p.copyStageFiles(stageDir, windowsStageFiles); err != nil {
		return errors.Wrapf(err, "copy Windows stage files")
	}

	// Copy the plain binaries to GCS. This is useful for install scripts that
	// download the binaries directly and don't need tars.
	logrus.Info("Copying plain binaries")
	if err := CopyBinaries(
		filepath.Join(p.opts.BuildDir, ReleaseStagePath),
		stageDir,
	); err != nil {
		return errors.Wrap(err, "stage binaries")
	}

	// Write the release checksums
	logrus.Info("Writing checksums")
	if err := WriteChecksums(stageDir); err != nil {
		return errors.Wrap(err, "write checksums")
	}
	return nil
}

// copyStageFiles takes the staging dir and copies each file of `files` into
// it. It also ensures that the base dir exists before copying the file (if the
// file is `required`).
func (p *PushBuild) copyStageFiles(stageDir string, files []stageFile) error {
	for _, file := range files {
		dstPath := filepath.Join(stageDir, file.dstPath)

		if file.required {
			if err := os.MkdirAll(
				filepath.Dir(dstPath), os.FileMode(0o755),
			); err != nil {
				return errors.Wrapf(
					err, "create destination path %s", file.dstPath,
				)
			}
		}

		if err := util.CopyFileLocal(
			filepath.Join(p.opts.BuildDir, file.srcPath),
			dstPath, file.required,
		); err != nil {
			return errors.Wrapf(err, "copy stage file")
		}
	}

	return nil
}

// PushReleaseArtifacts can be used to push local artifacts from the `srcPath`
// to the remote `gcsPath`. The Bucket has to be set via the `Bucket` option.
func (p *PushBuild) PushReleaseArtifacts(srcPath, gcsPath string) error {
	logrus.Info("Pushing release artifacts")

	copyOpts := gcs.DefaultGCSCopyOptions
	copyOpts.NoClobber = pointer.BoolPtr(p.opts.AllowDup)

	return errors.Wrap(gcs.CopyToGCS(
		srcPath, filepath.Join(p.opts.Bucket, gcsPath), copyOpts,
	), "copy artifacts to GCS")
}
