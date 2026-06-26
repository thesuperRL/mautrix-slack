package reactionmirror

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-slack/pkg/bridgeidentity"
)

const SummaryMarkerKey = "fi.mau.reaction_mirror_summary"
const MirrorReactionTrigger = "!mirror-reaction"

func MessageHasMirrorReactionTrigger(text string) bool {
	return strings.Contains(text, MirrorReactionTrigger)
}

type MatrixMemberLookup func(ctx context.Context, roomID id.RoomID, userID id.UserID) string

type AggregatedReactions map[string][]string

type RelationsClient interface {
	GetRelations(ctx context.Context, roomID id.RoomID, eventID id.EventID, req *mautrix.ReqGetRelations) (*mautrix.RespGetRelations, error)
}

func FetchAnnotationRelations(ctx context.Context, client RelationsClient, roomID id.RoomID, eventID id.EventID) ([]*event.Event, error) {
	if client == nil {
		return nil, fmt.Errorf("matrix client is nil")
	}
	var all []*event.Event
	from := ""
	for page := 0; page < 32; page++ {
		resp, err := client.GetRelations(ctx, roomID, eventID, &mautrix.ReqGetRelations{
			RelationType: event.RelAnnotation,
			From:         from,
		})
		if err != nil {
			return nil, err
		}
		for _, evt := range resp.Chunk {
			if evt.Type == event.EventReaction {
				_ = evt.Content.ParseRaw(evt.Type)
				all = append(all, evt)
			}
		}
		if resp.NextBatch == "" {
			break
		}
		from = resp.NextBatch
	}
	return all, nil
}

func EmojiDisplayKey(evt *event.Event) string {
	content, ok := evt.Content.Parsed.(*event.ReactionEventContent)
	if !ok || content == nil {
		return ""
	}
	key := content.RelatesTo.Key
	if key == "" {
		return ""
	}
	if strings.HasPrefix(key, "mxc://") {
		if sc, ok := evt.Content.Raw["com.beeper.reaction.shortcode"].(string); ok && sc != "" {
			return sc
		}
		if slackRx, ok := evt.Content.Raw["fi.mau.slack.reaction"].(map[string]any); ok {
			if name, ok := slackRx["name"].(string); ok && name != "" {
				return ":" + name + ":"
			}
		}
		if discordRx, ok := evt.Content.Raw["fi.mau.discord.reaction"].(map[string]any); ok {
			if name, ok := discordRx["name"].(string); ok && name != "" {
				return ":" + name + ":"
			}
		}
		return ":emoji:"
	}
	return key
}

func shouldSkipSender(sender id.UserID, excludeMXIDs []id.UserID, excludeNetworkNames func(id.UserID) bool) bool {
	if bridgeidentity.IsDiscordBridgeBot(sender) || bridgeidentity.IsSlackBridgeBot(sender) {
		return true
	}
	if IsReactionMirrorOperator(sender) {
		return true
	}
	for _, excluded := range excludeMXIDs {
		if excluded == sender {
			return true
		}
	}
	if excludeNetworkNames != nil && excludeNetworkNames(sender) {
		return true
	}
	return false
}

