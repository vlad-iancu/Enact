// migrate-organizations moves an existing installation into the
// organizations model: it creates the `default` organization, makes every
// existing user a member and an owner of it, and records what each user
// already owns so their resources keep working under RBAC.
//
// It changes NO resource documents. A resource's organization is inferred
// from its owner (see the RBAC ADR), so an agent, KB, conversation, MCP
// server or identity lands in `default` by virtue of its owner's membership
// — there is nothing to stamp.
//
// Idempotent: re-running adds nothing that is already there, so it is safe
// against a live deployment and safe to run twice.
//
//	go run ./scripts/migrate-organizations            (dry run)
//	go run ./scripts/migrate-organizations -apply
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/sethvargo/go-envconfig"

	"enact/internal/extidentities"
	"enact/internal/opensearch"
	"enact/internal/rbac"
	"enact/internal/tools"
)

// resourceIndex is one index whose documents are owned by a user, and the
// field naming that user. Ownership becomes a rule under the owner's hidden
// role.
type resourceIndex struct {
	index    string
	idField  string
	ownerKey string
	resource string
	// ruleGranted says whether ownership of this resource is expressed as an
	// RBAC rule. False for conversations: they are private to one person and
	// nothing evaluates a conversation permission, so granting rules would
	// write data that reads like access control and governs nothing.
	ruleGranted bool
}

// The two indices this migration rewrites. They are not read from the
// service configuration because nothing else here needs the identities
// config, and the names have been stable since the indices were introduced.
const (
	providersIndex  = "enact-identity-providers"
	identitiesIndex = "enact-identities"
	serversIndex    = "enact-tool-servers"
	rolesIndex      = "enact-roles"
	toolCacheIndex  = "enact-tool-cache"
)

