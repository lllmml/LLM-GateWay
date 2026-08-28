package controlplane

import (
	"fmt"
	"net/http"

	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/apikey"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/project"
)

func NewHandler(auth *AuthHandler, projects project.Store, keys apikey.Store, virtualKeyPepper []byte) (http.Handler, error) {
	root := http.NewServeMux()
	auth.Register(root)

	projectMux := http.NewServeMux()
	currentUserID := func(request *http.Request) (string, bool) {
		user, ok := UserFromContext(request.Context())
		return user.ID, ok
	}
	project.NewHandler(project.NewService(projects), currentUserID).Register(projectMux)

	keyService, err := apikey.NewService(keys, virtualKeyPepper)
	if err != nil {
		return nil, fmt.Errorf("configure virtual API keys: %w", err)
	}
	apikey.NewHandler(keyService, currentUserID).Register(projectMux)

	protectedProjects := auth.RequireSession(auth.RequireSameOrigin(projectMux))
	root.Handle("/api/v1/projects", protectedProjects)
	root.Handle("/api/v1/projects/", protectedProjects)
	return root, nil
}
