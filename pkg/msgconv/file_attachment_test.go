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
	"testing"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"

	"go.mau.fi/mautrix-slack/pkg/slackid"
)

// TestSlackFileAttachmentToMatrix verifies two things for Slack file attachment bridging:
// 1. The Matrix event type (m.image / m.video / m.file) is selected correctly based on MIME type,
//    covering GIF and other image types.
// 2. The network user ID derived from the Slack team+user pair is correct. The bridge
//    framework uses this network ID to look up (or create) the ghost user whose MXID
//    becomes the sender of the Matrix event, so a wrong value here means the event would
//    carry the wrong sender MXID.
func TestSlackFileAttachmentToMatrix(t *testing.T) {
	t.Run("event type by MIME", func(t *testing.T) {
		cases := []struct {
			name     string
			file     slack.File
			wantType event.MessageType
		}{
			{
				name:     "GIF is m.image",
				file:     slack.File{Name: "anim.gif", Mimetype: "image/gif", Size: 10000},
				wantType: event.MsgImage,
			},
			{
				name:     "JPEG is m.image",
				file:     slack.File{Name: "photo.jpg", Mimetype: "image/jpeg", OriginalW: 1920, OriginalH: 1080, Size: 200000},
				wantType: event.MsgImage,
			},
			{
				name:     "PNG is m.image",
				file:     slack.File{Name: "screenshot.png", Mimetype: "image/png", Size: 50000},
				wantType: event.MsgImage,
			},
			{
				name:     "MP4 is m.video",
				file:     slack.File{Name: "clip.mp4", Mimetype: "video/mp4", Size: 5000000},
				wantType: event.MsgVideo,
			},
			{
				name:     "PDF is m.file",
				file:     slack.File{Name: "doc.pdf", Mimetype: "application/pdf", Size: 100000},
				wantType: event.MsgFile,
			},
			{
				name:     "plain text is m.file",
				file:     slack.File{Name: "notes.txt", Mimetype: "text/plain", Size: 512},
				wantType: event.MsgFile,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				content := convertSlackFileMetadata(&tc.file)
				assert.Equal(t, tc.wantType, content.MsgType,
					"MIME %q should produce %q", tc.file.Mimetype, tc.wantType)
				assert.Equal(t, tc.file.Name, content.Body)
				if assert.NotNil(t, content.Info) {
					assert.Equal(t, tc.file.Mimetype, content.Info.MimeType)
					assert.Equal(t, tc.file.Size, content.Info.Size)
				}
			})
		}
	})

	t.Run("image dimensions preserved", func(t *testing.T) {
		file := slack.File{Name: "wide.jpg", Mimetype: "image/jpeg", OriginalW: 1920, OriginalH: 1080, Size: 150000}
		content := convertSlackFileMetadata(&file)
		assert.Equal(t, event.MsgImage, content.MsgType)
		assert.Equal(t, 1920, content.Info.Width)
		assert.Equal(t, 1080, content.Info.Height)
	})

	// Test that the network user ID, which the bridge uses to look up the ghost and
	// construct the sender MXID, is derived correctly from the Slack team and user IDs.
	// A file attachment sent by Slack user userID in team teamID must be attributed to
	// the ghost whose MXID the bridge builds from slackid.MakeUserID(teamID, userID).
	t.Run("sender network ID for MXID construction", func(t *testing.T) {
		cases := []struct {
			teamID    string
			userID    string
			wantNetID string
		}{
			{"T123ABC", "U456DEF", "t123abc-u456def"},
			{"TTEAM01", "UUSER01", "tteam01-uuser01"},
		}
		for _, tc := range cases {
			t.Run(tc.teamID+"/"+tc.userID, func(t *testing.T) {
				netID := slackid.MakeUserID(tc.teamID, tc.userID)
				assert.Equal(t, tc.wantNetID, string(netID),
					"wrong ghost network ID: file attachment would carry the wrong sender MXID")
			})
		}
	})
}

// TestDiscordFileAttachmentToSlack verifies bridging of Discord file attachments into
// Slack. Discord attachments arrive on Matrix as m.image / m.video / m.file / m.audio
// events (bridged by mautrix-discord) and are forwarded to Slack via relay with an
// OrigSender carrying the Discord user name.
//
// The test covers two properties:
// 1. The Matrix media message type is recognised as a file upload (not a text message).
// 2. The sender name from the OrigSender is prepended to the Slack initial comment,
//    so the Slack channel shows who on Discord sent the attachment.
func TestDiscordFileAttachmentToSlack(t *testing.T) {
	t.Run("media type detection", func(t *testing.T) {
		mediaMsgTypes := []event.MessageType{
			event.MsgImage,
			event.MsgVideo,
			event.MsgFile,
			event.MsgAudio,
		}
		for _, mt := range mediaMsgTypes {
			assert.True(t, isMediaMsgtype(mt),
				"Discord file attachment MsgType %q must be treated as media for Slack upload", mt)
		}

		nonMediaMsgTypes := []event.MessageType{
			event.MsgText,
			event.MsgEmote,
			event.MsgNotice,
		}
		for _, mt := range nonMediaMsgTypes {
			assert.False(t, isMediaMsgtype(mt),
				"MsgType %q must NOT be treated as a file attachment", mt)
		}
	})

	t.Run("sender name prepended to Slack initial comment", func(t *testing.T) {
		cases := []struct {
			name        string
			origSender  *bridgev2.OrigSender
			caption     string
			wantComment string
		}{
			{
				name:        "Discord sender with caption",
				origSender:  &bridgev2.OrigSender{FormattedName: "DiscordUser#1234"},
				caption:     "Check this image",
				wantComment: "DiscordUser#1234: Check this image",
			},
			{
				name:        "Discord sender no caption",
				origSender:  &bridgev2.OrigSender{FormattedName: "SomeUser"},
				caption:     "",
				wantComment: "SomeUser",
			},
			{
				name:        "no origSender (direct bridge, not relay)",
				origSender:  nil,
				caption:     "caption only",
				wantComment: "caption only",
			},
			{
				name:        "empty sender name falls back to caption",
				origSender:  &bridgev2.OrigSender{FormattedName: ""},
				caption:     "caption only",
				wantComment: "caption only",
			},
			{
				name:        "no sender no caption yields empty string",
				origSender:  nil,
				caption:     "",
				wantComment: "",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := relaySlackUploadInitialComment(tc.origSender, tc.caption)
				assert.Equal(t, tc.wantComment, got)
			})
		}
	})
}
