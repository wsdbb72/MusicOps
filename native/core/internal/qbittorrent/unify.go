package qbittorrent

import (
	"context"
	"strings"
)

// UnifyLegacyTags adds the project tag to torrents that still carry a
// per-task tag from older builds, then removes that old tag from the torrent.
// It does not delete a tag that another torrent still uses, and it does not
// start or move a download.
func (c *Client) UnifyLegacyTags(ctx context.Context) (int, error) {
	rows, err := c.Info(ctx)
	if err != nil {
		return 0, err
	}
	changed := 0
	removed := map[string]bool{}
	for _, row := range rows {
		for _, tag := range strings.Split(row.Tags, ",") {
			tag = strings.TrimSpace(tag)
			if !strings.HasPrefix(tag, "musicops-task-") {
				continue
			}
			if err = c.AddTags(ctx, row.Hash, ProjectTag); err != nil {
				return changed, err
			}
			if err = c.RemoveTags(ctx, row.Hash, tag); err != nil {
				return changed, err
			}
			removed[tag] = true
			changed++
		}
	}
	if len(removed) == 0 {
		return changed, nil
	}
	again, err := c.Info(ctx)
	if err != nil {
		return changed, err
	}
	for _, row := range again {
		for _, tag := range strings.Split(row.Tags, ",") {
			delete(removed, strings.TrimSpace(tag))
		}
	}
	for tag := range removed {
		if err = c.DeleteTags(ctx, tag); err != nil {
			return changed, err
		}
	}
	return changed, nil
}
