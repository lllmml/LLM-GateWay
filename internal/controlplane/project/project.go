package project

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
)

var (
	ErrNotFound = errors.New("project not found")
	ErrConflict = errors.New("project already exists")
)

const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

type Project struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	OwnerUserID string    `json:"-"`
}

type CreateParams struct {
	OwnerUserID string
	Name        string
	Slug        string
}

type UpdateParams struct {
	Name   *string
	Slug   *string
	Status *string
}

type Store interface {
	CreateProject(context.Context, CreateParams) (Project, error)
	ListProjects(context.Context, string) ([]Project, error)
	GetProject(context.Context, string, string) (Project, error)
	UpdateProject(context.Context, string, string, UpdateParams) (Project, error)
}

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

func (s *Service) Create(ctx context.Context, ownerUserID, name, slug string) (Project, error) {
	name, slug, err := validateNameAndSlug(name, slug)
	if err != nil {
		return Project{}, err
	}
	return s.store.CreateProject(ctx, CreateParams{
		OwnerUserID: ownerUserID,
		Name:        name,
		Slug:        slug,
	})
}

func (s *Service) List(ctx context.Context, ownerUserID string) ([]Project, error) {
	return s.store.ListProjects(ctx, ownerUserID)
}

func (s *Service) Get(ctx context.Context, ownerUserID, projectID string) (Project, error) {
	return s.store.GetProject(ctx, ownerUserID, projectID)
}

func (s *Service) Update(ctx context.Context, ownerUserID, projectID string, params UpdateParams) (Project, error) {
	if params.Name == nil && params.Slug == nil && params.Status == nil {
		return Project{}, &ValidationError{Field: "body", Message: "at least one field is required"}
	}
	if params.Name != nil {
		name, err := validateName(*params.Name)
		if err != nil {
			return Project{}, err
		}
		params.Name = &name
	}
	if params.Slug != nil {
		slug, err := validateSlug(*params.Slug)
		if err != nil {
			return Project{}, err
		}
		params.Slug = &slug
	}
	if params.Status != nil && *params.Status != StatusActive && *params.Status != StatusDisabled {
		return Project{}, &ValidationError{Field: "status", Message: "must be active or disabled"}
	}
	return s.store.UpdateProject(ctx, ownerUserID, projectID, params)
}

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return e.Field + " " + e.Message
}

func validateNameAndSlug(name, slug string) (string, string, error) {
	name, err := validateName(name)
	if err != nil {
		return "", "", err
	}
	slug, err = validateSlug(slug)
	if err != nil {
		return "", "", err
	}
	return name, slug, nil
}

func validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return "", &ValidationError{Field: "name", Message: "must contain 1 to 100 bytes"}
	}
	return name, nil
}

func validateSlug(slug string) (string, error) {
	slug = strings.TrimSpace(slug)
	if len(slug) > 63 || !slugPattern.MatchString(slug) {
		return "", &ValidationError{Field: "slug", Message: "must be 1 to 63 lowercase letters, digits, or single hyphen-separated segments"}
	}
	return slug, nil
}
