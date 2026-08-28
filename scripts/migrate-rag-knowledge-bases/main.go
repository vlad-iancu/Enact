// migrate-rag-knowledge-bases moves an existing installation off the
// "every agent owns a RAG collection" model and onto retrieval knowledge
// bases.
//
// Before this change, an agent's retrieval chunks were keyed by agent_id and
// nothing else could reach them. Now the chunks belong to a knowledge base of
// kind "rag", which an agent points at through rag_knowledge_base_id — so the
// same corpus can be shared, and its access is governed by the same
// enact:kb:… permissions as every other knowledge base.
//
// For each agent that has chunks, this:
//
//  1. creates a rag-kind knowledge base owned by the agent's owner, in the
//     agent's organization;
//  2. grants the owner every permission on it (the hidden ownership role),
//     so the knowledge base behaves exactly like one they had created;
//  3. re-keys the agent's chunks onto that knowledge base, giving each the
//     document id the current code would write (<kb>-<doc>-<index>) and
//     dropping the now-meaningless agent_id/user_id fields;
//  4. sets the agent's rag_knowledge_base_id.
//
// Idempotent. Every step is skipped when it has already happened: an agent
// that already names a retrieval knowledge base is left alone, and chunks are
// selected by "has agent_id, has no kb_id", which is empty after a successful
// run. Safe to run twice, and safe to re-run after a partial failure.
//
//	go run ./scripts/migrate-rag-knowledge-bases            (dry run)
//	go run ./scripts/migrate-rag-knowledge-bases -apply
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sethvargo/go-envconfig"

	"enact/internal/agents"
	"enact/internal/kb"
	"enact/internal/opensearch"
	"enact/internal/rbac"
)

// rekeyBatch is how many chunks are read, rewritten and deleted per round.
// Chunks carry their embedding, so a batch is megabytes; this keeps one
// round's memory bounded while still being far fewer round trips than one
// document at a time.
const rekeyBatch = 200

