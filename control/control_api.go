package control

import (
	"net/http"

	"github.com/gorilla/context"
	"github.com/twitchscience/rs_ingester/lib"
	"github.com/zenazn/goji/web"
	"github.com/zenazn/goji/web/middleware"
)

// NewControlRouter instantiates an http.Handler with the control routes
func NewControlRouter(cHandler *Handler) http.Handler {
	control := web.New()

	control.Use(middleware.EnvInit)
	control.Use(middleware.RequestID)
	control.Use(middleware.RealIP)
	control.Use(lib.SimpleLogger)
	control.Use(context.ClearHandler)

	control.Post("/control/ingest", cHandler.ForceIngest)
	control.Get("/control/table_exists/:id", cHandler.TableExists)
	control.Post("/control/increment_version/:id", cHandler.IncrementVersion)

	return control
}
