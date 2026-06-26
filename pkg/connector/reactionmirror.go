package connector

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/slack-go/slack"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-slack/pkg/emoji"
	"go.mau.fi/mautrix-slack/pkg/reactionmirror"
	"go.mau.fi/mautrix-slack/pkg/slackid"
)

var slackReactionMirrorDebouncer = reactionmirror.NewDebouncer(800 * time.Millisecond)
var slackReactionMirrorErrorDebouncer = reactionmirror.NewDebouncer(2 * time.Minute)
var slackReactionMirrorLocks sync.Map
var slackReactionMirrorRefreshing sync.Map

func (s *SlackConnector) reactionMirrorLockKey(ctx context.Context, portal *bridgev2.Portal, targetMXID id.EventID) string {
	_, _, channelID, parentTS, ok := s.resolveReactionMirrorTarget(ctx, portal, targetMXID)
	if ok && channelID != "" && parentTS != "" {
		return string(portal.ID) + "/" + channelID + "/" + parentTS
	}
	return string(portal.ID) + "/" + string(targetMXID)
}

func (s *SlackConnector) ShouldSkipReactionMirrorHook(ctx context.Context, portal *bridgev2.Portal, evt *event.Event) bool {
	if evt == nil {
		return false
	}
	if reactionmirror.IsReactionMirrorOperator(evt.Sender) {
		return true
	}
	if relay := s.relayClient(portal); relay != nil && isRelaySlackGhostMXID(s.br, relay, evt.Sender) {
		return true
	}
	return false
}

func isRelaySlackGhostMXID(br *bridgev2.Bridge, relay *SlackClient, sender id.UserID) bool {
	if br == nil || relay == nil {
		return false
	}
	if ghostID, ok := br.Matrix.ParseGhostMXID(sender); ok {
		_, uid := slackid.ParseUserID(ghostID)
		return uid != "" && strings.EqualFold(uid, relay.UserID)
	}
	return false
}

func (s *SlackConnector) ReactionMirrorHook(ctx context.Context, portal *bridgev2.Portal, targetMXID id.EventID) {
	if portal == nil || targetMXID == "" {
		return
	}
	key := s.reactionMirrorLockKey(ctx, portal, targetMXID)
	if _, refreshing := slackReactionMirrorRefreshing.Load(key); refreshing {
		return
	}
	slackReactionMirrorDebouncer.Schedule(key, func() {
		if _, refreshing := slackReactionMirrorRefreshing.Load(key); refreshing {
			return
		}
		s.refreshReactionMirror(context.Background(), portal, targetMXID)
	})
}

func (s *SlackConnector) summarySlackClient(ctx context.Context, portal *bridgev2.Portal) *SlackClient {
	if sc := s.relayClient(portal); sc != nil {
		return sc
	}
	for _, relayID := range s.br.Config.Relay.DefaultRelays {
		login, err := s.br.GetExistingUserLoginByID(ctx, relayID)
		if err != nil || login == nil || login.Client == nil {
			continue
		}
		if sc, ok := login.Client.(*SlackClient); ok && sc.Client != nil {
			return sc
		}
	}
	return nil
}

func (s *SlackConnector) relayClient(portal *bridgev2.Portal) *SlackClient {
	if portal == nil || portal.Relay == nil {
		return nil
	}
	sc, ok := portal.Relay.Client.(*SlackClient)
	if !ok || sc == nil || sc.Client == nil {
		return nil
	}
	return sc
}

func matrixReactionToSlackEmoji(evt *event.Event) string {
	content, ok := evt.Content.Parsed.(*event.ReactionEventContent)
	if !ok || content == nil {
		return ""
	}
	key := content.RelatesTo.Key
	if strings.HasPrefix(key, "mxc://") {
		if discordRx, ok := evt.Content.Raw["fi.mau.discord.reaction"].(map[string]any); ok {
			if name, ok := discordRx["name"].(string); ok {
				return name
			}
		}
		if slackRx, ok := evt.Content.Raw["fi.mau.slack.reaction"].(map[string]any); ok {
			if name, ok := slackRx["name"].(string); ok {
				return name
			}
		}
		return ""
	}
	if sc := emoji.GetShortcode(key); sc != "" {
		return sc
	}
	return key
}