func main() {
	apply := flag.Bool("apply", false, "write the changes; without it the run only reports what it would do")
	orgName := flag.String("name", "Default organization", "the name given to the default organization")
	// Not every actor that owns something is a registered user. The e2e
	// suite runs as TEST_USER_ID, and anything created without an X-User-Id
	// header belongs to identity.DefaultUser. Both own real documents and
	// would be locked out of them by a migration that only walked the users
	// index.
	extra := flag.String("extra-members", "integration-tests,default",
		"comma-separated user ids to add that are not registered users")
	flag.Parse()

	ctx := context.Background()
	var cfg struct {
		OpenSearch opensearch.Config
		RBAC       rbac.Config
		Users      struct {
			Index string `env:"OPENSEARCH_INDEX_USERS, default=enact-users"`
		}
	}
	if err := envconfig.Process(ctx, &cfg); err != nil {
		log.Fatalf("configuration: %v", err)
	}
	client, err := opensearch.NewClient(cfg.OpenSearch)
	if err != nil {
		log.Fatalf("opensearch: %v", err)
	}
	repo := rbac.NewRepository(client, cfg.RBAC)
	if err := repo.EnsureIndices(ctx); err != nil {
		log.Fatalf("%v", err)
	}

	if !*apply {
		fmt.Println("DRY RUN — pass -apply to write. Nothing below has happened yet.")
	}

	// 1. The organization itself.
	now := time.Now().UTC()
	if _, found, err := repo.GetOrganization(ctx, rbac.DefaultOrganizationID); err != nil {
		log.Fatalf("read the default organization: %v", err)
	} else if found {
		fmt.Printf("organization %q already exists\n", rbac.DefaultOrganizationID)
	} else {
		fmt.Printf("create organization %q (%s)\n", rbac.DefaultOrganizationID, *orgName)
		if *apply {
			if err := repo.SaveOrganization(ctx, rbac.Organization{
				ID:        rbac.DefaultOrganizationID,
				Name:      *orgName,
				CreatedAt: now,
				UpdatedAt: now,
			}); err != nil {
				log.Fatalf("create the organization: %v", err)
			}
		}
	}

	// 2. Every existing user becomes a member and an owner. Owner because
	//    before organizations existed, everyone administered their own
	//    world; demoting them silently would take away powers they had.
	users, err := allUserIDs(ctx, client, cfg.Users.Index)
	if err != nil {
		log.Fatalf("list users: %v", err)
	}
	// Plus everyone who owns a document. A user deleted before organizations
	// existed still owns their agents and conversations; without a membership
	// those documents would belong to no organization and become permanently
	// unreachable.
	owners, err := allOwners(ctx, client)
	if err != nil {
		log.Fatalf("collect owners: %v", err)
	}
	users = union(users, owners, splitList(*extra))
	fmt.Printf("\n%d members to place (registered users, resource owners and extras)\n", len(users))
	for _, userID := range users {
		existing, found, err := repo.GetMembership(ctx, userID)
		if err != nil {
			log.Fatalf("read membership for %s: %v", userID, err)
		}
		switch {
		case found && existing.OrganizationID == rbac.DefaultOrganizationID:
			fmt.Printf("  %s: already a member\n", userID)
			continue
		case found:
			// Never move a user: their resources would move with them.
			fmt.Printf("  %s: SKIPPED — already in organization %q\n", userID, existing.OrganizationID)
			continue
		}
		fmt.Printf("  %s: add as owner\n", userID)
		if *apply {
			if err := repo.SaveMembership(ctx, rbac.Membership{
				UserID:         userID,
				OrganizationID: rbac.DefaultOrganizationID,
				Owner:          true,
				CreatedAt:      now,
				UpdatedAt:      now,
			}); err != nil {
				log.Fatalf("add %s: %v", userID, err)
			}
		}
	}

	// 3. What each user already owns, read from the ownership field every
	//    resource already carries.
	fmt.Println("\nownership grants")
	for _, ri := range resourceIndices() {
		if !ri.ruleGranted {
			fmt.Printf("  %s: no rules — access is the owner's alone\n", ri.index)
			continue
		}
		owned, err := ownership(ctx, client, ri)
		if err != nil {
			log.Fatalf("read %s: %v", ri.index, err)
		}
		fmt.Printf("  %s: %d documents\n", ri.index, count(owned))
		for userID, ids := range owned {
			if userID == "" {
				fmt.Printf("    (unowned): %d documents left as they are\n", len(ids))
				continue
			}
			var rules []string
			for _, id := range ids {
				rules = append(rules, rbac.OwnerRules(ri.resource, id)...)
			}
			fmt.Printf("    %s: %d rules\n", userID, len(rules))
			if *apply {
				// Grant into the user's OWN organization, which for a
				// migrated installation is the default one — but read it
				// back rather than assuming, so a partially-migrated
				// cluster stays correct.
				m, found, err := repo.GetMembership(ctx, userID)
				if err != nil {
					log.Fatalf("read membership for %s: %v", userID, err)
				}
				if !found {
					fmt.Printf("      SKIPPED — %s belongs to no organization\n", userID)
					continue
				}
				if err := repo.Grant(ctx, m.OrganizationID, userID, rules); err != nil {
					log.Fatalf("grant to %s: %v", userID, err)
				}
			}
		}
	}

	// 4. Identity providers are the one resource that STORES its
	// organization, so unlike everything above they really are rewritten:
	// each is re-keyed from "<name>" to "<org>:<name>" and stamped, and each
	// stored credential records which organization's provider it came from.
	fmt.Println("\nRe-keying identity providers into the organization:")
	if err := migrateProviders(ctx, client, rbac.DefaultOrganizationID, *apply); err != nil {
		log.Fatalf("providers: %v", err)
	}

	// 5. Every resource records the organization it belongs to. It used to be
	// inferred from the owner, which held only while permission checks came
	// from roles inside the caller's own organization — and owner bypass
	// broke that, letting an owner of one organization reach another's
	// resources by id. Stamping it makes the boundary a stored fact.
	fmt.Println("\nStamping organization_id on owned resources:")
	for _, ri := range resourceIndices() {
		if err := stampOrganizations(ctx, client, repo, ri, *apply); err != nil {
			log.Fatalf("stamp %s: %v", ri.index, err)
		}
	}

	// 6. MCP servers additionally move to an organization-scoped document id,
	// which is what makes their caller-chosen ids unique per organization
	// rather than platform-wide.
	fmt.Println("\nRe-keying MCP servers into their organization:")
	if err := rekeyServers(ctx, client, *apply); err != nil {
		log.Fatalf("re-key servers: %v", err)
	}

	// 6b. Earlier runs granted conversation rules before it was settled that
	// conversations stay private. Nothing ever read them, so removing them
	// changes no access — it stops the rule listing from claiming a
	// permission the platform does not honour.
	fmt.Println("\nRemoving conversation rules, which govern nothing:")
	if err := dropConversationRules(ctx, client, *apply); err != nil {
		log.Fatalf("drop conversation rules: %v", err)
	}

	if !*apply {
		fmt.Println("\nDRY RUN — nothing was written. Re-run with -apply.")
		return
	}

	// 7. The tool cache is derived from the servers and keyed by them, so it
	// is dropped rather than migrated: the registry's refresh sweep rebuilds
	// it within one interval, and rebuilding is less to get wrong than
	// re-keying every cached tool.
	if err := client.DeleteByQuery(ctx, toolCacheIndex, []byte(`{"query":{"match_all":{}}}`)); err != nil {
		log.Fatalf("drop the tool cache: %v", err)
	}
	fmt.Println("\ntool cache dropped; the registry sweep will rebuild it")
	fmt.Println("\nmigration complete")
	os.Exit(0)
}

