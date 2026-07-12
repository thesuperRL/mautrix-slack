package bridgeidentity

import (
	"testing"
	"time"

	"maunium.net/go/mautrix/id"
)

func setTestMap(m *Map) {
	mapMu.Lock()
	globalMap = m
	loadedAt = time.Now()
	mapMu.Unlock()
}

func TestGetDoesNotBlockWhenUnconfigured(t *testing.T) {
	mapMu.Lock()
	globalMap = nil
	loadedAt = time.Time{}
	refreshInProgress = false
	mapMu.Unlock()

	done := make(chan *Map, 1)
	go func() { done <- Get() }()
	select {
	case m := <-done:
		if m == nil {
			t.Fatal("Get returned nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Get blocked on Keycloak load")
	}
}

func TestCrossPlatformMXIDLookup(t *testing.T) {
	setTestMap(&Map{
		discordToSlack: map[string]string{"111222333444555666": "U01SLACK"},
		slackToDiscord: map[string]string{"U01SLACK": "111222333444555666"},
	})

	discordGhost := id.UserID("@discord_111222333444555666:example.org")
	if got := SlackUserIDForMXID(discordGhost); got != "U01SLACK" {
		t.Fatalf("discord ghost -> slack: got %q", got)
	}

	slackGhost := id.UserID("@slack_U01SLACK:example.org")
	if got := DiscordIDForMXID(slackGhost); got != "111222333444555666" {
		t.Fatalf("slack ghost -> discord: got %q", got)
	}
}

func TestLinkedIdentityKeyDedupesCrossPlatformGhosts(t *testing.T) {
	setTestMap(&Map{
		discordToSlack: map[string]string{"999888777666555444": "U02SLACK"},
		slackToDiscord: map[string]string{"U02SLACK": "999888777666555444"},
	})

	discordGhost := id.UserID("@discord_999888777666555444:example.org")
	slackGhost := id.UserID("@slack_U02SLACK:example.org")
	if LinkedIdentityKey(discordGhost) != LinkedIdentityKey(slackGhost) {
		t.Fatalf("linked ghosts should share identity key")
	}

	deduped := DedupeLinkedMentions([]id.UserID{discordGhost, slackGhost})
	if len(deduped) != 1 {
		t.Fatalf("expected one mention after dedup, got %d", len(deduped))
	}
}

func TestIndexFederatedLinksRequiresBothPlatformsForCrossMap(t *testing.T) {
	m := emptyMap()
	indexFederatedLinks(m, []keycloakFedLink{
		{IdentityProvider: "slack", UserID: "U03SLACK"},
		{IdentityProvider: "discord", UserID: "333444555666777888"},
		{IdentityProvider: "codeberg", UserName: "member"},
	}, "member", "govmember")

	if m.SlackUserIDForDiscord("333444555666777888") != "U03SLACK" {
		t.Fatalf("cross-map missing")
	}
	if m.DiscordIDForSlack("U03SLACK") != "333444555666777888" {
		t.Fatalf("reverse cross-map missing")
	}
	if m.SlackUserIDForMatrixLocalpart("member", "example.org") != "U03SLACK" {
		t.Fatalf("matrix localpart map missing")
	}
	m.matrixDomain = "example.org"
	if got := m.MXIDForGovernanceUsername("GovMember"); got != "@member:example.org" {
		t.Fatalf("governance username MXID lookup: %q", got)
	}
	if got := m.MXIDForGovernanceUsername("nonexistent"); got != "" {
		t.Fatalf("expected empty MXID for unlinked username, got %q", got)
	}
}