func slackEmojiMatchesMirror(displayKey, mirrored string) bool {
	if displayKey == mirrored {
		return true
	}
	if strings.HasPrefix(displayKey, ":") {
		return strings.Trim(displayKey, ":") == mirrored
	}
	if sc := emoji.GetShortcode(displayKey); sc != "" {
		return sc == mirrored
	}
	return false
}

func mirroredEmojiReactorCountSlack(aggregated reactionmirror.AggregatedReactions, mirrored string) int {
	for emoji, names := range aggregated {
		if slackEmojiMatchesMirror(emoji, mirrored) {
			return len(names)
		}
	}
	return 0
}

func (s *SlackConnector) matrixRelationsClient() reactionmirror.RelationsClient {
	if as, ok := s.br.Bot.(*matrix.ASIntent); ok && as.Matrix != nil && as.Matrix.Client != nil {
		return as.Matrix.Client
	}
	return nil
}

func (s *SlackConnector) notifyReactionMirrorError(ctx context.Context, portal *bridgev2.Portal, channelID, parentTS, threadTS, summary string) {
	if portal == nil || summary == "" {
		return
	}
	key := string(portal.ID) + "/" + channelID + "/" + parentTS + "/error"
	slackReactionMirrorErrorDebouncer.Schedule(key, func() {
		relay := s.summarySlackClient(context.Background(), portal)
		if relay == nil {
			zerolog.Ctx(ctx).Warn().Str("summary", summary).Msg("Cannot post reaction mirror error to Slack: no relay login")
			return
		}
		body := "Reaction summary error: " + summary
		ts := threadTS
		if ts == "" {
			ts = parentTS
		}
		if _, _, err := relay.Client.PostMessageContext(context.Background(), channelID,
			slack.MsgOptionText(body, false),
			slack.MsgOptionTS(ts),
		); err != nil {
			zerolog.Ctx(ctx).Err(err).Str("summary", summary).Msg("Failed to post reaction mirror error to Slack")
		}
	})
}

func (s *SlackConnector) targetMessageHasMirrorReactionTrigger(ctx context.Context, portal *bridgev2.Portal, targetMXID id.EventID) bool {
	as, ok := s.br.Bot.(*matrix.ASIntent)
	if !ok || as.Matrix == nil || as.Matrix.Client == nil {
		return false
	}
	evt, err := as.Matrix.Client.GetEvent(ctx, portal.MXID, targetMXID)
	if err != nil || evt == nil {
		return false
	}
	_ = evt.Content.ParseRaw(evt.Type)
	content, ok := evt.Content.Parsed.(*event.MessageEventContent)
	return ok && reactionmirror.MessageHasMirrorReactionTrigger(content.Body)
}

