package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/repo"

	"github.com/gin-gonic/gin"
)

type githubGrant struct {
	id, code      string
	clientID      string
	traceID       string
	expires, next time.Time
	interval      time.Duration
	busy          bool
}

// Self-hosted deployments must configure their own OAuth App. Never silently
// send users to a shared upstream app registered under another operator.
const defaultGitHubClientID = ""

func githubLogText(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > 256 {
		return string(runes[:256])
	}
	return string(runes)
}

func githubTraceID(c *gin.Context) string {
	if traceID := c.GetString("githubTraceID"); traceID != "" {
		return traceID
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "unavailable"
	}
	traceID := hex.EncodeToString(raw[:])
	c.Set("githubTraceID", traceID)
	return traceID
}

func githubLog(c *gin.Context, event, format string, args ...any) {
	detail := fmt.Sprintf(format, args...)
	if detail == "" {
		log.Printf("[GitHub OAuth] trace=%s event=%s", githubTraceID(c), event)
		return
	}
	log.Printf("[GitHub OAuth] trace=%s event=%s %s", githubTraceID(c), event, detail)
}

type githubHandlers struct {
	*deps
	client *http.Client
	mu     sync.Mutex
	grants map[int64]*githubGrant
}

func newGitHubHandlers(shared *deps) *githubHandlers {
	return &githubHandlers{deps: shared, client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, grants: make(map[int64]*githubGrant)}
}

func (h *githubHandlers) current(c *gin.Context) (int64, bool) {
	c.Header("Cache-Control", "no-store")
	user, ok := h.requestUser(c.Request, false)
	if !ok || user == nil || user.IsGuest || user.ID <= 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请先登录 LoopForge，再连接 GitHub"})
		return 0, false
	}
	if c.Request.Method != http.MethodGet {
		origin, err := url.Parse(c.GetHeader("Origin"))
		if err != nil || origin.Host != c.Request.Host || (origin.Scheme != "http" && origin.Scheme != "https") {
			c.JSON(http.StatusForbidden, gin.H{"error": "请求来源不允许"})
			return 0, false
		}
	}
	return user.ID, true
}

func (h *githubHandlers) status(c *gin.Context) {
	if _, ok := h.current(c); !ok {
		return
	}
	clientID, clientIDSource, err := h.clientIDConfig()
	if err != nil {
		respond(c, err)
		return
	}
	admin := h.isPlatformAdmin(c.Request)
	result := gin.H{"enabled": clientID != "", "canConfigure": admin, "managed": clientIDSource == "environment", "clientIdSource": clientIDSource}
	if admin {
		result["clientId"] = clientID
	}
	c.JSON(http.StatusOK, result)
}

func (h *githubHandlers) clientID() (string, error) {
	clientID, _, err := h.clientIDConfig()
	return clientID, err
}

func (h *githubHandlers) clientIDConfig() (string, string, error) {
	if clientID := strings.TrimSpace(os.Getenv("AGENTFLOW_GITHUB_CLIENT_ID")); clientID != "" {
		return clientID, "environment", nil
	}
	return h.repo.UsersRepo.GitHubClientIDConfig(defaultGitHubClientID)
}

