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
	MirrorRole       string
	MirrorEveryone   bool
}

// MirrorTargetRole returns the Discord role name that mirrors @channel for this channel.
func (t *TeamInfo) MirrorTargetRole() string {
	if t == nil {
		return ""
	}
	if t.MirrorRole != "" {
		return t.MirrorRole
	}
	return t.TeamName
}

// RoleMirrorsToChannel reports whether pinging roleName in this channel should mirror to @channel.
func (t *TeamInfo) RoleMirrorsToChannel(roleName string) bool {
	if t == nil || t.MirrorEveryone || roleName == "" {
		return false
	}
	return strings.EqualFold(roleName, t.MirrorTargetRole())
}

// Data holds runtime governance channel ownership lookups.
type Data struct {
	slackToTeam   map[string]*TeamInfo
	discordToTeam map[string]*TeamInfo
	orgSlack      map[string]*TeamInfo
	orgDiscord    map[string]*TeamInfo
	members       map[string]bool
	teamMembers   map[string][]string
	forgejoURL    string
}

// TeamMembers returns the Codeberg usernames (leads+members) for a team slug.
func (d *Data) TeamMembers(slug string) []string {
	if d == nil || slug == "" {
		return nil
	}
	return d.teamMembers[slug]
}

const defaultForgejoURL = "https://codeberg.org"

// AllMembers returns deduped Codeberg usernames from team and project leads/members.
func (d *Data) AllMembers() []string {
	if d == nil || len(d.members) == 0 {
		return nil
	}
	out := make([]string, 0, len(d.members))
	for u := range d.members {
		out = append(out, u)
	}
	return out
}

// ForgejoURL returns the Forgejo/Codeberg API base URL from org.toml.
func (d *Data) ForgejoURL() string {
	if d == nil || d.forgejoURL == "" {
		return defaultForgejoURL
	}
	return d.forgejoURL
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

// DataStamp is the path + modtime of the last governance load. bridgeidentity uses
// stamp changes (governance data updated on the host) to trigger a full Keycloak scan.
func DataStamp() (path string, mod time.Time) {
	dataMu.RLock()
	defer dataMu.RUnlock()
	return loadedPath, loadedMod
}

func emptyData() *Data {
	return &Data{
		slackToTeam:   make(map[string]*TeamInfo),
		discordToTeam: make(map[string]*TeamInfo),
		orgSlack:      make(map[string]*TeamInfo),
		orgDiscord:    make(map[string]*TeamInfo),
		members:       make(map[string]bool),
		teamMembers:   make(map[string][]string),
	}
}

func load(path string) *Data {
	d := emptyData()
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return d
	}
	_ = loadOrg(filepath.Join(path, "org.toml"), d)
	_ = loadOrgForgejo(filepath.Join(path, "org.toml"), d)
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
				d.orgSlack[strings.ToUpper(val)] = &TeamInfo{}
			case "discord_hub_channel_id", "discord_leads_channel_id":
				d.orgDiscord[val] = &TeamInfo{}
			}
		}
	}
}

func loadOrgForgejo(path string, d *Data) error {
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	inForgejo := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case "[org.forgejo]":
			inForgejo = true
		case "[org]", "[org.github]", "[org.keycloak]", "[org.communication]":
			inForgejo = false
		default:
			if inForgejo {
				if val, ok := parseAssignment(trimmed, "url"); ok {
					d.forgejoURL = val
				}
			}
		}
	}
	return nil
}

func loadTeam(path string, d *Data) error {
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	var slug, name string
	var localMembers []string
	inTeamHeader := false
	inProject := false
	inChannelTable := false
	var channel slackDiscordPair
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case "[team]":
			inTeamHeader = true
			inProject = false
			inChannelTable = false
		case "[[team.projects]]":
			inTeamHeader = false
			inProject = true
			inChannelTable = false
		case "[[team.repos]]":
			inTeamHeader = false
			inProject = false
			inChannelTable = false
		case "[[team.channels]]", "[[team.projects.channels]]":
			flushTeamChannel(d, slug, name, &channel)
			inChannelTable = true
		default:
			if inChannelTable {
				parseChannelPair(trimmed, &channel)
			} else if inTeamHeader || inProject {
				if val, ok := parseAssignment(trimmed, "slug"); ok && inTeamHeader {
					slug = val
				} else if val, ok := parseAssignment(trimmed, "name"); ok && inTeamHeader {
					name = val
				} else if users, ok := parseStringArrayAssignment(trimmed, "leads"); ok {
					for _, u := range users {
						d.members[u] = true
						localMembers = append(localMembers, u)
					}
				} else if users, ok := parseStringArrayAssignment(trimmed, "members"); ok {
					for _, u := range users {
						d.members[u] = true
						localMembers = append(localMembers, u)
					}
				}
			}
		}
	}
	flushTeamChannel(d, slug, name, &channel)
	if slug != "" && len(localMembers) > 0 {
		d.teamMembers[slug] = append(d.teamMembers[slug], localMembers...)
	}
	return nil
}

