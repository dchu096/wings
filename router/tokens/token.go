package tokens

import (
	"strings"
)

// JwtScope identifies the intended purpose of a panel-signed JWT.
// Without scope checks, tokens issued for websocket/download could be replayed
// against upload endpoints (CVE-2026-54593 / GHSA-8r6w-3qq5-4p4r).
type JwtScope string

const (
	Websocket      = JwtScope("websocket")
	FileUpload     = JwtScope("file-upload")
	FileDownload   = JwtScope("file-download")
	BackupDownload = JwtScope("backup-download")
	ServerTransfer = JwtScope("transfer")
)

// Scoped embeds a space-delimited scope claim from panel JWTs.
type Scoped struct {
	Scope string `json:"scope"`
}

// Scopes returns the individual scope values.
func (s Scoped) Scopes() []string {
	if s.Scope == "" {
		return nil
	}
	return strings.Split(s.Scope, " ")
}

// HasScope reports whether the token carries the given scope.
// Tokens with an empty scope claim are rejected (fail closed for new panels
// that always set scope; old tokens without scope also fail closed).
func (s Scoped) HasScope(scope JwtScope) bool {
	for _, v := range s.Scopes() {
		if v == string(scope) {
			return true
		}
	}
	return false
}