func (s *SlackConnector) refreshReactionMirror(ctx context.Context, portal *bridgev2.Portal, targetMXID id.EventID) {
	targetMsg, meta, channelID, parentTS, ok := s.resolveReactionMirrorTarget(ctx, portal, targetMXID)
	if !ok {
		s.notifyReactionMirrorError(ctx, portal, "", "", "", "could not load bridged message")
		return
	}
	if !s.targetMessageHasMirrorReactionTrigger(ctx, portal, targetMXID) {
		return
	}
	lockKey := string(portal.ID) + "/" + channelID + "/" + parentTS
	slackReactionMirrorRefreshing.Store(lockKey, true)
	defer slackReactionMirrorRefreshing.Delete(lockKey)

	lock, _ := slackReactionMirrorLocks.LoadOrStore(lockKey, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

	targetMsg, meta, channelID, parentTS, ok = s.resolveReactionMirrorTarget(ctx, portal, targetMXID)
	if !ok {
		s.notifyReactionMirrorError(ctx, portal, "", "", "", "could not load bridged message")
		return
	}
	relay := s.summarySlackClient(ctx, portal)
	if relay == nil {
		zerolog.Ctx(ctx).Warn().Str("portal_id", string(portal.ID)).Msg("Skipping Slack reaction mirror refresh: no relay or default relay login")
		s.notifyReactionMirrorError(ctx, portal, channelID, parentTS, meta.SummaryThreadTS, "no Slack relay login available")
		return
	}
	client := s.matrixRelationsClient()
	if client == nil {
		s.notifyReactionMirrorError(ctx, portal, channelID, parentTS, meta.SummaryThreadTS, "no Matrix client")
		return
	}
	lookup := func(ctx context.Context, roomID id.RoomID, userID id.UserID) string {
		if member, err := s.br.Matrix.GetMemberInfo(ctx, portal.MXID, userID); err == nil && member != nil && member.Displayname != "" {
			return member.Displayname
		}
		return ""
	}
	aggregated, events, err := s.aggregateMirrorReactions(ctx, client, portal, targetMXID, lookup, relay)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to aggregate reactions for Slack mirror summary")
		s.notifyReactionMirrorError(ctx, portal, channelID, parentTS, meta.SummaryThreadTS, fmt.Sprintf("aggregate reactions: %v", err))
		return
	}

	if meta.MirroredEmoji == "" {
		if _, ok := reactionmirror.FirstCrossBridgeReaction(reactionmirror.IsDiscordSourcedReaction, events); ok {
			for _, evt := range events {
				if evt.Unsigned.RedactedBecause != nil || !reactionmirror.IsDiscordSourcedReaction(evt) {
					continue
				}
				if slackEmoji := matrixReactionToSlackEmoji(evt); slackEmoji != "" {
					meta.MirroredEmoji = slackEmoji
					break
				}
			}
		}
	}

	itemRef := slack.ItemRef{Channel: channelID, Timestamp: parentTS}

	summaryBody := reactionmirror.FormatSummary(aggregated)
	threadTS, summaryTS, err := s.ensureSlackReactionMirrorThread(ctx, relay, channelID, parentTS, meta, summaryBody)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to update Slack reaction mirror summary")
		s.notifyReactionMirrorError(ctx, portal, channelID, parentTS, meta.SummaryThreadTS, fmt.Sprintf("update summary thread: %v", err))
		return
	}
	meta.SummaryThreadTS = threadTS
	meta.SummaryMessageTS = summaryTS
	if err := s.persistReactionMirrorMetadata(ctx, portal, targetMsg.ID, meta); err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to persist Slack reaction mirror metadata")
		return
	}

	if meta.MirroredEmoji != "" {
		count := mirroredEmojiReactorCountSlack(aggregated, meta.MirroredEmoji)
		if count > 0 {
			_ = relay.Client.AddReactionContext(ctx, meta.MirroredEmoji, itemRef)
		} else {
			_ = relay.Client.RemoveReactionContext(ctx, meta.MirroredEmoji, itemRef)
		}
	}
}

func (s *SlackConnector) aggregateMirrorReactions(
	ctx context.Context,
	client reactionmirror.RelationsClient,
	portal *bridgev2.Portal,
	targetMXID id.EventID,
	lookup reactionmirror.MatrixMemberLookup,
	relay *SlackClient,
) (reactionmirror.AggregatedReactions, []*event.Event, error) {
	var aggregated reactionmirror.AggregatedReactions
	var events []*event.Event
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
		}
		events, err = reactionmirror.FetchAnnotationRelations(ctx, client, portal.MXID, targetMXID)
		if err != nil {
			return nil, nil, err
		}
		aggregated, err = reactionmirror.AggregateReactions(ctx, client, portal.MXID, targetMXID, lookup, nil, func(sender id.UserID) bool {
			return isRelaySlackGhostMXID(s.br, relay, sender)
		})
		if err != nil {
			return nil, nil, err
		}
		if len(events) == 0 {
			continue
		}
		if len(aggregated) > 0 || attempt == 5 {
			break
		}
	}
	return aggregated, events, nil
}

