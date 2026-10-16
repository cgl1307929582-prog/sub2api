package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/gin-gonic/gin"
)

// ColabControlHandler exposes a fixed set of Pool operations through the normal
// administrator/compliance/audit middleware. The worker origin and credential
// are deployment configuration, never account fields or browser input.
type ColabControlHandler struct {
	base       string
	secretFile string
	client     *http.Client
	startMu    sync.Mutex
}

func NewColabControlHandler(cfg *config.Config) *ColabControlHandler {
	h := &ColabControlHandler{client: &http.Client{
		Timeout:       90 * time.Second,
		Transport:     &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	if cfg == nil {
		return h
	}
	u, err := url.Parse(strings.TrimRight(cfg.ColabPool.ControlURL, "/"))
	if err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && u.RawQuery == "" && u.Fragment == "" && cfg.ColabPool.ControlSecretFile != "" {
		h.base, h.secretFile = u.String(), cfg.ColabPool.ControlSecretFile
	}
	return h
}

var colabPendingID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func (h *ColabControlHandler) call(ctx context.Context, method, path string, body any) (map[string]any, int, error) {
	if h == nil || h.base == "" {
		return nil, 503, errors.New("colab pool is not configured")
	}
	secret, err := os.ReadFile(h.secretFile)
	key := strings.TrimSpace(string(secret))
	if err != nil || len(key) < 16 || len(key) > 4096 || strings.ContainsAny(key, "\r\n") {
		return nil, 503, errors.New("colab pool authentication is unavailable")
	}
	var payload []byte
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, 400, errors.New("invalid pool request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, bytes.NewReader(payload))
	if err != nil {
		return nil, 503, errors.New("colab pool is not configured")
	}
	req.Header.Set("X-Internal-Auth", key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, 503, errors.New("colab pool is temporarily unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil || len(raw) > 2*1024*1024 {
		return nil, 502, errors.New("invalid pool response")
	}
	var result map[string]any
	if json.Unmarshal(raw, &result) != nil || result == nil {
		return nil, 502, errors.New("invalid pool response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 401 && resp.StatusCode != 403 {
			if detail, ok := result["detail"].(string); ok && len(detail) <= 1024 {
				return nil, resp.StatusCode, errors.New(logredact.RedactText(strings.ReplaceAll(detail, key, "[REDACTED]"), "token", "secret", "verifier"))
			}
		}
		return nil, 503, errors.New("colab pool rejected the operation; refresh its status")
	}
	sanitized, ok := sanitizeColab(result, key).(map[string]any)
	if !ok {
		return nil, 502, errors.New("invalid pool response")
	}
	return sanitized, 200, nil
}

func sanitizeColab(value any, secret string) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			switch strings.ToLower(k) {
			case "credentials", "access_token", "refresh_token", "id_token", "token", "api_key", "secret", "internal_secret", "password", "verifier", "code_verifier":
				continue
			}
			out[k] = sanitizeColab(item, secret)
		}
		return out
	case []any:
		for i := range v {
			v[i] = sanitizeColab(v[i], secret)
		}
		return v
	case string:
		return logredact.RedactText(strings.ReplaceAll(v, secret, "[REDACTED]"), "api_key", "secret", "verifier")
	default:
		return value
	}
}

func (h *ColabControlHandler) forward(c *gin.Context, method, path string, body any) {
	result, status, err := h.call(c.Request.Context(), method, path, body)
	c.Header("Cache-Control", "no-store")
	if err != nil {
		response.Error(c, status, err.Error())
		return
	}
	response.Success(c, result)
}

func (h *ColabControlHandler) Snapshot(c *gin.Context) {
	state, status, err := h.call(c.Request.Context(), "GET", "/state", nil)
	if err != nil {
		response.Error(c, status, err.Error())
		return
	}
	telemetry, _, telemetryErr := h.call(c.Request.Context(), "GET", "/telemetry", nil)
	// New-API gateway usage is not Sub2API usage. The frontend queries Sub2API's
	// account-filtered usage API separately; retain only worker hardware metrics.
	delete(telemetry, "gateway")
	delete(telemetry, "state")
	c.Header("Cache-Control", "no-store")
	response.Success(c, gin.H{"state": state, "telemetry": telemetry, "telemetry_available": telemetryErr == nil})
}

func decodeColab(c *gin.Context, target any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16384)
	d := json.NewDecoder(c.Request.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		response.BadRequest(c, "Invalid Pool request")
		return false
	}
	if d.Decode(new(any)) != io.EOF {
		response.BadRequest(c, "Invalid Pool request")
		return false
	}
	return true
}

func colabID(c *gin.Context) string {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		response.BadRequest(c, "Invalid Pool resource ID")
		return ""
	}
	return strconv.FormatInt(id, 10)
}

