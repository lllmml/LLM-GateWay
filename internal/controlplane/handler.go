package controlplane

import (
	"net/http"

	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/project"
)

func NewHandler(auth *AuthHandler, projects project.Store) http.Handler {
	root := http.NewServeMux()
	auth.Register(root)

	projectMux := http.NewServeMux()
	project.NewHandler(project.NewService(projects), func(request *http.Request) (string, bool) {
		user, ok := UserFromContext(request.Context())
		return user.ID, ok
	}).Register(projectMux)

	protectedProjects := auth.RequireSession(auth.RequireSameOrigin(projectMux))
	root.Handle("/api/v1/projects", protectedProjects)
	root.Handle("/api/v1/projects/", protectedProjects)
	return root
}
