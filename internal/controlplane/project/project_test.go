package project

import (
	"context"
	"errors"
	"testing"
)

func TestServiceValidatesCreateBeforeStore(t *testing.T) {
	store := &fakeStore{}
	service := NewService(store)

	_, err := service.Create(context.Background(), "owner-1", "Gateway", "Bad Slug")
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("Create error = %v, want validation error", err)
	}
	if store.createCalls != 0 {
		t.Fatalf("CreateProject calls = %d, want 0", store.createCalls)
	}
}

func TestServicePassesOwnerToEveryStoreOperation(t *testing.T) {
	store := &fakeStore{}
	service := NewService(store)

	if _, err := service.Create(context.Background(), "owner-1", " Gateway ", "gateway"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := service.List(context.Background(), "owner-1"); err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, err := service.Get(context.Background(), "owner-1", "project-1"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	name := "Renamed"
	if _, err := service.Update(context.Background(), "owner-1", "project-1", UpdateParams{Name: &name}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	for operation, owner := range store.owners {
		if owner != "owner-1" {
			t.Fatalf("%s owner = %q, want owner-1", operation, owner)
		}
	}
}

type fakeStore struct {
	createCalls int
	owners      map[string]string
}

func (s *fakeStore) record(operation, owner string) {
	if s.owners == nil {
		s.owners = make(map[string]string)
	}
	s.owners[operation] = owner
}

func (s *fakeStore) CreateProject(_ context.Context, params CreateParams) (Project, error) {
	s.createCalls++
	s.record("create", params.OwnerUserID)
	return Project{Name: params.Name, Slug: params.Slug, OwnerUserID: params.OwnerUserID}, nil
}

func (s *fakeStore) ListProjects(_ context.Context, owner string) ([]Project, error) {
	s.record("list", owner)
	return nil, nil
}

func (s *fakeStore) GetProject(_ context.Context, owner, _ string) (Project, error) {
	s.record("get", owner)
	return Project{}, nil
}

func (s *fakeStore) UpdateProject(_ context.Context, owner, _ string, _ UpdateParams) (Project, error) {
	s.record("update", owner)
	return Project{}, nil
}
