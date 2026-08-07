package enactkbindexer

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"enact/internal/agents"
	"enact/internal/bedrock"
	"enact/internal/kb"
	"enact/internal/logging"
	"enact/internal/queue"
	"enact/internal/rag"
	"enact/internal/tika"
)

// worker processes queued document uploads. Both kinds start with Tika text
// extraction and then diverge:
//
//   - kb_context documents are stored whole in the KB documents index; agents
//     referencing the KB load them into the model context at inference time.
//   - agent_rag documents are chunked, embedded via Bedrock, and written into
//     the agent's RAG chunk collection for k-NN retrieval.
type worker struct {
	documents    *kb.DocumentRepository
	rags         *agents.RAGRepository
	extractor    *tika.Client
	embedder     *bedrock.Client
	embedModel   string
	chunkSize    int
	chunkOverlap int
	logger       *logging.Logger
}

// handle processes one queued document message. It is the queue.Handler for
// the consumer: returning an error leaves the message pending for retry.
func (w *worker) handle(ctx context.Context, msg queue.DocumentMessage) error {
	// ctx carries the trace of the upload request that queued this message
	// (extracted by the queue consumer); binding it here puts that trace_id
	// on every log record of the processing pipeline.
	logger := w.logger.WithContext(ctx).WithFields(
		"type", string(msg.Type),
		"document_id", msg.DocumentID,
		"user_id", msg.UserID,
		"file_name", msg.Filename,
	)
	logger.Info("processing queued document")

	// Delete operations carry no content; branch before the decode/extract
	// steps, which assume an uploaded payload.
	switch msg.Type {
	case queue.DocumentTypeKBContextDelete:
		return w.deleteContextDocument(ctx, logger, msg)
	case queue.DocumentTypeAgentRAGDelete:
		return w.deleteRAGDocument(ctx, logger, msg)
	}

	// Content is the raw upload bytes base64-encoded (see queue.DocumentMessage).
	// A decode failure is a permanently malformed message, so ack it (return nil)
	// rather than retrying it forever.
	data, err := base64.StdEncoding.DecodeString(msg.Content)
	if err != nil {
		logger.Warn("document content is not valid base64; skipping", "err", err)
		return nil
	}
	logger.Info("content decoded", "size_bytes", len(data))

	text, err := w.extractor.Extract(ctx, msg.Filename, data)
	if err != nil {
		logger.Error("failed to extract text via tika", "size_bytes", len(data), "err", err)
		return fmt.Errorf("indexer: extract text from doc %s: %w", msg.DocumentID, err)
	}
	logger.Info("text extracted", "size_bytes", len(data), "text_chars", len(text))

	if strings.TrimSpace(text) == "" {
		logger.Warn("document produced no text; skipping")
		return nil
	}

	switch msg.Type {
	case queue.DocumentTypeAgentRAG:
		return w.indexRAGDocument(ctx, logger, msg, text)
	case queue.DocumentTypeKBContext, "":
		// An empty type is a message published before types existed; those
		// were KB uploads.
		return w.storeContextDocument(ctx, logger, msg, text)
	default:
		logger.Warn("unknown document type; skipping")
		return nil
	}
}

// deleteContextDocument removes one KB context document. Idempotent, so a
// redelivered message is harmless; errors leave the message pending for
// retry like any other handler failure.
func (w *worker) deleteContextDocument(ctx context.Context, logger *logging.Logger, msg queue.DocumentMessage) error {
	logger = logger.WithFields("kb_id", msg.KBID)
	logger.Info("deleting context document")
	if err := w.documents.DeleteByDocument(ctx, msg.KBID, msg.DocumentID); err != nil {
		logger.Error("failed to delete context document", "err", err)
		return fmt.Errorf("indexer: delete context doc %s: %w", msg.DocumentID, err)
	}
	logger.Info("context document deleted")
	return nil
}

// deleteRAGDocument removes one document's chunks from an agent's RAG
// collection. Idempotent; errors stay pending for retry.
func (w *worker) deleteRAGDocument(ctx context.Context, logger *logging.Logger, msg queue.DocumentMessage) error {
	logger = logger.WithFields("agent_id", msg.AgentID)
	logger.Info("deleting rag document")
	if err := w.rags.DeleteByDocument(ctx, msg.AgentID, msg.DocumentID); err != nil {
		logger.Error("failed to delete rag document", "err", err)
		return fmt.Errorf("indexer: delete rag doc %s: %w", msg.DocumentID, err)
	}
	logger.Info("rag document deleted")
	return nil
}

// storeContextDocument stores a KB document's extracted text whole.
func (w *worker) storeContextDocument(ctx context.Context, logger *logging.Logger, msg queue.DocumentMessage, text string) error {
	logger = logger.WithFields("kb_id", msg.KBID)
	logger.Info("storing context document", "text_chars", len(text))
	doc := kb.Document{
		UserID:     msg.UserID,
		KBID:       msg.KBID,
		DocumentID: msg.DocumentID,
		Filename:   msg.Filename,
		Text:       text,
		UploadedAt: time.Now().UTC(),
	}
	if err := w.documents.Index(ctx, doc); err != nil {
		logger.Error("failed to store context document", "err", err)
		return fmt.Errorf("indexer: store context doc %s: %w", msg.DocumentID, err)
	}
	logger.Info("context document stored", "text_chars", len(text))
	return nil
}

// indexRAGDocument chunks and embeds a document into the agent's RAG
// collection.
func (w *worker) indexRAGDocument(ctx context.Context, logger *logging.Logger, msg queue.DocumentMessage, text string) error {
	logger = logger.WithFields("agent_id", msg.AgentID)
	logger.Info("indexing rag document", "text_chars", len(text))
	chunks := rag.Chunk(text, w.chunkSize, w.chunkOverlap)
	if len(chunks) == 0 {
		logger.Warn("rag document produced no chunks; skipping")
		return nil
	}
	logger.Info("document chunked", "chunks", len(chunks))
	for i, chunkText := range chunks {
		vector, err := w.embedder.Embed(ctx, w.embedModel, chunkText)
		if err != nil {
			logger.Error("failed to embed rag chunk", "chunk_index", i, "err", err)
			return fmt.Errorf("indexer: embed chunk %d of doc %s: %w", i, msg.DocumentID, err)
		}
		c := agents.RAGChunk{
			UserID:     msg.UserID,
			AgentID:    msg.AgentID,
			DocumentID: msg.DocumentID,
			ChunkIndex: i,
			Filename:   msg.Filename,
			Text:       chunkText,
			Embedding:  vector,
		}
		if err := w.rags.Index(ctx, c); err != nil {
			logger.Error("failed to index rag chunk", "chunk_index", i, "err", err)
			return fmt.Errorf("indexer: index chunk %d of doc %s: %w", i, msg.DocumentID, err)
		}
		logger.Debug("rag chunk indexed", "chunk_index", i)
	}
	logger.Info("rag document indexed", "chunks", len(chunks))
	return nil
}
