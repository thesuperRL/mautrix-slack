// Shared source for mautrix-slack and mautrix-discord patches (copied to pkg/governancedata/loader.go).
package governancedata

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const defaultRefreshInterval = 60 * time.Second

// TeamInfo links a mirrored team channel pair to governance metadata.
type TeamInfo struct {
	TeamSlug         string
	TeamName         string
	SlackChannelID   string
	DiscordChannelID string
}

// Data holds runtime governance channel ownership lookups.
type Data struct {
	slackToTeam   map[string]*TeamInfo
	discordToTeam map[string]*TeamInfo
	orgSlack      map[string]bool
	orgDiscord    map[string]bool
}

var (
	dataMu      sync.RWMutex
	globalData  *Data
	loadedPath  string
	loadedMod   time.Time
	loadedAt    time.Time
)

func DefaultPath() string {
	if p := os.Getenv("GOVERNANCE_DATA_PATH"); p != "" {
		return p
	}
	return "/etc/governance/data"
}

func refreshInterval() time.Duration {
	if s := os.Getenv("GOVERNANCE_DATA_REFRESH_INTERVAL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return defaultRefreshInterval
}

// Get returns governance channel mappings (auto-refreshed from TOML files).
func Get() *Data {
	path := DefaultPath()
	interval := refreshInterval()

	dataMu.RLock()
	if globalData != nil && loadedPath == path && time.Since(loadedAt) < interval {
		if info, err := os.Stat(path); err == nil && !info.ModTime().After(loadedMod) {
			d := globalData
			dataMu.RUnlock()
			return d
		}
	}
	dataMu.RUnlock()

	dataMu.Lock()
	defer dataMu.Unlock()
	if globalData != nil && loadedPath == path && time.Since(loadedAt) < interval {
		if info, err := os.Stat(path); err == nil && !info.ModTime().After(loadedMod) {
			return globalData
		}
	}
	globalData = load(path)
	loadedPath = path
	loadedAt = time.Now()
	if info, err := os.Stat(path); err == nil {
		loadedMod = info.ModTime()
	} else {
		loadedMod = time.Time{}
	}
	return globalData
}

func emptyData() *Data {
	return &Data{
		slackToTeam:   make(map[string]*TeamInfo),
		discordToTeam: make(map[string]*TeamInfo),
		orgSlack:      make(map[string]bool),
		orgDiscord:    make(map[string]bool),
	}
}

func load(path string) *Data {
	d := emptyData()
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return d
	}
	_ = loadOrg(filepath.Join(path, "org.toml"), d)
	entries, err := os.ReadDir(filepath.Join(path, "teams"))
	if err != nil {
		return d
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".toml") {
			continue
		}
		_ = loadTeam(filepath.Join(path, "teams", entry.Name()), d)
	}
	return d
}

func loadOrg(path string, d *Data) error {
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	inOrgComm := false
	inOrgChannels := false
	var channel slackDiscordPair
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case "[org.communication]":
			inOrgComm = true
			inOrgChannels = false
		case "[[org.communication.channels]]":
			flushOrgChannel(d, &channel)
			inOrgComm = true
			inOrgChannels = true
		default:
			if !inOrgComm {
				continue
			}
			if inOrgChannels {
				parseChannelPair(trimmed, &channel)
			} else {
				parseOrgCommKey(trimmed, d)
			}
		}
	}
	flushOrgChannel(d, &channel)
	return nil
}

func parseOrgCommKey(line string, d *Data) {
	for _, key := range []string{
		"slack_hub_channel_id",
		"slack_leads_channel_id",
		"discord_hub_channel_id",
		"discord_leads_channel_id",
	} {
		if val, ok := parseAssignment(line, key); ok {
			switch key {
			case "slack_hub_channel_id", "slack_leads_channel_id":
				d.orgSlack[strings.ToUpper(val)] = true
			case "discord_hub_channel_id", "discord_leads_channel_id":
				d.orgDiscord[val] = true
			}
		}
	}
}

func loadTeam(path string, d *Data) error {
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	var slug, name string
	inTeamHeader := false
	inChannelTable := false
	var channel slackDiscordPair
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case "[team]":
			inTeamHeader = true
			inChannelTable = false
		case "[[team.projects]]", "[[team.repos]]":
			inTeamHeader = false
			inChannelTable = false
		case "[[team.channels]]", "[[team.projects.channels]]":
			flushTeamChannel(d, slug, name, &channel)
			inChannelTable = true
		default:
			if inChannelTable {
				parseChannelPair(trimmed, &channel)
			} else if inTeamHeader {
				if val, ok := parseAssignment(trimmed, "slug"); ok {
					slug = val
				} else if val, ok := parseAssignment(trimmed, "name"); ok {
					name = val
				}
			}
		}
	}
	flushTeamChannel(d, slug, name, &channel)
	return nil
}

type slackDiscordPair struct {
	slack   string
	discord string
}

func parseChannelPair(line string, channel *slackDiscordPair) {
	if val, ok := parseAssignment(line, "slack"); ok {
		channel.slack = val
	}
	if val, ok := parseAssignment(line, "discord"); ok {
		channel.discord = val
	}
}

func flushOrgChannel(d *Data, channel *slackDiscordPair) {
	if channel.slack != "" {
		d.orgSlack[strings.ToUpper(channel.slack)] = true
	}
	if channel.discord != "" {
		d.orgDiscord[channel.discord] = true
	}
	*channel = slackDiscordPair{}
}

func flushTeamChannel(d *Data, slug, name string, channel *slackDiscordPair) {
	if slug == "" || (channel.slack == "" && channel.discord == "") {
		*channel = slackDiscordPair{}
		return
	}
	info := &TeamInfo{
		TeamSlug:         slug,
		TeamName:         name,
		SlackChannelID:   strings.ToUpper(channel.slack),
		DiscordChannelID: channel.discord,
	}
	if channel.slack != "" {
		d.slackToTeam[strings.ToUpper(channel.slack)] = info
	}
	if channel.discord != "" {
		d.discordToTeam[channel.discord] = info
	}
	*channel = slackDiscordPair{}
}

func parseAssignment(line, key string) (string, bool) {
	prefix := key + " = "
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	val := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
		return val[1 : len(val)-1], true
	}
	return "", false
}

func readLines(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return strings.Split(string(raw), "\n"), nil
}

func (d *Data) TeamForSlackChannel(channelID string) *TeamInfo {
	if d == nil || channelID == "" {
		return nil
	}
	return d.slackToTeam[strings.ToUpper(channelID)]
}

func (d *Data) TeamForDiscordChannel(channelID string) *TeamInfo {
	if d == nil || channelID == "" {
		return nil
	}
	return d.discordToTeam[channelID]
}

func (d *Data) IsOrgSlackChannel(channelID string) bool {
	if d == nil || channelID == "" {
		return false
	}
	return d.orgSlack[strings.ToUpper(channelID)]
}

func (d *Data) IsOrgDiscordChannel(channelID string) bool {
	if d == nil || channelID == "" {
		return false
	}
	return d.orgDiscord[channelID]
}
