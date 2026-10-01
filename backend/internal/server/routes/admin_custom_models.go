package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/gin-gonic/gin"
)

func registerCustomModelRoutes(admin *gin.RouterGroup, h *handler.Handlers) {
	models := admin.Group("/custom-models")
	models.GET("", h.Admin.CustomModel.List)
	models.GET("/:id", h.Admin.CustomModel.Get)
	models.POST("", h.Admin.CustomModel.Create)
	models.PUT("/:id", h.Admin.CustomModel.Update)
	models.DELETE("/:id", h.Admin.CustomModel.Delete)
}
