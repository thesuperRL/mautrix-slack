// Shared source for mautrix-slack and mautrix-discord patches (copied to pkg/bridgeidentity/map.go).
package bridgeidentity

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-slack/pkg/governancedata"
)

const (
	defaultRefreshInterval = 60 * time.Second
	fullScanTZ             = "America/New_York"
	fullScanHour           = 4 // 4:00 Pittsburgh local (ET)
)

// Map holds Keycloak-linked Discord ↔ Slack user IDs for cross-platform mentions.
type Map struct {
	matrixDomain           string
	discordToSlack         map[string]string
	slackToDiscord         map[string]string
	matrixLocalpartDiscord map[string]string
	matrixLocalpartSlack   map[string]string
	slackToMatrixLocalpart map[string]string
	usernameToMatrixLocal  map[string]string
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
	lastFullScanAt    time.Time
	refreshLoopOnce   sync.Once
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

func pittsburghLoc() *time.Location {
	loc, err := time.LoadLocation(fullScanTZ)
	if err != nil {
		return time.FixedZone("EST", -5*3600)
	}
	return loc
}

// shouldFullScan: deploy/startup (never scanned), or at/after 4:00 America/New_York
// when the last full scan was before today's 4:00.
func shouldFullScan() bool {
	if lastFullScanAt.IsZero() {
		return true
	}
	now := time.Now().In(pittsburghLoc())
	today4 := time.Date(now.Year(), now.Month(), now.Day(), fullScanHour, 0, 0, 0, now.Location())
	if now.Before(today4) {
		return lastFullScanAt.Before(today4.Add(-24 * time.Hour))
	}
	return lastFullScanAt.Before(today4)
}

func cloneMap(src *Map) *Map {
	m := emptyMap()
	if src == nil {
		return m
	}
	m.matrixDomain = src.matrixDomain
	for k, v := range src.discordToSlack {
		m.discordToSlack[k] = v
	}
	for k, v := range src.slackToDiscord {
		m.slackToDiscord[k] = v
	}
	for k, v := range src.matrixLocalpartDiscord {
		m.matrixLocalpartDiscord[k] = v
	}
	for k, v := range src.matrixLocalpartSlack {
		m.matrixLocalpartSlack[k] = v
	}
	for k, v := range src.slackToMatrixLocalpart {
		m.slackToMatrixLocalpart[k] = v
	}
	for k, v := range src.usernameToMatrixLocal {
		m.usernameToMatrixLocal[k] = v
	}
	return m
}

func snapshotGlobal() *Map {
	mapMu.RLock()
	defer mapMu.RUnlock()
	return cloneMap(globalMap)
}

func publishMap(m *Map) {
	mapMu.Lock()
	globalMap = cloneMap(m)
	loadedAt = time.Now()
	mapMu.Unlock()
}

func ensureRefreshLoop() {
	refreshLoopOnce.Do(func() {
		go func() {
			t := time.NewTicker(refreshInterval())
			defer t.Stop()
			for range t.C {
				Get()
			}
		}()
	})
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

// Get returns identity links from Keycloak (stale-while-revalidate; never blocks).
func Get() *Map {
	ensureRefreshLoop()
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
	if !refreshInProgress {
		refreshInProgress = true
		go func() {
			defer func() {
				mapMu.Lock()
				refreshInProgress = false
				mapMu.Unlock()
			}()
			m, err := loadFromKeycloak()
			mapMu.Lock()
			if err == nil {
				globalMap = m
			} else {
				log.Printf("bridgeidentity: keycloak load failed: %v", err)
				if globalMap == nil {
					globalMap = emptyMap()
				}
			}
			// ponytail: advance loadedAt on failure too so a dead Keycloak doesn't tight-loop.
			loadedAt = time.Now()
			mapMu.Unlock()
		}()
	}
	mapMu.Unlock()
	if stale != nil {
		return stale
	}
	return emptyMap()
}

func emptyMap() *Map {
	return &Map{
		discordToSlack:         make(map[string]string),
		slackToDiscord:         make(map[string]string),
		matrixLocalpartDiscord: make(map[string]string),
		matrixLocalpartSlack:   make(map[string]string),
		slackToMatrixLocalpart: make(map[string]string),
		usernameToMatrixLocal:  make(map[string]string),
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

	full := shouldFullScan()
	var m *Map
	if full {
		m = emptyMap()
	} else {
		// Keep prior full-scan overflow links; re-apply governance members on top.
		m = snapshotGlobal()
	}
	m.matrixDomain = os.Getenv("MATRIX_DOMAIN")

	gov := governancedata.Get()
	for _, username := range gov.AllMembers() {
		loadGovernanceMember(m, baseURL, realm, token, gov.ForgejoURL(), username)
	}
	if full {
		// Publish members first so mirroring isn't empty-handed for the multi-minute crawl.
		publishMap(m)
		log.Printf("bridgeidentity: starting full keycloak user scan (%d governance links so far)", len(m.discordToSlack))
		loadFromKeycloakScan(m, baseURL, realm, token)
		lastFullScanAt = time.Now()
		log.Printf("bridgeidentity: full keycloak scan loaded %d cross-platform links", len(m.discordToSlack))
	} else {
		log.Printf("bridgeidentity: refreshed governance members, %d cross-platform links", len(m.discordToSlack))
	}
	return m, nil
}

func loadGovernanceMember(m *Map, baseURL, realm, token, forgejoURL, username string) {
	codebergID, err := forgejoUserID(forgejoURL, username)
	if err != nil || codebergID == "" {
		return
	}
	kcUser, err := keycloakUserByCodebergID(baseURL, realm, token, codebergID)
	if err != nil || kcUser == nil {
		return
	}
	fedURL := fmt.Sprintf("%s/admin/realms/%s/users/%s/federated-identity", baseURL, url.PathEscape(realm), url.PathEscape(kcUser.ID))
	var fed []keycloakFedLink
	if err := keycloakGetJSON(fedURL, token, &fed); err != nil {
		return
	}
	indexFederatedLinks(m, fed, kcUser.Username, username)
}

func loadFromKeycloakScan(m *Map, baseURL, realm, token string) {
	// ponytail: don't page all ~4k+ realm users (Keycloak returns garbage past first≈4400).
	// Only users with discord/slack IdP links can form cross-platform pings.
	seen := make(map[string]struct{})
	for _, alias := range []string{"discord", "slack"} {
		scanKeycloakUsersByIdp(m, baseURL, realm, token, alias, seen)
	}
}

// Keycloak admin user search breaks past first≈4400 (HTTP 200 with truncated/invalid JSON).
const keycloakScanMaxFirst = 4000

func scanKeycloakUsersByIdp(m *Map, baseURL, realm, token, idpAlias string, seen map[string]struct{}) {
	first := 0
	const pageSize = 100
	for first < keycloakScanMaxFirst {
		usersURL := fmt.Sprintf(
			"%s/admin/realms/%s/users?idpAlias=%s&first=%d&max=%d",
			baseURL, url.PathEscape(realm), url.QueryEscape(idpAlias), first, pageSize,
		)
		var users []keycloakUser
		if err := keycloakGetJSON(usersURL, token, &users); err != nil {
			log.Printf("bridgeidentity: keycloak %s scan stopped at first=%d: %v", idpAlias, first, err)
			return
		}
		if len(users) == 0 {
			return
		}
		for _, user := range users {
			if user.ID == "" {
				continue
			}
			if _, ok := seen[user.ID]; ok {
				continue
			}
			seen[user.ID] = struct{}{}
			fedURL := fmt.Sprintf("%s/admin/realms/%s/users/%s/federated-identity", baseURL, url.PathEscape(realm), url.PathEscape(user.ID))
			var fed []keycloakFedLink
			if err := keycloakGetJSON(fedURL, token, &fed); err != nil {
				continue
			}
			indexFederatedLinks(m, fed, user.Username, "")
		}
		if len(users) < pageSize {
			return
		}
		first += pageSize
	}
	log.Printf("bridgeidentity: keycloak %s scan hit first=%d cap", idpAlias, keycloakScanMaxFirst)
}

// indexFederatedLinks indexes a Keycloak user's linked Discord/Slack/Matrix identities.
// governanceUsername is the Codeberg/governance username driving this lookup — used to key
// usernameToMatrixLocal for team-role-ping → individual-member-mention expansion.
func indexFederatedLinks(m *Map, fed []keycloakFedLink, keycloakUsername string, governanceUsername string) {
	discordID := federatedDiscordID(fed)
	slackID := federatedSlackUserID(fed)
	matrixLocalpart := strings.ToLower(federatedMatrixLocalpart(fed, keycloakUsername))
	if slackID != "" && discordID != "" {
		m.discordToSlack[discordID] = slackID
		m.slackToDiscord[slackID] = discordID
	}
	if slackID != "" && matrixLocalpart != "" {
		m.matrixLocalpartSlack[matrixLocalpart] = slackID
		m.slackToMatrixLocalpart[slackID] = matrixLocalpart
	}
	if discordID != "" && matrixLocalpart != "" {
		m.matrixLocalpartDiscord[matrixLocalpart] = discordID
	}
	if governanceUsername != "" && matrixLocalpart != "" {
		m.usernameToMatrixLocal[strings.ToLower(governanceUsername)] = matrixLocalpart
	}
}

func forgejoUserID(forgejoURL, username string) (string, error) {
	apiURL := fmt.Sprintf("%s/api/v1/users/%s", strings.TrimRight(forgejoURL, "/"), url.PathEscape(username))
	resp, err := httpClient.Get(apiURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("forgejo GET %s: HTTP %d", apiURL, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var parsed struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	if parsed.ID == 0 {
		return "", fmt.Errorf("forgejo user %s has no id", username)
	}
	return strconv.FormatInt(parsed.ID, 10), nil
}

func keycloakUserByCodebergID(baseURL, realm, token, codebergID string) (*keycloakUser, error) {
	usersURL := fmt.Sprintf(
		"%s/admin/realms/%s/users?idpAlias=codeberg&idpUserId=%s",
		baseURL, url.PathEscape(realm), url.PathEscape(codebergID),
	)
	var users []keycloakUser
	if err := keycloakGetJSON(usersURL, token, &users); err != nil {
		return nil, err
	}
	if len(users) == 0 || users[0].ID == "" {
		return nil, fmt.Errorf("no keycloak user for codeberg id %s", codebergID)
	}
	return &users[0], nil
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
	if err := json.Unmarshal(body, out); err != nil {
		snippet := string(body)
		if len(snippet) > 120 {
			snippet = snippet[:120] + "…"
		}
		return fmt.Errorf("keycloak GET %s: %w (body %q)", reqURL, err, snippet)
	}
	return nil
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

// MXIDForGovernanceUsername returns the Matrix user ID for a governance (Codeberg) username,
// if that member is linked via Keycloak federated identity. Used to expand a team role ping
// into individual member mentions.
func (m *Map) MXIDForGovernanceUsername(username string) id.UserID {
	if m == nil || username == "" || m.matrixDomain == "" {
		return ""
	}
	localpart := m.usernameToMatrixLocal[strings.ToLower(username)]
	if localpart == "" {
		return ""
	}
	return id.NewUserID(localpart, m.matrixDomain)
}

// SlackUserIDForGovernanceUsername returns the Slack user ID for a governance (Codeberg)
// username, if that member is linked via Keycloak federated identity. Used to expand a team
// role ping into individual member mentions.
func (m *Map) SlackUserIDForGovernanceUsername(username string) string {
	if m == nil || username == "" {
		return ""
	}
	localpart := m.usernameToMatrixLocal[strings.ToLower(username)]
	if localpart == "" {
		return ""
	}
	return m.matrixLocalpartSlack[localpart]
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
		if GetCached().HasDiscord(discordID) {
			return "link:" + discordID
		}
	}
	if slackUID := ParseSlackGhostUserID(mxid); slackUID != "" {
		if discordID := GetCached().DiscordIDForSlack(slackUID); discordID != "" {
			return "link:" + discordID
		}
	}
	if localpart, domain, err := mxid.Parse(); err == nil {
		if discordID := GetCached().discordIDForMatrixLocalpart(localpart, domain); discordID != "" {
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
		return GetCached().DiscordIDForSlack(slackUID)
	}
	// For Matrix users, look up Discord ID via governance
	if localpart, domain, err := mxid.Parse(); err == nil {
		return GetCached().discordIDForMatrixLocalpart(localpart, domain)
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
		return GetCached().SlackUserIDForDiscord(discordID)
	}
	// For Matrix users, look up Slack ID via governance
	if localpart, domain, err := mxid.Parse(); err == nil {
		return GetCached().slackUserIDForMatrixLocalpart(localpart, domain)
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
