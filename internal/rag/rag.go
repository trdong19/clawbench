package rag

import (
	"fmt"
	"log/slog"
	"sync/atomic"

	"clawbench/internal/model"
)

var (
	// GlobalStore is the shared RAG store instance, initialized by Init.
	GlobalStore *Store
	// GlobalIndexer is the shared RAG indexer instance.
	GlobalIndexer *Indexer
	// GlobalEmbedder is the shared embedding client instance.
	GlobalEmbedder *EmbeddingClient
	// GlobalCleanupWorker runs periodic retention-based cleanup of expired RAG data.
	GlobalCleanupWorker *CleanupWorker
	embedderHealthyFlag atomic.Bool
)

// Init initializes the RAG store, embedder, and loads the segmenter.
func Init(cfg model.RAGConfig) error {
	if err := InitSegmenter(); err != nil {
		slog.Warn("rag: gse segmenter not available, Chinese FTS may be limited", slog.String("err", err.Error()))
	}

	store, err := InitStore()
	if err != nil {
		return fmt.Errorf("init rag store: %w", err)
	}

	existingDim, mismatch, err := store.CheckDimensionMismatch()
	if err != nil {
		slog.Warn("rag: failed to check dimension, continuing", slog.String("err", err.Error()))
	} else if mismatch {
		slog.Warn(
			"rag: embedding dimension mismatch, resetting table",
			slog.Int("existing_dim", existingDim),
			slog.Int("expected_dim", store.embeddingDim),
		)
		if err := store.ResetTable(); err != nil {
			_ = store.Close()
			return fmt.Errorf("reset rag table: %w", err)
		}
	}

	embedder := NewEmbeddingClient(cfg.BaseURL, cfg.Model, cfg.APIKey)

	GlobalStore = store
	GlobalEmbedder = embedder

	slog.Info(
		"rag initialized",
		slog.String("base_url", cfg.BaseURL),
		slog.String("model", cfg.Model),
		slog.Int("chunk_size", cfg.ChunkSize),
		slog.Bool("fts_available", store.ftsAvailable),
		slog.Int("embedding_dim", store.embeddingDim),
	)

	return nil
}

// StartIndexer starts the background RAG indexer.
func StartIndexer(cfg model.RAGConfig) {
	if GlobalStore == nil {
		slog.Warn("rag: cannot start indexer, store not initialized")
		return
	}
	GlobalIndexer = NewIndexer(GlobalStore, GlobalEmbedder, cfg)
	GlobalIndexer.Start()
}

// StartCleanupWorker starts the background cleanup worker that purges expired chunks.
func StartCleanupWorker(cfg model.RAGConfig) {
	if cfg.RetentionDays <= 0 {
		return
	}
	GlobalCleanupWorker = NewCleanupWorker(GlobalStore, cfg)
	GlobalCleanupWorker.Start()
}

// Shutdown stops all RAG background workers and closes the store.
func Shutdown() {
	if GlobalCleanupWorker != nil {
		GlobalCleanupWorker.Stop()
		GlobalCleanupWorker = nil
	}
	if GlobalIndexer != nil {
		GlobalIndexer.Stop()
		GlobalIndexer = nil
	}
	if GlobalStore != nil {
		_ = GlobalStore.Close()
		GlobalStore = nil
	}
	GlobalEmbedder = nil
	slog.Info("rag shutdown complete")
}

// EmbedderHealthy returns whether the embedding service is healthy.
func EmbedderHealthy() bool {
	return embedderHealthyFlag.Load()
}

// SetEmbedderHealthy sets the embedding service health flag.
func SetEmbedderHealthy(healthy bool) {
	embedderHealthyFlag.Store(healthy)
}