func messageMetadata(msg *database.Message) *slackid.MessageMetadata {
	if msg == nil || msg.Metadata == nil {
		return nil
	}
	meta, ok := msg.Metadata.(*slackid.MessageMetadata)
	if !ok {
		return nil
	}
	return meta
}

func (s *SlackConnector) resolveReactionMirrorTarget(ctx context.Context, portal *bridgev2.Portal, targetMXID id.EventID) (*database.Message, *slackid.MessageMetadata, string, string, bool) {
	targetMsg, err := s.br.DB.Message.GetPartByMXID(ctx, targetMXID)
	if err != nil || targetMsg == nil {
		return nil, nil, "", "", false
	}
	_, channelID, parentTS, ok := slackid.ParseMessageID(targetMsg.ID)
	if !ok {
		return nil, nil, "", "", false
	}
	meta := s.mergeReactionMirrorMetadata(ctx, portal, targetMsg.ID, targetMsg)
	if meta == nil {
		meta = &slackid.MessageMetadata{}
		targetMsg.Metadata = meta
	}
	return targetMsg, meta, channelID, parentTS, true
}

func (s *SlackConnector) mergeReactionMirrorMetadata(ctx context.Context, portal *bridgev2.Portal, messageID networkid.MessageID, primary *database.Message) *slackid.MessageMetadata {
	merged := messageMetadata(primary)
	if merged == nil {
		merged = &slackid.MessageMetadata{}
	} else {
		copy := *merged
		merged = &copy
	}
	parts, err := s.br.DB.Message.GetAllPartsByID(ctx, portal.Receiver, messageID)
	if err != nil {
		return merged
	}
	for _, part := range parts {
		pm := messageMetadata(part)
		if pm == nil {
			continue
		}
		if pm.SummaryMessageTS != "" {
			merged.SummaryMessageTS = pm.SummaryMessageTS
		}
		if pm.SummaryThreadTS != "" {
			merged.SummaryThreadTS = pm.SummaryThreadTS
		}
		if pm.MirroredEmoji != "" {
			merged.MirroredEmoji = pm.MirroredEmoji
		}
	}
	return merged
}

func (s *SlackConnector) persistReactionMirrorMetadata(ctx context.Context, portal *bridgev2.Portal, messageID networkid.MessageID, meta *slackid.MessageMetadata) error {
	parts, err := s.br.DB.Message.GetAllPartsByID(ctx, portal.Receiver, messageID)
	if err != nil {
		return err
	}
	for _, part := range parts {
		pm := messageMetadata(part)
		if pm == nil {
			pm = &slackid.MessageMetadata{}
			part.Metadata = pm
		}
		pm.SummaryMessageTS = meta.SummaryMessageTS
		pm.SummaryThreadTS = meta.SummaryThreadTS
		pm.MirroredEmoji = meta.MirroredEmoji
		if err := s.br.DB.Message.Update(ctx, part); err != nil {
			return err
		}
	}
	return nil
}

func isReactionSummaryText(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "Reactions") || strings.HasPrefix(text, "Reaction summary error:")
}

