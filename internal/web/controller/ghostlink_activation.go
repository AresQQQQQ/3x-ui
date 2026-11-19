package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mhsanaei/3x-ui/v3/internal/ghostlink"
	"github.com/mhsanaei/3x-ui/v3/internal/web/session"
)

func ghostLinkActivationStatus(c *gin.Context) {
	if session.GetLoginUser(c) == nil {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}
	c.Header("Cache-Control", "no-store")
	status, err := ghostlink.DefaultInstaller().Status()
	if err != nil {
		jsonMsg(c, "GhostLink activation status", err)
		return
	}
	jsonObj(c, status, nil)
}

func startGhostLinkActivation(c *gin.Context) {
	if session.GetLoginUser(c) == nil {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}
	c.Header("Cache-Control", "no-store")
	var body struct {
		Note string `json:"note" form:"note"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if err := c.ShouldBind(&body); err != nil && !errors.Is(err, io.EOF) {
		jsonMsg(c, "GhostLink activation", errors.New("invalid application"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if err := ghostlink.DefaultInstaller().Start(ctx, body.Note); err != nil {
		jsonMsg(c, "GhostLink activation", err)
		return
	}
	status, err := ghostlink.DefaultInstaller().Status()
	jsonObj(c, status, err)
}
