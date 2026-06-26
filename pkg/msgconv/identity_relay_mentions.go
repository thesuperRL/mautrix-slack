// Patched into mautrix-slack pkg/msgconv (package msgconv).
package msgconv

import (
	"context"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/slack-go/slack"
	"go.mau.fi/mautrix-slack/pkg/slackid"
)


func (mc *MessageConverter) matrixReplyToUser(ctx context.Context, content *event.MessageEventContent) id.UserID {
	if content == nil {
		return ""
	}
	rel := content.GetRelatesTo()
	if rel == nil {
		return ""
	}
	replyToMXID := rel.GetNonFallbackReplyTo()
	if replyToMXID == "" {
		return ""
	}
	replyTo, err := mc.Bridge.DB.Message.GetPartByMXID(ctx, replyToMXID)
	if err != nil || replyTo == nil {
		return ""
	}
	return replyTo.SenderMXID
}

func (mc *MessageConverter) ctxWithSlackReplyParent(ctx context.Context, portal *bridgev2.Portal, msg *slack.Msg) context.Context {
	parentTS := msg.ThreadTimestamp
	if parentTS == "" || parentTS == msg.Timestamp {
		return ctx
	}
	return context.WithValue(ctx, contextKeySlackParentTS, parentTS)
}

func (mc *MessageConverter) relaySlackUserID(ctx context.Context) string {
	portal, _ := ctx.Value(contextKeyPortal).(*bridgev2.Portal)
	if portal == nil || portal.Relay == nil {
		return ""
	}
	_, userID := slackid.ParseUserLoginID(portal.Relay.ID)
	return userID
}

func (mc *MessageConverter) matrixSenderForSlackParent(ctx context.Context) id.UserID {
	portal, _ := ctx.Value(contextKeyPortal).(*bridgev2.Portal)
	parentTS, _ := ctx.Value(contextKeySlackParentTS).(string)
	if portal == nil || parentTS == "" {
		return ""
	}
	teamID, channelID := slackid.ParsePortalID(portal.ID)
	msgKey := slackid.MakeMessageID(teamID, channelID, parentTS)
	msg, err := mc.Bridge.DB.Message.GetFirstPartByID(ctx, portal.Receiver, msgKey)
	if err != nil || msg == nil {
		return ""
	}
	return msg.SenderMXID
}
