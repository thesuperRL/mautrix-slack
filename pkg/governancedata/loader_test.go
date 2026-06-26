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
