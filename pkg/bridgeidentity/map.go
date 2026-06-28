// Shared source for mautrix-slack and mautrix-discord patches (copied to pkg/bridgeidentity/map.go).
package bridgeidentity

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix/id"
)

const defaultRefreshInterval = 60 * time.Second

// Map holds Keycloak-linked Discord ↔ Slack user IDs for cross-platform mentions.
type Map struct {
	matrixDomain           string
	discordToSlack         map[string]string
	slackToDiscord         map[string]string
	matrixLocalpartDiscord map[string]string
	matrixLocalpartSlack   map[string]string
	slackToMatrixLocalpart map[string]string
}

type keycloakFedLink struct {
	IdentityProvider string `json:"identityProvider"`
	UserID           string `json:"userId"`
	UserName         string `json:"userName"`
}

type keycloakUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

var (
	mapMu             sync.RWMutex
	globalMap         *Map
	loadedAt          time.Time
	refreshInProgress bool
	discordGhostRegex = regexp.MustCompile(`^@discord_([0-9]+):`)
	httpClient        = &http.Client{Timeout: 30 * time.Second}
)

func refreshInterval() time.Duration {
	if s := os.Getenv("BRIDGE_IDENTITY_REFRESH_INTERVAL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return defaultRefreshInterval
}

// GetCached returns the in-memory identity map without blocking on Keycloak refresh.
func GetCached() *Map {
	mapMu.RLock()
	defer mapMu.RUnlock()
	if globalMap != nil {
		return globalMap
	}
	return emptyMap()
}

// Get returns identity links from Keycloak (stale-while-revalidate; first load is synchronous).
func Get() *Map {
	interval := refreshInterval()
	mapMu.RLock()
	if globalMap != nil && time.Since(loadedAt) < interval {
		m := globalMap
		mapMu.RUnlock()
		return m
	}
	stale := globalMap
	mapMu.RUnlock()

	mapMu.Lock()
	if globalMap != nil && time.Since(loadedAt) < interval {
		m := globalMap
		mapMu.Unlock()
		return m
	}
	if stale != nil {
		if !refreshInProgress {
			refreshInProgress = true
			go func() {
				defer func() {
					mapMu.Lock()
					refreshInProgress = false
					mapMu.Unlock()
				}()
				if m, err := loadFromKeycloak(); err == nil {
					mapMu.Lock()
					globalMap = m
					loadedAt = time.Now()
					mapMu.Unlock()
				}
			}()
		}
		mapMu.Unlock()
		return stale
	}
	if m, err := loadFromKeycloak(); err == nil {
		globalMap = m
	} else {
		globalMap = emptyMap()
	}
	loadedAt = time.Now()
	m := globalMap
	mapMu.Unlock()
	return m
}

func emptyMap() *Map {
	return &Map{
		discordToSlack:         make(map[string]string),
		slackToDiscord:         make(map[string]string),
		matrixLocalpartDiscord: make(map[string]string),
		matrixLocalpartSlack:   make(map[string]string),
		slackToMatrixLocalpart: make(map[string]string),
	}
}

func loadFromKeycloak() (*Map, error) {
	baseURL := strings.TrimRight(os.Getenv("KEYCLOAK_URL"), "/")
	realm := os.Getenv("KEYCLOAK_REALM")
	clientID := os.Getenv("KEYCLOAK_CLIENT_ID")
	clientSecret := os.Getenv("KEYCLOAK_CLIENT_SECRET")
	if baseURL == "" || realm == "" || clientID == "" || clientSecret == "" {
		return emptyMap(), fmt.Errorf("keycloak env not configured")
	}

	token, err := keycloakToken(baseURL, realm, clientID, clientSecret)
	if err != nil {
		return nil, err
	}

	m := emptyMap()
	m.matrixDomain = os.Getenv("MATRIX_DOMAIN")

	first := 0
	const pageSize = 100
	for {
		usersURL := fmt.Sprintf("%s/admin/realms/%s/users?first=%d&max=%d", baseURL, url.PathEscape(realm), first, pageSize)
		var users []keycloakUser
		if err := keycloakGetJSON(usersURL, token, &users); err != nil {
			return nil, err
		}
		if len(users) == 0 {
			break
		}

		for _, user := range users {
			if user.ID == "" {
				continue
			}
			fedURL := fmt.Sprintf("%s/admin/realms/%s/users/%s/federated-identity", baseURL, url.PathEscape(realm), url.PathEscape(user.ID))
			var fed []keycloakFedLink
			if err := keycloakGetJSON(fedURL, token, &fed); err != nil {
				continue
			}
			discordID := federatedDiscordID(fed)
			slackID := federatedSlackUserID(fed)
			matrixLocalpart := strings.ToLower(federatedMatrixLocalpart(fed, user.Username))
			if slackID == "" || matrixLocalpart == "" {
				continue
			}
			m.matrixLocalpartSlack[matrixLocalpart] = slackID
			m.slackToMatrixLocalpart[slackID] = matrixLocalpart
			if discordID != "" {
				m.discordToSlack[discordID] = slackID
				m.slackToDiscord[slackID] = discordID
				m.matrixLocalpartDiscord[matrixLocalpart] = discordID
			}
		}

		if len(users) < pageSize {
			break
		}
		first += pageSize
	}

	return m, nil
}

func keycloakToken(baseURL, realm, clientID, clientSecret string) (string, error) {
	tokenURL := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token", baseURL, url.PathEscape(realm))
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	}
	resp, err := httpClient.PostForm(tokenURL, form)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("keycloak token: HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	if parsed.AccessToken == "" {
		return "", fmt.Errorf("keycloak token: empty access_token")
	}
	return parsed.AccessToken, nil
}

