package auth

// AuthorizeOwner compares only immutable GitHub user IDs. GitHub logins are
// mutable display metadata and must never be used for owner authorization.
func AuthorizeOwner(claims SessionClaims, ownerUserID string) error {
	if claims.Subject == "" || ownerUserID == "" || claims.Subject != ownerUserID {
		return ErrOwnerMismatch
	}
	return nil
}
