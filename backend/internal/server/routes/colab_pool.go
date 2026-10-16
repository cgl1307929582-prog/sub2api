package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/gin-gonic/gin"
)

func registerColabPoolRoutes(admin *gin.RouterGroup, h *handler.Handlers) {
	pool := admin.Group("/colab-pool")
	control := h.Admin.ColabPool
	pool.GET("/snapshot", control.Snapshot)
	pool.POST("/auth/start", control.AuthStart)
	pool.GET("/auth/pending/:pid", control.AuthPending)
	pool.POST("/auth/finish", control.AuthFinish)
	pool.PATCH("/accounts/:id", control.UpdateAccount)
	pool.DELETE("/accounts/:id", control.DeleteAccount)
	pool.POST("/instances", control.StartInstance)
	pool.POST("/instances/:id/stop", control.StopInstance)
	pool.PUT("/settings", control.UpdateSettings)
}
