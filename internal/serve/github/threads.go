// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// Only the root comment is fetched: the service creates threads and never
// replies in them, so ownership and the finding id are both read off the root.
const reviewThreadsQuery = `
query($owner: String!, $repo: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          isResolved
          root: comments(first: 1) { nodes { body author { login __typename } } }
        }
      }
    }
  }
}`

type threadsResponse struct {
	Data struct {
		Repository *struct {
			PullRequest *struct {
				ReviewThreads struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						ID         string `json:"id"`
						IsResolved bool   `json:"isResolved"`
						Root       struct {
							Nodes []struct {
								Body   string `json:"body"`
								Author *struct {
									Login    string `json:"login"`
									Typename string `json:"__typename"`
								} `json:"author"`
							} `json:"nodes"`
						} `json:"root"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// ListOCRThreads returns every review thread whose root the App wrote and
// tagged with a finding id. The walk fails rather than return a partial list,
// because an undercount of open threads would start the next round early.
func (f *Forge) ListOCRThreads(ctx context.Context, ref forge.PRRef) ([]forge.Thread, error) {
	if _, err := repoPath(ref); err != nil {
		return nil, err
	}
	bot, err := f.BotLogin(ctx)
	if err != nil {
		return nil, err
	}
	owner, name, _ := strings.Cut(ref.Repo, "/")
	var threads []forge.Thread
	var cursor *string
	for page := 0; ; page++ {
		if page == maxPages {
			return nil, fmt.Errorf("github: review threads exceed %d pages", maxPages)
		}
		vars := map[string]any{"owner": owner, "repo": name, "number": ref.Number, "cursor": cursor}
		data, _, err := f.call(ctx, ref.InstallationID, http.MethodPost, f.graphqlURL,
			map[string]any{"query": reviewThreadsQuery, "variables": vars})
		if err != nil {
			return nil, fmt.Errorf("github: list review threads: %w", err)
		}
		var resp threadsResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("github: list review threads: decode: %w", err)
		}
		if len(resp.Errors) > 0 {
			return nil, fmt.Errorf("github: list review threads: %s", resp.Errors[0].Message)
		}
		if resp.Data.Repository == nil || resp.Data.Repository.PullRequest == nil {
			return nil, errors.New("github: list review threads: pull request not found")
		}
		rt := resp.Data.Repository.PullRequest.ReviewThreads
		for _, n := range rt.Nodes {
			if len(n.Root.Nodes) == 0 {
				continue
			}
			root := n.Root.Nodes[0]
			if root.Author == nil || !ownedBy(graphqlUser(root.Author.Login, root.Author.Typename), bot) {
				continue
			}
			id, ok := forge.ParseFindingID(root.Body)
			if !ok {
				continue
			}
			threads = append(threads, forge.Thread{ID: n.ID, FindingID: id, Resolved: n.IsResolved})
		}
		if !rt.PageInfo.HasNextPage {
			return threads, nil
		}
		end := rt.PageInfo.EndCursor
		cursor = &end
	}
}

// graphqlUser converts a GraphQL author to the REST shape: GraphQL reports a
// bot by its bare slug, while REST and BotLogin use "<slug>[bot]".
func graphqlUser(login, typename string) ghUser {
	if typename == "Bot" {
		if !strings.HasSuffix(strings.ToLower(login), "[bot]") {
			login += "[bot]"
		}
	}
	return ghUser{Login: login, Type: typename}
}
