package enactcrawls

import (
	"testing"

	"enact/internal/crawls"
)

// TestMergeJIRAKeepsWhatAnUpdateDidNotSend covers the commonest edit there is:
// replacing an expired token. `{"jira":{"token":"..."}}` must not blank the
// base URL and the email and then fail validation complaining about the base
// URL — which is not the thing that changed.
func TestMergeJIRAKeepsWhatAnUpdateDidNotSend(t *testing.T) {
	stored := &crawls.JIRAConfig{
		BaseURL: "https://acme.atlassian.net", Email: "a@b.c",
		Token: "old-sealed", Projects: []string{"SCRUM"}, MaxDepth: 2,
	}

	got := mergeJIRA(stored, &crawls.JIRAConfig{Token: "new"})
	if got.Token != "new" {
		t.Errorf("token = %q, want the new one", got.Token)
	}
	for _, c := range []struct{ what, got, want string }{
		{"base_url", got.BaseURL, stored.BaseURL},
		{"email", got.Email, stored.Email},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want it kept as %q", c.what, c.got, c.want)
		}
	}
	if len(got.Projects) != 1 || got.MaxDepth != 2 {
		t.Errorf("projects/max_depth were lost: %+v", got)
	}

	// And the other direction: editing the projects must not require resending
	// the secret.
	got = mergeJIRA(stored, &crawls.JIRAConfig{Projects: []string{"ENG"}})
	if got.Token != "old-sealed" {
		t.Errorf("token = %q, want the stored one kept", got.Token)
	}
	if len(got.Projects) != 1 || got.Projects[0] != "ENG" {
		t.Errorf("projects = %v, want the update applied", got.Projects)
	}

	// Mutating the merged copy must not touch what is stored.
	if stored.Token != "old-sealed" || stored.Projects[0] != "SCRUM" {
		t.Error("mergeJIRA mutated the stored config")
	}

	// A crawl that had no JIRA config at all takes the update wholesale.
	if fresh := mergeJIRA(nil, &crawls.JIRAConfig{BaseURL: "x"}); fresh.BaseURL != "x" {
		t.Error("a first-time config was dropped")
	}
}