func AggregateReactions(
	ctx context.Context,
	client RelationsClient,
	roomID id.RoomID,
	targetMXID id.EventID,
	lookup MatrixMemberLookup,
	excludeMXIDs []id.UserID,
	excludeNetworkNames func(id.UserID) bool,
) (AggregatedReactions, error) {
	events, err := FetchAnnotationRelations(ctx, client, roomID, targetMXID)
	if err != nil {
		return nil, err
	}
	type emojiBucket struct {
		order []string
		names map[string]string
	}
	buckets := make(map[string]*emojiBucket)
	for _, evt := range events {
		if evt.Unsigned.RedactedBecause != nil {
			continue
		}
		if shouldSkipSender(evt.Sender, excludeMXIDs, excludeNetworkNames) {
			continue
		}
		emoji := EmojiDisplayKey(evt)
		if emoji == "" {
			continue
		}
		bucket := buckets[emoji]
		if bucket == nil {
			bucket = &emojiBucket{names: make(map[string]string)}
			buckets[emoji] = bucket
		}
		identityKey := bridgeidentity.LinkedIdentityKey(evt.Sender)
		if _, exists := bucket.names[identityKey]; exists {
			continue
		}
		name := lookup(ctx, roomID, evt.Sender)
		if name == "" {
			name = string(evt.Sender)
		}
		bucket.names[identityKey] = name
		bucket.order = append(bucket.order, identityKey)
	}
	out := make(AggregatedReactions, len(buckets))
	for emoji, bucket := range buckets {
		names := make([]string, 0, len(bucket.order))
		for _, key := range bucket.order {
			names = append(names, bucket.names[key])
		}
		out[emoji] = names
	}
	return out, nil
}

func FormatSummary(reactions AggregatedReactions) string {
	if len(reactions) == 0 {
		return "Reactions\n(none)"
	}
	emojis := make([]string, 0, len(reactions))
	for emoji := range reactions {
		emojis = append(emojis, emoji)
	}
	sort.Strings(emojis)
	var b strings.Builder
	b.WriteString("Reactions\n")
	for _, emoji := range emojis {
		names := reactions[emoji]
		b.WriteString(emoji)
		b.WriteString(" — ")
		b.WriteString(strings.Join(names, ", "))
		if len(names) > 1 {
			fmt.Fprintf(&b, " (%d)", len(names))
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func IsReactionMirrorOperator(mxid id.UserID) bool {
	localpart, _, err := mxid.Parse()
	if err != nil {
		return false
	}
	switch strings.ToLower(localpart) {
	case "reconciler", "bridge-bot", "ap-1":
		return true
	}
	return false
}

func IsSlackBridgeGhost(mxid id.UserID) bool {
	return bridgeidentity.ParseSlackGhostUserID(mxid) != ""
}

func IsDiscordBridgeGhost(mxid id.UserID) bool {
	return bridgeidentity.ParseDiscordGhostMXID(mxid) != ""
}

func reactionHasBridgeSource(evt *event.Event, key string) bool {
	if evt == nil || evt.Content.Raw == nil {
		return false
	}
	_, ok := evt.Content.Raw[key].(map[string]any)
	return ok
}

func IsSlackSourcedReaction(evt *event.Event) bool {
	if evt == nil {
		return false
	}
	if IsSlackBridgeGhost(evt.Sender) {
		return true
	}
	return reactionHasBridgeSource(evt, "fi.mau.slack.reaction")
}

func IsDiscordSourcedReaction(evt *event.Event) bool {
	if evt == nil {
		return false
	}
	if IsDiscordBridgeGhost(evt.Sender) {
		return true
	}
	return reactionHasBridgeSource(evt, "fi.mau.discord.reaction")
}

func FirstCrossBridgeReaction(isCrossBridge func(*event.Event) bool, events []*event.Event) (emoji string, ok bool) {
	sort.Slice(events, func(i, j int) bool {
		return events[i].Timestamp < events[j].Timestamp
	})
	for _, evt := range events {
		if evt.Unsigned.RedactedBecause != nil {
			continue
		}
		if !isCrossBridge(evt) {
			continue
		}
		emoji = EmojiDisplayKey(evt)
		if emoji != "" {
			return emoji, true
		}
	}
	return "", false
}

func IsSummaryMessage(content map[string]any) bool {
	if content == nil {
		return false
	}
	marker, ok := content[SummaryMarkerKey].(bool)
	return ok && marker
}

func SummaryMessageContent(body string) *event.MessageEventContent {
	return &event.MessageEventContent{
		MsgType: event.MsgNotice,
		Body:    body,
	}
}

func SummaryMessageRaw() map[string]any {
	return map[string]any{
		SummaryMarkerKey: true,
	}
}