func (h *githubHandlers) configure(c *gin.Context) {
	if _, ok := h.current(c); !ok {
		return
	}
	if !h.isPlatformAdmin(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "仅平台管理员可以配置 GitHub 授权应用"})
		return
	}
	if strings.TrimSpace(os.Getenv("AGENTFLOW_GITHUB_CLIENT_ID")) != "" {
		c.JSON(http.StatusConflict, gin.H{"error": "此实例由环境变量管理，请在部署配置中修改 Client ID"})
		return
	}
	var input struct {
		ClientID string `json:"clientId"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请填写 GitHub OAuth App 的 Client ID"})
		return
	}
	input.ClientID = strings.TrimSpace(input.ClientID)
	if input.ClientID != "" && !regexp.MustCompile(`^[A-Za-z0-9_.-]{10,80}$`).MatchString(input.ClientID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Client ID 格式不正确；不要填写 Client Secret 或个人访问令牌"})
		return
	}
	h.mu.Lock()
	err := h.repo.UsersRepo.SetGitHubClientID(input.ClientID)
	if err == nil {
		clear(h.grants)
	}
	h.mu.Unlock()
	if err != nil {
		respond(c, err)
		return
	}
	h.status(c)
}

func (h *githubHandlers) request(c *gin.Context, method, endpoint, token string, form url.Values, output any) error {
	endpointLabel := endpoint
	if parsed, err := url.Parse(endpoint); err == nil {
		endpointLabel = parsed.Host + parsed.EscapedPath()
	}
	started := time.Now()
	githubLog(c, "upstream.request.start", "method=%s endpoint=%s", method, endpointLabel)
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request, err := http.NewRequestWithContext(c.Request.Context(), method, endpoint, body)
	if err != nil {
		githubLog(c, "upstream.request.build_failed", "method=%s endpoint=%s error=%q", method, endpointLabel, githubLogText(err.Error()))
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "LoopForge")
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := h.client.Do(request)
	if err != nil {
		githubLog(c, "upstream.request.failed", "method=%s endpoint=%s duration_ms=%d error=%q", method, endpointLabel, time.Since(started).Milliseconds(), githubLogText(err.Error()))
		return errors.New("无法连接 GitHub，请稍后重试")
	}
	defer response.Body.Close()
	githubLog(c, "upstream.response", "method=%s endpoint=%s status=%d duration_ms=%d request_id=%q content_type=%q",
		method, endpointLabel, response.StatusCode, time.Since(started).Milliseconds(), githubLogText(response.Header.Get("X-GitHub-Request-Id")), githubLogText(response.Header.Get("Content-Type")))
	if response.StatusCode != http.StatusOK {
		var providerError struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
			Message          string `json:"message"`
		}
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		_ = json.Unmarshal(payload, &providerError)
		providerCode := providerError.Error
		if providerCode == "" {
			providerCode = providerError.Message
		}
		githubLog(c, "upstream.response.rejected", "method=%s endpoint=%s status=%d provider_error=%q provider_description=%q read_error=%q",
			method, endpointLabel, response.StatusCode, githubLogText(providerCode), githubLogText(providerError.ErrorDescription), githubLogText(errorString(readErr)))
		return errors.New("GitHub 请求失败（HTTP " + strconv.Itoa(response.StatusCode) + "），请检查授权与组织访问策略")
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(output); err != nil {
		githubLog(c, "upstream.response.decode_failed", "method=%s endpoint=%s error=%q", method, endpointLabel, githubLogText(err.Error()))
		return errors.New("GitHub 返回了无效响应")
	}
	githubLog(c, "upstream.response.decoded", "method=%s endpoint=%s", method, endpointLabel)
	return nil
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (h *githubHandlers) start(c *gin.Context) {
	traceID := githubTraceID(c)
	githubLog(c, "device.start.received", "")
	userID, ok := h.current(c)
	if !ok {
		githubLog(c, "device.start.rejected", "reason=authentication_or_origin")
		return
	}
	clientID, clientIDSource, err := h.clientIDConfig()
	if err != nil {
		githubLog(c, "device.start.config_failed", "error=%q", githubLogText(err.Error()))
		respond(c, err)
		return
	}
	if clientID == "" {
		githubLog(c, "device.start.rejected", "reason=client_id_missing")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "请管理员配置 GitHub OAuth App 的 Client ID 并启用 Device Flow；用户无需申请 PAT"})
		return
	}
	githubLog(c, "device.start.configured", "client_id_source=%s", clientIDSource)
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		githubLog(c, "device.start.nonce_failed", "error=%q", githubLogText(err.Error()))
		respond(c, err)
		return
	}
	grant := &githubGrant{id: hex.EncodeToString(nonce), clientID: clientID, traceID: traceID, expires: time.Now().Add(15 * time.Minute), next: time.Now().Add(10 * time.Second)}
	h.mu.Lock()
	currentClientID, configErr := h.clientID()
	if configErr != nil || currentClientID != clientID {
		h.mu.Unlock()
		githubLog(c, "device.start.rejected", "reason=client_id_changed_during_start")
		c.JSON(http.StatusConflict, gin.H{"error": "GitHub 应用配置已变更，请重新连接"})
		return
	}
	for owner, pending := range h.grants {
		if time.Now().After(pending.expires) {
			delete(h.grants, owner)
		}
	}
	if pending := h.grants[userID]; (pending != nil && (pending.busy || time.Now().Before(pending.next))) || len(h.grants) >= 1024 {
		h.mu.Unlock()
		githubLog(c, "device.start.rejected", "reason=rate_limited")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "请稍后再发起授权"})
		return
	}
	grant.busy = true
	h.grants[userID] = grant
	h.mu.Unlock()
	var device struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	err = h.request(c, http.MethodPost, "https://github.com/login/device/code", "", url.Values{"client_id": {clientID}, "scope": {"repo"}}, &device)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.grants[userID] != grant {
		githubLog(c, "device.start.rejected", "reason=grant_replaced_during_upstream_request")
		c.JSON(http.StatusConflict, gin.H{"error": "GitHub 应用配置已变更，请重新连接"})
		return
	}
	if err != nil || device.DeviceCode == "" || device.UserCode == "" || device.ExpiresIn <= 0 || device.ExpiresIn > 1800 || device.VerificationURI != "https://github.com/login/device" {
		githubLog(c, "device.start.invalid_response", "request_error=%t has_device_code=%t has_user_code=%t valid_verification_uri=%t expires_in=%d interval=%d",
			err != nil, device.DeviceCode != "", device.UserCode != "", device.VerificationURI == "https://github.com/login/device", device.ExpiresIn, device.Interval)
		delete(h.grants, userID)
		c.JSON(http.StatusBadGateway, gin.H{"error": "无法开始 GitHub 授权，请检查 OAuth App 是否启用 Device Flow 或稍后重试"})
		return
	}
	grant.code = device.DeviceCode
	grant.expires = time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)
	grant.interval = time.Duration(max(5, device.Interval)) * time.Second
	grant.next = time.Now().Add(grant.interval)
	grant.busy = false
	githubLog(c, "device.start.issued", "expires_in=%d poll_interval=%d", device.ExpiresIn, int(grant.interval.Seconds()))
	c.JSON(http.StatusOK, gin.H{"flowId": grant.id, "userCode": device.UserCode, "verificationUri": device.VerificationURI, "expiresIn": device.ExpiresIn, "interval": int(grant.interval.Seconds())})
}

func (h *githubHandlers) poll(c *gin.Context) {
	userID, ok := h.current(c)
	if !ok {
		githubLog(c, "device.poll.rejected", "reason=authentication_or_origin")
		return
	}
	var input struct {
		FlowID string `json:"flowId"`
	}
	if !decode(c, &input) {
		githubLog(c, "device.poll.rejected", "reason=invalid_request_body")
		return
	}
	h.mu.Lock()
	grant := h.grants[userID]
	if grant == nil || grant.id != input.FlowID || time.Now().After(grant.expires) {
		h.mu.Unlock()
		githubLog(c, "device.poll.rejected", "reason=grant_missing_mismatch_or_expired")
		c.JSON(http.StatusBadRequest, gin.H{"error": "授权已失效，请重新连接 GitHub"})
		return
	}
	c.Set("githubTraceID", grant.traceID)
	githubLog(c, "device.poll.received", "grant_matched=true")
	clientID, configErr := h.clientID()
	if configErr != nil || clientID != grant.clientID {
		delete(h.grants, userID)
		h.mu.Unlock()
		githubLog(c, "device.poll.rejected", "reason=client_id_changed")
		c.JSON(http.StatusConflict, gin.H{"error": "GitHub 应用配置已变更，请重新连接"})
		return
	}
	if grant.busy || time.Now().Before(grant.next) {
		interval := int(grant.interval.Seconds())
		h.mu.Unlock()
		githubLog(c, "device.poll.deferred", "reason=interval_or_request_busy interval=%d", interval)
		c.JSON(http.StatusOK, gin.H{"status": "pending", "interval": interval})
		return
	}
	grant.busy = true
	grant.next = time.Now().Add(grant.interval)
	h.mu.Unlock()
	var result struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ExpiresIn   int    `json:"expires_in"`
	}
	err := h.request(c, http.MethodPost, "https://github.com/login/oauth/access_token", "", url.Values{"client_id": {grant.clientID}, "device_code": {grant.code}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}, &result)
	if err != nil {
		githubLog(c, "device.poll.token_request_failed", "error=%q", githubLogText(err.Error()))
	} else if result.Error == "authorization_pending" || result.Error == "slow_down" {
		githubLog(c, "device.poll.pending", "provider_error=%q", githubLogText(result.Error))
	} else if result.Error != "" {
		githubLog(c, "device.poll.provider_rejected", "provider_error=%q", githubLogText(result.Error))
	} else if result.AccessToken != "" {
		githubLog(c, "device.poll.token_received", "has_expiry=%t", result.ExpiresIn > 0)
	} else {
		githubLog(c, "device.poll.empty_token_response", "provider_error_empty=true")
	}
	var identity struct {
		Login string `json:"login"`
	}
	if err == nil && result.AccessToken != "" {
		githubLog(c, "device.poll.identity_request.start", "")
		err = h.request(c, http.MethodGet, "https://api.github.com/user", result.AccessToken, nil, &identity)
		if identity.Login == "" && err == nil {
			githubLog(c, "device.poll.identity_request.invalid_response", "login_present=false")
			err = errors.New("GitHub 未返回账号信息")
		} else if err == nil {
			githubLog(c, "device.poll.identity_request.succeeded", "login_present=true")
		} else {
			githubLog(c, "device.poll.identity_request.failed", "error=%q", githubLogText(err.Error()))
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	grant.busy = false
	if h.grants[userID] != grant || time.Now().After(grant.expires) {
		githubLog(c, "device.poll.rejected", "reason=grant_replaced_or_expired_after_upstream_request")
		c.JSON(http.StatusBadRequest, gin.H{"error": "授权已失效，请重新连接 GitHub"})
		return
	}
	clientID, configErr = h.clientID()
	if configErr != nil || clientID != grant.clientID {
		delete(h.grants, userID)
		githubLog(c, "device.poll.rejected", "reason=client_id_changed_after_upstream_request")
		c.JSON(http.StatusConflict, gin.H{"error": "GitHub 应用配置已变更，请重新连接"})
		return
	}
	if err != nil {
		githubLog(c, "device.poll.failed", "error=%q", githubLogText(err.Error()))
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if result.Error == "authorization_pending" || result.Error == "slow_down" {
		if result.Error == "slow_down" {
			grant.interval += 5 * time.Second
		}
		grant.next = time.Now().Add(grant.interval)
		githubLog(c, "device.poll.waiting_for_user", "provider_error=%q next_interval=%d", githubLogText(result.Error), int(grant.interval.Seconds()))
		c.JSON(http.StatusOK, gin.H{"status": "pending", "interval": int(grant.interval.Seconds())})
		return
	}
	delete(h.grants, userID)
	if result.AccessToken == "" {
		githubLog(c, "device.poll.failed", "reason=authorization_denied_or_expired")
		c.JSON(http.StatusBadRequest, gin.H{"error": "GitHub 授权被拒绝或已过期，请重新连接"})
		return
	}
	if result.ExpiresIn > 0 {
		githubLog(c, "device.poll.failed", "reason=expiring_token_unsupported expires_in=%d", result.ExpiresIn)
		c.JSON(http.StatusBadRequest, gin.H{"error": "当前仅支持非过期 OAuth 令牌，请管理员关闭 OAuth App 的令牌过期选项后重试"})
		return
	}
	githubLog(c, "device.poll.credential_save.start", "")
	saved, err := h.repo.UpdateUserCredentials(userID, repo.UserCredentialUpdate{GitToken: result.AccessToken})
	if err != nil {
		githubLog(c, "device.poll.credential_save.failed", "error=%q", githubLogText(err.Error()))
		respond(c, err)
		return
	}
	githubLog(c, "device.poll.credential_save.succeeded", "")
	c.JSON(http.StatusOK, gin.H{"status": "connected", "login": identity.Login, "user": domain.PublicUser(saved)})
}

func (h *githubHandlers) repositories(c *gin.Context) {
	githubLog(c, "repositories.request.received", "")
	userID, ok := h.current(c)
	if !ok {
		githubLog(c, "repositories.request.rejected", "reason=authentication_or_origin")
		return
	}
	user, ok := h.repo.User(userID)
	if !ok || user.GitToken == "" {
		githubLog(c, "repositories.request.rejected", "reason=github_credential_missing")
		c.JSON(http.StatusBadRequest, gin.H{"error": "请先连接 GitHub"})
		return
	}
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page < 1 || page > 10000 {
		githubLog(c, "repositories.request.rejected", "reason=invalid_page")
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效页码"})
		return
	}
	var repositories []struct {
		FullName      string `json:"full_name"`
		HTMLURL       string `json:"html_url"`
		DefaultBranch string `json:"default_branch"`
		Private       bool   `json:"private"`
	}
	_, token := gitwork.SplitToken(user.GitToken)
	githubLog(c, "repositories.fetch.start", "page=%d", page)
	err = h.request(c, http.MethodGet, "https://api.github.com/user/repos?per_page=100&sort=updated&affiliation=owner,collaborator,organization_member&page="+strconv.Itoa(page), token, nil, &repositories)
	if err != nil {
		githubLog(c, "repositories.fetch.failed", "page=%d error=%q", page, githubLogText(err.Error()))
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	githubLog(c, "repositories.fetch.succeeded", "page=%d count=%d has_more=%t", page, len(repositories), len(repositories) == 100)
	c.JSON(http.StatusOK, gin.H{"repositories": repositories, "hasMore": len(repositories) == 100, "page": page})
}
