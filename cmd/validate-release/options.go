package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/blang/semver/v4"
	"github.com/google/go-github/v60/github"
	"github.com/sirupsen/logrus"
	"sigs.k8s.io/yaml"

	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	org  = "openshift"
	repo = "cincinnati-graph-data"
)

type Options struct {
	//PULL_BASE_REF
	PullBaseRef string
	//PULL_NUMBER
	PullNumber int
}

func GetOptionsFromEnv() (*Options, error) {
	pullBaseRef := os.Getenv("PULL_BASE_REF")
	if pullBaseRef == "" {
		return nil, fmt.Errorf("PULL_BASE_REF environment variable not set")
	}
	ret := &Options{
		PullBaseRef: pullBaseRef,
		PullNumber:  -1,
	}

	if pullNumber := os.Getenv("PULL_NUMBER"); pullNumber != "" {
		i, err := strconv.Atoi(pullNumber)
		if err != nil {
			return nil, fmt.Errorf("failed to convert PULL_NUMBER environment variable %s to int", pullNumber)
		}
		ret.PullNumber = i
	} else {
		logrus.Info("PULL_NUMBER environment variable not set: likely running in a Batch job")
	}
	return ret, nil
}

type Candidate struct {
	Versions []string
}

func (o *Options) Run(ctx context.Context) error {
	logrus.WithField("pullBaseRef", o.PullBaseRef).WithField("pullNumber", o.PullNumber).Info("Run options")
	githubClient := github.NewClient(nil)
	path := "internal-channels/candidate.yaml"
	opts := &github.RepositoryContentGetOptions{
		Ref: o.PullBaseRef,
	}

	fileContent, _, _, err := githubClient.Repositories.GetContents(ctx, org, repo, path, opts)
	if err != nil {
		return fmt.Errorf("failed to get contents: %w", err)
	}
	content, err := fileContent.GetContent()
	if err != nil {
		return fmt.Errorf("failed to get file content: %w", err)
	}

	var candidate Candidate
	if err := yaml.Unmarshal([]byte(content), &candidate); err != nil {
		return fmt.Errorf("failed to unmarshal: %w", err)
	}

	currentVersions := candidate.Versions

	var raw []byte
	if _, err := os.Stat(path); err == nil {
		logrus.WithField("path", path).Info("File exists on the local disk")
		raw, err = os.ReadFile(path)
		if err != nil {
			logrus.WithError(err).Error("Failed to read file")
		}
	}

	if raw == nil && o.PullNumber != -1 {
		logrus.WithField("path", path).Info("Fetching the file from GitHub")
		pr, _, err := githubClient.PullRequests.Get(ctx, org, repo, o.PullNumber)
		if err != nil {
			return fmt.Errorf("failed to get pull request: %w", err)
		}

		prHeadSHA := pr.Head.GetSHA()
		opts = &github.RepositoryContentGetOptions{
			Ref: prHeadSHA,
		}

		fileContent, _, _, err = githubClient.Repositories.GetContents(ctx, org, repo, path, opts)
		if err != nil {
			return fmt.Errorf("failed to get contents: %w", err)
		}

		content, err = fileContent.GetContent()
		if err != nil {
			return fmt.Errorf("failed to get file content: %w", err)
		}
		raw = []byte(content)
	}

	if raw == nil {
		return fmt.Errorf("failed to get the content to merge: %s", path)
	}

	var pullCandidate Candidate
	if err := yaml.Unmarshal(raw, &pullCandidate); err != nil {
		return fmt.Errorf("failed to unmarshal: %w", err)
	}

	pullVersions := pullCandidate.Versions

	diff := sets.New[string](pullVersions...).Difference(sets.New[string](currentVersions...))
	if diff.Len() == 0 {
		logrus.Info("No new versions found")
	}

	for versionStr := range diff {
		logrus.WithField("version", versionStr).Info("Check version")
		version, err := semver.Parse(versionStr)
		if err != nil {
			return fmt.Errorf("failed to parse version: %w", err)
		}
		// For 4.13 and earlier, only check x86_64 as multi-arch support was limited
		// For 4.14+, check all architectures
		archSuffixes := []ArchTagSuffix{ArchTagSuffixAMD64, ArchTagSuffixARM64, ArchTagSuffixS390x, ArchTagSuffixPPC64LE}
		if version.Major == 4 && version.Minor <= 13 {
			archSuffixes = []ArchTagSuffix{ArchTagSuffixAMD64}
		}
		for _, suffix := range archSuffixes {
			image := fmt.Sprintf("quay.io/openshift-release-dev/ocp-release:%s-%s", versionStr, string(suffix))
			c, err := getCincinnatiMetadata(image)
			if err != nil {
				return fmt.Errorf("failed to get image metadata: %w", err)
			}
			if len(c.Previous) == 0 {
				return fmt.Errorf("no previous found for %s", image)
			}
			for i, p := range c.Previous {
				pVersion, err := semver.Parse(p)
				if err != nil {
					return fmt.Errorf("error parsing previous version %s of image %s: %w", p, image, err)
				}
				if !pVersion.LT(version) {
					logrus.WithField("index", i).WithField("p", p).
						WithField("version", versionStr).
						Error("previous is not smaller")
					return fmt.Errorf("image's previous are not always smaller: %s", image)
				}
			}
		}
	}
	return nil
}

type ArchTagSuffix string

const (
	ArchTagSuffixAMD64   ArchTagSuffix = "x86_64"
	ArchTagSuffixARM64   ArchTagSuffix = "aarch64"
	ArchTagSuffixS390x   ArchTagSuffix = "s390x"
	ArchTagSuffixPPC64LE ArchTagSuffix = "ppc64le"
)
