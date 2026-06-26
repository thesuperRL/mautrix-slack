// mautrix-slack - A Matrix-Slack puppeting bridge.
// Copyright (C) 2024 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package msgconv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"image"
	"regexp"
	"strings"

	"github.com/rs/zerolog"
	"github.com/slack-go/slack"
	"go.mau.fi/util/ffmpeg"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-slack/pkg/bridgeidentity"
	"go.mau.fi/mautrix-slack/pkg/slackid"
)

var (
	ErrUnknownMsgType       = errors.New("unknown msgtype")
	ErrMediaDownloadFailed  = errors.New("failed to download media")
	ErrMediaUploadFailed    = errors.New("failed to reupload media")
	ErrMediaConvertFailed   = errors.New("failed to re-encode media")
	ErrMediaOnlyEditCaption = errors.New("only media message caption can be edited")
)

func appendLinkPreviewURLs(content *event.MessageEventContent, previews []*event.BeeperLinkPreview) {
	content.EnsureHasHTML()
	for _, lp := range previews {
		if lp == nil || lp.MatchedURL == "" {
			continue
		}
		if strings.Contains(content.Body, lp.MatchedURL) || strings.Contains(content.FormattedBody, lp.MatchedURL) {
			continue
		}
		escaped := html.EscapeString(lp.MatchedURL)
		content.FormattedBody += fmt.Sprintf(`<p><a href="%s">%s</a></p>`, escaped, escaped)
		if content.Body != "" {
			content.Body += "\n"
		}
		content.Body += lp.MatchedURL
	}
}

func (mc *MessageConverter) matrixUserDisplayName(ctx context.Context, portal *bridgev2.Portal, mxid id.UserID) string {
	if member, err := mc.Bridge.Matrix.GetMemberInfo(ctx, portal.MXID, mxid); err == nil && member != nil && member.Displayname != "" {
		return member.Displayname
	}
	return ""
}

func replaceMatrixUserMentionHTML(formattedBody string, userID id.UserID, label string) string {
	escapedLabel := html.EscapeString(label)
	mxid := regexp.QuoteMeta(string(userID))
	re := regexp.MustCompile(`<a\b[^>]*` + mxid + `[^>]*>[^<]*</a>`)
	return re.ReplaceAllString(formattedBody, escapedLabel)
}

func (mc *MessageConverter) replaceMatrixMentionsWithLabels(ctx context.Context, portal *bridgev2.Portal, content *event.MessageEventContent) {
	if content.Mentions == nil {
		return
	}
	content.EnsureHasHTML()
	var replyToMXID id.EventID
	if rel := content.GetRelatesTo(); rel != nil {
		replyToMXID = rel.GetNonFallbackReplyTo()
	}
	var replyToUser id.UserID
	if replyToMXID != "" {
		replyToUser = mc.matrixReplyToUser(ctx, content)
	}
	for _, userID := range content.Mentions.UserIDs {
		if bridgeidentity.SlackUserIDForMXID(userID) != "" || bridgeidentity.DiscordIDForMXID(userID) != "" {
			continue
		}
		if replyToUser != "" && (bridgeidentity.IsSlackBridgeBot(userID) || bridgeidentity.IsDiscordBridgeBot(userID)) {
			if bridgeidentity.SlackUserIDForMXID(replyToUser) != "" || bridgeidentity.DiscordIDForMXID(replyToUser) != "" {
				continue
			}
		}
		name := mc.matrixUserDisplayName(ctx, portal, userID)
		if name == "" {
			continue
		}
		label := "[" + name + "]"
		content.FormattedBody = replaceMatrixUserMentionHTML(content.FormattedBody, userID, label)
		content.Body = strings.ReplaceAll(content.Body, name, label)
	}
}

func relaySlackProfileOptions(mc *MessageConverter, origSender *bridgev2.OrigSender) []slack.MsgOption {
	options := []slack.MsgOption{slack.MsgOptionUsername(origSender.FormattedName)}
	urlProvider, ok := mc.Bridge.Matrix.(bridgev2.MatrixConnectorWithPublicMedia)
	avatarURL := origSender.AvatarURL
	if origSender.PerMessageProfile.AvatarURL != nil && *origSender.PerMessageProfile.AvatarURL != "" {
		avatarURL = *origSender.PerMessageProfile.AvatarURL
	}
	if ok && avatarURL != "" {
		if publicAvatarURL := urlProvider.GetPublicMediaAddress(avatarURL); publicAvatarURL != "" {
			options = append(options, slack.MsgOptionIconURL(publicAvatarURL))
		}
	}
	return options
}

