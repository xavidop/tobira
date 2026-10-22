package main

import (
	"context"
	"errors"
	"testing"

	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/cli"
	"github.com/xavidop/tobira/internal/config"
)

// The binary wires the Genkit embedder: built lazily, so no key and no
// network are needed to construct it.
func TestNewEmbedderBuildsGenkit(t *testing.T) {
	scrubGenkitEnv(t)
	e, err := newEmbedder(context.Background(), config.AI{Provider: "ollama", Embedder: "ollama/nomic-embed-text"}, cli.EmbedderDeps{})
	if err != nil || e == nil {
		t.Fatalf("newEmbedder = %v, %v", e, err)
	}
}

// No ai.embedder and a refused one both give a nil interface, never a typed
// nil a session would treat as an embedder.
func TestNewEmbedderNilInterface(t *testing.T) {
	scrubGenkitEnv(t)
	e, err := newEmbedder(context.Background(), config.AI{Provider: "ollama"}, cli.EmbedderDeps{})
	if err != nil || e != nil {
		t.Fatalf("empty: %#v, %v", e, err)
	}
	e, err = newEmbedder(context.Background(), config.AI{Provider: "anthropic", Embedder: "voyage"}, cli.EmbedderDeps{})
	if !errors.Is(err, assistant.ErrProvider) || e != nil {
		t.Fatalf("anthropic: %#v, %v", e, err)
	}
}
