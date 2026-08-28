# Analysis: OpenSearch k-NN as the RAG vector store

*Status: analysis of an existing decision — July 2026*

## What we do today

The chunk store (`internal/kb/chunks.go`, mapping in
`mappings/enact-agent-rag-chunks.json`) uses OpenSearch's built-in vector
engine:

| Aspect | Our choice |
|---|---|
| Field type | `knn_vector`, 1024-dim (Titan Text Embeddings v2) |
| Algorithm | HNSW (approximate nearest neighbour) |
| Engine | **Lucene** |
| Similarity | cosine |
| Filtering | a term filter on `kb_id` inside the k-NN query |
| Scale posture | 1 shard, 0 replicas, top-k=5 |

The filter used to include `user_id` as well. It was dropped when retrieval
collections became knowledge bases (ADR-0021): a knowledge base is shared
through RBAC rules, so filtering by the reader's own id would have hidden a
colleague's documents in a knowledge base they had been given. Authorization
belongs on the knowledge base; this index just holds its bytes.

The same cluster stores KB and agent metadata, so vectors ride on infrastructure we
already operate.

## Is OpenSearch k-NN used in big projects?

Yes — it is a mainstream, large-scale-proven choice, particularly in the AWS ecosystem:

- **Amazon Bedrock Knowledge Bases** — AWS's own managed RAG product — offers Amazon
  OpenSearch Serverless as its default/primary vector store (alongside Aurora,
  Pinecone, MongoDB Atlas, Redis). Every "quick create" Bedrock KB runs on OpenSearch
  vectors.
- AWS operates OpenSearch vector workloads at very large scale and keeps shipping
  heavy engineering into the engine: GPU-accelerated index builds (GA in 3.1, NVIDIA
  cuVS in 3.3, ~9× faster builds), disk-based vector search with binary quantization
  (2.17+), 2×–32× compression modes, and auto-optimized vector indexes (re:Invent
  2025).
- The common enterprise pattern is exactly ours: teams already running
  OpenSearch/Elasticsearch add vector search rather than introducing a new database,
  and get **hybrid search** (BM25 + vectors + metadata filters) in one system.

So the decision is not exotic. The honest caveat: in *pure vector benchmark*
comparisons, purpose-built engines (Qdrant, Milvus) post better latency/throughput
numbers, and the "default pick" discourse for greenfield RAG in 2026 tends toward
pgvector (small/medium) or Qdrant/Milvus (large). OpenSearch wins on consolidation,
hybrid search, and managed-AWS alignment more than on raw ANN speed.

## Why this decision is right for enact today

1. **One datastore.** KBs, agents, and chunks live in one OpenSearch cluster — one
   client (`internal/opensearch`), one `make infrastructure-up`, one security model.
   A dedicated vector DB would add a second stateful system to run, back up, and
   secure, for no capability we currently lack.
2. **Multi-tenancy filtering is first-class.** Our k-NN queries filter by
   `user_id`/`kb_id`. The Lucene engine does "smart" filtering (pre/post/exact
   selection automatically) — filtered ANN is a known weak spot of several dedicated
   stores, and it's the one thing we do on every query.
3. **Right engine for our scale.** Lucene HNSW is the documented best option below a
   few million vectors: best latency and recall at small scale, smallest index
   footprint. (nmslib is deprecated and blocked for new indices in 3.x — we're not on
   the losing horse.)
4. **AWS-native trajectory.** We already use Bedrock for embeddings and inference;
   OpenSearch Service/Serverless is the natural managed deployment, and it's the same
   stack Bedrock Knowledge Bases uses.
5. **Hybrid-search upside.** Chunk text is already indexed as `text` — adding BM25 +
   vector hybrid ranking (a consistent precision win in enterprise search, and the
   usual next step for RAG quality) is a query change, not a migration.

## Risks / where it ages poorly

- **Raw ANN performance at large scale.** Tens of millions of vectors per shard with
  tight p99s is where Qdrant/Milvus pull ahead and where OpenSearch requires real
  tuning (Faiss engine, quantization, disk-based mode, more shards).
- **Memory economics.** HNSW on-heap/off-heap costs grow with vector count;
  mitigations exist (binary/scalar quantization, disk-based search) but need
  deliberate adoption.
- **JVM/cluster ops.** If we never grow into the search features, a leaner store
  (pgvector) would be less to operate.
- **Serverless cost floor.** OpenSearch Serverless has an OCU minimum (~2 OCU); tiny
  deployments pay a fixed tax.

