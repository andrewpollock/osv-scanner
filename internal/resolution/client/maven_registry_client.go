package client

import (
	"context"
	"encoding/gob"
	"fmt"
	"os"
	"slices"
	"strings"

	"deps.dev/util/maven"
	"deps.dev/util/resolve"
	"deps.dev/util/resolve/version"
	"deps.dev/util/semver"
	"github.com/google/osv-scanner/internal/resolution/datasource"
	mavenutil "github.com/google/osv-scanner/internal/utility/maven"
)

const mavenRegistryCacheExt = ".resolve.maven"

type MavenRegistryClient struct {
	api *datasource.MavenRegistryAPIClient
}

func NewMavenRegistryClient(registry string) (*MavenRegistryClient, error) {
	return &MavenRegistryClient{
		api: datasource.NewMavenRegistryAPIClient(registry),
	}, nil
}

func (c *MavenRegistryClient) Version(ctx context.Context, vk resolve.VersionKey) (resolve.Version, error) {
	g, a, found := strings.Cut(vk.Name, ":")
	if !found {
		return resolve.Version{}, fmt.Errorf("invalid Maven package name %s", vk.Name)
	}
	proj, err := c.api.GetProject(ctx, g, a, vk.Version)
	if err != nil {
		return resolve.Version{}, err
	}

	regs := make([]string, len(proj.Repositories))
	// Repositories are served as dependency registries.
	// https://github.com/google/deps.dev/blob/main/util/resolve/api.go#L106
	for i, repo := range proj.Repositories {
		regs[i] = "dep:" + string(repo.URL)
	}
	var attr version.AttrSet
	if len(regs) > 0 {
		attr.SetAttr(version.Registries, strings.Join(regs, "|"))
	}

	return resolve.Version{VersionKey: vk, AttrSet: attr}, nil
}

// TODO: we should also include versions not listed in the metadata file
// There exist versions in the repository but not listed in the metada file,
// for example version 20030203.000550 of package commons-io:commons-io
// https://repo1.maven.org/maven2/commons-io/commons-io/20030203.000550/.
// A package may depend on such version if a soft requirement of this version
// is declared.
// We need to find out if there are such versions and include them in the
// returned versions.
func (c *MavenRegistryClient) Versions(ctx context.Context, pk resolve.PackageKey) ([]resolve.Version, error) {
	if pk.System != resolve.Maven {
		return nil, fmt.Errorf("wrong system: %v", pk.System)
	}

	g, a, found := strings.Cut(pk.Name, ":")
	if !found {
		return nil, fmt.Errorf("invalid Maven package name %s", pk.Name)
	}
	metadata, err := c.api.GetArtifactMetadata(ctx, g, a)
	if err != nil {
		return nil, err
	}

	vks := make([]resolve.Version, len(metadata.Versioning.Versions))
	for i, v := range metadata.Versioning.Versions {
		vks[i] = resolve.Version{
			VersionKey: resolve.VersionKey{
				PackageKey:  pk,
				Version:     string(v),
				VersionType: resolve.Concrete,
			}}
	}
	slices.SortFunc(vks, func(a, b resolve.Version) int { return semver.Maven.Compare(a.Version, b.Version) })

	return vks, nil
}

func (c *MavenRegistryClient) Requirements(ctx context.Context, vk resolve.VersionKey) ([]resolve.RequirementVersion, error) {
	if vk.System != resolve.Maven {
		return nil, fmt.Errorf("wrong system: %v", vk.System)
	}

	g, a, found := strings.Cut(vk.Name, ":")
	if !found {
		return nil, fmt.Errorf("invalid Maven package name %s", vk.Name)
	}
	proj, err := c.api.GetProject(ctx, g, a, vk.Version)
	if err != nil {
		return nil, err
	}

	// Only merge default profiles by passing empty JDK and OS information.
	if err := proj.MergeProfiles("", maven.ActivationOS{}); err != nil {
		return nil, err
	}
	// We need to merge parents for potential dependencies in parents.
	if err := mavenutil.MergeParents(ctx, c.api, &proj, proj.Parent, 1, "", false); err != nil {
		return nil, err
	}
	proj.ProcessDependencies(func(groupID, artifactID, version maven.String) (maven.DependencyManagement, error) {
		root := maven.Parent{ProjectKey: maven.ProjectKey{GroupID: groupID, ArtifactID: artifactID, Version: version}}
		var result maven.Project
		if err := mavenutil.MergeParents(ctx, c.api, &result, root, 0, "", false); err != nil {
			return maven.DependencyManagement{}, err
		}

		return result.DependencyManagement, nil
	})

	reqs := make([]resolve.RequirementVersion, 0, len(proj.Dependencies))
	for _, d := range proj.Dependencies {
		reqs = append(reqs, resolve.RequirementVersion{
			VersionKey: resolve.VersionKey{
				PackageKey: resolve.PackageKey{
					System: resolve.Maven,
					Name:   d.Name(),
				},
				VersionType: resolve.Requirement,
				Version:     string(d.Version),
			},
			Type: resolve.MavenDepType(d, ""),
		})
	}

	return reqs, nil
}

func (c *MavenRegistryClient) MatchingVersions(ctx context.Context, vk resolve.VersionKey) ([]resolve.Version, error) {
	if vk.System != resolve.Maven {
		return nil, fmt.Errorf("wrong system: %v", vk.System)
	}

	versions, err := c.Versions(ctx, vk.PackageKey)
	if err != nil {
		return nil, err
	}

	return resolve.MatchRequirement(vk, versions), nil
}

func (c *MavenRegistryClient) WriteCache(path string) error {
	f, err := os.Create(path + mavenRegistryCacheExt)
	if err != nil {
		return err
	}
	defer f.Close()

	return gob.NewEncoder(f).Encode(c.api)
}

func (c *MavenRegistryClient) LoadCache(path string) error {
	f, err := os.Open(path + mavenRegistryCacheExt)
	if err != nil {
		return err
	}
	defer f.Close()

	return gob.NewDecoder(f).Decode(&c.api)
}