func (s *SlackConnector) ensureSlackReactionMirrorThread(
	ctx context.Context,
	relay *SlackClient,
	channelID, parentTS string,
	meta *slackid.MessageMetadata,
	body string,
) (threadTS, summaryTS string, err error) {
	threadTS = parentTS
	if meta.SummaryThreadTS != "" {
		threadTS = meta.SummaryThreadTS
	}
	summaryTS = s.dedupeSlackReactionSummaryMessages(ctx, relay, channelID, threadTS, meta.SummaryMessageTS)
	meta.SummaryMessageTS = summaryTS
	if summaryTS == "" {
		_, ts, err := relay.Client.PostMessageContext(ctx, channelID,
			slack.MsgOptionText(body, false),
			slack.MsgOptionTS(threadTS),
		)
		if err != nil {
			return threadTS, "", err
		}
		if meta.SummaryThreadTS == "" {
			meta.SummaryThreadTS = threadTS
		}
		return meta.SummaryThreadTS, ts, nil
	}
	_, _, _, err = relay.Client.UpdateMessageContext(ctx, channelID, summaryTS, slack.MsgOptionText(body, false))
	if err != nil {
		if found := s.dedupeSlackReactionSummaryMessages(ctx, relay, channelID, threadTS, ""); found != "" && found != summaryTS {
			summaryTS = found
			meta.SummaryMessageTS = summaryTS
			_, _, _, err = relay.Client.UpdateMessageContext(ctx, channelID, summaryTS, slack.MsgOptionText(body, false))
		}
	}
	if err != nil {
		return threadTS, summaryTS, err
	}
	if meta.SummaryThreadTS == "" {
		meta.SummaryThreadTS = threadTS
	}
	return threadTS, summaryTS, nil
}

func (s *SlackConnector) findAllSlackReactionSummaryMessages(ctx context.Context, relay *SlackClient, channelID, threadTS string) []string {
	if relay == nil || relay.Client == nil || threadTS == "" {
		return nil
	}
	resp, err := relay.Client.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		GetConversationHistoryParameters: slack.GetConversationHistoryParameters{
			ChannelID: channelID,
			Limit:     50,
		},
		Timestamp: threadTS,
	})
	if err != nil || resp == nil {
		return nil
	}
	var out []string
	for _, msg := range resp.Messages {
		if msg.Timestamp == threadTS {
			continue
		}
		if isReactionSummaryText(msg.Text) {
			out = append(out, msg.Timestamp)
		}
	}
	sort.Strings(out)
	return out
}

func (s *SlackConnector) dedupeSlackReactionSummaryMessages(ctx context.Context, relay *SlackClient, channelID, threadTS, preferredTS string) string {
	all := s.findAllSlackReactionSummaryMessages(ctx, relay, channelID, threadTS)
	if len(all) == 0 {
		return preferredTS
	}
	keep := all[0]
	if preferredTS != "" {
		for _, ts := range all {
			if ts == preferredTS {
				keep = ts
				break
			}
		}
	}
	for _, ts := range all {
		if ts == keep {
			continue
		}
		_, _, _ = relay.Client.DeleteMessageContext(ctx, channelID, ts)
	}
	return keep
}

func (s *SlackConnector) findSlackReactionSummaryMessage(ctx context.Context, relay *SlackClient, channelID, threadTS string) string {
	all := s.findAllSlackReactionSummaryMessages(ctx, relay, channelID, threadTS)
	if len(all) == 0 {
		return ""
	}
	return all[0]
}

func (s *SlackConnector) isReactionMirrorSummaryMessage(portal *bridgev2.Portal, msg *slack.Msg) bool {
	if msg == nil || msg.ThreadTimestamp == "" {
		return false
	}
	return isReactionSummaryText(msg.Text)
}

func (s *SlackConnector) isMatrixReactionCrossBridge(evt *event.Event) bool {
	if reactionmirror.IsDiscordSourcedReaction(evt) {
		return true
	}
	return reactionmirror.IsSlackBridgeGhost(evt.Sender)
}

func (s *SlackConnector) isMatrixReactionCrossBridgeSender(sender id.UserID) bool {
	return reactionmirror.IsDiscordBridgeGhost(sender) || reactionmirror.IsSlackBridgeGhost(sender)
}