func keycloakGetJSON(reqURL, token string, out any) error {
	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("keycloak GET %s: HTTP %d", reqURL, resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}

func federatedDiscordID(links []keycloakFedLink) string {
	for _, link := range links {
		if link.IdentityProvider != "discord" {
			continue
		}
		for _, candidate := range []string{link.UserID, link.UserName} {
			if isDiscordSnowflake(candidate) {
				return candidate
			}
		}
	}
	return ""
}

func federatedSlackUserID(links []keycloakFedLink) string {
	for _, link := range links {
		if link.IdentityProvider != "slack" {
			continue
		}
		for _, candidate := range []string{link.UserID, link.UserName} {
			if id := slackMemberIDFromFederated(candidate); id != "" {
				return id
			}
		}
	}
	return ""
}

func slackMemberIDFromFederated(candidate string) string {
	candidate = strings.TrimSpace(candidate)
	candidate = strings.TrimPrefix(candidate, "@")
	if isSlackMemberID(candidate) {
		return normalizeSlackUserID(candidate)
	}
	if i := strings.LastIndex(candidate, "-"); i >= 0 {
		suffix := candidate[i+1:]
		if isSlackMemberID(suffix) {
			return normalizeSlackUserID(suffix)
		}
	}
	return ""
}

func federatedMatrixLocalpart(links []keycloakFedLink, keycloakUsername string) string {
	for _, link := range links {
		if (link.IdentityProvider == "codeberg" || link.IdentityProvider == "forgejo") && link.UserName != "" {
			return link.UserName
		}
	}
	return keycloakUsername
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func isSlackMemberID(id string) bool {
	id = strings.ToUpper(strings.TrimPrefix(strings.TrimSpace(id), "@"))
	return strings.HasPrefix(id, "U") || strings.HasPrefix(id, "W")
}

func isDiscordSnowflake(id string) bool {
	id = strings.TrimSpace(id)
	if len(id) < 15 {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func normalizeSlackUserID(id string) string {
	return strings.ToUpper(strings.TrimPrefix(id, "@"))
}

func (m *Map) SlackUserIDForDiscord(discordID string) string {
	if m == nil {
		return ""
	}
	return m.discordToSlack[discordID]
}

func (m *Map) DiscordIDForSlack(slackUserID string) string {
	if m == nil {
		return ""
	}
	return m.slackToDiscord[strings.ToUpper(slackUserID)]
}

// MatrixLocalpartForSlack returns the linked Matrix localpart for a Slack member ID.
func (m *Map) MatrixLocalpartForSlack(slackUserID string) string {
	if m == nil {
		return ""
	}
	return m.slackToMatrixLocalpart[strings.ToUpper(slackUserID)]
}

// SlackUserIDForMatrixLocalpart returns the linked Slack member ID for a Matrix localpart.
func (m *Map) SlackUserIDForMatrixLocalpart(localpart, domain string) string {
	return m.slackUserIDForMatrixLocalpart(localpart, domain)
}

func (m *Map) HasDiscord(discordID string) bool {
	return m.SlackUserIDForDiscord(discordID) != ""
}

func (m *Map) HasSlack(slackUserID string) bool {
	return m.DiscordIDForSlack(slackUserID) != ""
}


// LinkedIdentityKey returns a stable dedup key for the same person across Discord/Slack puppets.
func LinkedIdentityKey(mxid id.UserID) string {
	if discordID := ParseDiscordGhostMXID(mxid); discordID != "" {
		if Get().HasDiscord(discordID) {
			return "link:" + discordID
		}
	}
	if slackUID := ParseSlackGhostUserID(mxid); slackUID != "" {
		if discordID := Get().DiscordIDForSlack(slackUID); discordID != "" {
			return "link:" + discordID
		}
	}
	if localpart, domain, err := mxid.Parse(); err == nil {
		if discordID := Get().discordIDForMatrixLocalpart(localpart, domain); discordID != "" {
			return "link:" + discordID
		}
	}
	return string(mxid)
}

// DedupeLinkedMentions removes duplicate m.mentions entries for the same linked identity.
func DedupeLinkedMentions(userIDs []id.UserID) []id.UserID {
	if len(userIDs) < 2 {
		return userIDs
	}
	seen := make(map[string]struct{}, len(userIDs))
	out := make([]id.UserID, 0, len(userIDs))
	for _, uid := range userIDs {
		key := LinkedIdentityKey(uid)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, uid)
	}
	return out
}

// IsDiscordBridgeBot is true for the mautrix-discord appservice bot (@discord:domain).
func IsDiscordBridgeBot(mxid id.UserID) bool {
	localpart, _, err := mxid.Parse()
	return err == nil && localpart == "discord"
}

// IsSlackBridgeBot is true for the mautrix-slack appservice bot (@slack:domain).
func IsSlackBridgeBot(mxid id.UserID) bool {
	localpart, _, err := mxid.Parse()
	return err == nil && localpart == "slack"
}

func (m *Map) discordIDForMatrixLocalpart(localpart string, domain string) string {
	if m == nil || localpart == "" {
		return ""
	}
	if m.matrixDomain != "" && domain != m.matrixDomain {
		return ""
	}
	return m.matrixLocalpartDiscord[localpart]
}

func (m *Map) slackUserIDForMatrixLocalpart(localpart string, domain string) string {
	if m == nil || localpart == "" {
		return ""
	}
	if m.matrixDomain != "" && domain != m.matrixDomain {
		return ""
	}
	return m.matrixLocalpartSlack[localpart]
}

// DiscordIDForMXID returns a Discord user snowflake when mxid is a discord or slack ghost,
// or a Matrix user with Discord linked in governance. Uses governance data to map Slack→Discord.
func DiscordIDForMXID(mxid id.UserID) string {
	// Always return Discord ID for Discord ghosts, regardless of governance links
	if discordID := ParseDiscordGhostMXID(mxid); discordID != "" {
		return discordID
	}
	// For Slack ghosts, look up Discord ID via governance (cross-platform mapping)
	if slackUID := ParseSlackGhostUserID(mxid); slackUID != "" {
		return Get().DiscordIDForSlack(slackUID)
	}
	// For Matrix users, look up Discord ID via governance
	if localpart, domain, err := mxid.Parse(); err == nil {
		return Get().discordIDForMatrixLocalpart(localpart, domain)
	}
	return ""
}

// SlackUserIDForMXID returns a Slack user ID when mxid is a slack or discord ghost,
// or a Matrix user with Slack linked in governance. Uses governance data to map Discord→Slack.
func SlackUserIDForMXID(mxid id.UserID) string {
	// Always return Slack ID for Slack ghosts, regardless of governance links
	if slackUID := ParseSlackGhostUserID(mxid); slackUID != "" {
		return slackUID
	}
	// For Discord ghosts, look up Slack ID via governance (cross-platform mapping)
	if discordID := ParseDiscordGhostMXID(mxid); discordID != "" {
		return Get().SlackUserIDForDiscord(discordID)
	}
	// For Matrix users, look up Slack ID via governance
	if localpart, domain, err := mxid.Parse(); err == nil {
		return Get().slackUserIDForMatrixLocalpart(localpart, domain)
	}
	return ""
}

// ParseDiscordGhostMXID extracts a Discord snowflake from a discord bridge puppet MXID.
func ParseDiscordGhostMXID(mxid id.UserID) string {
	match := discordGhostRegex.FindStringSubmatch(string(mxid))
	if len(match) == 2 {
		return match[1]
	}
	return ""
}

// ParseSlackGhostUserID extracts a Slack user ID from a slack bridge puppet MXID.
func ParseSlackGhostUserID(mxid id.UserID) string {
	localpart, _, err := mxid.Parse()
	if err != nil {
		return ""
	}
	if localpart == "slack" {
		return ""
	}
	if strings.HasPrefix(localpart, "slack_") {
		localpart = strings.TrimPrefix(localpart, "slack_")
	}
	if decoded, err := id.DecodeUserLocalpart(localpart); err == nil {
		localpart = string(decoded)
	}
	if uid := slackMemberIDFromFederated(localpart); uid != "" {
		return uid
	}
	return ""
}
