package main

import (
	"context"
	"fmt"
	"strings"

	"fanoutd/internal/client"
	"fanoutd/internal/models"
)

// resolveID turns a prefix into a full task ID. Typing a whole UUID by hand is
// the difference between using this and not, so every command that takes an id
// goes through here.
//
// An exact match wins outright; otherwise the prefix must be unique across ids.
// Titles are matched too, but only as a fallback, so a title that happens to
// look like an id cannot shadow one.
func resolveID(ctx context.Context, c *client.Client, prefix string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", fmt.Errorf("task id is required")
	}

	tasks, err := c.ListTasks(ctx)
	if err != nil {
		return "", err
	}

	var byPrefix, byTitle []models.Task
	lower := strings.ToLower(prefix)
	for _, t := range tasks {
		switch {
		case t.ID == prefix:
			return t.ID, nil
		case strings.HasPrefix(t.ID, prefix):
			byPrefix = append(byPrefix, t)
		case strings.Contains(strings.ToLower(t.Title), lower):
			byTitle = append(byTitle, t)
		}
	}

	matches := byPrefix
	if len(matches) == 0 {
		matches = byTitle
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no task matches %q", prefix)
	case 1:
		return matches[0].ID, nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%q matches %d tasks:\n", prefix, len(matches))
		for _, t := range matches {
			fmt.Fprintf(&b, "  %s  %s\n", shortID(t.ID), t.Title)
		}
		b.WriteString("use a longer prefix")
		return "", fmt.Errorf("%s", b.String())
	}
}

// resolveGroupID turns a prefix into a group id, the same bargain resolveID
// makes for tasks.
//
// Groups have no rows of their own — a group is the set of tasks carrying one
// group_id — so the candidates come from the task list, and a group is named by
// any of its subtasks' titles as well as by its id.
func resolveGroupID(ctx context.Context, c *client.Client, prefix string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", fmt.Errorf("group id is required")
	}

	tasks, err := c.ListTasks(ctx)
	if err != nil {
		return "", err
	}

	// Ordered, because the ambiguity report has to be stable.
	var groups []string
	members := map[string][]models.Task{}
	for _, t := range tasks {
		if t.GroupID == "" {
			continue
		}
		if _, seen := members[t.GroupID]; !seen {
			groups = append(groups, t.GroupID)
		}
		members[t.GroupID] = append(members[t.GroupID], t)
	}

	var byPrefix, byTitle []string
	lower := strings.ToLower(prefix)
	for _, id := range groups {
		switch {
		case id == prefix:
			return id, nil
		case strings.HasPrefix(id, prefix):
			byPrefix = append(byPrefix, id)
		case matchesTitle(members[id], lower):
			byTitle = append(byTitle, id)
		}
	}

	matches := byPrefix
	if len(matches) == 0 {
		matches = byTitle
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no breakdown matches %q", prefix)
	case 1:
		return matches[0], nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%q matches %d breakdowns:\n", prefix, len(matches))
		for _, id := range matches {
			fmt.Fprintf(&b, "  %s  %s (%s)\n", shortID(id), members[id][0].Title, plural(len(members[id]), "subtask"))
		}
		b.WriteString("use a longer prefix")
		return "", fmt.Errorf("%s", b.String())
	}
}

func matchesTitle(tasks []models.Task, lower string) bool {
	for _, t := range tasks {
		if strings.Contains(strings.ToLower(t.Title), lower) {
			return true
		}
	}
	return false
}

// shortIDLen is what ls prints and what a user is expected to type back. Seven
// hex characters is the git convention and is unambiguous well past any board
// size this tool will see.
const shortIDLen = 7

func shortID(id string) string {
	if len(id) <= shortIDLen {
		return id
	}
	return id[:shortIDLen]
}