type slackDiscordPair struct {
	name           string
	slug           string
	slack          string
	discord        string
	mirrorRole     string
	mirrorEveryone bool
}

func parseChannelPair(line string, channel *slackDiscordPair) {
	if val, ok := parseAssignment(line, "name"); ok {
		channel.name = val
	}
	if val, ok := parseAssignment(line, "slug"); ok {
		channel.slug = val
	}
	if val, ok := parseAssignment(line, "slack"); ok {
		channel.slack = val
	}
	if val, ok := parseAssignment(line, "discord"); ok {
		channel.discord = val
	}
	if val, ok := parseAssignment(line, "mirror_role"); ok {
		channel.mirrorRole = val
	}
	if val, ok := parseBoolAssignment(line, "mirror_everyone"); ok {
		channel.mirrorEveryone = val
	}
}

// flushOrgChannel registers an org-level channel, carrying its own mirror_role/
// mirror_everyone config the same way team channels do (Announcements/General use
// mirror_everyone; TeamName/TeamSlug here are the channel's own name/slug, not a team).
func flushOrgChannel(d *Data, channel *slackDiscordPair) {
	if channel.slack == "" && channel.discord == "" {
		*channel = slackDiscordPair{}
		return
	}
	info := &TeamInfo{
		TeamSlug:         channel.slug,
		TeamName:         channel.name,
		SlackChannelID:   strings.ToUpper(channel.slack),
		DiscordChannelID: channel.discord,
		MirrorRole:       channel.mirrorRole,
		MirrorEveryone:   channel.mirrorEveryone,
	}
	if channel.slack != "" {
		d.orgSlack[strings.ToUpper(channel.slack)] = info
	}
	if channel.discord != "" {
		d.orgDiscord[channel.discord] = info
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
		MirrorRole:       channel.mirrorRole,
		MirrorEveryone:   channel.mirrorEveryone,
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

func parseBoolAssignment(line, key string) (bool, bool) {
	prefix := key + " = "
	if !strings.HasPrefix(line, prefix) {
		return false, false
	}
	val := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	return val == "true", true
}

func parseStringArrayAssignment(line, key string) ([]string, bool) {
	prefix := key + " = "
	if !strings.HasPrefix(line, prefix) {
		return nil, false
	}
	val := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if !strings.HasPrefix(val, "[") || !strings.HasSuffix(val, "]") {
		return nil, false
	}
	inner := strings.TrimSpace(val[1 : len(val)-1])
	if inner == "" {
		return nil, true
	}
	parts := strings.Split(inner, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if len(part) >= 2 && part[0] == '"' && part[len(part)-1] == '"' {
			out = append(out, part[1:len(part)-1])
		}
	}
	return out, true
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
	_, ok := d.orgSlack[strings.ToUpper(channelID)]
	return ok
}

func (d *Data) IsOrgDiscordChannel(channelID string) bool {
	if d == nil || channelID == "" {
		return false
	}
	_, ok := d.orgDiscord[channelID]
	return ok
}

// MirrorConfigForSlackChannel returns the mirror config for a Slack channel,
// whether it's team-owned or a mirror-enabled org channel (e.g. Announcements/General).
func (d *Data) MirrorConfigForSlackChannel(channelID string) *TeamInfo {
	if info := d.TeamForSlackChannel(channelID); info != nil {
		return info
	}
	if d == nil || channelID == "" {
		return nil
	}
	return d.orgSlack[strings.ToUpper(channelID)]
}

// MirrorConfigForDiscordChannel returns the mirror config for a Discord channel,
// whether it's team-owned or a mirror-enabled org channel (e.g. Announcements/General).
func (d *Data) MirrorConfigForDiscordChannel(channelID string) *TeamInfo {
	if info := d.TeamForDiscordChannel(channelID); info != nil {
		return info
	}
	if d == nil || channelID == "" {
		return nil
	}
	return d.orgDiscord[channelID]
}