func (h *ColabControlHandler) AuthStart(c *gin.Context) { h.forward(c, "POST", "/auth/start", nil) }
func (h *ColabControlHandler) AuthPending(c *gin.Context) {
	if !colabPendingID.MatchString(c.Param("pid")) {
		response.BadRequest(c, "Invalid authorization session")
		return
	}
	h.forward(c, "GET", "/auth/pending/"+c.Param("pid"), nil)
}
func (h *ColabControlHandler) AuthFinish(c *gin.Context) {
	var body struct {
		PID  string `json:"pid"`
		Code string `json:"code"`
	}
	if !decodeColab(c, &body) {
		return
	}
	if !colabPendingID.MatchString(body.PID) || len(body.Code) > 2048 {
		response.BadRequest(c, "Invalid authorization session or code")
		return
	}
	h.forward(c, "POST", "/auth/finish", body)
}
func (h *ColabControlHandler) UpdateAccount(c *gin.Context) {
	id := colabID(c)
	if id == "" {
		return
	}
	var body struct {
		Enabled  *bool   `json:"enabled,omitempty"`
		Priority *int    `json:"priority,omitempty"`
		Note     *string `json:"note,omitempty"`
		Refresh  bool    `json:"refresh,omitempty"`
	}
	if !decodeColab(c, &body) {
		return
	}
	if body.Priority != nil && (*body.Priority < -10000 || *body.Priority > 10000) || body.Note != nil && len(*body.Note) > 2000 {
		response.BadRequest(c, "Invalid account priority or note")
		return
	}
	h.forward(c, "POST", "/accounts/"+id, body)
}
func (h *ColabControlHandler) DeleteAccount(c *gin.Context) {
	if id := colabID(c); id != "" {
		h.forward(c, "DELETE", "/accounts/"+id, nil)
	}
}
func (h *ColabControlHandler) StartInstance(c *gin.Context) {
	var body struct {
		AccountID *int64 `json:"account_id,omitempty"`
	}
	if !decodeColab(c, &body) {
		return
	}
	if body.AccountID != nil && *body.AccountID < 1 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	h.startMu.Lock()
	defer h.startMu.Unlock()
	state, status, err := h.call(c.Request.Context(), "GET", "/state", nil)
	if err != nil {
		response.Error(c, status, err.Error())
		return
	}
	settings, _ := state["settings"].(map[string]any)
	instances, _ := state["instances"].([]any)
	limit, _ := settings["max_instances"].(float64)
	active := 0
	for _, value := range instances {
		inst, _ := value.(map[string]any)
		switch inst["state"] {
		case "starting", "ready", "draining", "stopping":
			active++
		}
	}
	if limit < 1 || float64(active) >= limit {
		response.BadRequest(c, "Pool capacity limit reached")
		return
	}
	if body.AccountID != nil {
		eligible := false
		accounts, _ := state["accounts"].([]any)
		minimum, _ := settings["min_start_cu"].(float64)
		for _, value := range accounts {
			a, _ := value.(map[string]any)
			id, _ := a["id"].(float64)
			balance, hasBalance := a["balance"].(float64)
			if id != float64(*body.AccountID) {
				continue
			}
			enabled := a["enabled"] == float64(1) || a["enabled"] == true
			assignments, _ := a["assignments"].(float64)
			gpus, _ := a["gpus"].([]any)
			supportsGPU := false
			for _, gpu := range gpus {
				if gpu == settings["gpu"] {
					supportsGPU = true
				}
			}
			eligible = enabled && a["status"] == "ok" && a["instance"] == nil && a["cooldown"] != true && assignments == 0 && supportsGPU && hasBalance && balance >= minimum
		}
		if !eligible {
			response.BadRequest(c, "Account is unavailable or has insufficient compute credits")
			return
		}
	}
	h.forward(c, "POST", "/instances", body)
}
func (h *ColabControlHandler) StopInstance(c *gin.Context) {
	id := colabID(c)
	if id == "" {
		return
	}
	var body struct {
		Drain bool `json:"drain"`
	}
	if !decodeColab(c, &body) {
		return
	}
	h.forward(c, "POST", "/instances/"+id+"/stop", body)
}

func (h *ColabControlHandler) UpdateSettings(c *gin.Context) {
	var body map[string]any
	if !decodeColab(c, &body) {
		return
	}
	if len(body) == 0 {
		response.BadRequest(c, "Pool settings must not be empty")
		return
	}
	ranges := map[string][2]float64{"max_instances": {1, 16}, "idle_stop_min": {1, 1440}, "switch_below_cu": {0, 100000}, "min_start_cu": {0, 100000}, "scale_up_running": {1, 10000}, "scale_up_sustain_sec": {15, 3600}, "scale_down_min": {1, 1440}}
	for key, value := range body {
		valid := false
		switch key {
		case "auto_start", "auto_rotate", "autoscale":
			_, valid = value.(bool)
		case "gpu":
			valid = value == "G4" || value == "A100" || value == "L4" || value == "T4"
		default:
			r, known := ranges[key]
			n, numeric := value.(float64)
			valid = known && numeric && !math.IsNaN(n) && n >= r[0] && n <= r[1] && (strings.HasSuffix(key, "_cu") || n == math.Trunc(n))
		}
		if !valid {
			response.BadRequest(c, "Invalid Pool setting: "+key)
			return
		}
	}
	h.forward(c, "POST", "/settings", body)
}
