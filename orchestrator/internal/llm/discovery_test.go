package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestDiscoverModelsCloudGeminiAndOpenAI(t *testing.T) {
	t.Run("gemini_cloud_catalog", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/models" {
				http.NotFound(w, r)
				return
			}
			gotAuth = r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{
					{"id": "gemini-2.5-flash"},
					{"id": "gemini-2.5-pro"},
					{"id": "text-embedding-004"},
					{"id": "imagen-3.0"},
				},
			})
		}))
		defer srv.Close()

		resolved, err := DiscoverModels(context.Background(), srv.URL, "gemini-key-123", ModelPreferences{
			Embedding: "auto",
			Triage:    "auto",
			Coding:    "auto",
		})
		if err != nil {
			t.Fatalf("DiscoverModels failed: %v", err)
		}
		if gotAuth != "Bearer gemini-key-123" {
			t.Fatalf("expected Bearer auth header, got %q", gotAuth)
		}
		if resolved.Embedding != "text-embedding-004" {
			t.Fatalf("expected embedding text-embedding-004, got %q", resolved.Embedding)
		}
		if resolved.Triage != "gemini-2.5-flash" {
			t.Fatalf("expected triage gemini-2.5-flash, got %q", resolved.Triage)
		}
		if resolved.Coding != "gemini-2.5-pro" {
			t.Fatalf("expected coding gemini-2.5-pro, got %q", resolved.Coding)
		}
	})

	t.Run("openai_cloud_catalog", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/models" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{
					{"id": "whisper-1"},
					{"id": "text-embedding-3-small"},
					{"id": "text-embedding-3-large"},
					{"id": "gpt-4o-mini"},
					{"id": "gpt-4o"},
				},
			})
		}))
		defer srv.Close()

		resolved, err := DiscoverModels(context.Background(), srv.URL, "sk-test", ModelPreferences{
			Embedding: "auto",
			Triage:    "auto",
			Coding:    "auto",
		})
		if err != nil {
			t.Fatalf("DiscoverModels failed: %v", err)
		}
		if resolved.Embedding != "text-embedding-3-small" {
			t.Fatalf("expected embedding text-embedding-3-small, got %q", resolved.Embedding)
		}
		if resolved.Triage != "gpt-4o-mini" {
			t.Fatalf("expected triage gpt-4o-mini, got %q", resolved.Triage)
		}
		if resolved.Coding != "gpt-4o" {
			t.Fatalf("expected coding gpt-4o, got %q", resolved.Coding)
		}
	})

	t.Run("partial_override_preserves_explicit_model", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{
					{"id": "text-embedding-004"},
					{"id": "gemini-2.5-flash"},
					{"id": "gemini-2.5-pro"},
				},
			})
		}))
		defer srv.Close()

		resolved, err := DiscoverModels(context.Background(), srv.URL, "key", ModelPreferences{
			Embedding: "auto",
			Triage:    "auto",
			Coding:    "claude-3-7-sonnet",
		})
		if err != nil {
			t.Fatalf("DiscoverModels failed: %v", err)
		}
		if resolved.Coding != "claude-3-7-sonnet" {
			t.Fatalf("expected explicit coding model preserved, got %q", resolved.Coding)
		}
		if resolved.Embedding != "text-embedding-004" || resolved.Triage != "gemini-2.5-flash" {
			t.Fatalf("unexpected resolved roles: %+v", resolved)
		}
	})
}

func TestDiscoverModelsLocalOllama(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]string{
				{"id": "nomic-embed-text"},
				{"id": "qwen2.5-coder:7b"},
				{"id": "llama3.2:3b"},
			},
		})
	}))
	defer srv.Close()

	resolved, err := DiscoverModels(context.Background(), srv.URL, "ollama", ModelPreferences{
		Embedding: "auto",
		Triage:    "auto",
		Coding:    "auto",
	})
	if err != nil {
		t.Fatalf("DiscoverModels Ollama failed: %v", err)
	}
	if resolved.Embedding != "nomic-embed-text" {
		t.Fatalf("expected embedding nomic-embed-text, got %q", resolved.Embedding)
	}
	if resolved.Triage != "llama3.2:3b" {
		t.Fatalf("expected triage llama3.2:3b, got %q", resolved.Triage)
	}
	if resolved.Coding != "qwen2.5-coder:7b" {
		t.Fatalf("expected coding qwen2.5-coder:7b, got %q", resolved.Coding)
	}
}

func TestEmbedSendsDimensions768AndFallbackOnUnsupported(t *testing.T) {
	t.Run("sends_dimensions_768", func(t *testing.T) {
		var captured embeddingRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/embeddings" {
				http.NotFound(w, r)
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Fatalf("decode embedding request: %v", err)
			}
			vec := make([]float32, 768)
			vec[0] = 0.75
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"embedding": vec}},
			})
		}))
		defer srv.Close()

		client := NewClient(srv.URL, "sk-test", "gpt-4o-mini", "gpt-4o", "text-embedding-3-small", 768)
		vec, err := client.Embed(context.Background(), "checkout button throws 500")
		if err != nil {
			t.Fatalf("Embed failed: %v", err)
		}
		if captured.Dimensions != 768 {
			t.Fatalf("expected dimensions=768 in JSON payload, got %d", captured.Dimensions)
		}
		if captured.Model != "text-embedding-3-small" {
			t.Fatalf("expected model text-embedding-3-small, got %q", captured.Model)
		}
		if len(vec) != 768 {
			t.Fatalf("expected 768-dim vector, got %d", len(vec))
		}
	})

	t.Run("retries_without_dimensions_when_endpoint_rejects_param", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			var raw map[string]any
			_ = json.NewDecoder(r.Body).Decode(&raw)
			if _, hasDim := raw["dimensions"]; hasDim {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"unknown parameter: dimensions"}}`))
				return
			}
			if n != 2 {
				t.Errorf("expected retry on second call, got call #%d", n)
			}
			vec := make([]float32, 768)
			vec[1] = 1.0
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"embedding": vec}},
			})
		}))
		defer srv.Close()

		client := NewClient(srv.URL, "ollama", "llama3.2:3b", "qwen2.5-coder:7b", "nomic-embed-text", 768)
		vec, err := client.Embed(context.Background(), "timeout error")
		if err != nil {
			t.Fatalf("Embed fallback failed: %v", err)
		}
		if len(vec) != 768 {
			t.Fatalf("expected 768 dims, got %d", len(vec))
		}
		if calls.Load() != 2 {
			t.Fatalf("expected 2 requests (initial + fallback), got %d", calls.Load())
		}
	})
}
