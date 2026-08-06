// Package health exposes the liveness endpoint used by load balancers,
// container orchestrators, and uptime checks.
package health

import (
	"net/http"

	"github.com/tapago/tapago-api/internal/handler"
)

// Response is the body returned by the health endpoint.
type Response struct {
	Status string `json:"status"`
}

// Check reports that the process is alive and serving HTTP.
//
// This is deliberately a liveness check with no dependency probing: an
// orchestrator restarting the API because Postgres blipped would turn a
// recoverable database outage into a full outage. A separate readiness
// endpoint can probe dependencies once we need one.
func Check(w http.ResponseWriter, _ *http.Request) {
	handler.JSON(w, http.StatusOK, Response{Status: "ok"})
}
