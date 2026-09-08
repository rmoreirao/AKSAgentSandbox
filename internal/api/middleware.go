package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/rmoreirao/AKSAgentSandbox/internal/auth"
)

type identityContextKey struct{}

func sessionMiddleware(sessions auth.SessionManager, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		token, ok := authBearerToken(request)
		if !ok {
			writeAPIError(writer, http.StatusUnauthorized, "unauthenticated", "valid platform session required", nil)
			return
		}
		claims, err := sessions.Verify(request.Context(), token)
		if err != nil {
			code := "invalid_session"
			if errors.Is(err, auth.ErrExpiredSession) {
				code = "expired_session"
			}
			writeAPIError(writer, http.StatusUnauthorized, code, "valid platform session required", nil)
			return
		}
		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), identityContextKey{}, claims)))
	})
}

func requestClaims(request *http.Request) auth.SessionClaims {
	value, _ := request.Context().Value(identityContextKey{}).(auth.SessionClaims)
	return value
}
