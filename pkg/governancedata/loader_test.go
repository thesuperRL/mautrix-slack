package governancedata

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadTeamAndOrgChannels(t *testing.T) {
	dir := t.TempDir()
	teamsDir := filepath.Join(dir, "teams")
	if err := os.MkdirAll(teamsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "org.toml"), []byte(`
[org.communication]
slack_hub_channel_id = "CHUB"
discord_hub_channel_id = "DHUB"

[[org.communication.channels]]
name = "General"
slack = "CORGG"
discord = "DORGG"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamsDir, "slai.toml"), []byte(`
[team]
name = "ScottyLabs AI"
slug = "slai"

[[team.channels]]
slack = "CTEAM"
discord = "DTEAM"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	d := load(dir)
	team := d.TeamForSlackChannel("CTEAM")
	if team == nil || team.TeamSlug != "slai" || team.TeamName != "ScottyLabs AI" {
		t.Fatalf("team lookup failed: %#v", team)
	}
	if d.TeamForDiscordChannel("DTEAM") != team {
		t.Fatalf("discord team lookup failed")
	}
	if !d.IsOrgSlackChannel("CORGG") || !d.IsOrgDiscordChannel("DORGG") {
		t.Fatalf("org channel not registered")
	}
	if d.TeamForSlackChannel("CORGG") != nil {
		t.Fatalf("org slack channel should not be team-owned")
	}
}

func TestMirrorTargetRoleDefaultsToTeamName(t *testing.T) {
	team := &TeamInfo{TeamName: "Terrier"}
	if team.MirrorTargetRole() != "Terrier" {
		t.Fatalf("expected default team name, got %q", team.MirrorTargetRole())
	}
	if !team.RoleMirrorsToChannel("terrier") {
		t.Fatalf("default team role should mirror case-insensitively")
	}
}

func TestLoadChannelMirrorConfig(t *testing.T) {
	dir := t.TempDir()
	teamsDir := filepath.Join(dir, "teams")
	if err := os.MkdirAll(teamsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamsDir, "custom.toml"), []byte(`
[team]
name = "Custom Team"
slug = "custom"

[[team.channels]]
slack = "CCUSTOM"
discord = "DCUSTOM"
mirror_role = "Developers"

[[team.channels]]
slack = "CEVERY"
discord = "DEVERY"
mirror_everyone = true
`), 0o644); err != nil {
		t.Fatal(err)
	}

	d := load(dir)
	custom := d.TeamForDiscordChannel("DCUSTOM")
	if custom == nil || custom.MirrorRole != "Developers" || custom.MirrorTargetRole() != "Developers" {
		t.Fatalf("custom mirror role: %#v", custom)
	}
	if !custom.RoleMirrorsToChannel("Developers") || custom.RoleMirrorsToChannel("Custom Team") {
		t.Fatalf("custom role should mirror Developers only")
	}
	every := d.TeamForDiscordChannel("DEVERY")
	if every == nil || !every.MirrorEveryone {
		t.Fatalf("mirror_everyone: %#v", every)
	}
	if every.RoleMirrorsToChannel("Custom Team") {
		t.Fatalf("mirror_everyone channels should not mirror role pings")
	}
}

func TestLoadOrgChannelMirrorConfig(t *testing.T) {
	dir := t.TempDir()
	teamsDir := filepath.Join(dir, "teams")
	if err := os.MkdirAll(teamsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "org.toml"), []byte(`
[org.communication]
slack_hub_channel_id = "CHUB"
discord_hub_channel_id = "DHUB"

[[org.communication.channels]]
name = "Announcements"
slug = "announcements"
slack = "CANN"
discord = "DANN"
mirror_everyone = true

[[org.communication.channels]]
name = "Random"
slug = "random"
slack = "CRAND"
discord = "DRAND"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	d := load(dir)

	ann := d.MirrorConfigForSlackChannel("CANN")
	if ann == nil || !ann.MirrorEveryone {
		t.Fatalf("announcements mirror_everyone: %#v", ann)
	}
	if d.MirrorConfigForDiscordChannel("DANN") != ann {
		t.Fatalf("discord announcements lookup mismatch")
	}
	if ann.RoleMirrorsToChannel("Announcements") {
		t.Fatalf("mirror_everyone org channels should not mirror role pings")
	}

	rand := d.MirrorConfigForSlackChannel("CRAND")
	if rand == nil || rand.MirrorEveryone {
		t.Fatalf("random channel should have no mirror_everyone: %#v", rand)
	}

	// Hub channel (no mirror config) registers but stays mirror-inert.
	hub := d.MirrorConfigForSlackChannel("CHUB")
	if hub == nil || hub.MirrorEveryone || hub.MirrorTargetRole() != "" {
		t.Fatalf("hub channel should be inert: %#v", hub)
	}
}

func TestLoadMembersAndForgejoURL(t *testing.T) {
	dir := t.TempDir()
	teamsDir := filepath.Join(dir, "teams")
	if err := os.MkdirAll(teamsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "org.toml"), []byte(`
[org.forgejo]
org = "ScottyLabs"
url = "https://codeberg.org"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamsDir, "devops.toml"), []byte(`
[team]
name = "DevOps"
slug = "devops"
leads = ["anish", "thesuperrl"]
members = ["xboxbedrock"]

[[team.projects]]
name = "Quest"
slug = "quest"
leads = ["alice"]
members = ["bob"]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	d := load(dir)
	if d.ForgejoURL() != "https://codeberg.org" {
		t.Fatalf("forgejo url: %q", d.ForgejoURL())
	}
	members := d.AllMembers()
	if len(members) != 5 {
		t.Fatalf("expected 5 members, got %v", members)
	}
	want := map[string]bool{"anish": true, "thesuperrl": true, "xboxbedrock": true, "alice": true, "bob": true}
	for _, m := range members {
		if !want[m] {
			t.Fatalf("unexpected member %q", m)
		}
		delete(want, m)
	}
	if len(want) != 0 {
		t.Fatalf("missing members: %v", want)
	}

	// Project members roll up into the parent team, matching governance-tfgen's Discord
	// role assignment (all team members, including project members, get the team role).
	devopsMembers := map[string]bool{}
	for _, m := range d.TeamMembers("devops") {
		devopsMembers[m] = true
	}
	if len(devopsMembers) != 5 || !devopsMembers["anish"] || !devopsMembers["thesuperrl"] || !devopsMembers["xboxbedrock"] || !devopsMembers["alice"] || !devopsMembers["bob"] {
		t.Fatalf("devops team members: %v", devopsMembers)
	}
	if got := d.TeamMembers("nonexistent"); got != nil {
		t.Fatalf("expected nil for unknown team, got %v", got)
	}
}

func TestLoadTeamReposDoNotOverwriteTeamName(t *testing.T) {
	dir := t.TempDir()
	teamsDir := filepath.Join(dir, "teams")
	if err := os.MkdirAll(teamsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamsDir, "devops.toml"), []byte(`
[team]
name = "DevOps"
slug = "devops"

[[team.repos]]
name = "infrastructure"

[[team.repos]]
name = "documentation"

[[team.channels]]
slack = "CDEVOPS"
discord = "DDEVOPS"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	d := load(dir)
	team := d.TeamForDiscordChannel("DDEVOPS")
	if team == nil || team.TeamName != "DevOps" || team.TeamSlug != "devops" {
		t.Fatalf("repo names must not overwrite team name: %#v", team)
	}
}