## Alternatives considered

| Alternative | When it would beat our choice | Why not now |
|---|---|---|
| **pgvector** | If the platform adopted Postgres for metadata anyway; fine to ~10–50M vectors | We have no Postgres; would *add* a system instead of removing one; no hybrid BM25 |
| **Qdrant** | Dedicated vector workloads, heavy filtered queries, low-latency SLOs | Second stateful system; our filters already work well; scale doesn't demand it |
| **Milvus** | Billion-scale, K8s-native distributed vector search | Massive operational overkill for per-user KBs |
| **Pinecone / managed SaaS** | Zero-ops, elastic scale | Data leaves our infra; cost; we already run OpenSearch |
| **S3 Vectors / object-storage-first engines** | Cold, huge, cost-sensitive corpora | Young; higher latency; not needed at our size |

## Future-proofing verdict

The decision is sound and *not* a dead end:

- **The engine is actively invested in** (GPU builds, quantization, disk-based ANN,
  auto-optimization) — its scale ceiling is rising each release.
- **Our blast radius is tiny by design.** After the repository refactor, every vector
  operation sits behind `kb.ChunkRepository` — three methods (`Index`, `Search`,
  `DeleteByKB`) and one mapping file. Swapping stores later is a one-package change
  plus a re-index; no handler or service logic would change.

### Triggers to revisit

Revisit the choice if any of these become true:
1. Chunk count approaches **several million per shard** or vector RAM dominates the
   cluster → first move to `engine: faiss` + quantization or disk-based mode (an
   in-place option), not to a new database.
2. **p99 retrieval latency** misses SLOs after Faiss/quantization tuning.
3. The platform **adopts Postgres** for other reasons → re-evaluate pgvector to
   consolidate the other way.
4. Requirements shift to **billion-scale or heavy multi-index ANN** → evaluate
   Qdrant/Milvus seriously.

### Cheap wins to bank meanwhile

- Move to **hybrid retrieval** (BM25 + k-NN, e.g. RRF) — biggest quality-per-effort
  improvement available, uses what we already store.
- Pin `engine: faiss` decision criteria in this doc when growth starts (Faiss is now
  OpenSearch's default engine; Lucene remains correct at our size).
- Consider **binary embeddings** (Titan v2 supports them) + binary quantization if
  memory cost appears — 32× compression with modest recall loss.

## Sources

- [OpenSearch: methods and engines (Lucene vs Faiss, nmslib deprecation)](https://docs.opensearch.org/latest/mappings/supported-field-types/knn-methods-engines/)
- [OpenSearch: disk-based vector search](https://docs.opensearch.org/latest/vector-search/optimizing-storage/disk-based-vector-search/)
- [OpenSearch 3.3: performance innovations for AI search](https://opensearch.org/blog/opensearch-3-3-performance-innovations-for-ai-search-solutions/)
- [AWS re:Invent 2025: GPU-accelerated and auto-optimized vector indexes](https://www.storagenewsletter.com/2025/12/08/aws-reinvent-2025-amazon-opensearch-service-adds-gpu-accelerated-and-auto-optimized-vector-indexes/)
- [Amazon OpenSearch Service's vector database capabilities explained](https://aws.amazon.com/blogs/big-data/amazon-opensearch-services-vector-database-capabilities-explained/)
- [Bedrock Knowledge Bases vector store options](https://aws.amazon.com/blogs/machine-learning/dive-deep-into-vector-data-stores-using-amazon-bedrock-knowledge-bases/)
- [Bedrock KB + binary embeddings + OpenSearch Serverless](https://aws.amazon.com/blogs/machine-learning/build-cost-effective-rag-applications-with-binary-embeddings-in-amazon-titan-text-embeddings-v2-amazon-opensearch-serverless-and-amazon-bedrock-knowledge-bases/)
- [Choosing the k-NN algorithm for billion-scale use cases](https://aws.amazon.com/blogs/big-data/choose-the-k-nn-algorithm-for-your-billion-scale-use-case-with-opensearch/)
- [pgvector vs Qdrant vs Milvus for RAG (2026)](https://dev.to/linou518/choosing-the-foundation-for-your-rag-system-pgvector-vs-qdrant-vs-milvus-2026-4i5o)
- [Best vector databases 2026 comparison](https://encore.dev/articles/best-vector-databases)
- [Scaling vector search with OpenSearch](https://bigdataboutique.com/blog/scaling-vector-search-with-opensearch-c0cdfc)