func main() {
	apply := flag.Bool("apply", false, "write the changes; without it the run only reports what it would do")
	flag.Parse()

	ctx := context.Background()
	var cfg struct {
		OpenSearch opensearch.Config
		Agents     agents.Config
		KB         kb.Config
		RBAC       rbac.Config
	}
	if err := envconfig.Process(ctx, &cfg); err != nil {
		log.Fatalf("load configuration: %v", err)
	}
	client, err := opensearch.NewClient(cfg.OpenSearch)
	if err != nil {
		log.Fatalf("connect to opensearch: %v", err)
	}
	repo := rbac.NewRepository(client, cfg.RBAC)

	if !*apply {
		fmt.Println("DRY RUN — nothing is written. Re-run with -apply to make these changes.")
	}

	// Which agents still hold chunks of their own. A chunk that already has a
	// kb_id has been migrated, so it is not counted and its agent is not
	// touched again.
	counts, err := unmigratedChunkCounts(ctx, client, cfg.KB.ChunksIndex)
	if err != nil {
		log.Fatalf("read %s: %v", cfg.KB.ChunksIndex, err)
	}
	if len(counts) == 0 {
		fmt.Println("\nNo unmigrated chunks — nothing to do.")
		return
	}
	fmt.Printf("\n%d agent(s) with chunks to migrate:\n", len(counts))
	for agentID, n := range counts {
		fmt.Printf("  %s: %d chunks\n", agentID, n)
	}

	for agentID, n := range counts {
		fmt.Printf("\nagent %s\n", agentID)
		var agent agents.Agent
		found, err := client.GetSource(ctx, cfg.Agents.Index, agentID, &agent)
		if err != nil {
			log.Fatalf("  read agent: %v", err)
		}
		if !found {
			// Orphaned chunks: the agent was deleted while its collection
			// survived. Reported rather than guessed at — there is no owner
			// to give the knowledge base to, and inventing one would hand
			// somebody a corpus nobody asked them to have.
			fmt.Printf("  SKIPPED — agent record is gone; %d orphaned chunks left in place\n", n)
			continue
		}
		if agent.RAGKnowledgeBaseID != "" {
			fmt.Printf("  SKIPPED — already points at knowledge base %s\n", agent.RAGKnowledgeBaseID)
			continue
		}
		if agent.UserID == "" || agent.OrganizationID == "" {
			fmt.Printf("  SKIPPED — agent has no owner (%q) or organization (%q)\n", agent.UserID, agent.OrganizationID)
			continue
		}

		kbID := uuid.NewString()
		name := knowledgeBaseName(agent)
		fmt.Printf("  create rag knowledge base %s (%q) owned by %s in %s\n", kbID, name, agent.UserID, agent.OrganizationID)
		fmt.Printf("  grant %s to %s\n", rbac.OwnerRules(rbac.ResourceKB, kbID), agent.UserID)
		fmt.Printf("  re-key %d chunks onto %s\n", n, kbID)
		fmt.Printf("  set rag_knowledge_base_id on the agent\n")
		if !*apply {
			continue
		}

		now := time.Now().UTC()
		record := kb.KnowledgeBase{
			ID:             kbID,
			UserID:         agent.UserID,
			OrganizationID: agent.OrganizationID,
			Name:           name,
			Kind:           kb.KindRetrieval,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		body, err := json.Marshal(record)
		if err != nil {
			log.Fatalf("  marshal knowledge base: %v", err)
		}
		if err := client.IndexDoc(ctx, cfg.KB.KnowledgeBasesIndex, kbID, body); err != nil {
			log.Fatalf("  create knowledge base: %v", err)
		}
		// Ownership before the chunks move: if this run dies in between, the
		// owner can still reach the (as yet empty) knowledge base, and a
		// re-run finishes the job. The reverse order would leave a corpus
		// nobody may open.
		if err := repo.Grant(ctx, agent.OrganizationID, agent.UserID, rbac.OwnerRules(rbac.ResourceKB, kbID)); err != nil {
			log.Fatalf("  grant ownership: %v", err)
		}
		moved, err := rekeyChunks(ctx, client, cfg.KB.ChunksIndex, agentID, kbID)
		if err != nil {
			log.Fatalf("  re-key chunks: %v", err)
		}
		fmt.Printf("  re-keyed %d chunks\n", moved)

		agent.RAGKnowledgeBaseID = kbID
		agent.UpdatedAt = time.Now().UTC()
		agentBody, err := json.Marshal(agent)
		if err != nil {
			log.Fatalf("  marshal agent: %v", err)
		}
		if err := client.IndexDoc(ctx, cfg.Agents.Index, agent.ID, agentBody); err != nil {
			log.Fatalf("  update agent: %v", err)
		}
		fmt.Printf("  done\n")
	}

	if !*apply {
		fmt.Println("\nDRY RUN — nothing was written.")
	}
}

// knowledgeBaseName names the knowledge base after the agent whose corpus it
// holds, so the owner can tell at a glance where it came from. Agents predate
// names, so an unnamed one falls back to its id — an ugly name is better than
// a list of knowledge bases all called "knowledge".
func knowledgeBaseName(agent agents.Agent) string {
	if name := strings.TrimSpace(agent.Name); name != "" {
		return name + " knowledge"
	}
	return "Agent " + agent.ID + " knowledge"
}

// unmigratedChunkCounts groups the chunks that still belong to an agent by
// that agent, via a terms aggregation.
func unmigratedChunkCounts(ctx context.Context, client *opensearch.Client, index string) (map[string]int, error) {
	body, err := json.Marshal(map[string]any{
		"size":  0,
		"query": unmigratedQuery(""),
		"aggs": map[string]any{
			"by_agent": map[string]any{"terms": map[string]any{"field": "agent_id", "size": 1000}},
		},
	})
	if err != nil {
		return nil, err
	}
	res, err := client.SearchWithAggregations(ctx, index, body)
	if err != nil {
		return nil, err
	}
	if res.Aggregations == nil {
		return nil, nil
	}
	var aggs struct {
		ByAgent struct {
			Buckets []struct {
				Key      string `json:"key"`
				DocCount int    `json:"doc_count"`
			} `json:"buckets"`
		} `json:"by_agent"`
	}
	if err := json.Unmarshal(res.Aggregations, &aggs); err != nil {
		return nil, err
	}
	out := make(map[string]int, len(aggs.ByAgent.Buckets))
	for _, b := range aggs.ByAgent.Buckets {
		out[b.Key] = b.DocCount
	}
	return out, nil
}

// unmigratedQuery matches chunks that still carry an agent_id and have not
// yet been given a kb_id. Passing an agentID narrows it to one agent.
//
// This pair of conditions is what makes the migration re-runnable: a chunk
// stops matching the moment it has been rewritten, so a run that dies part
// way through resumes exactly where it stopped.
func unmigratedQuery(agentID string) map[string]any {
	filter := []any{map[string]any{"exists": map[string]any{"field": "agent_id"}}}
	if agentID != "" {
		filter = append(filter, map[string]any{"term": map[string]any{"agent_id": agentID}})
	}
	return map[string]any{"bool": map[string]any{
		"filter":   filter,
		"must_not": []any{map[string]any{"exists": map[string]any{"field": "kb_id"}}},
	}}
}

// legacyChunk is a chunk as it was stored before the move: keyed by agent and
// user. The embedding is carried through verbatim so nothing has to be
// re-embedded (which would cost a Bedrock call per chunk and, for a model
// that has since changed, would not even reproduce the same vectors).
type legacyChunk struct {
	DocumentID string    `json:"document_id"`
	ChunkIndex int       `json:"chunk_index"`
	Filename   string    `json:"filename,omitempty"`
	Text       string    `json:"text"`
	Embedding  []float32 `json:"embedding"`
}

// rekeyChunks rewrites one agent's chunks onto a knowledge base, in batches,
// and returns how many moved.
//
// Each chunk is written under the id the current code would give it and the
// old document is then deleted. Writing under a NEW id rather than updating
// in place matters: the id is what makes re-uploading a document overwrite
// its chunks instead of duplicating them, and a chunk left under its old
// agent-keyed id would be invisible to that rule.
func rekeyChunks(ctx context.Context, client *opensearch.Client, index, agentID, kbID string) (int, error) {
	moved := 0
	for {
		body, err := json.Marshal(map[string]any{
			"size":  rekeyBatch,
			"query": unmigratedQuery(agentID),
		})
		if err != nil {
			return moved, err
		}
		hits, err := client.Search(ctx, index, body)
		if err != nil {
			return moved, err
		}
		if len(hits) == 0 {
			return moved, nil
		}
		for _, h := range hits {
			var old legacyChunk
			if err := json.Unmarshal(h.Source, &old); err != nil {
				return moved, fmt.Errorf("decode chunk %s: %w", h.ID, err)
			}
			next := kb.Chunk{
				KBID:       kbID,
				DocumentID: old.DocumentID,
				ChunkIndex: old.ChunkIndex,
				Filename:   old.Filename,
				Text:       old.Text,
				Embedding:  old.Embedding,
			}
			nextBody, err := json.Marshal(next)
			if err != nil {
				return moved, fmt.Errorf("marshal chunk %s: %w", h.ID, err)
			}
			newID := fmt.Sprintf("%s-%s-%d", kbID, old.DocumentID, old.ChunkIndex)
			if err := client.IndexDoc(ctx, index, newID, nextBody); err != nil {
				return moved, fmt.Errorf("write chunk %s: %w", newID, err)
			}
			// Only after the replacement is durable. The other order would
			// lose a chunk outright if the write failed.
			if newID != h.ID {
				if err := client.DeleteDoc(ctx, index, h.ID); err != nil {
					return moved, fmt.Errorf("delete legacy chunk %s: %w", h.ID, err)
				}
			}
			moved++
		}
	}
}
