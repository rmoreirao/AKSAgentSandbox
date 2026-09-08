package api

import (
	"encoding/json"
	"net/http"
)

type Error struct {
	Code    string      `json:"code"`
	Message string      `json:"message"`
	Details interface{} `json:"details,omitempty"`
}

func writeAPIError(writer http.ResponseWriter, status int, code, message string, details interface{}) {
	writeJSON(writer, status, Error{Code: code, Message: message, Details: details})
}

func writeJSON(writer http.ResponseWriter, status int, value interface{}) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
