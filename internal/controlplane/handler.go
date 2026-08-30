package controlplane

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/apikey"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/credential"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/project"
	"github.com/lllmml/production-go-llm-gateway/internal/security"
)

func NewHandler(
	auth *AuthHandler,
	projects project.Store,
	keys apikey.Store,
	virtualKeyPepper []byte,
	credentials credential.Store,
	credentialCipher *security.CredentialCipher,
) (http.Handler, error) {
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

	if credentialCipher == nil {
		return nil, errors.New("provider credential cipher is required")
	}
	sealCredential := func(secret []byte) (credential.SealedSecret, error) {
		encrypted, err := credentialCipher.Encrypt(secret)
		if err != nil {
			return credential.SealedSecret{}, err
		}
		return credential.SealedSecret{
			Ciphertext: encrypted.Ciphertext,
			Nonce:      encrypted.Nonce,
			KeyVersion: encrypted.KeyVersion,
		}, nil
	}
	credentialService, err := credential.NewService(credentials, sealCredential)
	if err != nil {
		return nil, fmt.Errorf("configure provider credentials: %w", err)
	}
	credential.NewHandler(credentialService, currentUserID).Register(projectMux)

	protectedProjects := auth.RequireSession(auth.RequireSameOrigin(projectMux))
	root.Handle("/api/v1/projects", protectedProjects)
	root.Handle("/api/v1/projects/", protectedProjects)
	return root, nil
}
