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

package connector

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"maunium.net/go/mautrix/event"
)

// simulateCapGate mirrors the logic of bridgev2.Portal.checkMessageContentCaps in mautrix-go:
//
//	capType = content.GetCapMsgType()
//	feat    = caps.File[capType]        (missing → unsupported message type)
//	if content.Info.MimeType != "" && feat.GetMimeSupport(mime).Reject() → rejected
//
// Any mismatch between what we declare in roomCaps and what we actually accept shows
// up as a test failure here, before it reaches the live bridge.
func simulateCapGate(caps *event.RoomFeatures, content *event.MessageEventContent) error {
	capType := content.GetCapMsgType()
	feat, ok := caps.File[capType]
	if !ok {
		return fmt.Errorf("unsupported message type (%s)", capType)
	}
	if content.Info != nil && content.Info.MimeType != "" {
		if feat.GetMimeSupport(content.Info.MimeType).Reject() {
			return fmt.Errorf("unsupported media type (%s in %s)", content.Info.MimeType, capType)
		}
	}
	return nil
}

// TestCapabilityGate_GetCapMsgType verifies that MessageEventContent.GetCapMsgType returns
// the right capability type for each combination the bridge handles.  A wrong mapping here
// means the gate looks up the wrong FileFeatures row and either wrongly rejects or wrongly
// accepts the message.
func TestCapabilityGate_GetCapMsgType(t *testing.T) {
	cases := []struct {
		name        string
		content     event.MessageEventContent
		wantCapType event.CapabilityMsgType
	}{
		{
			name:        "m.image → MsgImage",
			content:     event.MessageEventContent{MsgType: event.MsgImage},
			wantCapType: event.MsgImage,
		},
		{
			name:        "m.video plain → MsgVideo",
			content:     event.MessageEventContent{MsgType: event.MsgVideo, Info: &event.FileInfo{}},
			wantCapType: event.MsgVideo,
		},
		{
			// This is the exact scenario that caused the live bridge to reject
			// Discord GIFs: m.video with fi.mau.gif=true must map to CapMsgGIF, not MsgVideo.
			// If GetCapMsgType returns MsgVideo here, roomCaps.File[MsgVideo] is used,
			// which does not list video/mp4, and the gate rejects the message.
			name: "m.video + fi.mau.gif=true → CapMsgGIF",
			content: event.MessageEventContent{
				MsgType: event.MsgVideo,
				Info:    &event.FileInfo{MauGIF: true},
			},
			wantCapType: event.CapMsgGIF,
		},
		{
			name:        "m.audio plain → MsgAudio",
			content:     event.MessageEventContent{MsgType: event.MsgAudio},
			wantCapType: event.MsgAudio,
		},
		{
			name: "m.audio + MSC3245 voice → CapMsgVoice",
			content: event.MessageEventContent{
				MsgType:     event.MsgAudio,
				MSC3245Voice: &event.MSC3245Voice{},
			},
			wantCapType: event.CapMsgVoice,
		},
		{
			name:        "m.file → MsgFile",
			content:     event.MessageEventContent{MsgType: event.MsgFile},
			wantCapType: event.MsgFile,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.content.GetCapMsgType()
			assert.Equal(t, tc.wantCapType, got,
				"wrong capability type: gate will look up wrong FileFeatures row")
		})
	}
}

