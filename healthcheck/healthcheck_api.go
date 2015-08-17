/*
Manage database users

*/

package healthcheck

import (
	"net/http"

	"github.com/gorilla/context"
	"github.com/zenazn/goji"
	"github.com/zenazn/goji/web"
	"github.com/zenazn/goji/web/middleware"
)

func MakeHealthRouter(hcHandler *HealthCheckHandler) http.Handler {

	health := web.New()

	goji.Handle("/health", health)

	health.Use(middleware.EnvInit)
	health.Use(middleware.RequestID)
	health.Use(middleware.RealIP)
	health.Use(middleware.Logger)
	health.Use(context.ClearHandler)

	health.Get("/health", hcHandler.HealthCheck)

	return health
}