func relaySlackUploadInitialComment(origSender *bridgev2.OrigSender, caption string) string {
	if origSender == nil || origSender.FormattedName == "" {
		return caption
	}
	if caption == "" {
		return origSender.FormattedName
	}
	return origSender.FormattedName + ": " + caption
}

func shouldBroadcastDiscordReplyToChannel(content *event.MessageEventContent) bool {
	rel := content.GetRelatesTo()
	if rel == nil {
		return false
	}
	if rel.GetThreadParent() != "" {
		return false
	}
	return rel.GetNonFallbackReplyTo() != ""
}

func appendSlackThreadOptions(options []slack.MsgOption, threadRootID string, content *event.MessageEventContent) []slack.MsgOption {
	if threadRootID == "" {
		return options
	}
	options = append(options, slack.MsgOptionTS(threadRootID))
	if shouldBroadcastDiscordReplyToChannel(content) {
		options = append(options, slack.MsgOptionBroadcast())
	}
	return options
}

func isMediaMsgtype(msgType event.MessageType) bool {
	return msgType == event.MsgImage || msgType == event.MsgAudio || msgType == event.MsgVideo || msgType == event.MsgFile
}

type ConvertedSlackMessage struct {
	SendReq    slack.MsgOption
	FileUpload *slack.UploadFileV2Parameters
	FileShare  *slack.ShareFileParams
}

