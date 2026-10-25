package handler

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// Its HTTP methods are registered only inside the existing admin-authenticated group.
type ConversationLogHandler struct {
	service *service.ConversationLogService
}

func NewConversationLogHandler(s *service.ConversationLogService) *ConversationLogHandler {
	return &ConversationLogHandler{service: s}
}

func (h *ConversationLogHandler) Middleware() gin.HandlerFunc {
	if h == nil {
		return func(c *gin.Context) { c.Next() }
	}
	return ConversationLoggerMiddleware(h.service)
}

func (h *ConversationLogHandler) GetConfig(c *gin.Context) {
	cfg, err := h.service.GetConfig(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"enabled": cfg.Enabled, "retention_days": cfg.RetentionDays, "max_text_bytes": service.ConversationTextLimit, "stats": h.service.Stats()})
}

func (h *ConversationLogHandler) UpdateConfig(c *gin.Context) {
	var body struct {
		Enabled       *bool `json:"enabled" binding:"required"`
		RetentionDays *int  `json:"retention_days" binding:"required,min=0,max=3650"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.BadRequest(c, "enabled and retention_days (0-3650) are required")
		return
	}
	if err := h.service.SetConfig(c.Request.Context(), service.ConversationLogConfig{Enabled: *body.Enabled, RetentionDays: *body.RetentionDays}); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	h.GetConfig(c)
}

func (h *ConversationLogHandler) GetUsage(c *gin.Context) { h.get(c, false) }
func (h *ConversationLogHandler) GetError(c *gin.Context) { h.get(c, true) }
func (h *ConversationLogHandler) get(c *gin.Context, byError bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid record id")
		return
	}
	var entry *service.ConversationLog
	if byError {
		entry, err = h.service.GetByErrorID(c.Request.Context(), id)
	} else {
		entry, err = h.service.GetByUsageID(c.Request.Context(), id)
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, entry)
}
