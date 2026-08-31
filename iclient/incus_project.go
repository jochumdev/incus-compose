package iclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/lxc/incus/v7/shared/api"
)

// incusProjectsPath is the collection every project call hangs off.
const incusProjectsPath = "/projects"

// GetProject returns one project and its ETag.
func (c *Connection) GetProject(ctx context.Context, name string) (*api.Project, string, error) {
	project := api.Project{}

	etag, err := c.getStruct(ctx, "", incusProjectsPath+"/"+url.PathEscape(name), nil, &project)
	if err != nil {
		return nil, "", err
	}

	return &project, etag, nil
}

// GetProjectNames returns the names of every project the certificate may see.
func (c *Connection) GetProjectNames(ctx context.Context) ([]string, error) {
	// Without recursion the collection is a list of resource URLs.
	uris := []string{}

	_, err := c.getStruct(ctx, "", incusProjectsPath, nil, &uris)
	if err != nil {
		return nil, err
	}

	return resourceNames(incusProjectsPath, uris)
}

// GetProjects returns every project the certificate may see.
func (c *Connection) GetProjects(ctx context.Context) ([]api.Project, error) {
	projects := []api.Project{}

	query := url.Values{}
	query.Set("recursion", "1")

	_, err := c.getStruct(ctx, "", incusProjectsPath, query, &projects)
	if err != nil {
		return nil, err
	}

	return projects, nil
}

// CreateProject adds a project.
func (c *Connection) CreateProject(ctx context.Context, project api.ProjectsPost) error {
	_, _, err := c.do(ctx, "", http.MethodPost, incusProjectsPath, nil, project, "")

	return err
}

// UpdateProject replaces a project's configuration.
func (c *Connection) UpdateProject(ctx context.Context, name string, project api.ProjectPut, etag string) error {
	_, _, err := c.do(ctx, "", http.MethodPut, incusProjectsPath+"/"+url.PathEscape(name), nil, project, etag)

	return err
}

// RenameProject renames a project and follows the operation.
func (c *Connection) RenameProject(ctx context.Context, name string, project api.ProjectPost) (<-chan api.Operation, error) {
	return c.asyncOperation(ctx, "", http.MethodPost, incusProjectsPath+"/"+url.PathEscape(name), project, "")
}

// DeleteProject removes a project, which Incus refuses while it holds anything
// unless args.Force says to take that with it.
func (c *Connection) DeleteProject(ctx context.Context, name string, args *DeleteProjectArgs) error {
	if args == nil {
		args = &DeleteProjectArgs{}
	}

	var query url.Values

	if args.Force {
		// As a query value, not glued onto the path: do appends the project.
		query = url.Values{}
		query.Set("force", "1")
	}

	_, _, err := c.do(ctx, "", http.MethodDelete, incusProjectsPath+"/"+url.PathEscape(name), query, nil, "")

	return err
}