func (mc *MessageConverter) ToSlack(
	ctx context.Context,
	client *slack.Client,
	portal *bridgev2.Portal,
	content *event.MessageEventContent,
	evt *event.Event,
	threadRoot *database.Message,
	editTarget *database.Message,
	origSender *bridgev2.OrigSender,
	isRealUser bool,
) (conv *ConvertedSlackMessage, err error) {
	log := zerolog.Ctx(ctx)

	if evt.Type == event.EventSticker {
		// Slack doesn't have stickers, just bridge stickers as images
		content.MsgType = event.MsgImage
	}

	var editTargetID, threadRootID string
	if editTarget != nil {
		if isMediaMsgtype(content.MsgType) {
			content.MsgType = event.MsgText
			if content.FileName == "" || content.FileName == content.Body {
				return nil, ErrMediaOnlyEditCaption
			}
		}
		var ok bool
		_, _, editTargetID, ok = slackid.ParseMessageID(editTarget.ID)
		if !ok {
			return nil, fmt.Errorf("failed to parse edit target ID")
		}
	}
	if threadRoot != nil {
		threadRootMessageID := threadRoot.ID
		if threadRoot.ThreadRoot != "" {
			threadRootMessageID = threadRoot.ThreadRoot
		}
		var ok bool
		_, _, threadRootID, ok = slackid.ParseMessageID(threadRootMessageID)
		if !ok {
			return nil, fmt.Errorf("failed to parse thread root ID")
		}
	}

	switch content.MsgType {
	case event.MsgText, event.MsgEmote, event.MsgNotice:
		if origSender != nil {
			appendLinkPreviewURLs(content, content.BeeperLinkPreviews)
			if content.Mentions != nil {
				content.Mentions.UserIDs = bridgeidentity.DedupeLinkedMentions(content.Mentions.UserIDs)
			}
			mc.replaceMatrixMentionsWithLabels(ctx, portal, content)
		}
		replyToUser := mc.matrixReplyToUser(ctx, content)
		options := make([]slack.MsgOption, 0, 4)
		var block slack.Block
		if content.Format == event.FormatHTML {
			block = mc.MatrixHTMLParser.Parse(ctx, content.FormattedBody, content.Mentions, portal, replyToUser)
		} else {
			block = mc.MatrixHTMLParser.ParseText(ctx, content.Body, content.Mentions, portal, replyToUser)
		}
		options = append(options, slack.MsgOptionBlocks(block))
		if editTargetID != "" {
			options = append(options, slack.MsgOptionUpdate(editTargetID))
		} else {
			options = appendSlackThreadOptions(options, threadRootID, content)
		}
		if content.MsgType == event.MsgEmote {
			options = append(options, slack.MsgOptionMeMessage())
		}
		if content.BeeperLinkPreviews != nil && len(content.BeeperLinkPreviews) == 0 {
			options = append(options, slack.MsgOptionDisableLinkUnfurl(), slack.MsgOptionDisableMediaUnfurl())
		}
		if origSender != nil {
			options = append(options, relaySlackProfileOptions(mc, origSender)...)
		}
		return &ConvertedSlackMessage{SendReq: slack.MsgOptionCompose(options...)}, nil
	case event.MsgAudio, event.MsgFile, event.MsgImage, event.MsgVideo:
		data, err := mc.Bridge.Bot.DownloadMedia(ctx, content.URL, content.File)
		if err != nil {
			log.Err(err).Msg("Failed to download Matrix attachment")
			return nil, ErrMediaDownloadFailed
		}

		var filename, caption, captionHTML, subtype string
		if content.FileName == "" || content.FileName == content.Body {
			filename = content.Body
		} else {
			filename = content.FileName
			caption = content.Body
			captionHTML = content.FormattedBody
		}
		if content.MSC3245Voice != nil && content.Info.MimeType != "audio/webm; codecs=opus" && ffmpeg.Supported() {
			data, err = ffmpeg.ConvertBytes(ctx, data, ".webm", []string{}, []string{"-c:a", "copy"}, content.Info.MimeType)
			if err != nil {
				log.Err(err).Msg("Failed to convert voice message")
				return nil, ErrMediaConvertFailed
			}
			// Slack web will upload a webm/opus audio file with a .m4a extension.
			// Slack servers appear to then convert it to m4a with aac.
			filename += ".m4a"
			content.Info.MimeType = "audio/webm; codecs=opus"
			subtype = "slack_audio"
		} else if content.MSC3245Voice != nil && (content.Info.MimeType == "audio/webm; codecs=opus" || content.Info.MimeType == "audio/mp4") {
			subtype = "slack_audio"
			if !strings.HasSuffix(filename, ".m4a") {
				filename += ".m4a"
			}
		}
		_, channelID := slackid.ParsePortalID(portal.ID)
		if !isRealUser {
			fileUpload := &slack.UploadFileV2Parameters{
				Filename:        filename,
				Reader:          bytes.NewReader(data),
				FileSize:        len(data),
				Channel:         channelID,
				ThreadTimestamp: threadRootID,
			}
			if comment := relaySlackUploadInitialComment(origSender, caption); comment != "" {
				fileUpload.InitialComment = comment
			}
			return &ConvertedSlackMessage{FileUpload: fileUpload}, nil
		} else {
			resp, err := client.GetFileUploadURL(ctx, slack.GetFileUploadURLParameters{
				Filename: filename,
				Length:   len(data),
				SubType:  subtype,
			})
			if err != nil {
				log.Err(err).Msg("Failed to get file upload URL")
				return nil, ErrMediaUploadFailed
			}
			err = client.UploadToURLB(ctx, resp, content.Info.MimeType, data)
			if err != nil {
				log.Err(err).Msg("Failed to upload file")
				return nil, ErrMediaUploadFailed
			}
			err = client.CompleteFileUpload(ctx, resp)
			if err != nil {
				log.Err(err).Msg("Failed to complete file upload")
				return nil, ErrMediaUploadFailed
			}
			var block slack.Block
			if captionHTML != "" {
				block = mc.MatrixHTMLParser.Parse(ctx, content.FormattedBody, content.Mentions, portal, mc.matrixReplyToUser(ctx, content))
			} else if caption != "" {
				block = slack.NewRichTextBlock("", slack.NewRichTextSection(slack.NewRichTextSectionTextElement(caption, nil)))
			}
			fileShare := &slack.ShareFileParams{
				Files:    []string{resp.File},
				Channel:  channelID,
				ThreadTS: threadRootID,
			}
			if block != nil {
				fileShare.Blocks = []slack.Block{block}
			}
			return &ConvertedSlackMessage{FileShare: fileShare}, nil
		}
	default:
		return nil, ErrUnknownMsgType
	}
}

func (mc *MessageConverter) uploadMedia(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, data []byte, content *event.MessageEventContent) error {
	content.Info.Size = len(data)
	if content.Info.Width == 0 && content.Info.Height == 0 && strings.HasPrefix(content.Info.MimeType, "image/") {
		cfg, _, _ := image.DecodeConfig(bytes.NewReader(data))
		content.Info.Width, content.Info.Height = cfg.Width, cfg.Height
	}

	mxc, file, err := intent.UploadMedia(ctx, portal.MXID, data, "", content.Info.MimeType)
	if err != nil {
		return err
	}
	if file != nil {
		content.File = file
	} else {
		content.URL = mxc
	}
	return nil
}
