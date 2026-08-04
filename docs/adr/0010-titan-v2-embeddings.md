# ADR-0010: Amazon Titan Text Embeddings v2 as the embedding model

**Date**: 2026-08-02
**Status**: accepted
**Deciders**: Iancu Vlad-Alexandru (with Claude Code)

## Context

Agent RAG retrieval embeds document chunks at indexing time and the query at
inference time, storing 1024-dim vectors in OpenSearch `knn_vector` fields
(Lucene HNSW, cosine). The embedding model determines retrieval quality,
vector dimension (index size), latency, and cost — and both the indexer and
the inference service must use the same model (`BEDROCK_EMBEDDING_MODEL`,
dimension pinned by `BEDROCK_EMBEDDING_DIM` in the index mapping).

## Decision

We use **Amazon Titan Text Embeddings v2** (`amazon.titan-embed-text-v2:0`)
at its default 1024 dimensions. Rationale: we are already on Bedrock for
inference, so it adds no new provider, credentials, or SDK; it is
inexpensive; v2 supports flexible dimensions (256/512/1024) and binary
embeddings, giving a documented cost/scale lever (see
`docs/rag-vector-store-analysis.md`) without a model migration; and its
quality is sufficient for the current corpus sizes and top-k ≤ 50 retrieval.

## Alternatives Considered

### Alternative 1: Cohere Embed v3 (also on Bedrock)
- **Pros**: stronger multilingual retrieval benchmarks; same Bedrock integration path
- **Cons**: higher price per token; compression-aware training only pays off with int8/binary index support wired up
- **Why not**: benchmark edge does not matter at our corpus size; Titan is cheaper and one fewer model family to reason about. Revisit if multilingual corpora arrive.

### Alternative 2: OpenAI text-embedding-3 (large/small)
- **Pros**: strong general-purpose quality; ubiquitous tooling
- **Cons**: introduces a second provider (credentials, billing, egress) into an otherwise AWS-native stack; conflicts with the platform's Bedrock-only integration
- **Why not**: provider sprawl for marginal gain at this scale

### Alternative 3: Self-hosted open-source model (e.g. bge-m3, e5-large)
- **Pros**: no per-token cost; data never leaves the host; controllable versioning
- **Cons**: we would own GPU/CPU serving, scaling, and model updates; latency on CPU is poor for indexing bursts
- **Why not**: operational cost dwarfs the API savings for a platform of this size

## Consequences

### Positive
- Zero new infrastructure; embeddings and inference share auth, region, and SDK
- Cost/scale levers (256-dim, binary embeddings + quantization) available within the same model — a config/mapping change, not a migration

### Negative
- Vector dimension is baked into the OpenSearch index mapping: switching models (or dimensions) requires re-embedding and re-indexing the entire RAG corpus
- Retrieval quality ceiling is Titan's — if corpora become multilingual or retrieval precision becomes the bottleneck, this ADR should be superseded

### Risks
- Silent model/dimension mismatch between indexer and inference would corrupt retrieval — mitigated: both read the same env variable, and the index mapping's dimension is filled from `BEDROCK_EMBEDDING_DIM` at infrastructure setup