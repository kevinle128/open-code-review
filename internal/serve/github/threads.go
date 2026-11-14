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

// Ownership and the finding id are read off the first comment, the root.
// The later comments are the replies; a thread with more than 100 comments
// lists only the first 100, which is enough to read why a finding was
// disputed.
const reviewThreadsQuery = `
query($owner: String!, $repo: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          isResolved
          path
          line
          originalLine
          comments(first: 100) { nodes { body author { login __typename } } }
        }
      }
    }
  }
}`

type threadComment struct {
	Body   string `json:"body"`
	Author *struct {
		Login    string `json:"login"`
		Typename string `json:"__typename"`
	} `json:"author"`
}

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
						ID           string `json:"id"`
						IsResolved   bool   `json:"isResolved"`
						Path         string `json:"path"`
						Line         *int   `json:"line"`
						OriginalLine *int   `json:"originalLine"`
						Comments     struct {
							Nodes []threadComment `json:"nodes"`
						} `json:"comments"`
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
			if len(n.Comments.Nodes) == 0 {
				continue
			}
			root := n.Comments.Nodes[0]
			if !byBot(root, bot) {
				continue
			}
			id, ok := forge.ParseFindingID(root.Body)
			if !ok {
				continue
			}
			t := forge.Thread{ID: n.ID, FindingID: id, Resolved: n.IsResolved, Path: n.Path, Body: root.Body}
			// line is null once the thread is outdated by a later push.
			switch {
			case n.Line != nil:
				t.Line = *n.Line
			case n.OriginalLine != nil:
				t.Line = *n.OriginalLine
			}
			for _, c := range n.Comments.Nodes[1:] {
				if byBot(c, bot) {
					continue
				}
				author := ""
				if c.Author != nil {
					author = c.Author.Login
				}
				t.Replies = append(t.Replies, forge.Reply{Author: author, Body: c.Body})
			}
			threads = append(threads, t)
		}
		if !rt.PageInfo.HasNextPage {
			return threads, nil
		}
		end := rt.PageInfo.EndCursor
		cursor = &end
	}
}

func byBot(c threadComment, bot string) bool {
	return c.Author != nil && ownedBy(graphqlUser(c.Author.Login, c.Author.Typename), bot)
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
