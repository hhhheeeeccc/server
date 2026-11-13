package audit_file

import (
	"encoding/json"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/store"
	"github.com/mattermost/mattermost/server/v8/channels/web"
)

// RecordContext is derived from current server data. The original audit record
// remains unchanged in Page.Records, including when a user or channel is gone.
type RecordContext struct {
	Actor      Identity   `json:"actor"`
	Channel    *Place     `json:"channel,omitempty"`
	Recipients []Identity `json:"recipients,omitempty"`
	TargetUser *Identity  `json:"target_user,omitempty"`
}

type Identity struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Username string `json:"username,omitempty"`
}

type Place struct {
	ID   string            `json:"id"`
	Name string            `json:"name,omitempty"`
	Type model.ChannelType `json:"type,omitempty"`
}

type recordRefs struct {
	actorID   string
	channelID string
	postID    string
	targetID  string
}

func recipientIDs(channel *model.Channel, actorID string) []string {
	if channel == nil || (channel.Type != model.ChannelTypeDirect && channel.Type != model.ChannelTypeGroup) {
		return nil
	}
	var ids []string
	for _, id := range strings.Split(channel.Name, "__") {
		if id != actorID && model.IsValidId(id) {
			ids = append(ids, id)
		}
	}
	return ids
}

func extractRefs(raw json.RawMessage) recordRefs {
	var record model.AuditRecord
	if json.Unmarshal(raw, &record) != nil {
		return recordRefs{}
	}
	refs := recordRefs{actorID: record.Actor.UserId}
	data := record.EventData
	for _, source := range []map[string]any{object(data.Parameters, "post"), data.ResultState, data.PriorState, data.Parameters} {
		if refs.channelID == "" {
			refs.channelID = stringValue(source, "channel_id")
		}
		if refs.postID == "" {
			refs.postID = stringValue(source, "post_id")
		}
	}
	if refs.postID == "" && data.ObjectType == "post" {
		refs.postID = stringValue(data.ResultState, "id")
	}
	if refs.channelID == "" && data.ObjectType == "channel" {
		refs.channelID = stringValue(data.ResultState, "id")
		if refs.channelID == "" {
			refs.channelID = stringValue(data.PriorState, "id")
		}
	}
	refs.targetID = stringValue(data.Parameters, "target_user_id")
	if refs.targetID == "" && data.ObjectType != "post" {
		refs.targetID = stringValue(data.Parameters, "user_id")
	}
	if refs.targetID == "" && data.ObjectType == "user" {
		refs.targetID = stringValue(data.ResultState, "id")
		if refs.targetID == "" {
			refs.targetID = stringValue(data.PriorState, "id")
		}
	}
	if refs.targetID == refs.actorID {
		refs.targetID = ""
	}
	return refs
}

func object(source map[string]any, key string) map[string]any {
	value, _ := source[key].(map[string]any)
	return value
}

func stringValue(source map[string]any, key string) string {
	value, _ := source[key].(string)
	return value
}

func identity(id string, users map[string]*model.User) Identity {
	result := Identity{ID: id}
	if user := users[id]; user != nil {
		result.Username = user.Username
		first, last := strings.TrimSpace(user.FirstName), strings.TrimSpace(user.LastName)
		if first != "" && last != "" {
			result.Name = first + " " + last
		} else {
			result.Name = user.Username
		}
	}
	if result.Name == "" {
		result.Name = id
	}
	return result
}

func enrichPage(c *web.Context, page *Page) {
	refs := make([]recordRefs, len(page.Records))
	channels := make(map[string]*model.Channel)
	userIDs := make(map[string]struct{})
	for i, raw := range page.Records {
		refs[i] = extractRefs(raw)
		ref := &refs[i]
		if ref.channelID == "" && model.IsValidId(ref.postID) {
			if post, err := c.App.GetSinglePost(c.AppContext, ref.postID, true); err == nil {
				ref.channelID = post.ChannelId
			}
		}
		for _, id := range []string{ref.actorID, ref.targetID} {
			if model.IsValidId(id) {
				userIDs[id] = struct{}{}
			}
		}
		if model.IsValidId(ref.channelID) {
			if _, found := channels[ref.channelID]; !found {
				channel, err := c.App.GetChannel(c.AppContext, ref.channelID)
				if err == nil {
					channels[ref.channelID] = channel
				}
			}
			if channel := channels[ref.channelID]; channel != nil {
				for _, id := range recipientIDs(channel, ref.actorID) {
					userIDs[id] = struct{}{}
				}
			}
		}
	}

	users := make(map[string]*model.User)
	ids := make([]string, 0, len(userIDs))
	for id := range userIDs {
		ids = append(ids, id)
	}
	for start := 0; start < len(ids); start += 100 {
		end := min(start+100, len(ids))
		found, err := c.App.GetUsersByIds(c.AppContext, ids[start:end], &store.UserGetByIdsOpts{})
		if err != nil {
			continue
		}
		for _, user := range found {
			users[user.Id] = user
		}
	}

	page.Contexts = make([]RecordContext, len(refs))
	for i, ref := range refs {
		context := RecordContext{Actor: identity(ref.actorID, users)}
		if ref.targetID != "" {
			target := identity(ref.targetID, users)
			context.TargetUser = &target
		}
		if ref.channelID != "" {
			place := &Place{ID: ref.channelID}
			if channel := channels[ref.channelID]; channel != nil {
				place.Type = channel.Type
				if channel.Type == model.ChannelTypeDirect || channel.Type == model.ChannelTypeGroup {
					for _, id := range recipientIDs(channel, ref.actorID) {
						context.Recipients = append(context.Recipients, identity(id, users))
					}
				} else {
					place.Name = channel.DisplayName
					if place.Name == "" {
						place.Name = channel.Name
					}
				}
			}
			context.Channel = place
		}
		page.Contexts[i] = context
	}
}