// TestCapabilityGate_MimeTypes verifies that every MIME type the bridge is expected to
// accept passes the capability gate (simulateCapGate returns nil).  Each case here
// corresponds to a real file type users send through the Discord→Slack relay.
//
// A missing entry in roomCaps.File[capType].MimeTypes causes GetMimeSupport to return
// CapLevelRejected, which makes the live bridge post "unsupported media type" and drop
// the message — exactly what happened with video/mp4 GIFs.
func TestCapabilityGate_MimeTypes(t *testing.T) {
	cases := []struct {
		name    string
		content event.MessageEventContent
	}{
		// ── m.image ────────────────────────────────────────────────────────────
		{
			name: "m.image image/jpeg",
			content: event.MessageEventContent{
				MsgType: event.MsgImage,
				Info:    &event.FileInfo{MimeType: "image/jpeg"},
			},
		},
		{
			name: "m.image image/png",
			content: event.MessageEventContent{
				MsgType: event.MsgImage,
				Info:    &event.FileInfo{MimeType: "image/png"},
			},
		},
		{
			name: "m.image image/gif",
			content: event.MessageEventContent{
				MsgType: event.MsgImage,
				Info:    &event.FileInfo{MimeType: "image/gif"},
			},
		},
		{
			name: "m.image image/webp",
			content: event.MessageEventContent{
				MsgType: event.MsgImage,
				Info:    &event.FileInfo{MimeType: "image/webp"},
			},
		},
		// ── m.video ────────────────────────────────────────────────────────────
		{
			name: "m.video video/mp4",
			content: event.MessageEventContent{
				MsgType: event.MsgVideo,
				Info:    &event.FileInfo{MimeType: "video/mp4"},
			},
		},
		{
			name: "m.video video/webm",
			content: event.MessageEventContent{
				MsgType: event.MsgVideo,
				Info:    &event.FileInfo{MimeType: "video/webm"},
			},
		},
		// ── fi.mau.gif (m.video + MauGIF=true) ────────────────────────────────
		// These are the cases that caused the live outage. Discord sends animated
		// GIFs as video/mp4 with fi.mau.gif=true; the Slack capability table must
		// explicitly list video/mp4 under CapMsgGIF or the gate rejects them.
		{
			name: "fi.mau.gif video/mp4 (Discord GIF via klipy/tenor/giphy)",
			content: event.MessageEventContent{
				MsgType: event.MsgVideo,
				Info:    &event.FileInfo{MimeType: "video/mp4", MauGIF: true},
			},
		},
		{
			name: "fi.mau.gif image/gif",
			content: event.MessageEventContent{
				MsgType: event.MsgVideo,
				Info:    &event.FileInfo{MimeType: "image/gif", MauGIF: true},
			},
		},
		{
			name: "fi.mau.gif video/webm",
			content: event.MessageEventContent{
				MsgType: event.MsgVideo,
				Info:    &event.FileInfo{MimeType: "video/webm", MauGIF: true},
			},
		},
		// ── m.audio ────────────────────────────────────────────────────────────
		{
			name: "m.audio audio/mpeg",
			content: event.MessageEventContent{
				MsgType: event.MsgAudio,
				Info:    &event.FileInfo{MimeType: "audio/mpeg"},
			},
		},
		{
			name: "m.audio audio/webm",
			content: event.MessageEventContent{
				MsgType: event.MsgAudio,
				Info:    &event.FileInfo{MimeType: "audio/webm"},
			},
		},
		{
			name: "m.audio audio/wav",
			content: event.MessageEventContent{
				MsgType: event.MsgAudio,
				Info:    &event.FileInfo{MimeType: "audio/wav"},
			},
		},
		// ── m.file (wildcard */* so everything goes through) ──────────────────
		{
			name: "m.file application/pdf",
			content: event.MessageEventContent{
				MsgType: event.MsgFile,
				Info:    &event.FileInfo{MimeType: "application/pdf"},
			},
		},
		{
			name: "m.file application/octet-stream",
			content: event.MessageEventContent{
				MsgType: event.MsgFile,
				Info:    &event.FileInfo{MimeType: "application/octet-stream"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.NoError(t, simulateCapGate(roomCaps, &tc.content),
				"capability gate must not reject this MIME type")
		})
	}
}

// TestCapabilityGate_DeclaredMimesAreSupported iterates the roomCaps table itself and
// asserts that every MIME type declared at CapLevelPartialSupport or above actually
// passes GetMimeSupport — i.e. the table is internally consistent.  This catches typos
// and copy-paste errors in the MIME strings.
func TestCapabilityGate_DeclaredMimesAreSupported(t *testing.T) {
	for capType, feat := range roomCaps.File {
		for mime, level := range feat.MimeTypes {
			if mime == "*/*" {
				continue // wildcard; GetMimeSupport always finds it
			}
			capType, mime, level := capType, mime, level // capture
			t.Run(fmt.Sprintf("%s/%s", capType, mime), func(t *testing.T) {
				got := feat.GetMimeSupport(mime)
				if level >= event.CapLevelPartialSupport {
					assert.False(t, got.Reject(),
						"MIME %q is declared supported in %q but GetMimeSupport rejects it", mime, capType)
				}
			})
		}
	}
}
