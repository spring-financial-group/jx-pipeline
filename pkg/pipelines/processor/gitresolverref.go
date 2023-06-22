package processor

import (
	"context"
	"fmt"
	"github.com/jenkins-x-plugins/jx-gitops/pkg/apis/gitops/v1alpha1"
	"github.com/jenkins-x/go-scm/scm"
	"github.com/jenkins-x/jx-helpers/v3/pkg/files"
	"github.com/jenkins-x/jx-helpers/v3/pkg/scmhelpers"
	"github.com/pkg/errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/jenkins-x/jx-helpers/v3/pkg/gitclient/giturl"
)

type GitRefResolver struct {
	reversionOverride string

	isPublicRepositories map[string]bool
	scms                 map[string]*scm.Client
	catalogs             []v1alpha1.PipelineCatalogSource
}

func NewGitRefResolver(reversionOverride string, catalogs []v1alpha1.PipelineCatalogSource) *GitRefResolver {
	return &GitRefResolver{
		scms:                 map[string]*scm.Client{},
		reversionOverride:    reversionOverride,
		isPublicRepositories: map[string]bool{},
		catalogs:             catalogs,
	}
}

// NewRefFromUsesImage parses an image name of "uses:image" syntax as a GitRef
func (g *GitRefResolver) NewRefFromUsesImage(image, stepName string) (*GitRef, error) {
	if !strings.HasPrefix(image, "uses:") {
		return nil, nil
	}

	fullURL, imageRevision := g.getURLAndRevisionFromImage(image)

	catalogRepo, catalogRevision, err := g.getCatalogRepositoryAndRevisionFromURL(fullURL)
	if err != nil {
		return nil, err
	}
	if catalogRepo == nil {
		// Todo: handle pipelines that are not in a defined catalog
		return nil, errors.Errorf("unable to find catalog repository for %s", fullURL)
	}

	pathInRepo := strings.TrimPrefix(fullURL, catalogRepo.URL)
	if stepName != "" {
		pathInRepo = filepath.Join(strings.TrimSuffix(pathInRepo, ".yaml"), stepName) + ".yaml"
	}

	scmClient, err := g.getSCMClient(catalogRepo.HostURL())
	if err != nil {
		return nil, err
	}

	isPublic, err := g.isRepositoryPublic(scmClient, catalogRepo.Organisation, catalogRepo.Name)
	if err != nil {
		return nil, err
	}

	exists, err := files.FileExists(filepath.Join(".", pathInRepo))
	if err != nil {
		return nil, errors.Wrap(err, "failed to check if file exists in current repository")
	}

	var resolvedFile []byte
	if exists {
		// If we're currently in the repository that contains the catalog then we can just read the file from the local filesystem
		resolvedFile, err = g.getResolvedFileFromLocal(filepath.Join(".", pathInRepo))
		if err != nil {
			return nil, err
		}
	} else {
		// Otherwise, we need to read the file from the remote repository using the ref specified in the catalog source
		resolvedFile, err = g.getResolvedFileFromSCM(scmClient, catalogRepo.Organisation, catalogRepo.Name, pathInRepo, catalogRevision)
		if err != nil {
			return nil, err
		}
	}

	return &GitRef{
		URL:          catalogRepo.URL,
		Org:          catalogRepo.Organisation,
		Repository:   catalogRepo.Name,
		Revision:     imageRevision,
		StepName:     stepName,
		PathInRepo:   pathInRepo,
		IsPublic:     isPublic,
		ResolvedFile: resolvedFile,
	}, nil
}

func (g *GitRefResolver) getURLAndRevisionFromImage(image string) (string, string) {
	image = strings.TrimPrefix(image, "uses:")
	split := strings.Split(image, "@")

	fullURL, revision := split[0], split[1]
	if g.reversionOverride != "" {
		revision = g.reversionOverride
	}
	if !strings.HasPrefix(fullURL, "https://") {
		// If the URL doesn't start with https:// then we can assume that it is a GitHub URL and so prepend that domain
		fullURL = fmt.Sprintf("%s/%s", giturl.GitHubURL, fullURL)
	}
	return fullURL, revision
}

func (g *GitRefResolver) getCatalogRepositoryAndRevisionFromURL(fullURL string) (*giturl.GitRepository, string, error) {
	for _, catalog := range g.catalogs {
		if strings.HasPrefix(fullURL, catalog.GitURL) {
			repo, err := giturl.ParseGitURL(catalog.GitURL)
			if err != nil {
				return nil, "", errors.Wrap(err, "failed to parse git url")
			}
			return repo, catalog.GitRef, nil
		}
	}
	return nil, "", nil
}

func (g *GitRefResolver) getSCMClient(gitServerURL string) (*scm.Client, error) {
	scmClient, ok := g.scms[gitServerURL]
	if !ok {
		var err error
		scmClient, _, err = scmhelpers.NewScmClient("", gitServerURL, "", false)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create scm client")
		}
		g.scms[gitServerURL] = scmClient
	}
	return scmClient, nil
}

func (g *GitRefResolver) isRepositoryPublic(scm *scm.Client, org, repoName string) (bool, error) {
	path := filepath.Join(org, repoName)
	isPublic, ok := g.isPublicRepositories[path]
	if !ok {
		var err error
		repo, _, err := scm.Repositories.Find(context.Background(), path)
		if err != nil {
			return false, errors.Wrap(err, "failed to find repository")
		}
		isPublic = repo != nil && !repo.Private
		g.isPublicRepositories[path] = isPublic
	}
	return isPublic, nil
}

func (g *GitRefResolver) getResolvedFileFromLocal(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (g *GitRefResolver) getResolvedFileFromSCM(scm *scm.Client, owner, repoName, path, ref string) ([]byte, error) {
	content, _, err := scm.Contents.Find(context.Background(), filepath.Join(owner, repoName), path, ref)
	if err != nil {
		return nil, errors.Wrap(err, "failed to find file in repository")
	}
	return content.Data, nil
}
