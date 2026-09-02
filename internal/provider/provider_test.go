package provider

import (
	"context"
	"testing"
)

type fakeClient struct {
	name string
}

func (fakeClient) CompleteChat(context.Context, ChatRequest, Credential) (Result, error) {
	return Result{}, nil
}

func TestParseModelRequiresProviderNamespace(t *testing.T) {
	got, err := ParseModel("openai/gpt-test")
	if err != nil {
		t.Fatalf("parse model: %v", err)
	}
	if got.Provider != OpenAI || got.Model != "gpt-test" {
		t.Fatalf("model ref = %+v", got)
	}
	for _, raw := range []string{"gpt-test", "openai/", "/gpt-test", "openai/foo/bar"} {
		if _, err := ParseModel(raw); err == nil {
			t.Fatalf("ParseModel(%q) returned nil error", raw)
		}
	}
}

func TestParseModelAcceptsDynamicProviderNamespace(t *testing.T) {
	got, err := ParseModel("ollama/llama3")
	if err != nil {
		t.Fatalf("parse model: %v", err)
	}
	if got.Provider != Name("ollama") || got.Model != "llama3" {
		t.Fatalf("model ref = %+v", got)
	}
}

func TestRegistryLookup(t *testing.T) {
	client := fakeClient{name: "openai"}
	registry, err := NewRegistry(map[Name]Client{OpenAI: client})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}

	got, ok := registry.Lookup(OpenAI)
	if !ok {
		t.Fatal("Lookup(openai) returned miss")
	}
	if got != client {
		t.Fatalf("Lookup(openai) = %#v, want %#v", got, client)
	}

	if got, ok := registry.Lookup(Anthropic); ok || got != nil {
		t.Fatalf("Lookup(anthropic) = %#v, %t; want nil, false", got, ok)
	}
}

func TestNewRegistryValidatesClients(t *testing.T) {
	if _, err := NewRegistry(nil); err == nil {
		t.Fatal("NewRegistry(nil) returned nil error")
	}
	if _, err := NewRegistry(map[Name]Client{}); err == nil {
		t.Fatal("NewRegistry(empty) returned nil error")
	}
	if _, err := NewRegistry(map[Name]Client{OpenAI: nil}); err == nil {
		t.Fatal("NewRegistry(nil client) returned nil error")
	}
}

func TestNewRegistryCopiesClientMap(t *testing.T) {
	openAIClient := fakeClient{name: "openai"}
	anthropicClient := fakeClient{name: "anthropic"}
	clients := map[Name]Client{OpenAI: openAIClient}
	registry, err := NewRegistry(clients)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}

	clients[OpenAI] = anthropicClient
	clients[Anthropic] = anthropicClient

	got, ok := registry.Lookup(OpenAI)
	if !ok {
		t.Fatal("Lookup(openai) returned miss")
	}
	if got != openAIClient {
		t.Fatalf("Lookup(openai) = %#v, want original client %#v", got, openAIClient)
	}
	if got, ok := registry.Lookup(Anthropic); ok || got != nil {
		t.Fatalf("Lookup(anthropic) = %#v, %t; want nil, false", got, ok)
	}
}
