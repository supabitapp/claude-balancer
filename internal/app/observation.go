package app

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

const httpResponseIOWait = 10 * time.Second

type observationKey struct{}

type requestObservation struct {
	admitted bool
}

func observation(context.Context) *requestObservation { return &requestObservation{} }

func (*requestObservation) event(context.Context, string, ...attribute.KeyValue) {}

func (*requestObservation) reject(context.Context, int, string) {
}

func firstWebSocketHeader(headers http.Header, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func appendUniqueOwner(owners []string, owner string) []string {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return owners
	}
	for _, current := range owners {
		if current == owner {
			return owners
		}
	}
	return append(owners, owner)
}
