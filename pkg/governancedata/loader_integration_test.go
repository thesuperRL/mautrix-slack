package governancedata

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRealGovernanceData(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", "..", "..", "governance", "data"))
	if _, err := os.Stat(root); err != nil {
		t.Skip("governance data not available")
	}
	d := load(root)
	team := d.TeamForSlackChannel("C08SRUGUQ05")
	if team == nil || team.TeamSlug != "slai" {
		t.Fatalf("expected SLAI team for C08SRUGUQ05, got %#v", team)
	}
	if d.TeamForDiscordChannel("1515571704204623903") != team {
		t.Fatalf("discord SLAI channel lookup failed")
	}
	if d.TeamForSlackChannel("C019R0WM1T4") != nil {
		t.Fatalf("general org channel should not be team-owned")
	}
	devops := d.TeamForDiscordChannel("1461933322505818156")
	if devops == nil || devops.TeamSlug != "devops" || devops.TeamName != "DevOps" {
		t.Fatalf("expected DevOps team for devops discord channel, got %#v", devops)
	}
}