// resourceIndices are the indices whose documents carry an owner.
func resourceIndices() []resourceIndex {
	return []resourceIndex{
		{"enact-agents", "id", "user_id", rbac.ResourceAgent, true},
		{"enact-knowledge-bases", "id", "user_id", rbac.ResourceKB, true},
		// Conversations are stamped with their organization like everything
		// else, but carry no rules — see resourceIndex.ruleGranted.
		{"enact-conversations", "id", "user_id", rbac.ResourceConversation, false},
		{"enact-tool-servers", "id", "owner", rbac.ResourceMCPServer, true},
	}
}

// allOwners collects every user id that owns at least one document.
func allOwners(ctx context.Context, client *opensearch.Client) ([]string, error) {
	var out []string
	for _, ri := range resourceIndices() {
		owned, err := ownership(ctx, client, ri)
		if err != nil {
			return nil, err
		}
		for owner := range owned {
			if owner != "" {
				out = append(out, owner)
			}
		}
	}
	return out, nil
}

// union merges id lists, dropping duplicates and blanks, preserving order.
func union(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, id := range list {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// splitList parses a comma-separated flag value.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// allUserIDs reads every user's id from the users index.
func allUserIDs(ctx context.Context, client *opensearch.Client, index string) ([]string, error) {
	body, err := json.Marshal(map[string]any{
		"size":    10000,
		"_source": []string{"id"},
		"query":   map[string]any{"match_all": map[string]any{}},
	})
	if err != nil {
		return nil, err
	}
	hits, err := client.Search(ctx, index, body)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		var doc struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(h.Source, &doc); err != nil {
			return nil, err
		}
		if doc.ID != "" {
			out = append(out, doc.ID)
		}
	}
	return out, nil
}

// ownership maps each owner to the resource ids they own in one index.
func ownership(ctx context.Context, client *opensearch.Client, ri resourceIndex) (map[string][]string, error) {
	body, err := json.Marshal(map[string]any{
		"size":    10000,
		"_source": []string{ri.idField, ri.ownerKey},
		"query":   map[string]any{"match_all": map[string]any{}},
	})
	if err != nil {
		return nil, err
	}
	hits, err := client.Search(ctx, ri.index, body)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, h := range hits {
		var doc map[string]any
		if err := json.Unmarshal(h.Source, &doc); err != nil {
			return nil, err
		}
		id, _ := doc[ri.idField].(string)
		owner, _ := doc[ri.ownerKey].(string)
		if id == "" {
			continue
		}
		out[owner] = append(out[owner], id)
	}
	return out, nil
}

func count(m map[string][]string) int {
	total := 0
	for _, ids := range m {
		total += len(ids)
	}
	return total
}

// migrateProviders moves every provider document to its organization-scoped
// id and stamps the same organization onto every stored credential.
//
// This is the only part of the migration that rewrites documents, because a
// provider has no owning user to infer an organization from. Re-running is
// safe: a provider already carrying the organization is left alone, and the
// identity update is a no-op once stamped.
func migrateProviders(ctx context.Context, client *opensearch.Client, organizationID string, apply bool) error {
	body, err := json.Marshal(map[string]any{"size": 1000, "query": map[string]any{"match_all": map[string]any{}}})
	if err != nil {
		return err
	}
	hits, err := client.Search(ctx, providersIndex, body)
	if err != nil {
		return err
	}
	moved := 0
	for _, h := range hits {
		var rec extidentities.ProviderRecord
		if err := json.Unmarshal(h.Source, &rec); err != nil {
			return fmt.Errorf("decode provider %s: %w", h.ID, err)
		}
		// A provider that already names an organization is finished — even
		// if that organization is not the default one. Re-keying by target
		// alone would drag every other organization's providers into the
		// default, which is data loss rather than migration.
		if rec.OrganizationID != "" {
			fmt.Printf("  %s: already scoped to %s\n", rec.Name, rec.OrganizationID)
			continue
		}
		target := extidentities.ProviderDocID(organizationID, rec.Name)
		fmt.Printf("  %s: %s -> %s\n", rec.Name, h.ID, target)
		moved++
		if !apply {
			continue
		}
		rec.OrganizationID = organizationID
		updated, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if err := client.IndexDoc(ctx, providersIndex, target, updated); err != nil {
			return fmt.Errorf("write provider %s: %w", target, err)
		}
		// Only after the new document exists, so an interrupted run leaves a
		// duplicate rather than nothing.
		if h.ID != target {
			if err := client.DeleteDoc(ctx, providersIndex, h.ID); err != nil {
				return fmt.Errorf("remove old provider %s: %w", h.ID, err)
			}
		}
	}
	fmt.Printf("  %d provider(s) re-keyed\n", moved)

	// Stamp the credentials. update_by_query keeps this one statement
	// regardless of how many identities exist.
	if !apply {
		fmt.Println("  identities would be stamped with the organization")
		return nil
	}
	script, err := json.Marshal(map[string]any{
		"query": map[string]any{"bool": map[string]any{"must_not": []any{
			map[string]any{"exists": map[string]any{"field": "organization_id"}},
		}}},
		"script": map[string]any{
			"source": "ctx._source.organization_id = params.org",
			"lang":   "painless",
			"params": map[string]any{"org": organizationID},
		},
	})
	if err != nil {
		return err
	}
	updated, err := client.UpdateByQuery(ctx, identitiesIndex, script)
	if err != nil {
		return fmt.Errorf("stamp identities: %w", err)
	}
	fmt.Printf("  %d identit(ies) stamped with %s\n", updated, organizationID)
	return nil
}

// stampOrganizations writes organization_id onto every document of one index,
// taking it from the owner's membership.
//
// Documents whose owner belongs to no organization are reported and left
// alone. Guessing would put somebody's data in an organization that never
// asked for it, and the services treat a missing organization_id as
// unreachable — so leaving it blank fails closed.
func stampOrganizations(ctx context.Context, client *opensearch.Client, repo *rbac.Repository, ri resourceIndex, apply bool) error {
	owned, err := ownership(ctx, client, ri)
	if err != nil {
		return err
	}
	stamped, skipped := 0, 0
	for userID, ids := range owned {
		if userID == "" {
			skipped += len(ids)
			continue
		}
		m, found, err := repo.GetMembership(ctx, userID)
		if err != nil {
			return fmt.Errorf("read membership for %s: %w", userID, err)
		}
		if !found {
			fmt.Printf("    %s: SKIPPED — belongs to no organization (%d documents)\n", userID, len(ids))
			skipped += len(ids)
			continue
		}
		stamped += len(ids)
		if !apply {
			continue
		}
		script, err := json.Marshal(map[string]any{
			"query": map[string]any{"term": map[string]any{ri.ownerKey: userID}},
			"script": map[string]any{
				"source": "ctx._source.organization_id = params.org",
				"lang":   "painless",
				"params": map[string]any{"org": m.OrganizationID},
			},
		})
		if err != nil {
			return err
		}
		if _, err := client.UpdateByQuery(ctx, ri.index, script); err != nil {
			return fmt.Errorf("stamp %s for %s: %w", ri.index, userID, err)
		}
	}
	fmt.Printf("  %s: %d stamped, %d skipped\n", ri.index, stamped, skipped)
	return nil
}

// rekeyServers moves each MCP server document from "<id>" to
// "<organization>:<id>". Re-running is safe: a server already at its target
// id is left alone.
func rekeyServers(ctx context.Context, client *opensearch.Client, apply bool) error {
	body, err := json.Marshal(map[string]any{"size": 1000, "query": map[string]any{"match_all": map[string]any{}}})
	if err != nil {
		return err
	}
	hits, err := client.Search(ctx, serversIndex, body)
	if err != nil {
		return err
	}
	moved := 0
	for _, h := range hits {
		var server tools.Server
		if err := json.Unmarshal(h.Source, &server); err != nil {
			return fmt.Errorf("decode server %s: %w", h.ID, err)
		}
		if server.OrganizationID == "" {
			if !apply {
				// Expected in a dry run: the stamping step above has not
				// written anything yet, so nothing has an organization to
				// key on. An -apply run stamps first, then re-keys.
				fmt.Printf("  %s: would be re-keyed once stamped\n", h.ID)
			} else {
				fmt.Printf("  %s: SKIPPED — no organization (its owner has none)\n", h.ID)
			}
			continue
		}
		target := tools.ServerDocID(server.OrganizationID, server.ID)
		if h.ID == target {
			fmt.Printf("  %s: already scoped\n", server.ID)
			continue
		}
		fmt.Printf("  %s: %s -> %s\n", server.ID, h.ID, target)
		moved++
		if !apply {
			continue
		}
		// Write the new document before removing the old one, so an
		// interrupted run leaves a duplicate rather than nothing.
		if err := client.IndexDoc(ctx, serversIndex, target, h.Source); err != nil {
			return fmt.Errorf("write server %s: %w", target, err)
		}
		if err := client.DeleteDoc(ctx, serversIndex, h.ID); err != nil {
			return fmt.Errorf("remove old server %s: %w", h.ID, err)
		}
	}
	fmt.Printf("  %d server(s) re-keyed\n", moved)
	return nil
}

// dropConversationRules removes every enact:conversation:… rule from every
// role.
//
// Conversations are private to their owner and enforced by comparing the
// stored user id, not by evaluating a permission — no code path reads a
// conversation rule. Left in place they would appear in a user's effective
// rules and in GET /organizations/me, implying a grant that does nothing,
// which is worse than no rule at all when the question being asked is "who
// can see what".
func dropConversationRules(ctx context.Context, client *opensearch.Client, apply bool) error {
	prefix := rbac.Namespace + rbac.Separator + rbac.ResourceConversation + rbac.Separator
	query := map[string]any{"prefix": map[string]any{"rules": prefix}}

	body, err := json.Marshal(map[string]any{"size": 1000, "query": query, "_source": []string{"name", "rules"}})
	if err != nil {
		return err
	}
	hits, err := client.Search(ctx, rolesIndex, body)
	if err != nil {
		return err
	}
	affected := 0
	for _, h := range hits {
		var role struct {
			Rules []string `json:"rules"`
		}
		if err := json.Unmarshal(h.Source, &role); err != nil {
			return err
		}
		n := 0
		for _, rule := range role.Rules {
			if strings.HasPrefix(rule, prefix) {
				n++
			}
		}
		if n == 0 {
			continue
		}
		affected += n
		fmt.Printf("  %s: %d conversation rule(s)\n", h.ID, n)
	}
	if affected == 0 {
		fmt.Println("  none")
		return nil
	}
	if !apply {
		fmt.Printf("  %d rule(s) would be removed\n", affected)
		return nil
	}
	script, err := json.Marshal(map[string]any{
		"query": query,
		"script": map[string]any{
			"source": "ctx._source.rules.removeIf(r -> r.startsWith(params.prefix))",
			"lang":   "painless",
			"params": map[string]any{"prefix": prefix},
		},
	})
	if err != nil {
		return err
	}
	updated, err := client.UpdateByQuery(ctx, rolesIndex, script)
	if err != nil {
		return err
	}
	fmt.Printf("  %d rule(s) removed from %d role(s)\n", affected, updated)
	return nil
}
